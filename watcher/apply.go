package watcher

import (
	"errors"
	"fmt"
	"time"

	"bsc/engine"
	"bsc/metrics"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
)

// apply folds one finalized block into the store, inside the caller's write
// transaction. Ordering matters and is the reason this reads as a list:
//
//  1. per receipt, its confirmation first and then its transfer logs, so a flow
//     that just finished has released its wallet, and so the credits and debits
//     of a transaction are applied after the flow waiting on it resolved;
//  2. within a log, the credit before the debit — they touch different wallets,
//     so this only matters for reading it;
//  3. the work rules last, once, over every wallet the whole block touched, so
//     they see post-block balances on wallets no longer mid-flow.
//
// Note that (1) means a drain's ledger credit runs *before* that same receipt's
// custody credit. Nothing outside can observe the gap: the whole block is one
// write transaction, which is the point (§3).
//
// Nothing here credits an app's ledger. Custody is what the chain says a wallet
// holds; the ledger is what an app is owed, and a deposit joins it only when its
// drain lands (§22).
func (w *Watcher) apply(tx *store.Tx, block uint64, receipts []*types.Receipt, now time.Time) error {
	touched := make(map[uuid.UUID]struct{})

	for _, rc := range receipts {
		if err := w.confirm(tx, rc, now, touched); err != nil {
			return err
		}
		for _, lg := range rc.Logs {
			if err := w.transfer(tx, rc, lg, block, now, touched); err != nil {
				return err
			}
		}
	}
	return w.evaluateTouched(tx, touched, now)
}

// confirm resolves a finalized transaction against the in-flight set and
// advances the flow that was waiting on it. The watchlist and the router are the
// same structure, so this is one lookup.
func (w *Watcher) confirm(tx *store.Tx, rc *types.Receipt, now time.Time, touched map[uuid.UUID]struct{}) error {
	ref, found, err := tx.TxRefByHash(rc.TxHash)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	f, found, err := tx.Flow(ref.Flow)
	if err != nil {
		return err
	}
	if !found {
		// The flow is gone but its watchlist entry survived — clean up rather
		// than wedging every future block on the same orphan.
		w.log.Warn("watchlist entry for a missing flow", "tx", rc.TxHash.Hex(), "flow", ref.Flow)
		return w.forget(tx, rc.TxHash, ref)
	}

	success := rc.Status == types.ReceiptStatusSuccessful
	if !success {
		f.Error = "transaction reverted on-chain"
	}
	// Drop the journal and the watchlist entry before advancing: the
	// transaction is final, so re-broadcasting it is neither possible nor useful.
	if err := w.forget(tx, rc.TxHash, ref); err != nil {
		return err
	}
	next, err := engine.Advance(tx, f, success, now)
	if err != nil {
		return fmt.Errorf("watcher: advance flow %s: %w", f.ID, err)
	}
	touched[f.Wallet] = struct{}{}

	metrics.FlowTransitions.WithLabelValues(f.Kind.String(), next.State.String()).Inc()
	w.log.Info("flow advanced",
		"flow", f.ID, "kind", f.Kind, "from", f.State, "to", next.State,
		"tx", rc.TxHash.Hex(), "ok", success)
	return nil
}

// forget removes a finalized transaction from the watchlist and the journal.
func (w *Watcher) forget(tx *store.Tx, hash common.Hash, ref store.TxRef) error {
	if err := tx.UnlinkTx(hash); err != nil {
		return err
	}
	return tx.DeleteSend(ref.Signer, ref.Nonce)
}

// transfer applies one USDT Transfer log to the wallets it touches.
func (w *Watcher) transfer(tx *store.Tx, rc *types.Receipt, lg *types.Log, block uint64, now time.Time, touched map[uuid.UUID]struct{}) error {
	// Every BEP-20 shares the Transfer topic, so the emitter decides whether a
	// log is money we care about.
	if lg.Address != w.token {
		return nil
	}
	ev, err := w.abi.UnpackTransferEvent(lg)
	if err != nil {
		if errors.Is(err, bind.ErrNoEventSignature) || errors.Is(err, bind.ErrEventSignatureMismatch) {
			return nil // an Approval, or anything else the token emits
		}
		return fmt.Errorf("watcher: decode transfer: %w", err)
	}
	metrics.TransfersSeen.Inc()

	if id, ok := w.addrs.Lookup(ev.To); ok {
		if err := w.credit(tx, id, ev, rc, lg, block, now); err != nil {
			return err
		}
		touched[id] = struct{}{}
	}
	if id, ok := w.addrs.Lookup(ev.From); ok {
		if _, underflow, err := tx.Debit(id, ev.Value); err != nil {
			return err
		} else if underflow {
			// Only a bug in our own accounting can cause this. Aborting the
			// block would wedge chain sync on a condition retrying cannot fix,
			// so it is recorded loudly and processing continues.
			metrics.BalanceUnderflows.Inc()
			w.log.Error("balance underflow", "wallet", id, "amount", ev.Value, "tx", rc.TxHash.Hex())
		}
		touched[id] = struct{}{}
	}
	return nil
}

// credit adds an incoming transfer to a wallet's custody and, if the wallet is a
// deposit address and the transfer is worth at least a whole cent, records it as
// a deposit — one future ledger entry, floored once, here and nowhere else.
func (w *Watcher) credit(tx *store.Tx, id uuid.UUID, ev *usdt.UsdtTransfer, rc *types.Receipt, lg *types.Log, block uint64, now time.Time) error {
	wallet, err := tx.Credit(id, ev.Value)
	if err != nil {
		return err
	}
	// A drain arriving at a top-level wallet is our own money moving, not an
	// external payment: crediting it is right, recording it as a deposit would
	// double-count it in the app's feed.
	if wallet.Kind != store.KindDeposit {
		return nil
	}
	// Flooring to cents is the only rounding in the service and it always
	// rounds towards the house. A transfer not worth a whole cent gets no
	// record at all: there is no ledger entry to write, and a feed item that
	// credits nothing is worse than silence.
	cents, ok := w.scale.ToCents(ev.Value)
	if !ok {
		// Unrepresentable as cents. On a real token this cannot happen, so it
		// is a loud refusal rather than a clamped number in the books.
		metrics.DepositsIgnored.Inc()
		w.log.Error("transfer too large to record",
			"app", wallet.App, "ref", wallet.Ref, "amount", ev.Value, "tx", rc.TxHash.Hex())
		return nil
	}
	if cents == 0 {
		metrics.DepositsIgnored.Inc()
		return nil
	}
	created, err := tx.PutDeposit(store.Deposit{
		Wallet: id, App: wallet.App, Block: block, LogIndex: uint32(lg.Index),
		TxHash: rc.TxHash, From: ev.From, AmountWei: ev.Value, Cents: cents,
		Status: store.DepositPending, CreatedAt: now.UTC(),
	})
	if err != nil {
		return err
	}
	if created {
		metrics.DepositsRecorded.Inc()
		w.log.Info("deposit",
			"app", wallet.App, "ref", wallet.Ref, "cents", cents, "wei", ev.Value,
			"dust", w.scale.Dust(ev.Value), "tx", rc.TxHash.Hex(), "log", lg.Index)
	}
	return nil
}

// evaluateTouched applies the drain rule to the wallets this block changed.
// Scoping to touched wallets keeps the per-block cost proportional to activity
// rather than to how many deposit addresses exist.
func (w *Watcher) evaluateTouched(tx *store.Tx, touched map[uuid.UUID]struct{}, now time.Time) error {
	for id := range touched {
		wallet, found, err := tx.Wallet(id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		started, err := engine.EvaluateWallet(tx, wallet, w.cfg, now)
		if err != nil {
			return err
		}
		if started {
			metrics.FlowsStarted.WithLabelValues(store.FlowDrain.String()).Inc()
			w.log.Info("drain started", "app", wallet.App, "ref", wallet.Ref, "balance", wallet.Balance)
		}
	}
	return nil
}

// evaluateApps runs the app-level rules — pending withdrawals and due house
// sweeps. Unlike the drain rule this is not driven by block contents, so it
// runs once per commit rather than per block.
func (w *Watcher) evaluateApps(tx *store.Tx, now time.Time) error {
	apps, err := tx.Apps()
	if err != nil {
		return err
	}
	for _, a := range apps {
		started, err := engine.EvaluateApp(tx, a, w.cfg, now)
		if err != nil {
			return err
		}
		if started {
			w.log.Info("app work started", "app", a.Slug)
		}
	}
	return nil
}
