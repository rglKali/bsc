package store

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
)

// ErrDuplicate means an idempotency key has already been used.
var ErrDuplicate = errors.New("store: duplicate idempotency key")

// PutWithdrawal writes a withdrawal and its four indexes: the global history,
// the per-wallet history, the outstanding set the rules poll, and the
// idempotency key. Idempotency matters here more than anywhere else — over HTTP
// a client retry after a timeout would otherwise double-pay.
//
// The idempotency namespace is global now rather than per-app. One caller owns
// this service, and it must make its keys unique across every wallet it drives
// (§37).
func (t *Tx) PutWithdrawal(wd Withdrawal) error {
	if wd.ID == uuid.Nil {
		return errors.New("store: withdrawal id must be set")
	}
	if wd.Wallet == uuid.Nil {
		return errors.New("store: withdrawal must name the wallet it is paid from")
	}
	// A debit with no reason says nothing about why the money left, which is the
	// one thing the record exists to explain.
	//
	// Nothing in the service can reach this: there are three construction sites
	// and all three set it. What it catches is the fourth — Go's zero value is a
	// valid uint8, so a new call site that forgets the field compiles silently
	// and writes a record that explains nothing. Defaulting it here would hide
	// exactly that bug; the fifteen test fixtures this refused the day it was
	// added are the evidence it is not hypothetical (§42).
	if wd.Reason == 0 {
		return fmt.Errorf("store: withdrawal %s has no reason", wd.ID)
	}
	if err := put(t, bWithdrawal, wd.ID[:], wd.encode); err != nil {
		return err
	}
	created := be64(stampNanos(wd.CreatedAt))
	if err := t.tx.Bucket(iWd).Put(join(created, wd.ID[:]), nil); err != nil {
		return fmt.Errorf("store: put wd index: %w", err)
	}
	if err := t.tx.Bucket(iWdWallet).Put(join(wd.Wallet[:], created, wd.ID[:]), nil); err != nil {
		return fmt.Errorf("store: put wd_wallet index: %w", err)
	}
	if wd.Status.IsTerminal() {
		if err := t.tx.Bucket(iWdOpen).Delete(wd.ID[:]); err != nil {
			return fmt.Errorf("store: delete wd_open: %w", err)
		}
		// Settled debits join the feed, which is what a caller polls the way it
		// polls deposits. A drain is born here, having never been pending (§42).
		if err := t.tx.Bucket(iWdFeed).Put(wd.Settled().Key(), nil); err != nil {
			return fmt.Errorf("store: put wd_feed: %w", err)
		}
	} else if err := t.tx.Bucket(iWdOpen).Put(wd.ID[:], wd.Wallet[:]); err != nil {
		return fmt.Errorf("store: put wd_open: %w", err)
	}
	if wd.IdempotencyKey != "" {
		if err := t.tx.Bucket(iWdIdem).Put([]byte(wd.IdempotencyKey), wd.ID[:]); err != nil {
			return fmt.Errorf("store: put wd_idem: %w", err)
		}
	}
	return nil
}

// Withdrawal looks up one withdrawal by id — the handle the caller has held
// since it created the request.
func (t *Tx) Withdrawal(id uuid.UUID) (Withdrawal, bool, error) {
	return get(t, bWithdrawal, id[:], decodeWithdrawal)
}

// WithdrawalByKey resolves an idempotency key so a retried request replays the
// original response instead of paying twice.
func (t *Tx) WithdrawalByKey(key string) (Withdrawal, bool, error) {
	id := t.tx.Bucket(iWdIdem).Get([]byte(key))
	if id == nil {
		return Withdrawal{}, false, nil
	}
	var u uuid.UUID
	copy(u[:], id)
	return t.Withdrawal(u)
}

// OpenWithdrawals returns every withdrawal not yet terminal, across all
// wallets. `confirmed` is the only terminal status, so this is everything still
// pending — the set the work rules scan and the caller polls.
func (t *Tx) OpenWithdrawals() ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefix(t, iWdOpen, nil, func(k, _ []byte) error {
		var u uuid.UUID
		copy(u[:], k)
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

// WalletOpenWithdrawals is the outstanding set for one wallet. The open index
// stores the wallet as its value, so this filters without loading every record.
func (t *Tx) WalletOpenWithdrawals(wallet uuid.UUID) ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefix(t, iWdOpen, nil, func(k, v []byte) error {
		if len(v) != len(wallet) || string(v) != string(wallet[:]) {
			return nil
		}
		var u uuid.UUID
		copy(u[:], k)
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

// Committed is the total a wallet has promised but not yet paid: the sum of its
// pending withdrawals. Drains never appear — they are recorded once they have
// already happened, so there is never a moment at which one is owed (§42).
//
// This is the overdraft guard, and it is the whole of what replaced the
// reservation ledger. There is nothing to reserve against any more — the wallet
// balance is custody, and custody is the authority — so "may I accept this
// payout" is simply `committed + amount <= balance`, computed from records that
// already exist (§39).
func (t *Tx) Committed(wallet uuid.UUID) (*big.Int, error) {
	open, err := t.WalletOpenWithdrawals(wallet)
	if err != nil {
		return nil, err
	}
	total := new(big.Int)
	for _, wd := range open {
		total.Add(total, orZero(wd.Amount))
	}
	return total, nil
}

// SettledSince reads the finalized-debit feed strictly after `after`, in the
// order the chain settled them, returning the cursor to pass back next time.
//
// It is the mirror of DepositsSince, and deliberately so: a caller reconciles a
// wallet by walking credits and debits, and having to poll one of them by a
// different mechanism would be the asymmetry that makes people write two code
// paths (§42).
func (t *Tx) SettledSince(after Settled, limit int) ([]Withdrawal, Settled, error) {
	next := after
	var out []Withdrawal
	err := scanFrom(t, iWdFeed, nil, after.Next().Key(), func(k, _ []byte) error {
		pos, ok := settledFromKey(k)
		if !ok {
			return fmt.Errorf("store: malformed wd_feed key")
		}
		wd, found, err := t.Withdrawal(pos.ID)
		if err != nil {
			return err
		}
		if found {
			out = append(out, wd)
		}
		next = pos
		if limit > 0 && len(out) >= limit {
			return errStop
		}
		return nil
	})
	if err != nil {
		return nil, after, err
	}
	return out, next, nil
}

// Withdrawals lists every withdrawal, most recent first.
func (t *Tx) Withdrawals(limit int) ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefixReverse(t, iWd, nil, func(k, _ []byte) error {
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

// WalletWithdrawals lists one wallet's withdrawals, most recent first.
func (t *Tx) WalletWithdrawals(wallet uuid.UUID, limit int) ([]Withdrawal, error) {
	var out []Withdrawal
	err := scanPrefixReverse(t, iWdWallet, walletPrefix(wallet), func(k, _ []byte) error {
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
// status reaching terminal removes it from what the caller polls. The id,
// wallet and idempotency key are immutable.
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
	case wd.Wallet != before.Wallet:
		return Withdrawal{}, fmt.Errorf("%w: wallet", ErrImmutable)
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
