package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdkclient "github.com/squall-chua/sbx-go-sdk/client"
	sdkexec "github.com/squall-chua/sbx-go-sdk/exec"
	sdkpolicy "github.com/squall-chua/sbx-go-sdk/policy"
	sdksandbox "github.com/squall-chua/sbx-go-sdk/sandbox"
	sdksecret "github.com/squall-chua/sbx-go-sdk/secret"
	sdktemplate "github.com/squall-chua/sbx-go-sdk/template"
)

// WorkspaceResolver maps a logical workspace name to a host path + ro flag.
type WorkspaceResolver func(name string) (hostPath string, readOnly bool, ok bool)

// SDKBackend implements Backend over sbx-go-sdk v0.1.2. Workspaces are resolved
// to host paths via the resolver (config-provided). It is a thin translation
// layer: lifecycle/exec/ports/files all resolve a *sandbox.Sandbox handle by
// name and call the SDK, mapping the SDK's not-found sentinel to ErrNotFound.
type SDKBackend struct {
	cl      *sdkclient.Client
	resolve WorkspaceResolver
}

// NewSDKBackend connects to the local daemon (auto-starting it if needed) and
// requires a compatible daemon version.
func NewSDKBackend(ctx context.Context, resolve WorkspaceResolver) (*SDKBackend, error) {
	cl, err := sdkclient.New(ctx, sdkclient.WithAutoStart(), sdkclient.WithStrictVersion())
	if err != nil {
		return nil, fmt.Errorf("connect daemon: %w", err)
	}
	return &SDKBackend{cl: cl, resolve: resolve}, nil
}

// translateNotFound maps the SDK's not-found sentinel to sandbox.ErrNotFound.
func translateNotFound(err error) error {
	if errors.Is(err, sdkclient.ErrSandboxNotFound) {
		return ErrNotFound
	}
	return err
}

// handle resolves a sandbox handle by name, translating not-found.
func (b *SDKBackend) handle(ctx context.Context, name string) (*sdksandbox.Sandbox, error) {
	sb, err := sdksandbox.Get(ctx, b.cl, name)
	if err != nil {
		return nil, translateNotFound(err)
	}
	return sb, nil
}

func (b *SDKBackend) Create(ctx context.Context, spec CreateSpec) (BackendSandbox, error) {
	opts := []sdksandbox.Option{sdksandbox.WithName(spec.Name)}
	if spec.CPUs > 0 {
		opts = append(opts, sdksandbox.WithCPUs(spec.CPUs))
	}
	if spec.MemoryBytes > 0 {
		opts = append(opts, sdksandbox.WithMemory(memString(spec.MemoryBytes)))
	}
	if spec.Agent != "" {
		opts = append(opts, sdksandbox.WithAgent(spec.Agent))
	}
	if spec.Template != "" {
		opts = append(opts, sdksandbox.WithTemplate(spec.Template))
	}
	if spec.Clone {
		opts = append(opts, sdksandbox.WithClone())
	}
	for i, w := range spec.Workspaces {
		host, ro, ok := b.resolve(w.Name)
		if !ok {
			return BackendSandbox{}, fmt.Errorf("unknown workspace %q", w.Name)
		}
		path := host
		// In --clone mode sbx clones the PRIMARY (first) workspace and mounts it
		// read-only itself; it rejects an explicit ":ro" on the primary ("primary
		// workspace must be read/write"). Extra workspaces may still be read-only.
		primaryClone := spec.Clone && i == 0
		if (ro || w.ReadOnly) && !primaryClone {
			path += ":ro"
		}
		opts = append(opts, sdksandbox.WithWorkspace(path))
	}
	sb, err := sdksandbox.Create(ctx, b.cl, opts...)
	if err != nil {
		return BackendSandbox{}, err
	}
	return BackendSandbox{Name: sb.Name(), Status: sb.State()}, nil
}

func (b *SDKBackend) Get(ctx context.Context, name string) (BackendSandbox, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return BackendSandbox{}, err
	}
	return BackendSandbox{Name: sb.Name(), Status: sb.State()}, nil
}

func (b *SDKBackend) List(ctx context.Context) ([]BackendSandbox, error) {
	sbs, err := sdksandbox.List(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]BackendSandbox, 0, len(sbs))
	for _, sb := range sbs {
		out = append(out, BackendSandbox{Name: sb.Name(), Status: sb.State()})
	}
	return out, nil
}

func (b *SDKBackend) Start(ctx context.Context, name string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.Start(ctx)
}

func (b *SDKBackend) Stop(ctx context.Context, name string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.Stop(ctx)
}

func (b *SDKBackend) Remove(ctx context.Context, name string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.Remove(ctx)
}

func (b *SDKBackend) Exec(ctx context.Context, name string, cmd []string, opts ExecOpts) (ExecResult, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	popts := []sdkexec.ProcessOption{
		sdkexec.WithAutoStart(),
		sdkexec.WithMultiplexed(&stdout, &stderr),
	}
	if opts.Workdir != "" {
		popts = append(popts, sdkexec.WithWorkdir(opts.Workdir))
	}
	if len(opts.Env) > 0 {
		popts = append(popts, sdkexec.WithEnv(opts.Env))
	}
	code, _, err := sdkexec.Exec(ctx, sb, cmd, popts...)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func (b *SDKBackend) ExecDetached(ctx context.Context, name string, cmd []string, opts ExecOpts) (string, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return "", err
	}
	popts := []sdkexec.ProcessOption{sdkexec.WithAutoStart()}
	if opts.Workdir != "" {
		popts = append(popts, sdkexec.WithWorkdir(opts.Workdir))
	}
	if len(opts.Env) > 0 {
		popts = append(popts, sdkexec.WithEnv(opts.Env))
	}
	return sdkexec.ExecDetached(ctx, sb, cmd, popts...)
}

func (b *SDKBackend) PollDetached(ctx context.Context, name, detachedID string) (DetachedStatus, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return DetachedStatus{}, err
	}
	st, err := sdkexec.InspectExec(ctx, sb, detachedID)
	if err != nil {
		return DetachedStatus{}, err
	}
	return DetachedStatus{Done: !st.Running, ExitCode: st.ExitCode}, nil
}

func (b *SDKBackend) PublishPort(ctx context.Context, name string, containerPort int) (PublishedPort, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return PublishedPort{}, err
	}
	ports, err := sb.PublishPort(ctx, sdksandbox.Port{SandboxPort: containerPort})
	if err != nil {
		return PublishedPort{}, err
	}
	for _, p := range ports {
		if p.SandboxPort == containerPort {
			return PublishedPort{ContainerPort: p.SandboxPort, HostPort: p.HostPort}, nil
		}
	}
	return PublishedPort{ContainerPort: containerPort}, nil
}

func (b *SDKBackend) Ports(ctx context.Context, name string) ([]PublishedPort, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return nil, err
	}
	ports, err := sb.Ports(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PublishedPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, PublishedPort{ContainerPort: p.SandboxPort, HostPort: p.HostPort})
	}
	return out, nil
}

func (b *SDKBackend) UnpublishPort(ctx context.Context, name string, containerPort int) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	// The daemon requires a HOST_PORT:SANDBOX_PORT spec for unpublish, so resolve
	// the host port(s) currently mapped to this container port and unpublish each.
	ports, err := sb.Ports(ctx)
	if err != nil {
		return err
	}
	for _, p := range ports {
		if p.SandboxPort == containerPort {
			if err := sb.UnpublishPort(ctx, strconv.Itoa(p.HostPort)+":"+strconv.Itoa(containerPort)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *SDKBackend) CopyTo(ctx context.Context, name, localPath, remotePath string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.CopyTo(ctx, localPath, remotePath)
}

func (b *SDKBackend) CopyFrom(ctx context.Context, name, remotePath, localPath string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.CopyFrom(ctx, remotePath, localPath)
}

// memString converts a byte count to a Docker-style size string for the SDK's
// WithMemory option (which takes a human string like "8g"), choosing the
// largest exact binary unit.
func memString(b int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case b%gib == 0:
		return strconv.FormatInt(b/gib, 10) + "g"
	case b%mib == 0:
		return strconv.FormatInt(b/mib, 10) + "m"
	case b%kib == 0:
		return strconv.FormatInt(b/kib, 10) + "k"
	default:
		return strconv.FormatInt(b, 10) + "b"
	}
}

// Stats returns a point-in-time resource snapshot for the named sandbox.
// Maps to exec.Stats in sbx-go-sdk v0.1.2.
func (b *SDKBackend) Stats(ctx context.Context, name string) (Usage, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return Usage{}, err
	}
	u, err := sdkexec.Stats(ctx, sb)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		Cores:         u.Cores,
		CPUPercent:    u.CPUPercent,
		MemTotalKB:    int64(u.MemTotalKB),
		MemUsedKB:     int64(u.MemUsedKB),
		DiskTotalGB:   u.DiskTotalGB,
		DiskUsedGB:    u.DiskUsedGB,
		UptimeSeconds: int64(u.UptimeSeconds),
	}, nil
}

// Logs follows the log file at path inside the named sandbox. Lines are
// streamed to out until ctx is cancelled or the session ends.
// Maps to exec.Logs in sbx-go-sdk v0.1.2.
func (b *SDKBackend) Logs(ctx context.Context, name, path string, out chan<- LogLine) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	sess, err := sdkexec.Logs(ctx, sb, path)
	if err != nil {
		return err
	}
	// Unblock a parked scanner.Scan() when ctx is cancelled: closing the session
	// closes the underlying reader, so Scan returns instead of leaking.
	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()
	go func() {
		defer sess.Close()
		scanner := bufio.NewScanner(sess.Stdout())
		for scanner.Scan() {
			select {
			case out <- LogLine{Line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case out <- LogLine{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return nil
}

// BlockedEgress returns the daemon-wide set of blocked (host, vm) pairs.
// Maps to policy.Log in sbx-go-sdk v0.1.2.
func (b *SDKBackend) BlockedEgress(ctx context.Context) ([]BlockedHost, error) {
	pl, err := sdkpolicy.Log(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedHost, 0, len(pl.BlockedHosts))
	for _, e := range pl.BlockedHosts {
		out = append(out, BlockedHost{Host: e.Host, VMName: e.VMName})
	}
	return out, nil
}

// Policy methods — delegate to sdkpolicy (shells out to `sbx policy`).

func (b *SDKBackend) PolicyAllow(ctx context.Context, scope, host string) error {
	return sdkpolicy.Allow(ctx, b.cl, scope, host)
}

func (b *SDKBackend) PolicyDeny(ctx context.Context, scope, host string) error {
	return sdkpolicy.Deny(ctx, b.cl, scope, host)
}

func (b *SDKBackend) PolicySetDefault(ctx context.Context, profile string) error {
	return sdkpolicy.SetDefault(ctx, b.cl, profile)
}

func (b *SDKBackend) PolicyRemoveRule(ctx context.Context, scope, resource string) error {
	return sdkpolicy.RemoveRule(ctx, b.cl, scope, resource)
}

func (b *SDKBackend) PolicyReset(ctx context.Context) error {
	return sdkpolicy.Reset(ctx, b.cl)
}

// PolicyList returns parsed rules. On ErrUnexpectedFormat it falls back to
// ListRaw and returns a single synthetic rule with Type:"raw".
func (b *SDKBackend) PolicyList(ctx context.Context, scope string) ([]PolicyRule, error) {
	rules, err := sdkpolicy.List(ctx, b.cl, scope)
	if err != nil {
		if errors.Is(err, sdkclient.ErrUnexpectedFormat) {
			raw, rerr := sdkpolicy.ListRaw(ctx, b.cl, scope)
			if rerr != nil {
				return nil, rerr
			}
			return []PolicyRule{{Type: "raw", Rule: raw}}, nil
		}
		return nil, err
	}
	out := make([]PolicyRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, PolicyRule{
			Provenance: r.Provenance,
			AppliesTo:  r.AppliesTo,
			Rule:       r.Rule,
			Type:       r.Type,
			Decision:   r.Decision,
			Resources:  strings.Join(r.Resources, ","),
		})
	}
	return out, nil
}

// PolicyProfiles returns the raw profile listing text as a single-element slice.
func (b *SDKBackend) PolicyProfiles(ctx context.Context) ([]string, error) {
	raw, err := sdkpolicy.Profiles(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	return []string{raw}, nil
}

// Secret methods — delegate to sdksecret (shells out to `sbx secret`).
// Values are NEVER stored or returned (spec §11).

func (b *SDKBackend) SecretSet(ctx context.Context, scope string, s CustomSecret) error {
	return sdksecret.SetCustom(ctx, b.cl, scope, sdksecret.CustomSecret{
		Host:  s.Host,
		Env:   s.Env,
		Value: s.Value, // passed to the CLI; never stored or logged here
	})
}

// SecretList returns the secret inventory with values masked (the SDK already
// returns ValueMasked, never the real value).
func (b *SDKBackend) SecretList(ctx context.Context, scope string) (Secrets, error) {
	secs, err := sdksecret.List(ctx, b.cl, scope)
	if err != nil {
		return Secrets{}, err
	}
	out := Secrets{}
	for _, st := range secs.Stored {
		out.Stored = append(out.Stored, StoredSecret{Name: st.Name})
	}
	for _, c := range secs.Custom {
		// Value field is intentionally empty — write-only (spec §11).
		out.Custom = append(out.Custom, CustomSecret{Host: c.Target, Env: c.Env})
	}
	return out, nil
}

// SecretRemove deletes a secret in scope. NOTE (unverified, integration-only):
// sdksecret.Remove documents its last arg as `service`, but custom secrets are
// keyed by target host. Whether passing the host here actually removes a
// set-custom entry needs a live-daemon integration test to confirm.
func (b *SDKBackend) SecretRemove(ctx context.Context, scope, host string) error {
	return sdksecret.Remove(ctx, b.cl, scope, host)
}

// ListTemplates returns the template refs the daemon holds (repository:tag).
// ponytail: ref format assumed repository:tag to match WithTemplate; confirm
// against a live daemon (integration-only) before relying on exact matching.
func (b *SDKBackend) ListTemplates(ctx context.Context) ([]string, error) {
	imgs, err := sdktemplate.List(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(imgs))
	for _, im := range imgs {
		ref := im.Repository
		if im.Tag != "" {
			ref += ":" + im.Tag
		}
		out = append(out, ref)
	}
	return out, nil
}

var _ Backend = (*SDKBackend)(nil)
