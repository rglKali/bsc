package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"go.etcd.io/bbolt"
)

// schemaVersion is bumped only when a change cannot be expressed as an appended
// record field. Record-level evolution is handled by each record's version byte
// (see codec.go), so this should move very rarely — and when it does, it needs
// a migration written for it here. There is nothing to migrate from yet.
const schemaVersion = 1

// ErrNotFound is returned by helpers whose caller cannot sensibly continue
// without the record. Lookups that legitimately miss return (zero, false, nil).
var ErrNotFound = errors.New("store: not found")

// Store is the whole datastore: one bbolt file, three namespaces, all indexes.
// bbolt is single-writer, which suits a design where one process owns the data
// and every mutation is meant to be serialized anyway.
type Store struct {
	db *bbolt.DB
}

// Open opens (or creates) the database at path and ensures every bucket and the
// schema version exist.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store.Open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close() //nolint:errcheck // the open failed; the close error is noise
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("store.init bucket %s: %w", name, err)
			}
		}
		meta := tx.Bucket(bMeta)
		switch v := meta.Get(keyVersion); {
		case v == nil:
			return meta.Put(keyVersion, be64(schemaVersion))
		case len(v) != 8:
			return fmt.Errorf("store: corrupt schema version (%d bytes)", len(v))
		default:
			got := beUint64(v)
			if got > schemaVersion {
				return fmt.Errorf("store: database is schema v%d, binary understands v%d", got, schemaVersion)
			}
			// Older schemas would migrate forward here; v1 is the first.
			return nil
		}
	})
}

// OpenReadOnly opens an existing database without taking the write lock and
// without creating anything. This is how snapshots are inspected: bbolt allows a
// single writer, so the live file cannot be read while the service holds it, and
// the inspection tool has to work on copies.
func OpenReadOnly(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o400, &bbolt.Options{
		Timeout:  3 * time.Second,
		ReadOnly: true,
	})
	if err != nil {
		return nil, fmt.Errorf("store.OpenReadOnly %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the file lock.
func (s *Store) Close() error { return s.db.Close() }

// Path is the database file, for logs and snapshot naming.
func (s *Store) Path() string { return s.db.Path() }

// Tx is a transaction-scoped handle. Every operation hangs off it rather than
// off Store, because the design's correctness rests on composing many
// operations into one write — a finalized block confirms flows, records
// deposits, moves balances, starts drains and advances the cursor atomically
// (§8), and that is only expressible if the caller owns the transaction.
type Tx struct {
	tx *bbolt.Tx
}

// Update runs fn in a single write transaction, committing on nil error.
func (s *Store) Update(fn func(*Tx) error) error {
	return s.db.Update(func(btx *bbolt.Tx) error { return fn(&Tx{tx: btx}) })
}

// View runs fn in a read transaction. Values handed to fn are only valid inside
// it; every accessor here decodes into owned memory, so returned records are
// safe to keep.
func (s *Store) View(fn func(*Tx) error) error {
	return s.db.View(func(btx *bbolt.Tx) error { return fn(&Tx{tx: btx}) })
}

// Snapshot writes a consistent copy of the entire database to w from inside a
// read transaction. This is the backup story: one call, no external tooling, and
// the only way to inspect the data while the service is running — bbolt holds an
// exclusive lock, so `bsc inspect` works on snapshots, not the live file (§3).
func (s *Store) Snapshot(w io.Writer) (int64, error) {
	var n int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		n, err = tx.WriteTo(w)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store.Snapshot: %w", err)
	}
	return n, nil
}

// --- chain cursor ---

// Cursor returns the next block to process. ok is false on a fresh database,
// which is the caller's signal to fall back to chain.start_block — and that fallback
// must resolve to the current finalized head, never 0, or the watcher would try
// to scan from genesis (§11).
func (t *Tx) Cursor() (next uint64, ok bool, err error) {
	v := t.tx.Bucket(bStat).Get(keyCursor)
	if v == nil {
		return 0, false, nil
	}
	if len(v) != 8 {
		return 0, false, fmt.Errorf("store: corrupt cursor (%d bytes)", len(v))
	}
	return beUint64(v), true, nil
}

// SetCursor advances the block cursor. It is written in the same transaction as
// the data it describes, which is what makes a block exactly-once with no dedup
// window (§1).
func (t *Tx) SetCursor(next uint64) error {
	return t.tx.Bucket(bStat).Put(keyCursor, be64(next))
}

// --- small helpers shared by the accessors ---

// get fetches and decodes one record. Missing keys are (zero, false, nil).
func get[T any](t *Tx, bucket, key []byte, decode func([]byte) (T, error)) (T, bool, error) {
	var zero T
	v := t.tx.Bucket(bucket).Get(key)
	if v == nil {
		return zero, false, nil
	}
	rec, err := decode(v)
	if err != nil {
		return zero, false, fmt.Errorf("store: decode %s: %w", bucket, err)
	}
	return rec, true, nil
}

// put encodes and stores one record.
func put(t *Tx, bucket, key []byte, encode func() ([]byte, error)) error {
	b, err := encode()
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", bucket, err)
	}
	if err := t.tx.Bucket(bucket).Put(key, b); err != nil {
		return fmt.Errorf("store: put %s: %w", bucket, err)
	}
	return nil
}

// scanPrefix walks every key with the given prefix in ascending order, stopping
// early if fn returns errStop. Prefix scans are how per-app indexes are read.
func scanPrefix(t *Tx, bucket, prefix []byte, fn func(k, v []byte) error) error {
	c := t.tx.Bucket(bucket).Cursor()
	for k, v := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
		if err := fn(k, v); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// scanFrom walks from `start` (inclusive) to the end of the prefix range.
func scanFrom(t *Tx, bucket, prefix, start []byte, fn func(k, v []byte) error) error {
	c := t.tx.Bucket(bucket).Cursor()
	for k, v := c.Seek(start); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
		if err := fn(k, v); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// errStop ends a scan without failing it.
var errStop = errors.New("store: stop scan")

func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// scanPrefixReverse walks a prefix range newest-key-first. History listings want
// most-recent-first, and bbolt gives it to us by seeking just past the range and
// stepping backwards.
func scanPrefixReverse(t *Tx, bucket, prefix []byte, fn func(k, v []byte) error) error {
	c := t.tx.Bucket(bucket).Cursor()
	end := prefixEnd(prefix)
	var k, v []byte
	switch {
	case len(end) == 0:
		// No upper bound: either the prefix is empty (the whole bucket) or it
		// is all 0xFF, and both run to the end. Seeking nil would land on the
		// *first* key rather than past the last, so the scan has to start from
		// the end explicitly.
		k, v = c.Last()
	default:
		if k, v = c.Seek(end); k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
	}
	for ; k != nil && hasPrefix(k, prefix); k, v = c.Prev() {
		if err := fn(k, v); err != nil {
			if errors.Is(err, errStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// prefixEnd is the smallest key strictly greater than every key with this
// prefix, or nil when the prefix is all 0xFF (in which case the range runs to
// the end of the bucket).
func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// Meta records what this database is about. It is written on first run and
// checked on every later one: pointing a populated store at a different token,
// or at a token whose decimals differ, would silently reinterpret every amount
// it holds.
type Meta struct {
	ChainID  uint64
	Token    common.Address
	Decimals uint8
}

// Meta reads the recorded token identity. ok is false on a store that has never
// been started, which is the only time SetMeta may introduce one.
func (t *Tx) Meta() (Meta, bool, error) {
	b := t.tx.Bucket(bMeta)
	raw := b.Get(keyToken)
	if raw == nil {
		return Meta{}, false, nil
	}
	if len(raw) != common.AddressLength {
		return Meta{}, false, fmt.Errorf("store: token meta is %d bytes", len(raw))
	}
	dec := b.Get(keyDecimals)
	if len(dec) != 1 {
		return Meta{}, false, fmt.Errorf("store: decimals meta is %d bytes", len(dec))
	}
	id := b.Get(keyChainID)
	if len(id) != 8 {
		return Meta{}, false, fmt.Errorf("store: chain id meta is %d bytes", len(id))
	}
	return Meta{
		ChainID:  binary.BigEndian.Uint64(id),
		Token:    common.BytesToAddress(raw),
		Decimals: dec[0],
	}, true, nil
}

// SetMeta records the token identity, refusing to change one already stored.
// Every amount in the database is denominated by it, so a change is a migration
// and not a configuration edit.
func (t *Tx) SetMeta(m Meta) error {
	existing, ok, err := t.Meta()
	if err != nil {
		return err
	}
	if ok {
		if existing != m {
			return fmt.Errorf(
				"store: this database belongs to chain %d, token %s with %d decimals — not chain %d, token %s with %d",
				existing.ChainID, existing.Token.Hex(), existing.Decimals,
				m.ChainID, m.Token.Hex(), m.Decimals)
		}
		return nil
	}
	b := t.tx.Bucket(bMeta)
	if err := b.Put(keyToken, m.Token.Bytes()); err != nil {
		return fmt.Errorf("store: write token meta: %w", err)
	}
	if err := b.Put(keyDecimals, []byte{m.Decimals}); err != nil {
		return fmt.Errorf("store: write decimals meta: %w", err)
	}
	if err := b.Put(keyChainID, binary.BigEndian.AppendUint64(nil, m.ChainID)); err != nil {
		return fmt.Errorf("store: write chain id meta: %w", err)
	}
	return nil
}
