package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// kitInspectTimeout bounds each kit's boot-time inspection independently.
// Kits are inspected concurrently, so one slow reference costs its own
// timeout, not the whole set's. A var, not a const, so a test can shorten it.
// Tests mutate this global directly and restore it via t.Cleanup: safe only
// because internal/ has no t.Parallel() today. Don't add t.Parallel() to
// this package without revisiting that.
var kitInspectTimeout = 15 * time.Second

// KitInfo is the part of a kit's manifest the node checks before advertising the
// kit. It is deliberately not the SDK's kit.Info: the node needs a handful of
// facts, and the SDK's shape tracks an EXPERIMENTAL upstream schema.
type KitInfo struct {
	Kind          string // "mixin" | "sandbox"
	HasResources  bool   // sandbox.resources is non-empty
	HasRunOptions bool   // sandbox.command.default is non-empty
	HasTemplate   bool   // sandbox.image is set: would swap the sandbox's base image
	HasVolumes    bool   // volumes is non-empty: host mounts bypassing workspaceResolver's read-only guarantee (ADR-0015)
	// HiddenCapability names a kit spec v3 capability that `kit inspect` does
	// not report, so none of the checks above can see what it does.
	HiddenCapability string
	// DescriptorError is why a kit spec v3 descriptor could not be read, which
	// leaves its capabilities unknown.
	DescriptorError string
	// LegacySchema is set for a kit spec v1/v2 kit. sbx v0.51.0 runs the
	// built-in agents from v3 kits and refuses to mix one with a v1/v2 kit, so
	// every create using it would fail.
	LegacySchema bool
}

// kitSpecV3 is the schemaVersion of kit spec v3, which sbx v0.51.0 requires.
const kitSpecV3 = "3"

// inspectVisibleCapabilities are the kit spec v3 capability types whose effect
// `kit inspect --json` reports, so admit() judges them through the other
// fields. Probed against sbx v0.51.0: network-policy@1 shows as
// permissions.network, port@1 as ports, volume@1 as volumes (and so is refused
// by HasVolumes). "Visible" means inspect reports it, not that admit() checks
// it: network allows and ports carry no capacity or isolation harm and pass.
// Anything else -- host-mount@1 shows nothing at all -- is refused, including
// later versions of these types, until probed.
var inspectVisibleCapabilities = map[string]bool{
	"com.docker.sandbox/network-policy@1": true,
	"com.docker.sandbox/port@1":           true,
	"com.docker.sandbox/volume@1":         true,
}

// hiddenCapability returns the first capability in a v3 kit's descriptor that
// inspect cannot show, or "" when every one is visible. Only a local directory
// kit's spec.yaml can be read; any other reference (OCI, ZIP, git) returns an
// error, as does a descriptor that will not read or parse, and admit() refuses
// on it: fail closed.
//
// ponytail: refusing every non-directory v3 kit is the ceiling. Reading an OCI
// kit's descriptor would need the image's layers; add that when a published v3
// mixin is actually wanted.
func hiddenCapability(ref string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(ref, "spec.yaml"))
	if err != nil {
		return "", err
	}
	var d struct {
		Capabilities []struct {
			Type string `yaml:"type"`
		} `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return "", err
	}
	for _, c := range d.Capabilities {
		if !inspectVisibleCapabilities[c.Type] {
			return c.Type, nil
		}
	}
	return "", nil
}

// admit reports why a kit must not be advertised, or nil when it may be.
//
// Only kind "mixin" is supported: a "sandbox" kit supplies the base image, which
// would make the scheduler's template constraint a lie. The SDK documents the
// whole sandbox block, and the security block that pairs with it, as meaningful
// only for a "sandbox" kit and empty for a mixin -- an expectation, not a
// promise. Four fields are checked here, because each could hand a sandbox more
// than the node admitted if that expectation ever breaks: resources and
// command.default could exceed the capacity this node advertised to the swarm;
// image would change the base image, the exact harm the Kind check above exists
// to prevent, reached by a different field; and volumes would mount host paths
// outside workspaceResolver, bypassing the ADR-0015 read-only guarantee. The
// rest (sourceURL, entrypoint, agentInstructions, build, security) carry no
// capacity or isolation harm and are not checked.
//
// Kit spec v3 (sbx v0.51.0) declares behaviour as capabilities, and inspect
// reports only some of them through those fields. HiddenCapability refuses a
// v3 kit carrying any other, since nothing here could see what it does, and a
// v1/v2 kit, which sbx v0.51.0 will not combine with its v3 built-in agents.
func admit(i KitInfo) error {
	if i.Kind != "mixin" {
		return fmt.Errorf("kind is %q, want \"mixin\"", i.Kind)
	}
	if i.HasResources {
		return fmt.Errorf("mixin declares resources, which could exceed admitted capacity")
	}
	if i.HasRunOptions {
		return fmt.Errorf("mixin declares runOptions, which could exceed admitted capacity")
	}
	if i.HasTemplate {
		return fmt.Errorf("mixin declares a template, which would change the sandbox's base image")
	}
	if i.HasVolumes {
		return fmt.Errorf("mixin declares volumes, which would mount host paths outside the workspace resolver")
	}
	if i.LegacySchema {
		return fmt.Errorf("kit is not kit spec v3; sbx v0.51.0 refuses to mix a v1/v2 kit with the v3 built-in agents")
	}
	if i.DescriptorError != "" {
		return fmt.Errorf("kit spec v3 descriptor cannot be read, so its capabilities cannot be checked: %s", i.DescriptorError)
	}
	if i.HiddenCapability != "" {
		return fmt.Errorf("mixin declares a capability kit inspect cannot show, so it cannot be checked: %s", i.HiddenCapability)
	}
	return nil
}

// admitKits inspects every configured kit concurrently and returns the name to
// reference map this node resolves and advertises.
//
// The two failures are treated differently on purpose. A kit that inspects
// cleanly and fails admit() is dropped: that is an operator mistake and it will
// never fix itself. A kit whose reference fails to LOAD is kept: a typo and a
// briefly unreachable registry look identical from here, and dropping it would
// quietly shrink this node's advertised capacity. A kept-but-broken kit fails at
// create instead, carrying the CLI's own message.
//
// This runs to completion before boot continues. The gossiped NodeState is built
// once at boot and nothing re-gossips it, so a later advertisement would never
// reach the swarm.
func admitKits(ctx context.Context, inspect func(context.Context, string) (KitInfo, error), kits map[string]string, log *slog.Logger) map[string]string {
	admitted := make(map[string]string, len(kits))
	if len(kits) == 0 {
		return admitted
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for name, ref := range kits {
		wg.Add(1)
		go func() {
			defer wg.Done()

			kctx, kcancel := context.WithTimeout(ctx, kitInspectTimeout)
			info, err := inspect(kctx, ref)
			kcancel()

			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.Warn("kit: inspect timed out, advertising anyway", "kit", name, "ref", ref, "timeout", kitInspectTimeout)
				} else {
					log.Warn("kit: inspect failed, advertising anyway", "kit", name, "ref", ref, "err", err)
				}
			} else if rejected := admit(info); rejected != nil {
				log.Error("kit: refused, not advertised", "kit", name, "ref", ref, "reason", rejected)
				return
			}
			mu.Lock()
			admitted[name] = ref
			mu.Unlock()
		}()
	}
	wg.Wait()
	return admitted
}

// kitNames returns a kit map's names, sorted, so the advertisement is stable.
func kitNames(kits map[string]string) []string {
	out := make([]string, 0, len(kits))
	for name := range kits {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
