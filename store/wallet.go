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
	// ErrImmutable means a mutation tried to change a field that indexes point
	// at. Those are fixed at creation; changing one would silently orphan an index.
	ErrImmutable = errors.New("store: immutable wallet field changed")

	// ErrDrainCycle means a DrainTo would make funds circulate between managed
	// wallets instead of arriving somewhere.
	ErrDrainCycle = errors.New("store: drain destination forms a cycle")

	// ErrDrainDepth means a drain chain is longer than the service will follow.
	ErrDrainDepth = errors.New("store: drain chain is too deep")

	// ErrRefTaken means another wallet already holds this ref.
	ErrRefTaken = errors.New("store: ref is already in use")
)

// MaxDrainDepth bounds how long a chain of DrainTo references may be. A chain
// costs one transfer per hop, each paid for by the master, so depth is gas
// spent on moving the same money repeatedly. Two hops covers every topology
// anyone has asked for; the limit exists to make an accidental chain loud.
const MaxDrainDepth = 8

// ErrAddressReissued means a newly derived wallet landed on an address that
// another wallet already holds. It cannot happen while the id sequence only
// moves forward, so it means the sequence was rewound (§48).
var ErrAddressReissued = errors.New("store: derived address already belongs to another wallet")

// --- wallets ---

// PutWallet writes a wallet and its two lookup indexes, by address and by ref.
// Both are maintained in the same transaction as the record — the price of
// hand-rolled indexes is that this must never be bypassed.
func (t *Tx) PutWallet(w Wallet) error {
	// Ids start at 1: every wallet here is derived, every wallet is nameable,
	// and there is no special row (§49). Zero is refused so a call site that
	// forgot to fill the field in fails here rather than writing a wallet whose
	// key keys.Derive will not produce.
	if w.ID == 0 {
		return errors.New("store: wallet id must be set")
	}
	if w.Address == (common.Address{}) {
		return errors.New("store: wallet address must be set")
	}
	if err := ValidRef(w.Ref); err != nil {
		return err
	}
	if w.DrainTo == w.Address {
		return fmt.Errorf("%w: wallet %s drains to itself", ErrDrainCycle, w.Ref)
	}
	// A derived address that already belongs to a different id means the id
	// sequence was rewound — a restored snapshot is the way that happens. Two
	// refs sharing one address would merge two callers' money silently, so this
	// refuses rather than overwrites (§48).
	if owner := t.tx.Bucket(bAddr).Get(w.Address.Bytes()); owner != nil {
		if got := walletIDFromKey(owner); got != w.ID {
			return fmt.Errorf("%w: address %s already belongs to wallet %s",
				ErrAddressReissued, w.Address.Hex(), got)
		}
	}
	if err := put(t, bWallet, w.ID.Key(), w.encode); err != nil {
		return err
	}
	if err := t.tx.Bucket(bAddr).Put(w.Address.Bytes(), w.ID.Key()); err != nil {
		return fmt.Errorf("store: put addr index: %w", err)
	}
	if err := t.tx.Bucket(bRef).Put([]byte(w.Ref), w.ID.Key()); err != nil {
		return fmt.Errorf("store: put ref index: %w", err)
	}
	return nil
}

// NextWalletID returns the id a new wallet should take: one past the highest
// that exists.
//
// There is deliberately no stored counter. Wallet keys are 8-byte big-endian,
// so bbolt's key order is numeric order and the last key IS the highest id —
// which means the "next" id is derived from the table itself and cannot drift
// from it. A counter kept beside the table could; this cannot.
//
// It is only a starting point. keys.Allocate may step past it if an index does
// not yield a valid scalar, so a wallet's id is not its position in creation
// order and nothing may assume the sequence is gapless.
//
// KNOWN LIMIT: this reads the database, so restoring an older snapshot rewinds
// it, and wallets created after that snapshot would have their ids reissued to
// different refs — two handles, one address. PutWallet refuses the collision if
// the address is still known, but after a restore it is not. The recovery is a
// chain-side gap scan (§48).
func (t *Tx) NextWalletID() (WalletID, error) {
	c := t.tx.Bucket(bWallet).Cursor()
	k, _ := c.Last()
	if k == nil {
		return 1, nil
	}
	last := walletIDFromKey(k)
	if last == ^WalletID(0) {
		return 0, errors.New("store: wallet ids exhausted")
	}
	return last + 1, nil
}

// Wallet looks up a wallet by its derivation id.
func (t *Tx) Wallet(id WalletID) (Wallet, bool, error) {
	return get(t, bWallet, id.Key(), decodeWallet)
}

// WalletByAddress resolves an on-chain address. The watcher uses an in-memory
// set on the hot path and this to seed it.
func (t *Tx) WalletByAddress(addr common.Address) (Wallet, bool, error) {
	id := t.tx.Bucket(bAddr).Get(addr.Bytes())
	if id == nil {
		return Wallet{}, false, nil
	}
	return t.Wallet(walletIDFromKey(id))
}

// WalletByRef resolves a wallet by the handle the caller chose. Refs are unique
// across the service, which is what makes wallet creation idempotent on ref.
func (t *Tx) WalletByRef(ref string) (Wallet, bool, error) {
	id := t.tx.Bucket(bRef).Get([]byte(ref))
	if id == nil {
		return Wallet{}, false, nil
	}
	return t.Wallet(walletIDFromKey(id))
}

// EachWallet visits every wallet, master included. Used at startup to build the
// watcher's address set and to evaluate the work rules.
func (t *Tx) EachWallet(fn func(Wallet) error) error {
	return scanPrefix(t, bWallet, nil, func(_, v []byte) error {
		w, err := decodeWallet(v)
		if err != nil {
			return err
		}
		return fn(w)
	})
}

// Wallets lists caller-created wallets in ref order, starting after afterRef.
// The ref index doubles as the pagination index, so listing needs no extra
// bucket — and the master, having no ref, is correctly absent from it.
func (t *Tx) Wallets(afterRef string, limit int) ([]Wallet, error) {
	start := []byte(nil)
	if afterRef != "" {
		start = append([]byte(afterRef), 0x00) // strictly after
	}
	var out []Wallet
	err := scanFrom(t, bRef, nil, start, func(_, v []byte) error {
		w, ok, err := t.Wallet(walletIDFromKey(v))
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
//
// DrainTo is deliberately *not* immutable — retargeting is the one piece of
// configuration a wallet has — but it is validated here rather than trusted,
// because a cycle is not a bad value the caller sees, it is gas burning until
// somebody notices (§35).
func (t *Tx) MutateWallet(id WalletID, fn func(*Wallet) error) (Wallet, error) {
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
	case w.Ref != before.Ref:
		return Wallet{}, fmt.Errorf("%w: ref", ErrImmutable)
	case w.Address != before.Address:
		return Wallet{}, fmt.Errorf("%w: address", ErrImmutable)
	}
	if w.DrainTo != before.DrainTo {
		if err := t.CheckDrainChain(w.ID, w.Address, w.DrainTo); err != nil {
			return Wallet{}, err
		}
		w.UpdatedAt = time.Now().UTC()
	}
	return w, put(t, bWallet, w.ID.Key(), w.encode)
}

// CheckDrainChain walks the DrainTo references from one wallet and refuses a
// destination that leads back to it or runs longer than MaxDrainDepth.
//
// The two-level design could not express a cycle: a deposit wallet drained to
// its app's top-level and a top-level drained nowhere, so the shape was a fact
// about the type rather than a value anyone could set. Making the topology a
// field is what buys arbitrary shapes, and A→B→A is one of them — a loop that
// would re-fire every block and spend the master's gas as fast as the chain
// produces blocks. Nothing downstream would notice, because every individual
// transfer succeeds (§35).
//
// A destination bsc does not manage ends the walk: it is somebody else's
// address and cannot point back at us.
func (t *Tx) CheckDrainChain(id WalletID, from, drainTo common.Address) error {
	if drainTo == (common.Address{}) {
		return nil
	}
	if drainTo == from {
		return fmt.Errorf("%w: wallet %s drains to itself", ErrDrainCycle, id)
	}
	seen := map[WalletID]bool{id: true}
	next := drainTo
	for depth := 1; ; depth++ {
		w, ok, err := t.WalletByAddress(next)
		if err != nil {
			return err
		}
		if !ok {
			return nil // an address we do not manage: the chain ends here
		}
		if seen[w.ID] {
			return fmt.Errorf("%w: %s is already in the chain from %s", ErrDrainCycle, w.Address, id)
		}
		if depth >= MaxDrainDepth {
			return fmt.Errorf("%w: more than %d hops from %s", ErrDrainDepth, MaxDrainDepth, id)
		}
		if !w.Proxies() {
			return nil // it accumulates: the money stops there
		}
		seen[w.ID] = true
		next = w.DrainTo
	}
}

// Credit adds an observed incoming transfer to a wallet's balance. The watcher
// credits every transfer it sees, so the balance tracks the chain exactly.
func (t *Tx) Credit(id WalletID, amount *big.Int) (Wallet, error) {
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
func (t *Tx) Debit(id WalletID, amount *big.Int) (w Wallet, underflow bool, err error) {
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
// activation.
func (t *Tx) ClaimWallet(id WalletID, flow uuid.UUID) (Wallet, error) {
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
func (t *Tx) ReleaseWallet(id WalletID) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.Flow = uuid.Nil
		return nil
	})
}

// SetActive records that a wallet has approved the master, so later flows can
// skip straight past funding and approving.
func (t *Tx) SetActive(id WalletID, active bool) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.Active = active
		return nil
	})
}

// BackOff records that this wallet's flow failed and when to allow the next
// attempt. Without it, a declarative work rule plus a flow that can fail is a
// retry loop that burns gas as fast as the chain allows. It is per-wallet
// rather than per-flow because one flow owns a wallet at a time, so one counter
// covers a failed drain and a failed payout alike.
func (t *Tx) BackOff(id WalletID, retryAfter time.Time) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.FailedAttempts++
		w.RetryAfter = retryAfter.UTC()
		return nil
	})
}

// ClearBackoff resets the counter after a flow succeeds.
func (t *Tx) ClearBackoff(id WalletID) (Wallet, error) {
	return t.MutateWallet(id, func(w *Wallet) error {
		w.FailedAttempts = 0
		w.RetryAfter = time.Time{}
		return nil
	})
}
