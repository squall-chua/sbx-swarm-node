// Package events is a best-effort, in-process notification bus (ADR-0008):
// not durable, not a source of truth. Subscribers reconcile against state if
// they need certainty.
package events

import (
	"encoding/json"
	"time"
)

// Event is a single domain notification.
type Event struct {
	ID        string          `json:"id"` // "<node_id>-<seq>"
	Seq       uint64          `json:"seq"`
	TS        time.Time       `json:"ts"`
	Type      string          `json:"type"`
	NodeID    string          `json:"node_id"`
	SandboxID string          `json:"sandbox_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Filter selects events by type set (empty = all) and/or sandbox id.
type Filter struct {
	Types     []string
	SandboxID string
}

func (f Filter) matches(e Event) bool {
	if f.SandboxID != "" && e.SandboxID != f.SandboxID {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == e.Type {
			return true
		}
	}
	return false
}

// Publisher publishes domain events. The Bus implements it; the sandbox and ops
// managers depend on it (nil-safe via a wrapper).
type Publisher interface {
	Publish(eventType, sandboxID string, payload any) Event
}
