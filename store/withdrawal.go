package store

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Debits live in two keyspaces, and which one a record is in *is* its status.
//
//	state/withdrawal  a promise: accepted, committed against its wallet,
//	                  not yet on-chain. Self-pruning — settling removes it.
//	log/withdrawal    a fact: money that left. Kept forever.
//
// Settling moves the record. That is the whole design (§51): there is no stored
// `pending`/`confirmed` field to disagree with anything, no partial index to
// keep the two populations apart, and neither record carries fields that are
// meaningless in its own half of the life — a promise has no transaction hash,
// and a fact has no retry count.
//
// The cost is that three lookups have to try both buckets: by id, by
// idempotency key, and a wallet's full history. Everything else got narrower —
// the work rules and the overdraft guard scan only state/, the settled feed
// scans only log/.

// ErrDuplicate means an idempotency key has already been used.
var ErrDuplicate = errors.New("store: duplicate idempotency key")

// --- promises: state/withdrawal ---

// PutPending writes a promise. It maintains the idempotency key and nothing
// else: the bucket is the outstanding set, so there is no open index to keep in
// step, and the per-wallet listing is a filter over what is in flight rather
// than an index over history.
//
// The idempotency namespace is global rather than per-wallet. One caller owns
// this service and must make its keys unique across every wallet it drives
// (§37). Idempotency matters here more than anywhere else — over HTTP a client
// retry after a timeout would otherwise double-pay.
func (t *Tx) PutPending(p Pending) error {
	if err := validDebit(p.ID, p.Wallet, p.Reason); err != nil {
		return err
	}
	if err := put(t, bPending, p.ID[:], p.encode); err != nil {
		return err
	}
	if p.IdempotencyKey != "" {
		if err := t.tx.Bucket(iWdIdem).Put([]byte(p.IdempotencyKey), p.ID[:]); err != nil {
			return fmt.Errorf("store: put wd_idem: %w", err)
		}
	}
	return nil
}

// Pending looks up one outstanding promise.
func (t *Tx) Pending(id uuid.UUID) (Pending, bool, error) {
	return get(t, bPending, id[:], decodePending)
}

// OpenPending returns every promise still outstanding, across all wallets — the
// set the work rules scan.
//
// It scans the bucket directly. That is not a regression: the old open index
// was itself a full scan filtered by value, so the cost is unchanged and is
// proportional to what is in flight rather than to history.
func (t *Tx) OpenPending() ([]Pending, error) {
	var out []Pending
	err := scanPrefix(t, bPending, nil, func(_, v []byte) error {
		p, err := decodePending(v)
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// WalletPending is the outstanding set for one wallet.
func (t *Tx) WalletPending(wallet WalletID) ([]Pending, error) {
	all, err := t.OpenPending()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if p.Wallet == wallet {
			out = append(out, p)
		}
	}
	return out, nil
}

// MutatePending applies fn in place. The id, wallet, amount, destination and
// idempotency key are immutable — what may change is how the attempt is going.
func (t *Tx) MutatePending(id uuid.UUID, fn func(*Pending) error) (Pending, error) {
	p, ok, err := t.Pending(id)
	if err != nil {
		return Pending{}, err
	}
	if !ok {
		return Pending{}, fmt.Errorf("%w: pending withdrawal %s", ErrNotFound, id)
	}
	before := p
	if err := fn(&p); err != nil {
		return Pending{}, err
	}
	switch {
	case p.ID != before.ID:
		return Pending{}, fmt.Errorf("%w: id", ErrImmutable)
	case p.Wallet != before.Wallet:
		return Pending{}, fmt.Errorf("%w: wallet", ErrImmutable)
	case p.Destination != before.Destination:
		return Pending{}, fmt.Errorf("%w: destination", ErrImmutable)
	case cmpAmount(p.Amount, before.Amount) != 0:
		return Pending{}, fmt.Errorf("%w: amount", ErrImmutable)
	case p.IdempotencyKey != before.IdempotencyKey:
		return Pending{}, fmt.Errorf("%w: idempotency key", ErrImmutable)
	case !p.CreatedAt.Equal(before.CreatedAt):
		return Pending{}, fmt.Errorf("%w: created_at", ErrImmutable)
	}
	p.UpdatedAt = time.Now().UTC()
	return p, put(t, bPending, p.ID[:], p.encode)
}

// --- facts: log/withdrawal ---

// Settle moves a promise into the log: the record leaves state/, arrives in
// log/ with the transfer that kept it, and joins the settled feed. One
// transaction, so there is never a moment where it is in both or neither.
func (t *Tx) Settle(id uuid.UUID, txHash common.Hash, block uint64, now time.Time) (Withdrawal, error) {
	p, ok, err := t.Pending(id)
	if err != nil {
		return Withdrawal{}, err
	}
	if !ok {
		return Withdrawal{}, fmt.Errorf("%w: pending withdrawal %s", ErrNotFound, id)
	}
	if err := t.tx.Bucket(bPending).Delete(id[:]); err != nil {
		return Withdrawal{}, fmt.Errorf("store: delete pending: %w", err)
	}
	wd := p.Settle(txHash, block, now)
	return wd, t.PutWithdrawal(wd)
}

// PutWithdrawal writes a settled debit straight into the log. A payout arrives
// here through Settle; a drain arrives here directly, having never been a
// promise — nothing was owed to anybody while it was in flight (§42).
func (t *Tx) PutWithdrawal(wd Withdrawal) error {
	if err := validDebit(wd.ID, wd.Wallet, wd.Reason); err != nil {
		return err
	}
	if wd.TxHash == (common.Hash{}) {
		return fmt.Errorf("store: settled withdrawal %s has no transaction", wd.ID)
	}
	if err := put(t, bWithdrawal, wd.ID[:], wd.encode); err != nil {
		return err
	}
	created := be64(stampNanos(wd.CreatedAt))
	if err := t.tx.Bucket(iWdWallet).Put(join(wd.Wallet.Key(), created, wd.ID[:]), nil); err != nil {
		return fmt.Errorf("store: put wd_wallet index: %w", err)
	}
	// The settled feed, which a caller polls the way it polls deposits (§42).
	if err := t.tx.Bucket(iWdFeed).Put(wd.Settled().Key(), nil); err != nil {
		return fmt.Errorf("store: put wd_feed: %w", err)
	}
	if wd.IdempotencyKey != "" {
		if err := t.tx.Bucket(iWdIdem).Put([]byte(wd.IdempotencyKey), wd.ID[:]); err != nil {
			return fmt.Errorf("store: put wd_idem: %w", err)
		}
	}
	return nil
}

// Withdrawal looks up one settled debit.
func (t *Tx) Withdrawal(id uuid.UUID) (Withdrawal, bool, error) {
	return get(t, bWithdrawal, id[:], decodeWithdrawal)
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

// WalletSettled lists one wallet's settled debits, most recent first.
func (t *Tx) WalletSettled(wallet WalletID, limit int) ([]Withdrawal, error) {
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

// --- across both ---

// DebitByKey resolves an idempotency key so a retried request replays the
// original response instead of paying twice. It has to look in both keyspaces:
// the original may still be a promise, or it may already have settled.
func (t *Tx) DebitByKey(key string) (pending *Pending, settled *Withdrawal, err error) {
	raw := t.tx.Bucket(iWdIdem).Get([]byte(key))
	if raw == nil {
		return nil, nil, nil
	}
	var id uuid.UUID
	copy(id[:], raw)
	if p, ok, err := t.Pending(id); err != nil {
		return nil, nil, err
	} else if ok {
		return &p, nil, nil
	}
	wd, ok, err := t.Withdrawal(id)
	if err != nil || !ok {
		return nil, nil, err
	}
	return nil, &wd, nil
}

// Committed is the total a wallet has promised but not yet paid. Drains never
// appear — they are recorded once they have already happened, so there is never
// a moment at which one is owed (§42).
//
// This is the overdraft guard, and it is the whole of what replaced the
// reservation ledger. There is nothing to reserve against any more — the wallet
// balance is custody, and custody is the authority — so "may I accept this
// payout" is simply `committed + amount <= balance`, computed from records that
// already exist (§39). Splitting the keyspaces made it a scan of exactly the
// promises and nothing else (§51).
func (t *Tx) Committed(wallet WalletID) (*big.Int, error) {
	open, err := t.WalletPending(wallet)
	if err != nil {
		return nil, err
	}
	total := new(big.Int)
	for _, p := range open {
		total.Add(total, orZero(p.Amount))
	}
	return total, nil
}

// validDebit is the shape both records share.
//
// A debit with no reason says nothing about why the money left, which is the
// one thing the record exists to explain. Nothing in the service can reach it:
// there are three construction sites and all three set it. What it catches is
// the fourth — Go's zero value is a valid uint8, so a new call site that forgets
// the field compiles silently and writes a record that explains nothing.
// Defaulting it here would hide exactly that bug; the fifteen test fixtures this
// refused the day it was added are the evidence it is not hypothetical (§42).
func validDebit(id uuid.UUID, wallet WalletID, reason DebitReason) error {
	switch {
	case id == uuid.Nil:
		return errors.New("store: withdrawal id must be set")
	case wallet == 0:
		return errors.New("store: withdrawal must name the wallet it is paid from")
	case reason == 0:
		return fmt.Errorf("store: withdrawal %s has no reason", id)
	}
	return nil
}

func cmpAmount(a, b *big.Int) int { return orZero(a).Cmp(orZero(b)) }

// stampNanos renders a timestamp for use inside a sort key.
func stampNanos(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}
