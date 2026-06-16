package store

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Put stores val under key in the named bucket.
func (s *Store) Put(bucket, key string, val []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.Put([]byte(key), val)
	})
}

// Get returns the value for key; ok is false if absent.
func (s *Store) Get(bucket, key string) (val []byte, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		val = append([]byte(nil), raw...) // copy: bbolt memory is txn-scoped
		ok = true
		return nil
	})
	return val, ok, err
}

// Delete removes key from the bucket (no error if absent).
func (s *Store) Delete(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.Delete([]byte(key))
	})
}

// ForEach calls fn for every key/value in the bucket.
func (s *Store) ForEach(bucket string, fn func(k, v []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.ForEach(fn)
	})
}
