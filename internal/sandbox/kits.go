package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// kitInspectTimeout bounds each kit's boot-time inspection independently.
// Kits are inspected concurrently, so one slow reference costs its own
// timeout, not the whole set's. A var, not a const, so a test can shorten it.
var kitInspectTimeout = 15 * time.Second

// KitInfo is the part of a kit's manifest the node checks before advertising the
// kit. It is deliberately not the SDK's kit.Info: the node needs a handful of
// facts, and the SDK's shape tracks an EXPERIMENTAL upstream schema.
type KitInfo struct {
	Kind          string // "mixin" | "sandbox"
	HasResources  bool   // manifest.resources is non-empty
	HasRunOptions bool   // manifest.runOptions is non-empty
	HasTemplate   bool   // manifest.template is set: would swap the sandbox's base image
	HasVolumes    bool   // manifest.volumes is non-empty: host mounts bypassing workspaceResolver's read-only guarantee (ADR-0015)
}

// admit reports why a kit must not be advertised, or nil when it may be.
//
// Only kind "mixin" is supported: a "sandbox" kit supplies the base image, which
// would make the scheduler's template constraint a lie. The SDK documents nine
// further Manifest fields as meaningful only for a "sandbox" kit and empty for a
// mixin -- an expectation, not a promise. Four of those nine are checked here,
// because each could hand a sandbox more than the node admitted if that
// expectation ever breaks: resources and runOptions could exceed the capacity
// this node advertised to the swarm; template would change the base image, the
// exact harm the Kind check above exists to prevent, reached by a different
// field; and volumes would mount host paths outside workspaceResolver, bypassing
// the ADR-0015 read-only guarantee. The remaining five (sourceURL, binary,
// aiFilename, build, security) carry no capacity or isolation harm and are not
// checked.
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
