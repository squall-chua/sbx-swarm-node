package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	sdkclient "github.com/squall-chua/sbx-go-sdk/client"
	sdkexec "github.com/squall-chua/sbx-go-sdk/exec"
	sdkpolicy "github.com/squall-chua/sbx-go-sdk/policy"
	sdksandbox "github.com/squall-chua/sbx-go-sdk/sandbox"
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
	for _, w := range spec.Workspaces {
		host, ro, ok := b.resolve(w.Name)
		if !ok {
			return BackendSandbox{}, fmt.Errorf("unknown workspace %q", w.Name)
		}
		path := host
		if ro || w.ReadOnly {
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
	// The SDK's UnpublishPort takes a CLI port spec; the bare sandbox port is
	// the minimal accepted form.
	return sb.UnpublishPort(ctx, strconv.Itoa(containerPort))
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

var _ Backend = (*SDKBackend)(nil)
