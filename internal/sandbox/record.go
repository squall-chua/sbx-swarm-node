package sandbox

import "time"

// Record is the persisted, authoritative sandbox state on its owner node.
type Record struct {
	ID          string            `json:"id"`           // self-routing <node_id>.<ulid>
	BackendName string            `json:"backend_name"` // SDK sandbox name
	OwnerNode   string            `json:"owner_node"`
	Spec        CreateSpec        `json:"spec"`
	Status      string            `json:"status"` // running|stopped|failed|lost
	Ports       []PublishedPort   `json:"ports,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	IdempKey    string            `json:"idempotency_key,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	LastActivity time.Time         `json:"last_activity,omitempty"`
	LastPublish  time.Time         `json:"last_publish,omitempty"`
}
