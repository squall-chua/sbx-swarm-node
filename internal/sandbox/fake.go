package sandbox

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Fake is an in-memory Backend for tests.
type Fake struct {
	mu        sync.Mutex
	sandboxes map[string]*BackendSandbox
	ports     map[string][]PublishedPort
	detached  map[string]bool // detachedID -> done
	seq       int
	blocked   []BlockedHost
	allowed   []BlockedHost
	rules     []PolicyRule
	secrets   map[string][]CustomSecret
	templates []string
	kits      map[string]bool

	// Optional test hooks. When non-nil they override the default Exec/PublishPort/
	// CopyFrom behavior (used to drive git-publish tests against real git repos).
	ExecFunc        func(name string, cmd []string) (ExecResult, error)
	PublishPortFunc func(name string, cp int) (PublishedPort, error)
	CopyFromFunc    func(name, remotePath, localPath string) error
	CopyToFunc      func(name, localPath, remotePath string) error

	// KitPorts is seeded into a sandbox's ports when its create spec names a kit,
	// standing in for ports a real kit publishes by itself.
	KitPorts []PublishedPort
}

// NewFake returns an empty fake backend advertising the given kit names. It is
// variadic so a test that does not care about kits can keep calling NewFake().
func NewFake(kits ...string) *Fake {
	f := &Fake{
		sandboxes: map[string]*BackendSandbox{},
		ports:     map[string][]PublishedPort{},
		detached:  map[string]bool{},
		kits:      make(map[string]bool, len(kits)),
	}
	for _, k := range kits {
		f.kits[k] = true
	}
	return f
}

// AdmittedKits returns the fake's configured kit names, sorted. The fake never
// inspects anything: it has no daemon, and it exists for tests and daemonless
// nodes.
func (f *Fake) AdmittedKits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.kits))
	for k := range f.kits {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *Fake) Create(_ context.Context, spec CreateSpec) (BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[spec.Name]; ok {
		return BackendSandbox{}, fmt.Errorf("exists: %s", spec.Name)
	}
	for _, k := range spec.Kits {
		if !f.kits[k] {
			return BackendSandbox{}, fmt.Errorf("unknown kit %q", k)
		}
	}
	sb := &BackendSandbox{Name: spec.Name, Status: "running"}
	f.sandboxes[spec.Name] = sb
	if len(spec.Kits) > 0 && len(f.KitPorts) > 0 {
		f.ports[spec.Name] = append(f.ports[spec.Name], f.KitPorts...)
	}
	return *sb, nil
}

func (f *Fake) Get(_ context.Context, name string) (BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return BackendSandbox{}, ErrNotFound
	}
	return *sb, nil
}

func (f *Fake) List(_ context.Context) ([]BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BackendSandbox, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, *sb)
	}
	return out, nil
}

func (f *Fake) setStatus(name, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return ErrNotFound
	}
	sb.Status = status
	return nil
}

func (f *Fake) Start(_ context.Context, name string) error { return f.setStatus(name, "running") }
func (f *Fake) Stop(_ context.Context, name string) error  { return f.setStatus(name, "stopped") }

func (f *Fake) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		return ErrNotFound
	}
	delete(f.sandboxes, name)
	delete(f.ports, name)
	return nil
}

func (f *Fake) Exec(_ context.Context, name string, cmd []string, _ ExecOpts) (ExecResult, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return ExecResult{}, err
	}
	_ = f.setStatus(name, "running") // the daemon auto-starts a stopped sandbox on exec
	if f.ExecFunc != nil {
		return f.ExecFunc(name, cmd)
	}
	return ExecResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

func (f *Fake) ExecDetached(_ context.Context, name string, _ []string, _ ExecOpts) (string, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("d%d", f.seq)
	f.detached[id] = true // completes immediately in the fake
	return id, nil
}

func (f *Fake) PollDetached(_ context.Context, _, detachedID string) (DetachedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done, ok := f.detached[detachedID]
	if !ok {
		return DetachedStatus{}, fmt.Errorf("no such detached exec %s", detachedID)
	}
	return DetachedStatus{Done: done, ExitCode: 0}, nil
}

func (f *Fake) PublishPort(_ context.Context, name string, cp int) (PublishedPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		return PublishedPort{}, ErrNotFound
	}
	p := PublishedPort{ContainerPort: cp, HostPort: 30000 + cp}
	if f.PublishPortFunc != nil {
		var err error
		if p, err = f.PublishPortFunc(name, cp); err != nil {
			return PublishedPort{}, err
		}
	}
	f.ports[name] = append(f.ports[name], p)
	return p, nil
}

func (f *Fake) Ports(_ context.Context, name string) ([]PublishedPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[name], nil
}

func (f *Fake) UnpublishPort(_ context.Context, name string, cp int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.ports[name][:0]
	for _, p := range f.ports[name] {
		if p.ContainerPort != cp {
			kept = append(kept, p)
		}
	}
	f.ports[name] = kept
	return nil
}

func (f *Fake) CopyTo(_ context.Context, name, localPath, remotePath string) error {
	if _, err := f.Get(context.Background(), name); err != nil {
		return err
	}
	if f.CopyToFunc != nil {
		return f.CopyToFunc(name, localPath, remotePath)
	}
	return nil
}

func (f *Fake) CopyFrom(_ context.Context, name, remotePath, localPath string) error {
	if _, err := f.Get(context.Background(), name); err != nil {
		return err
	}
	if f.CopyFromFunc != nil {
		return f.CopyFromFunc(name, remotePath, localPath)
	}
	return nil
}

func (f *Fake) Stats(_ context.Context, name string) (Usage, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return Usage{}, err
	}
	return Usage{Cores: 2, CPUPercent: 10, MemTotalKB: 1 << 20, MemUsedKB: 1 << 18}, nil
}

func (f *Fake) Logs(ctx context.Context, name, _ string, out chan<- LogLine) error {
	if _, err := f.Get(ctx, name); err != nil {
		return err
	}
	go func() {
		select {
		case out <- LogLine{Line: "log from " + name}:
		case <-ctx.Done():
		}
	}()
	return nil
}

// SetTemplates sets the advertised template refs (tests).
func (f *Fake) SetTemplates(t []string) {
	f.mu.Lock()
	f.templates = append([]string(nil), t...)
	f.mu.Unlock()
}

// ListTemplates returns the configured template refs.
func (f *Fake) ListTemplates(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.templates...), nil
}

// ListTemplateInfo returns a canned template so tests need no daemon.
func (b *Fake) ListTemplateInfo(_ context.Context) ([]TemplateInfo, error) {
	return []TemplateInfo{{Repository: "fake/base", Tag: "latest", ID: "img-fake", Agent: "shell"}}, nil
}

// SetBlocked sets the fake's blocked-egress list (test helper).
func (f *Fake) SetBlocked(b []BlockedHost) { f.mu.Lock(); f.blocked = b; f.mu.Unlock() }

// SetAllowed sets the fake's allowed-egress list (test helper).
func (f *Fake) SetAllowed(a []BlockedHost) { f.mu.Lock(); f.allowed = a; f.mu.Unlock() }

func (f *Fake) BlockedEgress(_ context.Context) ([]BlockedHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BlockedHost(nil), f.blocked...), nil
}

func (f *Fake) AllowedEgress(_ context.Context) ([]BlockedHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BlockedHost(nil), f.allowed...), nil
}

// Policy methods.

func (f *Fake) PolicyAllow(_ context.Context, _, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, PolicyRule{Rule: host, Decision: "allow"})
	return nil
}

func (f *Fake) PolicyDeny(_ context.Context, _, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, PolicyRule{Rule: host, Decision: "deny"})
	return nil
}

func (f *Fake) PolicySetDefault(_ context.Context, _ string) error { return nil }

func (f *Fake) PolicyRemoveRule(_ context.Context, _, _ string) error { return nil }

func (f *Fake) PolicyReset(_ context.Context) error {
	f.mu.Lock()
	f.rules = nil
	f.mu.Unlock()
	return nil
}

func (f *Fake) PolicyList(_ context.Context, _ string) ([]PolicyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PolicyRule(nil), f.rules...), nil
}

// PolicyCheck mirrors the daemon's default-deny: the target is allowed only when
// an allow rule names it, and an explicit deny always wins.
// ponytail: exact host match only — no wildcards, no ports. The real daemon
// normalises; the Fake just needs a deterministic allow/deny for tests.
func (f *Fake) PolicyCheck(_ context.Context, _, target string) (PolicyDecision, error) {
	host, _, found := strings.Cut(target, ":")
	if !found {
		host = target
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := PolicyDecision{Resource: host, Origin: "local"}
	for _, r := range f.rules {
		if r.Rule != host {
			continue
		}
		if r.Decision == "deny" {
			return PolicyDecision{Resource: host, Origin: "local", Rule: r.Rule, Reason: "denied by rule"}, nil
		}
		d.Allowed, d.Rule = true, r.Rule
	}
	if !d.Allowed {
		d.DenyKind, d.Reason = "implicit", "no rule matched; default deny"
	}
	return d, nil
}

func (f *Fake) PolicyProfiles(_ context.Context) ([]string, error) {
	return []string{"allow-all", "balanced", "deny-all"}, nil
}

// Secret methods.

func (f *Fake) SecretSet(_ context.Context, scope string, s CustomSecret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.secrets == nil {
		f.secrets = map[string][]CustomSecret{}
	}
	f.secrets[scope] = append(f.secrets[scope], s)
	return nil
}

func (f *Fake) SecretList(_ context.Context, scope string) (Secrets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out Secrets
	for _, s := range f.secrets[scope] {
		// Value is intentionally omitted — write-only (spec §11).
		out.Custom = append(out.Custom, CustomSecret{Host: s.Host, Env: s.Env})
	}
	return out, nil
}

func (f *Fake) SecretRemove(_ context.Context, scope, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.secrets[scope][:0]
	for _, s := range f.secrets[scope] {
		if s.Host != host {
			kept = append(kept, s)
		}
	}
	f.secrets[scope] = kept
	return nil
}

// SecretRemoveStored is a no-op: the fake tracks only custom secrets.
func (f *Fake) SecretRemoveStored(_ context.Context, _, _ string) error {
	return nil
}

type fakeSession struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func (s *fakeSession) Stdin() io.Writer                       { return s.w } // echo: Stdin -> Stdout
func (s *fakeSession) Stdout() io.Reader                      { return s.r }
func (s *fakeSession) Resize(context.Context, int, int) error { return nil }
func (s *fakeSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.closed:
		return 0, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (s *fakeSession) Close() error {
	s.once.Do(func() { close(s.closed); _ = s.w.Close(); _ = s.r.Close() })
	return nil
}

// ExecInteractive returns an echo session (bytes written to Stdin appear on Stdout).
func (b *Fake) ExecInteractive(_ context.Context, _ string, _ []string, _ bool) (Session, error) {
	pr, pw := io.Pipe()
	return &fakeSession{r: pr, w: pw, closed: make(chan struct{})}, nil
}
