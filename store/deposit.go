package store

import (
	"bsc/money"

	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// PutDeposit records a deposit, returning created=false if this transfer was
// already recorded. Dedup is a property of the key (tx hash ++ log index), so
// there is no uniqueness check to forget and a reprocessed block is harmless.
//
// It maintains two indexes: idx/dep, whose key is the app-facing cursor, and
// idx/dep_open, the set awaiting a drain.
func (t *Tx) PutDeposit(d Deposit) (created bool, err error) {
	if err := ValidSlug(d.App); err != nil {
		return false, err
	}
	key := depositKey(d.TxHash, d.LogIndex)
	if t.tx.Bucket(bDeposit).Get(key) != nil {
		return false, nil
	}
	if err := put(t, bDeposit, key, d.encode); err != nil {
		return false, err
	}
	// The cursor index: "<slug>\0<block><logindex>" -> deposit key. Written in
	// block-then-log order by construction, which is what makes the cursor
	// monotonic for readers (§9).
	if err := t.tx.Bucket(iDep).Put(scoped(d.App, d.Cursor().Key()), key); err != nil {
		return false, fmt.Errorf("store: put dep index: %w", err)
	}
	if d.Status == DepositConfirmed {
		if err := t.tx.Bucket(iDepOpen).Put(scoped(d.App, d.Wallet[:], key), key); err != nil {
			return false, fmt.Errorf("store: put dep_open index: %w", err)
		}
	}
	return true, nil
}

// Deposit looks up one recorded deposit.
func (t *Tx) Deposit(tx common.Hash, logIndex uint32) (Deposit, bool, error) {
	return get(t, bDeposit, depositKey(tx, logIndex), decodeDeposit)
}

// DepositsSince reads an app's deposit feed strictly after `after`, in chain
// order, returning the cursor to pass back next time. This is the only cursor
// in the system: deposits are unsolicited, so the app needs "what is new", while
// withdrawals it initiated are polled by id (§9).
//
// The returned cursor is `after` unchanged when nothing new exists, so an app
// that keeps passing it back never loses its place.
func (t *Tx) DepositsSince(slug string, after Cursor, limit int) ([]Deposit, Cursor, error) {
	prefix := scopePrefix(slug)
	start := scoped(slug, after.Next().Key())
	next := after
	var out []Deposit
	err := scanFrom(t, iDep, prefix, start, func(k, v []byte) error {
		d, err := decodeDeposit(t.tx.Bucket(bDeposit).Get(v))
		if err != nil {
			return fmt.Errorf("store: decode deposit for index key: %w", err)
		}
		out = append(out, d)
		if c, ok := cursorFromScoped(k); ok {
			next = c
		}
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

// OpenDeposits lists an app's deposits that have been detected but not yet
// swept into its top-level wallet — the `?status=confirmed` view.
func (t *Tx) OpenDeposits(slug string, limit int) ([]Deposit, error) {
	var out []Deposit
	err := scanPrefix(t, iDepOpen, scopePrefix(slug), func(_, v []byte) error {
		d, err := decodeDeposit(t.tx.Bucket(bDeposit).Get(v))
		if err != nil {
			return err
		}
		out = append(out, d)
		if limit > 0 && len(out) >= limit {
			return errStop
		}
		return nil
	})
	return out, err
}

// CreditDeposits marks every deposit awaiting a drain on this wallet as
// credited, stamping the sweep that did it, and returns the cents they are
// worth. One sweep moves the whole balance and so credits several deposits at
// once, which is exactly why crediting is a status on the record rather than an
// entry in the feed (§9).
//
// The returned total is what the caller must add to the app's ledger, in the
// same transaction: the sweep landing is precisely the moment the money becomes
// spendable, because it is the moment it reaches the wallet payouts draw on.
func (t *Tx) CreditDeposits(slug string, wallet uuid.UUID, drainTx common.Hash) (int, money.Cents, error) {
	var keys [][]byte
	if err := scanPrefix(t, iDepOpen, scoped(slug, wallet[:]), func(_, v []byte) error {
		keys = append(keys, append([]byte(nil), v...))
		return nil
	}); err != nil {
		return 0, 0, err
	}
	var credited money.Cents
	for _, key := range keys {
		raw := t.tx.Bucket(bDeposit).Get(key)
		if raw == nil {
			return 0, 0, fmt.Errorf("store: dep_open points at missing deposit")
		}
		d, err := decodeDeposit(raw)
		if err != nil {
			return 0, 0, err
		}
		d.Status = DepositCredited
		d.DrainTx = drainTx
		if err := put(t, bDeposit, key, d.encode); err != nil {
			return 0, 0, err
		}
		if err := t.tx.Bucket(iDepOpen).Delete(scoped(slug, wallet[:], key)); err != nil {
			return 0, 0, fmt.Errorf("store: delete dep_open: %w", err)
		}
		credited += d.Cents
	}
	return len(keys), credited, nil
}
