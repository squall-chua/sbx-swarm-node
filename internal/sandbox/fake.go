package sandbox

import (
	"context"
	"fmt"
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
	rules     []PolicyRule
	secrets   map[string][]CustomSecret
	templates []string
}

// NewFake returns an empty fake backend.
func NewFake() *Fake {
	return &Fake{sandboxes: map[string]*BackendSandbox{}, ports: map[string][]PublishedPort{}, detached: map[string]bool{}}
}

func (f *Fake) Create(_ context.Context, spec CreateSpec) (BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[spec.Name]; ok {
		return BackendSandbox{}, fmt.Errorf("exists: %s", spec.Name)
	}
	sb := &BackendSandbox{Name: spec.Name, Status: "running"}
	f.sandboxes[spec.Name] = sb
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

func (f *Fake) Exec(_ context.Context, name string, _ []string, _ ExecOpts) (ExecResult, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return ExecResult{}, err
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

func (f *Fake) CopyTo(_ context.Context, name, _, _ string) error {
	_, err := f.Get(context.Background(), name)
	return err
}

func (f *Fake) CopyFrom(_ context.Context, name, _, _ string) error {
	_, err := f.Get(context.Background(), name)
	return err
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
func (f *Fake) SetTemplates(t []string) { f.mu.Lock(); f.templates = append([]string(nil), t...); f.mu.Unlock() }

// ListTemplates returns the configured template refs.
func (f *Fake) ListTemplates(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.templates...), nil
}

// SetBlocked sets the fake's blocked-egress list (test helper).
func (f *Fake) SetBlocked(b []BlockedHost) { f.mu.Lock(); f.blocked = b; f.mu.Unlock() }

func (f *Fake) BlockedEgress(_ context.Context) ([]BlockedHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BlockedHost(nil), f.blocked...), nil
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
