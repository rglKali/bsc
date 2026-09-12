package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrDuplicate means an idempotency key has already been used by this app.
var ErrDuplicate = errors.New("store: duplicate idempotency key")

// PutWithdrawal writes a withdrawal and its three indexes: the history listing,
// the open set an app polls, and the idempotency key. Idempotency matters more
// here than it did in v1, which got dedup free from the message broker — over
// HTTP, a client retry after a timeout would otherwise double-pay (§6).
func (t *Tx) PutWithdrawal(wd Withdrawal) error {
	if err := ValidSlug(wd.App); err != nil {
		return err
	}
	if wd.ID == uuid.Nil {
		return errors.New("store: withdrawal id must be set")
	}
	if err := put(t, bWithdrawal, wd.ID[:], wd.encode); err != nil {
		return err
	}
	if err := t.tx.Bucket(iWd).Put(scoped(wd.App, be64(stampNanos(wd.CreatedAt)), wd.ID[:]), nil); err != nil {
		return fmt.Errorf("store: put wd index: %w", err)
	}
	openKey := scoped(wd.App, wd.ID[:])
	if wd.Status.IsTerminal() {
		if err := t.tx.Bucket(iWdOpen).Delete(openKey); err != nil {
			return fmt.Errorf("store: delete wd_open: %w", err)
		}
	} else if err := t.tx.Bucket(iWdOpen).Put(openKey, nil); err != nil {
		return fmt.Errorf("store: put wd_open: %w", err)
	}
	if wd.IdempotencyKey != "" {
		if err := t.tx.Bucket(iWdIdem).Put(scoped(wd.App, []byte(wd.IdempotencyKey)), wd.ID[:]); err != nil {
			return fmt.Errorf("store: put wd_idem: %w", err)
		}
	}
	return nil
}

// Withdrawal looks up one withdrawal by id — the handle the app has held since
// it created the request.
func (t *Tx) Withdrawal(id uuid.UUID) (Withdrawal, bool, error) {
	return get(t, bWithdrawal, id[:], decodeWithdrawal)
}

// WithdrawalByKey resolves an app's idempotency key so a retried request can
// replay the original response instead of paying twice.
func (t *Tx) WithdrawalByKey(slug, key string) (Withdrawal, bool, error) {
	id := t.tx.Bucket(iWdIdem).Get(scoped(slug, []byte(key)))
	if id == nil {
		return Withdrawal{}, false, nil
	}
	var u uuid.UUID
	copy(u[:], id)
	return t.Withdrawal(u)
}

// OpenWithdrawals returns everything not yet terminal for an app. This is the
// whole withdrawal notification mechanism: the app holds the ids, so it polls
// its own open set and drops each entry as it settles (§9). The set is bounded
// by how many the app has in flight.
func (t *Tx) OpenWithdrawals(slug string) ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefix(t, iWdOpen, scopePrefix(slug), func(k, _ []byte) error {
		var u uuid.UUID
		copy(u[:], k[len(k)-len(u):])
		wd, ok, err := t.Withdrawal(u)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, wd)
		}
		return nil
	})
	return out, err
}

// Withdrawals lists an app's withdrawals most-recent-first.
func (t *Tx) Withdrawals(slug string, limit int) ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefixReverse(t, iWd, scopePrefix(slug), func(k, _ []byte) error {
		var u uuid.UUID
		copy(u[:], k[len(k)-len(u):])
		wd, ok, err := t.Withdrawal(u)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, wd)
		}
		if limit > 0 && len(out) >= limit {
			return errStop
		}
		return nil
	})
	return out, err
}

// MutateWithdrawal applies fn in place and re-maintains the open set, so a
// status reaching terminal removes it from what the app polls. The app, id and
// idempotency key are immutable.
func (t *Tx) MutateWithdrawal(id uuid.UUID, fn func(*Withdrawal) error) (Withdrawal, error) {
	wd, ok, err := t.Withdrawal(id)
	if err != nil {
		return Withdrawal{}, err
	}
	if !ok {
		return Withdrawal{}, fmt.Errorf("%w: withdrawal %s", ErrNotFound, id)
	}
	before := wd
	if err := fn(&wd); err != nil {
		return Withdrawal{}, err
	}
	switch {
	case wd.ID != before.ID:
		return Withdrawal{}, fmt.Errorf("%w: id", ErrImmutable)
	case wd.App != before.App:
		return Withdrawal{}, fmt.Errorf("%w: app", ErrImmutable)
	case wd.IdempotencyKey != before.IdempotencyKey:
		return Withdrawal{}, fmt.Errorf("%w: idempotency key", ErrImmutable)
	case wd.CreatedAt != before.CreatedAt:
		return Withdrawal{}, fmt.Errorf("%w: created_at", ErrImmutable)
	}
	wd.UpdatedAt = time.Now().UTC()
	return wd, t.PutWithdrawal(wd)
}

// stampNanos renders a timestamp for use inside a sort key.
func stampNanos(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}
