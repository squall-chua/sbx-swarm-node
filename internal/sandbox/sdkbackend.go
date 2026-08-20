package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	sdkclient "github.com/squall-chua/sbx-go-sdk/client"
	sdkexec "github.com/squall-chua/sbx-go-sdk/exec"
	sdkkit "github.com/squall-chua/sbx-go-sdk/kit"
	sdkpolicy "github.com/squall-chua/sbx-go-sdk/policy"
	sdksandbox "github.com/squall-chua/sbx-go-sdk/sandbox"
	sdksecret "github.com/squall-chua/sbx-go-sdk/secret"
	sdktemplate "github.com/squall-chua/sbx-go-sdk/template"
)

// WorkspaceResolver maps a logical workspace name to a host path + ro flag.
type WorkspaceResolver func(name string) (hostPath string, readOnly bool, ok bool)

// SDKBackend implements Backend over sbx-go-sdk v0.1.11. Workspaces are resolved
// to host paths via the resolver (config-provided). It is a thin translation
// layer: lifecycle/exec/ports/files all resolve a *sandbox.Sandbox handle by
// name and call the SDK, mapping the SDK's not-found sentinel to ErrNotFound.
type SDKBackend struct {
	cl      *sdkclient.Client
	resolve WorkspaceResolver
	kits    map[string]string // admitted kit name -> reference
	log     *slog.Logger
}

// NewSDKBackend connects to the local daemon (auto-starting it if needed) and
// admits the configured kits. kits maps a caller-facing kit name to its
// configured reference; only admitted kits are resolvable and advertised.
//
// The daemon version is NOT enforced. The SDK's WithStrictVersion compared
// api_version by exact string equality, and api_version bumps on every sbx
// release — so a release with byte-identical wire types still blocked node
// boot. A drifted daemon is logged once here and left running.
// ponytail: warn-only; add a floor check if a real incompatibility shows up.
func NewSDKBackend(ctx context.Context, resolve WorkspaceResolver, kits map[string]string, log *slog.Logger) (*SDKBackend, error) {
	cl, err := sdkclient.New(ctx, sdkclient.WithAutoStart())
	if err != nil {
		return nil, fmt.Errorf("connect daemon: %w", err)
	}
	if h, err := cl.DaemonHealth(ctx); err == nil && h.APIVersion != sdkclient.TestedAPIVersion {
		log.Warn("sbx daemon version differs from the one this SDK was tested against",
			"daemon_version", h.Version, "daemon_api_version", h.APIVersion,
			"sdk_client_version", sdkclient.ClientVersion, "sdk_tested_api_version", sdkclient.TestedAPIVersion)
	}
	// log is set here, not after: admitKits runs before the return, and a later
	// warning path would panic on a nil logger (see logger() below).
	b := &SDKBackend{cl: cl, resolve: resolve, log: log}
	b.kits = admitKits(ctx, b.inspectKit, kits, log)
	return b, nil
}

// logger never returns nil. Only NewSDKBackend sets log, so a construction path
// that adds a field and forgets the assignment would otherwise panic on the first
// warning — and the warnings here sit on rare paths, so it would ship unnoticed.
// A missing logger costs a log line, not the process.
func (b *SDKBackend) logger() *slog.Logger {
	if b.log == nil {
		return slog.Default()
	}
	return b.log
}

// inspectKit loads a kit reference and reduces it to the facts admit() checks.
func (b *SDKBackend) inspectKit(ctx context.Context, ref string) (KitInfo, error) {
	info, err := sdkkit.Inspect(ctx, b.cl, ref)
	if err != nil {
		return KitInfo{}, err
	}
	return kitInfoFrom(info), nil
}

// kitInfoFrom reduces the SDK's kit report to the facts admit() checks.
//
// sbx v0.39.0 flattened `kit inspect --json`: the old manifest wrapper is gone,
// so kind and volumes now sit at the top level, and three fields moved into the
// new "sandbox" block -- template became image, runOptions became
// command.default, and resources stayed named but moved in. A schemaVersion "1"
// kit is normalized up to the same shape, so only this one reading is needed.
func kitInfoFrom(info sdkkit.Info) KitInfo {
	var sb struct {
		Image     string          `json:"image"`
		Resources json.RawMessage `json:"resources"`
		Command   struct {
			Default []string `json:"default"`
		} `json:"command"`
	}
	if raw := bytes.TrimSpace(info.Sandbox); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &sb); err != nil {
			// Fail closed, as hasResources does: a sandbox block that will not
			// parse must not read as "declares nothing". HasResources alone is
			// enough for admit() to refuse the kit.
			return KitInfo{Kind: info.Kind, HasResources: true}
		}
	}
	return KitInfo{
		Kind:          info.Kind,
		HasResources:  hasResources(sb.Resources),
		HasRunOptions: len(sb.Command.Default) > 0,
		HasTemplate:   sb.Image != "",
		HasVolumes:    hasVolumes(info.Volumes),
	}
}

// hasResources reports whether a kit's raw resources block declares anything.
// The field is raw JSON, so its byte length is not a count: an absent field,
// an explicit null, and an empty object all mean "no resources" despite
// having different lengths (e.g. "{}" vs "{ }"). The shape is always an
// object, so emptiness is decided by unmarshalling into a map and counting
// keys; anything that fails to unmarshal as an object (including a list, which
// has no meaning here) is treated as "declares resources" -- fail closed,
// since an unparseable or unexpectedly-shaped block must not be read as safe.
func hasResources(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return true
	}
	return len(m) > 0
}

// hasVolumes reports whether a kit's raw volumes block declares anything, the
// same emptiness test as hasResources, except upstream's schema also lets a
// kit author write volumes as a list (the modern form) alongside the legacy
// map, so an empty list also reads as "no volumes" here.
func hasVolumes(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		return len(list) > 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return true
	}
	return len(m) > 0
}

// AdmittedKits returns the sorted names of the kits this node advertises.
func (b *SDKBackend) AdmittedKits() []string { return kitNames(b.kits) }

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

// primaryWorkspaceDir is the in-container path of the sandbox's primary (first)
// workspace — sbx's own default working dir for `sbx exec`. The raw exec/attach
// endpoint instead lands in a generic dir (/home/agent/workspace), so terminal
// and default-workdir exec callers default `-w` to this. Best-effort: "" on
// inspect failure or when no workspace is mounted, leaving the daemon default.
func (b *SDKBackend) primaryWorkspaceDir(ctx context.Context, sb *sdksandbox.Sandbox) string {
	info, err := sb.Inspect(ctx)
	if err != nil {
		return ""
	}
	return info.Workspace
}

// workspaceArg builds the sbx workspace argument for one mount, applying sbx's
// rule that the PRIMARY (first) workspace must be read/write. In --clone mode sbx
// clones the primary and mounts that clone read-only itself, so we just drop the
// requested ":ro". Without clone there is no clone to protect the host directory
// and a read-only primary would leave the agent no writable working directory, so
// we reject it rather than silently bind-mount the host read/write. Secondary
// workspaces may be read-only.
func workspaceArg(name, host string, readOnly, primary, clone bool) (string, error) {
	if primary {
		if readOnly && !clone {
			return "", fmt.Errorf("primary workspace %q cannot be read-only: the agent's working directory must be writable (use clone mode, or mount it as a non-primary workspace)", name)
		}
		return host, nil
	}
	if readOnly {
		return host + ":ro", nil
	}
	return host, nil
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
		path, err := workspaceArg(w.Name, host, ro || w.ReadOnly, i == 0, spec.Clone)
		if err != nil {
			return BackendSandbox{}, err
		}
		opts = append(opts, sdksandbox.WithWorkspace(path))
	}
	for _, name := range spec.Kits {
		ref, ok := b.kits[name]
		if !ok {
			return BackendSandbox{}, fmt.Errorf("unknown kit %q: %w", name, ErrUnknownKit)
		}
		// The SDK makes a local reference absolute when it builds the argument
		// vector; an OCI reference passes through. The node does no path work.
		opts = append(opts, sdksandbox.WithKit(ref))
	}
	sb, err := sdksandbox.Create(ctx, b.cl, opts...)
	if err != nil {
		return BackendSandbox{}, err
	}
	// The daemon can refuse a workspace mount by policy and still start the
	// sandbox, so the agent comes up silently without its workspace. It reports
	// one bool for the whole sandbox, so we cannot name which mount was denied.
	if sb.MountPolicyDenied() {
		b.logger().Warn("daemon denied a workspace mount by policy; sandbox is running without it",
			"sandbox", sb.Name())
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

// Remove force-deletes. sbx v0.37.0 enables SSH by default, so a sandbox with an
// open session refuses a plain Remove — and a delete that fails here leaves an
// orphan the node still has a record for. Delete is always a deliberate request,
// so an attached session must not be able to block it.
func (b *SDKBackend) Remove(ctx context.Context, name string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.Remove(ctx, sdksandbox.WithForce())
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
	wd := opts.Workdir
	if wd == "" {
		wd = b.primaryWorkspaceDir(ctx, sb)
	}
	if wd != "" {
		popts = append(popts, sdkexec.WithWorkdir(wd))
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
	wd := opts.Workdir
	if wd == "" {
		wd = b.primaryWorkspaceDir(ctx, sb)
	}
	if wd != "" {
		popts = append(popts, sdkexec.WithWorkdir(wd))
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
	return dedupePorts(out), nil
}

// dedupePorts collapses mappings that are identical in the fields we surface
// (container + host port). The daemon lists one row per host IP, so a single
// published port appears twice (e.g. 127.0.0.1 and ::1) — confusingly identical
// once host_ip is dropped. Order is preserved.
func dedupePorts(ports []PublishedPort) []PublishedPort {
	seen := make(map[PublishedPort]struct{}, len(ports))
	out := make([]PublishedPort, 0, len(ports))
	for _, p := range ports {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func (b *SDKBackend) UnpublishPort(ctx context.Context, name string, containerPort int) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	// The daemon requires a HOST_PORT:SANDBOX_PORT spec for unpublish, so resolve
	// the host port(s) currently mapped to this container port and unpublish each.
	//
	// Read them through b.Ports, NOT the raw sb.Ports: the daemon lists one row per
	// host IP (127.0.0.1 and ::1), and one unpublish removes every IP row for that
	// mapping. Iterating the raw rows issues the same spec twice and the second call
	// fails with "no published port matches". b.Ports applies dedupePorts, so each
	// host port appears once.
	ports, err := b.Ports(ctx, name)
	if err != nil {
		return err
	}
	for _, p := range ports {
		if p.ContainerPort == containerPort {
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
// Maps to exec.Stats in sbx-go-sdk v0.1.7.
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
// Maps to exec.Logs in sbx-go-sdk v0.1.7.
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
// Maps to policy.Log in sbx-go-sdk v0.1.7.
func (b *SDKBackend) BlockedEgress(ctx context.Context) ([]BlockedHost, error) {
	pl, err := sdkpolicy.Log(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedHost, 0, len(pl.BlockedHosts))
	for _, e := range pl.BlockedHosts {
		out = append(out, BlockedHost{Host: e.Host, VMName: e.VMName, Count: e.CountSince})
	}
	return out, nil
}

// AllowedEgress returns the daemon-wide set of allowed (host, vm) pairs.
// Same policy.Log source as BlockedEgress, reading the allowed list.
func (b *SDKBackend) AllowedEgress(ctx context.Context) ([]BlockedHost, error) {
	pl, err := sdkpolicy.Log(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedHost, 0, len(pl.AllowedHosts))
	for _, e := range pl.AllowedHosts {
		out = append(out, BlockedHost{Host: e.Host, VMName: e.VMName, Count: e.CountSince})
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

// PolicyCheck evaluates one access request against the policy. An empty scope
// evaluates globally; a scope names a sandbox context (the daemon rejects an
// unknown name).
func (b *SDKBackend) PolicyCheck(ctx context.Context, scope, target string) (PolicyDecision, error) {
	var opts []sdkpolicy.CheckOption
	if scope != "" {
		opts = append(opts, sdkpolicy.WithCheckSandbox(scope))
	}
	auth, err := sdkpolicy.Check(ctx, b.cl, target, opts...)
	if err != nil {
		return PolicyDecision{}, err
	}
	return PolicyDecision{
		Allowed:  auth.Allowed,
		Reason:   auth.Reason,
		Rule:     auth.Rule,
		Origin:   auth.Origin,
		Resource: auth.ResourceValue,
		DenyKind: auth.DenyKind,
	}, nil
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
		rule := r.Name // SDK v0.1.8: rule name, or ID when unnamed
		if rule == "" {
			rule = r.ID
		}
		out = append(out, PolicyRule{
			Provenance: r.Origin,
			AppliesTo:  r.AppliesTo,
			Rule:       rule,
			Type:       r.ResourceType,
			Decision:   r.Decision,
			Resources:  strings.Join(r.Resources, ","),
		})
	}
	return out, nil
}

// PolicyProfiles returns the raw profile listing text as a single-element slice.
//
// ponytail: stays on the deprecated sdkpolicy.Profiles. Its replacement,
// ProfileNames, only lists *governance* profiles and returns an empty slice on
// an ungoverned host — it does not report the CLI's built-in profiles this
// returns. Switch when a caller actually needs the names as a slice.
func (b *SDKBackend) PolicyProfiles(ctx context.Context) ([]string, error) {
	raw, err := sdkpolicy.Profiles(ctx, b.cl) //nolint:staticcheck // see note above
	if err != nil {
		return nil, err
	}
	return []string{raw}, nil
}

// Secret methods — delegate to sdksecret (shells out to `sbx secret`).
// Values are NEVER stored or returned (spec §11).

func (b *SDKBackend) SecretSet(ctx context.Context, scope string, s CustomSecret) error {
	// The daemon rejects a second write to the same (scope, env) unless the caller
	// re-supplies the existing placeholder, so an update needs a read first. Reusing
	// it is also what makes rotation safe: the sandbox env value stays put and only
	// the real secret behind the proxy changes.
	//
	// ponytail: on a read failure, fall through and let SetCustom report the real
	// error. That is today's behaviour, so no new failure mode. The failure is
	// logged below so a table-format drift (which has happened before) doesn't
	// fail silently.
	if cur, err := b.SecretList(ctx, scope); err == nil {
		for _, c := range cur.Custom {
			if c.Env == s.Env {
				if c.Host != s.Host {
					// A create-or-replace under a different host destroys the old
					// host's credential: values are write-only, so it cannot be
					// recovered. None of these fields is a secret value.
					b.logger().Warn("secret set: replacing host on existing entry",
						"env", s.Env, "old_host", c.Host, "new_host", s.Host, "placeholder", c.Placeholder)
				}
				s.Placeholder = c.Placeholder
				break
			}
		}
	} else {
		b.logger().Warn("secret set: placeholder lookup skipped, update may fail",
			"scope", scope, "err", err)
	}
	err := sdksecret.SetCustom(ctx, b.cl, scope, sdksecret.CustomSecret{
		Host:        s.Host,
		Env:         s.Env,
		Value:       s.Value, // passed to the CLI; never stored or logged here
		Placeholder: s.Placeholder,
	})
	// The underlying sbx CLI echoes the full "--value <key>" argv in its error;
	// scrub the raw value so it never reaches logs or the caller.
	return scrubSecretValue(err, s.Value)
}

// scrubSecretValue replaces every occurrence of value in err's message with a
// placeholder. Returns the original error when there is nothing to scrub, to
// preserve the unwrap chain.
func scrubSecretValue(err error, value string) error {
	if err == nil || value == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), value, "<redacted>")
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// SecretList returns the secret inventory with values masked (the SDK already
// returns ValueMasked, never the real value).
//
// Rows are filtered to the requested scope. An empty scope means node-global,
// but `sbx secret ls` with no scope lists EVERY scope — the CLI's `-g` is the
// only way to ask for global-only and the SDK cannot pass it — so a global
// listing would otherwise leak every sandbox's secret names. Both sides use the
// same "" == global convention, so the comparison is exact.
func (b *SDKBackend) SecretList(ctx context.Context, scope string) (Secrets, error) {
	secs, err := sdksecret.List(ctx, b.cl, scope)
	if err != nil {
		return Secrets{}, err
	}
	out := Secrets{}
	for _, st := range secs.Stored {
		if st.Scope != scope {
			continue
		}
		out.Stored = append(out.Stored, StoredSecret{Name: st.Name, Type: st.Type, Scope: st.Scope})
	}
	for _, c := range secs.Custom {
		if c.Scope != scope {
			continue
		}
		// Value field is intentionally empty — write-only (spec §11). Placeholder
		// is the non-secret injection token and IS surfaced. Targets is comma-joined
		// when one secret covers several hosts (SDK v0.1.5); we set single-host
		// secrets, so it is normally one host.
		out.Custom = append(out.Custom, CustomSecret{Host: c.Targets, Env: c.Env, Placeholder: c.Placeholder})
	}
	return out, nil
}

// SecretRemove deletes the custom (set-custom) secret for a target host in scope.
// Custom secrets are keyed by host, so this uses sdksecret.RemoveCustom (which
// shells out to `secret rm --host`) — NOT sdksecret.Remove, whose positional arg
// is a *service* name (passing a host there is a silent no-op). Added in SDK v0.1.4.
func (b *SDKBackend) SecretRemove(ctx context.Context, scope, host string) error {
	return sdksecret.RemoveCustom(ctx, b.cl, scope, host)
}

// SecretRemoveStored deletes a stored (service/registry) secret by name in scope
// ("" = node-global). Stored secrets are keyed by name, so this uses the SDK's
// `secret rm [--sandbox SCOPE] <name>` (sdksecret.Remove) — not the --host form.
func (b *SDKBackend) SecretRemoveStored(ctx context.Context, scope, name string) error {
	return sdksecret.Remove(ctx, b.cl, scope, name)
}

// ListTemplates returns the template refs the daemon holds (repository:tag).
//
// Confirmed against a live daemon (sbx v0.37.0, TestSDKBackend_SaveRemoveTemplate):
// the format is repository:tag, but the daemon canonicalizes an unqualified
// repository the way Docker does — a template saved with a bare tag like
// "name:tag" is reported back as "docker.io/library/name:tag", not the bare
// tag it was saved with. RemoveTemplate still accepts the original bare tag.
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

// ListTemplateInfo returns the daemon's templates with metadata.
func (b *SDKBackend) ListTemplateInfo(ctx context.Context) ([]TemplateInfo, error) {
	imgs, err := sdktemplate.List(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]TemplateInfo, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, TemplateInfo{
			Repository: im.Repository, Tag: im.Tag, ID: im.ID, Agent: im.Agent, CreatedAt: im.CreatedAt,
		})
	}
	return out, nil
}

// SaveTemplate snapshots the sandbox as a template image. The SDK shells out to
// `sbx template save NAME TAG`; the daemon refuses a running sandbox, and the
// CLI's prompt fails on a non-interactive stdin, so the caller stops it first.
func (b *SDKBackend) SaveTemplate(ctx context.Context, name, tag string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.SaveTemplate(ctx, tag)
}

// RemoveTemplate deletes a template image by ref (REST DELETE on the daemon).
func (b *SDKBackend) RemoveTemplate(ctx context.Context, ref string) error {
	return sdktemplate.Remove(ctx, b.cl, ref)
}

// ExecInteractive opens a Terminal session via the SDK's hijacking attach.
func (b *SDKBackend) ExecInteractive(ctx context.Context, name string, cmd []string, tty bool) (Session, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return nil, err
	}
	popts := []sdkexec.ProcessOption{sdkexec.WithAutoStart()}
	if wd := b.primaryWorkspaceDir(ctx, sb); wd != "" {
		// Start the terminal in the primary workspace (like `sbx exec`) instead of
		// the raw attach default (/home/agent/workspace).
		popts = append(popts, sdkexec.WithWorkdir(wd))
	}
	if tty {
		// Advertise a terminal type so pagers/editors (e.g. `git branch` -> less)
		// don't warn "terminal is not fully functional"; xterm.js speaks xterm-256color.
		popts = append(popts, sdkexec.WithTTY(),
			sdkexec.WithEnv(map[string]string{"TERM": "xterm-256color"}))
	}
	return sdkexec.ExecInteractive(ctx, sb, cmd, popts...) // *AttachSession satisfies Session
}

var _ Backend = (*SDKBackend)(nil)
