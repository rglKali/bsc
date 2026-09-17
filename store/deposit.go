package store

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// PutDeposit records a deposit, returning created=false if this transfer was
// already recorded. Dedup is a property of the key (tx hash ++ log index), so
// there is no uniqueness check to forget and a reprocessed block is harmless.
//
// It maintains three indexes: the global feed whose key is the caller-facing
// cursor, a per-wallet listing, and the set still awaiting a drain.
func (t *Tx) PutDeposit(d Deposit) (created bool, err error) {
	if d.Wallet == uuid.Nil {
		return false, fmt.Errorf("store: deposit needs a wallet")
	}
	key := depositKey(d.TxHash, d.LogIndex)
	if t.tx.Bucket(bDeposit).Get(key) != nil {
		return false, nil
	}
	if err := put(t, bDeposit, key, d.encode); err != nil {
		return false, err
	}
	cur := d.Cursor().Key()
	// The cursor index: "<block><logindex>" -> deposit key. Written in
	// block-then-log order by construction, which is what makes the cursor
	// monotonic for readers.
	if err := t.tx.Bucket(iDep).Put(cur, key); err != nil {
		return false, fmt.Errorf("store: put dep index: %w", err)
	}
	if err := t.tx.Bucket(iDepWallet).Put(join(d.Wallet[:], cur), key); err != nil {
		return false, fmt.Errorf("store: put dep_wallet index: %w", err)
	}
	// Only a deposit that still has somewhere to go joins the open set. On a
	// wallet that accumulates there is no drain coming, so the deposit is
	// already in its final state and would otherwise sit in this index forever
	// (§34).
	if d.Status == DepositReceived {
		w, found, err := t.Wallet(d.Wallet)
		if err != nil {
			return false, err
		}
		if found && w.Proxies() {
			if err := t.tx.Bucket(iDepOpen).Put(join(d.Wallet[:], key), key); err != nil {
				return false, fmt.Errorf("store: put dep_open index: %w", err)
			}
		}
	}
	return true, nil
}

// Deposit looks up one recorded deposit.
func (t *Tx) Deposit(tx common.Hash, logIndex uint32) (Deposit, bool, error) {
	return get(t, bDeposit, depositKey(tx, logIndex), decodeDeposit)
}

// DepositsSince reads the deposit feed strictly after `after`, in chain order,
// returning the cursor to pass back next time. This is the only cursor in the
// system: deposits are unsolicited, so the caller needs "what is new", while
// withdrawals it initiated are polled by id.
//
// The returned cursor is `after` unchanged when nothing new exists, so a caller
// that keeps passing it back never loses its place.
func (t *Tx) DepositsSince(after Cursor, limit int) ([]Deposit, Cursor, error) {
	next := after
	var out []Deposit
	err := scanFrom(t, iDep, nil, after.Next().Key(), func(k, v []byte) error {
		d, err := decodeDeposit(t.tx.Bucket(bDeposit).Get(v))
		if err != nil {
			return fmt.Errorf("store: decode deposit for index key: %w", err)
		}
		out = append(out, d)
		if c, ok := cursorFromKey(k); ok {
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

// WalletDeposits lists one wallet's deposits in chain order.
func (t *Tx) WalletDeposits(wallet uuid.UUID, limit int) ([]Deposit, error) {
	var out []Deposit
	err := scanPrefix(t, iDepWallet, walletPrefix(wallet), func(_, v []byte) error {
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

// OpenDeposits lists deposits on proxy wallets that have arrived but not yet
// been forwarded — the `?status=received` view on a wallet that drains.
func (t *Tx) OpenDeposits(wallet uuid.UUID, limit int) ([]Deposit, error) {
	var out []Deposit
	err := scanPrefix(t, iDepOpen, walletPrefix(wallet), func(_, v []byte) error {
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

// ForwardDeposits marks every deposit awaiting a drain on this wallet as
// forwarded, linking each to the debit that carried it, and returns how many.
// One drain moves the whole balance and so consumes several credits at once,
// which is exactly why this is a link on the record rather than an entry in the
// feed.
//
// Nothing is credited anywhere. The status is a statement about where the money
// physically is, not about who is owed it — bsc does not know that any more
// (§34).
func (t *Tx) ForwardDeposits(wallet, debit uuid.UUID) (int, error) {
	var keys [][]byte
	if err := scanPrefix(t, iDepOpen, walletPrefix(wallet), func(_, v []byte) error {
		keys = append(keys, append([]byte(nil), v...))
		return nil
	}); err != nil {
		return 0, err
	}
	for _, key := range keys {
		raw := t.tx.Bucket(bDeposit).Get(key)
		if raw == nil {
			return 0, fmt.Errorf("store: dep_open points at missing deposit")
		}
		d, err := decodeDeposit(raw)
		if err != nil {
			return 0, err
		}
		d.Status = DepositForwarded
		d.SweptBy = debit
		if err := put(t, bDeposit, key, d.encode); err != nil {
			return 0, err
		}
		if err := t.tx.Bucket(iDepOpen).Delete(join(wallet[:], key)); err != nil {
			return 0, fmt.Errorf("store: delete dep_open: %w", err)
		}
	}
	return len(keys), nil
}
