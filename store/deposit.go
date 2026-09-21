package store

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// PutDeposit records a deposit, returning created=false if this transfer was
// already recorded. Dedup is a property of the key (tx hash ++ log index), so
// there is no uniqueness check to forget and a reprocessed block is harmless.
//
// It maintains two indexes: the global feed whose key is the caller-facing
// cursor, and a per-wallet listing.
func (t *Tx) PutDeposit(d Deposit) (created bool, err error) {
	if d.Wallet == 0 {
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
	if err := t.tx.Bucket(iDepWallet).Put(join(d.Wallet.Key(), cur), key); err != nil {
		return false, fmt.Errorf("store: put dep_wallet index: %w", err)
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
func (t *Tx) WalletDeposits(wallet WalletID, limit int) ([]Deposit, error) {
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
