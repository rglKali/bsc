package watcher

import (
	"errors"
	"fmt"
	"time"

	"bsc/flow"
	"bsc/metrics"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
	touched := make(map[store.WalletID]struct{})

	for _, rc := range receipts {
		if err := w.confirm(tx, rc, block, now, touched); err != nil {
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
func (w *Watcher) confirm(tx *store.Tx, rc *types.Receipt, block uint64, now time.Time, touched map[store.WalletID]struct{}) error {
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
	// The block is passed down because a settled debit is ordered by it: that is
	// the feed a caller polls, and this is the only place the number is known
	// (§42).
	next, err := flow.Advance(tx, f, block, success, now)
	if err != nil {
		return fmt.Errorf("watcher: advance flow %s: %w", f.ID, err)
	}
	touched[f.Wallet] = struct{}{}

	metrics.FlowTransitions.WithLabelValues(f.Label(), next.State.String()).Inc()
	w.log.Info("flow advanced",
		"flow", f.ID, "kind", f.Label(), "from", f.State, "to", next.State,
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
func (w *Watcher) transfer(tx *store.Tx, rc *types.Receipt, lg *types.Log, block uint64, now time.Time, touched map[store.WalletID]struct{}) error {
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

// credit adds an incoming transfer to a wallet's custody and, unless the wallet
// is bsc's own master, records it as a deposit.
//
// There is no threshold and no rounding. Under the ledger this function floored
// the amount to cents and dropped anything worth less than one, because a
// sub-cent credit was a ledger entry that changed nothing. With the chain's own
// units on the wire there is no floor to apply and no dust to create: every
// transfer is recordable exactly as it arrived (§36).
func (w *Watcher) credit(tx *store.Tx, id store.WalletID, ev *usdt.UsdtTransfer, rc *types.Receipt, lg *types.Log, block uint64, now time.Time) error {
	wallet, err := tx.Credit(id, ev.Value)
	if err != nil {
		return err
	}
	// A drain arriving at the wallet it was aimed at *is* recorded. It is a real
	// transfer onto a wallet the caller owns, and with no ledger to double-count
	// into there is nothing to protect against — the caller sees the deposit and
	// the matching `forwarded` deposit upstream and can tell they are two ends
	// of one movement by the drain's tx hash.
	created, err := tx.PutDeposit(store.Deposit{
		Wallet: id, Block: block, LogIndex: uint32(lg.Index),
		TxHash: rc.TxHash, From: ev.From, Amount: ev.Value,
		CreatedAt: now.UTC(),
	})
	if err != nil {
		return err
	}
	if created {
		metrics.DepositsRecorded.Inc()
		w.log.Info("deposit",
			"ref", wallet.Ref, "amount", ev.Value,
			"tx", rc.TxHash.Hex(), "log", lg.Index)
	}
	return nil
}

// evaluateTouched applies the drain rule to the wallets this block changed.
// Scoping to touched wallets keeps the per-block cost proportional to activity
// rather than to how many deposit addresses exist.
func (w *Watcher) evaluateTouched(tx *store.Tx, touched map[store.WalletID]struct{}, now time.Time) error {
	for id := range touched {
		wallet, found, err := tx.Wallet(id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		started, err := flow.EvaluateWallet(tx, wallet, w.cfg, now)
		if err != nil {
			return err
		}
		if started {
			metrics.FlowsStarted.WithLabelValues("drain").Inc()
			w.log.Info("work started", "ref", wallet.Ref, "balance", wallet.Balance)
		}
	}
	return nil
}

// evaluatePending gives every wallet with an outstanding withdrawal a chance to
// start it. Unlike the drain rule this is not driven by block contents — a
// withdrawal arrives over HTTP, not from the chain — so it runs once per commit
// rather than per block.
//
// It scans the outstanding set rather than every wallet, so its cost is
// proportional to withdrawals in flight rather than to how many wallets exist.
// That replaces the pass over every app, which was proportional to the number
// of apps whether or not any of them had work.
func (w *Watcher) evaluatePending(tx *store.Tx, now time.Time) error {
	// state/withdrawal holds exactly the promises, so this is one bucket scan
	// proportional to what is in flight (§51).
	open, err := tx.OpenPending()
	if err != nil {
		return err
	}
	seen := map[store.WalletID]bool{}
	for _, wd := range open {
		if seen[wd.Wallet] {
			continue
		}
		seen[wd.Wallet] = true
		wallet, found, err := tx.Wallet(wd.Wallet)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		started, err := flow.EvaluateWallet(tx, wallet, w.cfg, now)
		if err != nil {
			return err
		}
		if started {
			metrics.FlowsStarted.WithLabelValues("payout").Inc()
			w.log.Info("withdrawal started", "ref", wallet.Ref, "withdrawal", wd.ID)
		}
	}
	return nil
}
