// Package sandbox manages sandboxes on this node behind a Backend abstraction
// over the sbx-go-sdk, with an in-memory fake for tests.
package sandbox

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a sandbox does not exist in the backend.
var ErrNotFound = errors.New("sandbox not found")

// WorkspaceMount describes a workspace to attach.
type WorkspaceMount struct {
	Name     string // logical workspace name (resolved to a host path by the backend/config)
	ReadOnly bool
}

// CreateSpec describes a sandbox to provision.
type CreateSpec struct {
	Name        string
	Agent       string
	Template    string
	CPUs        int
	MemoryBytes int64
	DiskGB      float64 // requested disk (GB); scheduling-only in v1 (no SDK create option)
	Clone       bool
	Workspaces  []WorkspaceMount
	Env         map[string]string
}

// Resources is a per-sandbox resource triple (cores / bytes / GB). Used for
// the configured default applied to unsized requests.
type Resources struct {
	CPUCores    float64
	MemoryBytes int64
	DiskGB      float64
}

// BackendSandbox is the backend's view of a sandbox.
type BackendSandbox struct {
	Name   string
	Status string // "running" | "stopped" | ...
}

// ExecOpts are options for exec/agent-run.
type ExecOpts struct {
	Workdir string
	Env     map[string]string
}

// ExecResult is the captured outcome of a synchronous exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// DetachedStatus is the poll result for a detached exec / agent run.
type DetachedStatus struct {
	Done     bool
	ExitCode int // valid when Done
}

// PublishedPort maps a container port to a host port.
type PublishedPort struct {
	ContainerPort int
	HostPort      int
}

// Usage is a point-in-time per-sandbox resource snapshot.
type Usage struct {
	Cores         int
	CPUPercent    float64
	MemTotalKB    int64
	MemUsedKB     int64
	DiskTotalGB   float64
	DiskUsedGB    float64
	UptimeSeconds int64
}

// BlockedHost is one denied egress attempt: host + sandbox VM name.
type BlockedHost struct {
	Host   string
	VMName string
}

// LogLine is one streamed log line.
type LogLine struct {
	Line string
	Err  error // set on stream error/EOF
}

// PolicyRule mirrors a structured row from policy.List (SDK v0.1.2).
type PolicyRule struct {
	Provenance string `json:"provenance"`
	AppliesTo  string `json:"applies_to"`
	Rule       string `json:"rule"`
	Type       string `json:"type"`
	Decision   string `json:"decision"` // allow|deny
	Resources  string `json:"resources"`
}

// CustomSecret is a proxy-injected credential. Value is write-only and never
// returned by reads.
type CustomSecret struct {
	Host  string `json:"host"`
	Env   string `json:"env"`
	Value string `json:"value,omitempty"`
}

// StoredSecret is a non-custom secret entry (name only).
type StoredSecret struct {
	Name string `json:"name"`
}

// Secrets is the structured secret inventory (values always masked).
type Secrets struct {
	Stored []StoredSecret `json:"stored"`
	Custom []CustomSecret `json:"custom"`
}

// Backend is the abstraction over sbx-go-sdk used by the manager.
type Backend interface {
	Create(ctx context.Context, spec CreateSpec) (BackendSandbox, error)
	Get(ctx context.Context, name string) (BackendSandbox, error)
	List(ctx context.Context) ([]BackendSandbox, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Exec(ctx context.Context, name string, cmd []string, opts ExecOpts) (ExecResult, error)
	ExecDetached(ctx context.Context, name string, cmd []string, opts ExecOpts) (detachedID string, err error)
	PollDetached(ctx context.Context, name, detachedID string) (DetachedStatus, error)
	PublishPort(ctx context.Context, name string, containerPort int) (PublishedPort, error)
	Ports(ctx context.Context, name string) ([]PublishedPort, error)
	UnpublishPort(ctx context.Context, name string, containerPort int) error
	CopyTo(ctx context.Context, name, localPath, remotePath string) error
	CopyFrom(ctx context.Context, name, remotePath, localPath string) error
	Stats(ctx context.Context, name string) (Usage, error)
	// Logs follows the log at path; lines are sent to out until ctx is done or
	// the stream ends (a final LogLine with non-nil Err signals end/error).
	Logs(ctx context.Context, name, path string, out chan<- LogLine) error
	// BlockedEgress returns the daemon-wide set of blocked (host, vm) pairs.
	BlockedEgress(ctx context.Context) ([]BlockedHost, error)

	// Policy management (egress rules).
	PolicyAllow(ctx context.Context, scope, host string) error
	PolicyDeny(ctx context.Context, scope, host string) error
	PolicySetDefault(ctx context.Context, profile string) error
	PolicyRemoveRule(ctx context.Context, scope string) error
	PolicyReset(ctx context.Context) error
	PolicyList(ctx context.Context, scope string) ([]PolicyRule, error)
	PolicyProfiles(ctx context.Context) ([]string, error)

	// Secret management (values write-only; reads always mask them).
	SecretSet(ctx context.Context, scope string, s CustomSecret) error
	SecretList(ctx context.Context, scope string) (Secrets, error)
	SecretRemove(ctx context.Context, scope, host string) error
}
