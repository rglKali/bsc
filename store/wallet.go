package store

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

var (
	// ErrInsufficient means a reserve would exceed the wallet's available
	// balance. The store refuses it rather than trusting callers, because an
	// over-reserve is an overdraft and there is no on-chain backstop for one.
	ErrInsufficient = errors.New("store: insufficient available balance")

	// ErrImmutable means a mutation tried to change a field that indexes point
	// at. Those are fixed at creation; changing one would silently orphan an index.
	ErrImmutable = errors.New("store: immutable wallet field changed")
)

// --- apps ---

// PutApp writes an app record. Registration is idempotent, so this is both
// create and update (§6).
func (t *Tx) PutApp(a App) error {
	if err := ValidSlug(a.Slug); err != nil {
		return err
	}
	return put(t, bApp, []byte(a.Slug), a.encode)
}

// App looks up one app by slug.
func (t *Tx) App(slug string) (App, bool, error) {
	return get(t, bApp, []byte(slug), decodeApp)
}

// Apps returns every app, slug-ordered.
func (t *Tx) Apps() ([]App, error) {
	var out []App
	err := scanPrefix(t, bApp, nil, func(_, v []byte) error {
		a, err := decodeApp(v)
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

// MutateApp applies fn to an app in place. The slug is immutable.
func (t *Tx) MutateApp(slug string, fn func(*App) error) (App, error) {
	a, ok, err := t.App(slug)
	if err != nil {
		return App{}, err
	}
	if !ok {
		return App{}, fmt.Errorf("%w: app %q", ErrNotFound, slug)
	}
	before := a.Slug
	if err := fn(&a); err != nil {
		return App{}, err
	}
	if a.Slug != before {
		return App{}, fmt.Errorf("%w: slug", ErrImmutable)
	}
	a.UpdatedAt = time.Now().UTC()
	return a, t.PutApp(a)
}

// --- wallets ---

// PutWallet writes a wallet and its two lookup indexes (by address, and by
// app+ref for deposit wallets). Both are maintained in the same transaction as
// the record — the price of hand-rolled indexes is that this must never be
// bypassed.
func (t *Tx) PutWallet(w Wallet) error {
	// The master belongs to no app — it is the service's own wallet — so it is
	// the one record with an empty slug. Everything else must name a real app.
	if w.Kind == KindMaster {
		if w.App != "" {
			return fmt.Errorf("store: the master wallet must not belong to an app (got %q)", w.App)
		}
	} else if err := ValidSlug(w.App); err != nil {
		return err
	}
	if w.ID == uuid.Nil {
		return errors.New("store: wallet id must be set")
	}
	if w.Address == (common.Address{}) {
		return errors.New("store: wallet address must be set")
	}
	if err := put(t, bWallet, w.ID[:], w.encode); err != nil {
		return err
	}
	if err := t.tx.Bucket(bAddr).Put(w.Address.Bytes(), w.ID[:]); err != nil {
		return fmt.Errorf("store: put addr index: %w", err)
	}
	if w.Kind == KindDeposit {
		if w.Ref == "" {
			return errors.New("store: deposit wallet requires a ref")
		}
		if err := t.tx.Bucket(bRef).Put(scoped(w.App, []byte(w.Ref)), w.ID[:]); err != nil {
			return fmt.Errorf("store: put ref index: %w", err)
		}
	}
	return nil
}

// Wallet looks up a wallet by its derivation id.
func (t *Tx) Wallet(id uuid.UUID) (Wallet, bool, error) {
	return get(t, bWallet, id[:], decodeWallet)
}

// WalletByAddress resolves an on-chain address. The watcher uses an in-memory
// set on the hot path and this to seed it.
func (t *Tx) WalletByAddress(addr common.Address) (Wallet, bool, error) {
	id := t.tx.Bucket(bAddr).Get(addr.Bytes())
	if id == nil {
		return Wallet{}, false, nil
	}
	var u uuid.UUID
	copy(u[:], id)
	return t.Wallet(u)
}

// WalletByRef resolves an app's deposit wallet by the handle the app chose.
// This is what makes deposit-address creation idempotent on ref (§6).
func (t *Tx) WalletByRef(slug, ref string) (Wallet, bool, error) {
	id := t.tx.Bucket(bRef).Get(scoped(slug, []byte(ref)))
	if id == nil {
		return Wallet{}, false, nil
	}
	var u uuid.UUID
	copy(u[:], id)
	return t.Wallet(u)
}

// EachWallet visits every wallet. Used at startup to build the watcher's
// address set and to evaluate the drain invariant (§4).
func (t *Tx) EachWallet(fn func(Wallet) error) error {
	return scanPrefix(t, bWallet, nil, func(_, v []byte) error {
		w, err := decodeWallet(v)
		if err != nil {
			return err
		}
		return fn(w)
	})
}

// DepositWallets lists an app's deposit wallets in ref order, starting after
// afterRef. The ref index doubles as the pagination index, so listing needs no
// extra bucket.
func (t *Tx) DepositWallets(slug, afterRef string, limit int) ([]Wallet, error) {
	prefix := scopePrefix(slug)
	start := prefix
	if afterRef != "" {
		start = append(scoped(slug, []byte(afterRef)), 0x00) // strictly after
	}
	var out []Wallet
	err := scanFrom(t, bRef, prefix, start, func(_, v []byte) error {
		var u uuid.UUID
		copy(u[:], v)
		w, ok, err := t.Wallet(u)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, w)
		}
		if limit > 0 && len(out) >= limit {
			return errStop
		}
		return nil
	})
	return out, err
}

// MutateWallet applies fn to a wallet in place, rejecting changes to any field
// an index points at.
func (t *Tx) MutateWallet(id uuid.UUID, fn func(*Wallet) error) (Wallet, error) {
	w, ok, err := t.Wallet(id)
	if err != nil {
		return Wallet{}, err
	}
	if !ok {
		return Wallet{}, fmt.Errorf("%w: wallet %s", ErrNotFound, id)
	}
	before := w
	if err := fn(&w); err != nil {
		return Wallet{}, err
	}
	switch {
	case w.ID != before.ID:
		return Wallet{}, fmt.Errorf("%w: id", ErrImmutable)
	case w.App != before.App:
		return Wallet{}, fmt.Errorf("%w: app", ErrImmutable)
	case w.Kind != before.Kind:
		return Wallet{}, fmt.Errorf("%w: kind", ErrImmutable)
	case w.Ref != before.Ref:
		return Wallet{}, fmt.Errorf("%w: ref", ErrImmutable)
	case w.Address != before.Address:
		return Wallet{}, fmt.Errorf("%w: address", ErrImmutable)
	}
	return w, put(t, bWallet, w.ID[:], w.encode)
}

// Credit adds an observed incoming transfer to a wallet's balance. The watcher
// credits every transfer it sees, including sub-threshold dust, so the balance
// tracks the chain exactly even where no deposit record was written (§8).
func (t *Tx) Credit(id uuid.UUID, amount *big.Int) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.Balance = new(big.Int).Add(orZero(w.Balance), orZero(amount))
		return nil
	})
}

// Debit subtracts an observed outgoing transfer. underflow reports that the
// balance would have gone negative, which can only mean a bug in our own
// accounting: the value is clamped at zero and the caller is expected to log
// loudly and raise a metric rather than abort the block, since aborting would
// wedge chain sync on a condition retrying cannot fix.
func (t *Tx) Debit(id uuid.UUID, amount *big.Int) (w Wallet, underflow bool, err error) {
	w, err = t.MutateWallet(id, func(w *Wallet) error {
		next := new(big.Int).Sub(orZero(w.Balance), orZero(amount))
		if next.Sign() < 0 {
			underflow = true
			next = new(big.Int)
		}
		w.Balance = next
		return nil
	})
	return w, underflow, err
}

// ClaimWallet points a wallet at the flow that now owns it, refusing if another
// flow already does. This is the enforcement point for "at most one live flow
// per wallet" — the invariant that stops a second deposit re-triggering
// activation (§4).
func (t *Tx) ClaimWallet(id, flow uuid.UUID) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		if !w.Idle() && w.Flow != flow {
			return fmt.Errorf("store: wallet %s is already owned by flow %s", id, w.Flow)
		}
		w.Flow = flow
		return nil
	})
}

// ReleaseWallet marks a wallet idle again, which is what makes it eligible for
// the next round of the declarative work rules.
func (t *Tx) ReleaseWallet(id uuid.UUID) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.Flow = uuid.Nil
		return nil
	})
}

// SetActive records that a wallet has approved the master, so later flows can
// skip straight past funding and approving.
func (t *Tx) SetActive(id uuid.UUID, active bool) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.Active = active
		return nil
	})
}

// BackOff records that this wallet's flow failed and when to allow the next
// attempt. Without it, a declarative work rule plus a flow that can fail is a
// retry loop that burns gas as fast as the chain allows (§4). It is per-wallet
// rather than per-drain because a failed fee sweep re-fires its rule the same
// way — and since one flow owns a wallet at a time, one counter covers both.
func (t *Tx) BackOff(id uuid.UUID, retryAfter time.Time) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.FailedAttempts++
		w.RetryAfter = retryAfter.UTC()
		return nil
	})
}

// ClearBackoff resets the counter after a flow succeeds.
func (t *Tx) ClearBackoff(id uuid.UUID) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.FailedAttempts = 0
		w.RetryAfter = time.Time{}
		return nil
	})
}
