// Package audit is the durable, append-only record of credentialed/sensitive
// actions. It never stores secret values (spec §11/§15).
package audit

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
	bolt "go.etcd.io/bbolt"
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

// Record appends an entry in a single transaction. The Seq is the bucket's
// monotonic sequence (atomic via NextSequence), and the big-endian-encoded Seq
// is the key, so concurrent appends never collide and List stays in Seq order.
func (l *Log) Record(e Entry) error {
	e.TSUnix = l.now()
	return l.store.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e.Seq = int64(seq)
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put(key, raw)
	})
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
