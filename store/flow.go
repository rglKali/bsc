package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// --- flows ---

// PutFlow writes a flow record. A flow's state is the instruction to the sender,
// so committing a state change *is* enqueueing the work — there is no separate
// intent, and a crash between the two is impossible by construction (§4).
func (t *Tx) PutFlow(f Flow) error {
	if f.ID == uuid.Nil {
		return errors.New("store: flow id must be set")
	}
	// A gas top-up is the service's own work on its own wallet, so it names no
	// app — the same exemption the master wallet record gets.
	if f.Kind == FlowGasTopUp {
		if f.App != "" {
			return fmt.Errorf("store: a gas top-up must not belong to an app (got %q)", f.App)
		}
	} else if err := ValidSlug(f.App); err != nil {
		return err
	}
	return put(t, bFlow, f.ID[:], f.encode)
}

// Flow looks up one flow.
func (t *Tx) Flow(id uuid.UUID) (Flow, bool, error) {
	return get(t, bFlow, id[:], decodeFlow)
}

// EachFlow visits every live flow. Terminal flows are deleted, so this is the
// live set: the sender scans it for work and startup uses it to resume.
func (t *Tx) EachFlow(fn func(Flow) error) error {
	return scanPrefix(t, bFlow, nil, func(_, v []byte) error {
		f, err := decodeFlow(v)
		if err != nil {
			return err
		}
		return fn(f)
	})
}

// MutateFlow applies fn to a flow in place.
func (t *Tx) MutateFlow(id uuid.UUID, fn func(*Flow) error) (Flow, error) {
	f, ok, err := t.Flow(id)
	if err != nil {
		return Flow{}, err
	}
	if !ok {
		return Flow{}, fmt.Errorf("%w: flow %s", ErrNotFound, id)
	}
	if err := fn(&f); err != nil {
		return Flow{}, err
	}
	f.UpdatedAt = time.Now().UTC()
	return f, t.PutFlow(f)
}

// DeleteFlow removes a finished flow and releases its wallet in one step.
// Anything worth keeping — the final error, the settling transaction hash —
// must already have been copied onto the withdrawal or deposit record by the
// caller, in this same transaction, because after this the flow is gone (§3).
func (t *Tx) DeleteFlow(f Flow) error {
	if err := t.tx.Bucket(bFlow).Delete(f.ID[:]); err != nil {
		return fmt.Errorf("store: delete flow: %w", err)
	}
	if f.Wallet != uuid.Nil {
		if _, err := t.MutateWallet(f.Wallet, func(w *Wallet) error {
			if w.Flow == f.ID {
				w.Flow = uuid.Nil
			}
			return nil
		}); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// --- the in-flight transaction index (also the watcher's tx watchlist) ---

// LinkTx records that a broadcast transaction belongs to a flow. This bucket is
// simultaneously the router for confirmations and the watcher's tx watchlist —
// one structure instead of the two v1 kept in different services (§4).
func (t *Tx) LinkTx(hash common.Hash, ref TxRef) error {
	return put(t, bTx, hash.Bytes(), ref.encode)
}

// TxRefByHash resolves a confirmed transaction to its flow and journal
// coordinates in a single lookup.
func (t *Tx) TxRefByHash(hash common.Hash) (TxRef, bool, error) {
	return get(t, bTx, hash.Bytes(), decodeTxRef)
}

// UnlinkTx drops a transaction from the watchlist once it is finalized.
func (t *Tx) UnlinkTx(hash common.Hash) error {
	if err := t.tx.Bucket(bTx).Delete(hash.Bytes()); err != nil {
		return fmt.Errorf("store: unlink tx: %w", err)
	}
	return nil
}

// EachWatchedTx visits every transaction awaiting finality; the watcher loads
// these at startup.
func (t *Tx) EachWatchedTx(fn func(common.Hash, TxRef) error) error {
	return scanPrefix(t, bTx, nil, func(k, v []byte) error {
		ref, err := decodeTxRef(v)
		if err != nil {
			return err
		}
		return fn(common.BytesToHash(k), ref)
	})
}

// --- the send journal ---

// PutSend journals a signed transaction. It must be committed *before* the
// transaction is broadcast: on restart the same bytes are re-broadcast, which
// is idempotent on-chain, rather than a second transaction being signed (§4).
func (t *Tx) PutSend(s Send) error {
	return put(t, bSend, sendKey(s.Signer, s.Nonce), s.encode)
}

// Send reads one journal entry.
func (t *Tx) Send(signer common.Address, nonce uint64) (Send, bool, error) {
	return get(t, bSend, sendKey(signer, nonce), decodeSend)
}

// DeleteSend drops a journal entry once its transaction is finalized.
func (t *Tx) DeleteSend(signer common.Address, nonce uint64) error {
	if err := t.tx.Bucket(bSend).Delete(sendKey(signer, nonce)); err != nil {
		return fmt.Errorf("store: delete send: %w", err)
	}
	return nil
}

// EachSend visits journal entries in signer-then-nonce order. That ordering is
// the point of the key layout: re-broadcasting a dropped transaction has to go
// out in nonce order or the gap never closes.
func (t *Tx) EachSend(fn func(Send) error) error {
	return scanPrefix(t, bSend, nil, func(_, v []byte) error {
		s, err := decodeSend(v)
		if err != nil {
			return err
		}
		return fn(s)
	})
}
