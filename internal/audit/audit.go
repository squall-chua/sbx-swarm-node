// Package audit is the durable, append-only record of credentialed/sensitive
// actions. It never stores secret values (spec §11/§15).
package audit

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const bucket = "audit"

// Entry is one audited action. It intentionally has no Value field — secret
// values and env values are write-only (spec §11).
type Entry struct {
	Seq     int64  `json:"seq"`
	Actor   string `json:"actor"`   // role or key id, never the secret
	Action  string `json:"action"`  // e.g. policy.deny, secret.set
	Target  string `json:"target"`  // host/scope, never a value
	Outcome string `json:"outcome"` // ok|error
	TSUnix  int64  `json:"ts_unix"`
}

// Log appends and lists audit entries.
type Log struct {
	store *store.Store
	now   func() int64
}

// New builds an audit log. now returns a unix timestamp (injected for tests).
func New(st *store.Store, now func() int64) *Log { return &Log{store: st, now: now} }

// Record appends an entry with a monotonic seq (big-endian bbolt key).
func (l *Log) Record(e Entry) error {
	e.TSUnix = l.now()
	cur, err := l.List()
	if err != nil {
		return err
	}
	e.Seq = int64(len(cur)) + 1
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(e.Seq))
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return l.store.Put(bucket, string(key), raw)
}

// List returns all entries in seq order.
func (l *Log) List() ([]Entry, error) {
	var out []Entry
	err := l.store.ForEach(bucket, func(_, v []byte) error {
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("decode audit entry: %w", err)
		}
		out = append(out, e)
		return nil
	})
	return out, err
}
