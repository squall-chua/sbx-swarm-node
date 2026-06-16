// Package store is the node's durable state in a single bbolt file. It owns
// schema versioning: fresh databases start at SchemaVersion, older ones are
// migrated forward, and a database newer than the binary is refused
// (downgrade guard).
package store

import (
	"encoding/binary"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SchemaVersion is the schema this binary understands.
const SchemaVersion uint64 = 1

var (
	bucketNames = []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit"}
	schemaKey   = []byte("schema_version")
	bucketMeta  = []byte("meta")
)

// migration migrates the database from version i to i+1. migrations[i-1] takes
// the store from version i to i+1. Empty until a v2 schema exists.
var migrations = []func(tx *bolt.Tx) error{}

// Store wraps the bbolt database.
type Store struct {
	db *bolt.DB
}

// DB exposes the underlying handle for later packages and tests.
func (s *Store) DB() *bolt.DB { return s.db }

// Open opens (or creates) the database at path, ensures all buckets exist, and
// reconciles the schema version.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		meta := tx.Bucket(bucketMeta)

		raw := meta.Get(schemaKey)
		if raw == nil { // fresh database
			return putUint64(meta, schemaKey, SchemaVersion)
		}

		v := binary.BigEndian.Uint64(raw)
		if v > SchemaVersion {
			return fmt.Errorf("on-disk schema v%d is newer than supported v%d (downgrade guard)", v, SchemaVersion)
		}
		for v < SchemaVersion {
			if int(v-1) >= len(migrations) {
				return fmt.Errorf("missing migration from v%d", v)
			}
			if err := migrations[v-1](tx); err != nil {
				return fmt.Errorf("migrate v%d: %w", v, err)
			}
			v++
		}
		return putUint64(meta, schemaKey, SchemaVersion)
	})
}

func putUint64(b *bolt.Bucket, key []byte, v uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return b.Put(key, buf)
}
