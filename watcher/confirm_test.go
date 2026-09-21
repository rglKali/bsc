package watcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"bsc/flow"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// startFlow puts a flow in flight: claimed wallet, journal entry, watchlist
// entry — the state the sender leaves behind after broadcasting.
func (h *harness) startFlow(kind store.FlowKind, w store.Wallet, state store.FlowState, txh common.Hash, params flow.Params) store.Flow {
	h.t.Helper()
	params.Kind = kind
	params.Wallet = w.ID
	params.Active = true
	if params.To == (common.Address{}) {
		params.To = h.hot.Address
	}
	f, err := flow.Begin(params)
	if err != nil {
		h.t.Fatalf("Begin: %v", err)
	}
	f.State = state
	f.Tx = txh
	h.update(func(tx *store.Tx) error {
		if err := tx.PutFlow(f); err != nil {
			return err
		}
		if _, err := tx.ClaimWallet(w.ID, f.ID); err != nil {
			return err
		}
		if err := tx.LinkTx(txh, store.TxRef{Flow: f.ID, Signer: addr(0x01), Nonce: 7}); err != nil {
			return err
		}
		return tx.PutSend(store.Send{
			Flow: f.ID, Signer: addr(0x01), Nonce: 7, Hash: txh, Raw: []byte{0xf8},
		})
	})
	return f
}

func TestConfirmationAdvancesAndClearsTheJournal(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	txh := hash(0x31)
	f := h.startFlow(store.FlowTransfer, h.proxy, store.StateFunding, txh, flow.Params{})
	h.chain.put(1, receipt(txh, true))

	h.runOnce()

	var got store.Flow
	h.view(func(tx *store.Tx) error {
		var ok bool
		var err error
		got, ok, err = tx.Flow(f.ID)
		if err != nil || !ok {
			t.Fatalf("flow gone: ok=%v err=%v", ok, err)
		}
		// The watchlist and the journal are cleared together with the advance:
		// the transaction is final, so re-broadcasting it is meaningless.
		if _, ok, err := tx.TxRefByHash(txh); err != nil || ok {
			t.Fatalf("watchlist entry survived: ok=%v err=%v", ok, err)
		}
		if _, ok, err := tx.Send(addr(0x01), 7); err != nil || ok {
			t.Fatalf("journal entry survived: ok=%v err=%v", ok, err)
		}
		return nil
	})
	if got.State != store.StateApproving {
		t.Fatalf("state = %s, want approving", got.State)
	}
	if got.Waiting() {
		t.Fatal("flow still marked as waiting on a transaction")
	}
}

func TestApprovalConfirmationMarksTheWalletActive(t *testing.T) {
	// Leaving `approving` is what activates a wallet, whichever kind of flow
	// happened to be the one that ran the activation.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	txh := hash(0x32)
	h.startFlow(store.FlowTransfer, h.proxy, store.StateApproving, txh, flow.Params{})
	h.chain.put(1, receipt(txh, true))

	h.runOnce()

	if w := h.walletOf(h.proxy.ID); !w.Active {
		t.Fatal("wallet not marked active after its approve confirmed")
	}
	flows := h.flows()
	if len(flows) != 1 || flows[0].State != store.StateMoving {
		t.Fatalf("flows = %+v, want one sweeping", flows)
	}
}

func TestDrainCompletionCreditsEveryWaitingDeposit(t *testing.T) {
	h := newHarness(t, 2)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.addrs.Add(h.hot.Address, h.hot.ID)

	// Two recorded deposits awaiting a sweep. Their log indexes avoid the
	// sweep's own, since (block, log_index) is unique across a whole block.
	h.update(func(tx *store.Tx) error {
		for i, amount := range []int64{300, 400} {
			if _, err := tx.PutDeposit(store.Deposit{
				Wallet: h.proxy.ID, Block: 1, LogIndex: uint32(10 + i),
				TxHash: hash(byte(0x40 + i)), Amount: wei(amount),
				CreatedAt: time.Now(),
			}); err != nil {
				return err
			}
		}
		_, err := tx.Credit(h.proxy.ID, wei(700))
		return err
	})

	sweep := hash(0x41)
	h.startFlow(store.FlowTransfer, h.proxy, store.StateMoving, sweep, flow.Params{})
	h.chain.put(1, receipt(sweep, true,
		transferLog(usdt.MainnetAddress, h.proxy.Address, h.hot.Address, wei(700), 0)))

	h.runOnce()

	// Three records now: the two that were forwarded, plus the arrival of that
	// same money on the wallet it was aimed at. The caller pairs the two ends
	// by the drain's hash (§34).
	deps := h.deposits()
	if len(deps) != 3 {
		t.Fatalf("deposits = %d, want 3", len(deps))
	}
	// The credits are recorded and left alone: a drain moves a balance, not a
	// set of deposits, so nothing is stamped on them when it settles (§50).
	onProxy := 0
	for _, d := range deps {
		if d.Wallet == h.proxy.ID {
			onProxy++
		}
	}
	if onProxy != 2 {
		t.Fatalf("credits on the proxy = %d, want 2", onProxy)
	}
	// The flow is gone and the wallet is idle again.
	if got := h.flows(); len(got) != 0 {
		t.Fatalf("flow survived settlement: %+v", got)
	}
	if w := h.walletOf(h.proxy.ID); !w.Idle() {
		t.Fatalf("wallet still owned by %s", w.Flow)
	}
}

func TestRevertFailsTheFlowAndBacksTheWalletOff(t *testing.T) {
	// Without the backoff, the declarative drain rule would immediately start
	// another attempt and burn gas as fast as blocks arrive.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.update(func(tx *store.Tx) error {
		_, err := tx.Credit(h.proxy.ID, wei(500))
		return err
	})
	txh := hash(0x51)
	h.startFlow(store.FlowTransfer, h.proxy, store.StateMoving, txh, flow.Params{})
	h.chain.put(1, receipt(txh, false)) // reverted

	h.runOnce()

	w := h.walletOf(h.proxy.ID)
	if w.FailedAttempts != 1 {
		t.Fatalf("failed attempts = %d, want 1", w.FailedAttempts)
	}
	if w.RetryAfter.IsZero() || !w.RetryAfter.After(time.Now()) {
		t.Fatalf("retry deadline = %v, want a future time", w.RetryAfter)
	}
	// Backed off, so the rule must not immediately restart it even though the
	// balance still clears the threshold.
	if got := h.flows(); len(got) != 0 {
		t.Fatalf("a new flow started during backoff: %+v", got)
	}
}

func TestWithdrawalSettlementConfirmsAndReleasesTheCommitment(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.hot.Address, h.hot.ID)

	wd := store.Pending{
		ID: uuid.New(), Wallet: h.hot.ID, Reason: store.ReasonPayout, Destination: addr(0xDD),
		Amount: wei(50), CreatedAt: time.Now(),
	}
	h.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(h.hot.ID, wei(1000)); err != nil {
			return err
		}
		return tx.PutPending(wd)
	})

	txh := hash(0x61)
	h.startFlow(store.FlowTransfer, h.hot, store.StateMoving, txh, flow.Params{
		Amount: wd.Amount, To: wd.Destination, Withdrawal: wd.ID,
	})
	h.chain.put(1, receipt(txh, true,
		transferLog(usdt.MainnetAddress, h.hot.Address, wd.Destination, wei(50), 0)))

	h.runOnce()

	h.view(func(tx *store.Tx) error {
		// Settling moved it into the log (§51).
		if _, still, err := tx.Pending(wd.ID); err != nil || still {
			t.Fatalf("the promise outlived settlement (still=%v err=%v)", still, err)
		}
		got, ok, err := tx.Withdrawal(wd.ID)
		if err != nil || !ok {
			t.Fatalf("withdrawal: ok=%v err=%v", ok, err)
		}
		if got.TxHash != txh {
			t.Fatalf("withdrawal = %+v", got)
		}
		committed, err := tx.Committed(h.hot.ID)
		if err != nil {
			return err
		}
		if committed.Sign() != 0 {
			t.Fatalf("committed = %s after settlement, want 0", committed)
		}
		return nil
	})
	// Custody is debited from the observed Transfer, not by the settlement:
	// one source of truth for what a wallet holds.
	if w := h.walletOf(h.hot.ID); w.Balance.Cmp(wei(950)) != 0 {
		t.Fatalf("custody = %s, want 950 after the payout left", w.Balance)
	}
}

func TestRevertedWithdrawalStaysPendingAndKeepsItsCommitment(t *testing.T) {
	// A payout that never happened is not failed either: it keeps its place in
	// the outstanding set and waits for the retry (§28).
	h := newHarness(t, 1)
	h.addrs.Add(h.hot.Address, h.hot.ID)

	wd := store.Pending{
		ID: uuid.New(), Wallet: h.hot.ID, Reason: store.ReasonPayout, Destination: addr(0xDD),
		Amount: wei(50), CreatedAt: time.Now(),
	}
	h.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(h.hot.ID, wei(1000)); err != nil {
			return err
		}
		return tx.PutPending(wd)
	})

	txh := hash(0x62)
	h.startFlow(store.FlowTransfer, h.hot, store.StateMoving, txh, flow.Params{
		Amount: wd.Amount, To: wd.Destination, Withdrawal: wd.ID,
	})
	h.chain.put(1, receipt(txh, false)) // reverted: no Transfer log

	h.runOnce()

	h.view(func(tx *store.Tx) error {
		got, ok, err := tx.Pending(wd.ID)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("a reverted payout left the pending keyspace")
		}
		if got.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", got.Attempts)
		}
		committed, err := tx.Committed(h.hot.ID)
		if err != nil {
			return err
		}
		if committed.Cmp(wei(50)) != 0 {
			t.Fatalf("committed = %s, want the 50 still promised", committed)
		}
		return nil
	})
	if w := h.walletOf(h.hot.ID); w.Balance.Cmp(wei(1000)) != 0 {
		t.Fatalf("custody = %s, want 1000 — nothing moved", w.Balance)
	}
}

func TestFreshDatabaseStartsAtTheFinalizedHead(t *testing.T) {
	// StartBlock 0 must never mean block 0: that would scan from genesis, which
	// at 2.2 blocks/s and one RPC call per block is effectively forever.
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ch := newFakeChain(48_210_577)
	w := New(st, ch, NewAddrSet(), Options{StartBlock: 0, Poll: time.Millisecond})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if w.cursor != 48_210_577 {
		t.Fatalf("cursor = %d, want the finalized head", w.cursor)
	}
}

func TestExistingCursorWins(t *testing.T) {
	h := newHarness(t, 500)
	h.update(func(tx *store.Tx) error { return tx.SetCursor(42) })
	if err := h.w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.w.cursor != 42 {
		t.Fatalf("cursor = %d, want the stored 42", h.w.cursor)
	}
}

func TestBackfillBatchesBlocksIntoOneCommit(t *testing.T) {
	// 8,000 blocks one-per-transaction is 8,000 fsyncs; batching is what makes
	// catching up after an outage practical.
	h := newHarness(t, 25)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	for b := uint64(1); b <= 25; b++ {
		h.chain.put(b, receipt(hash(byte(b)), true))
	}
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	caughtUp, err := h.w.Step(ctx)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if caughtUp {
		t.Fatal("reported caught up after one batch of 10 out of 25 blocks")
	}
	if h.w.cursor != 11 {
		t.Fatalf("cursor = %d, want 11 after a batch of 10", h.w.cursor)
	}
	for !caughtUp {
		if caughtUp, err = h.w.Step(ctx); err != nil {
			t.Fatalf("step: %v", err)
		}
	}
	if h.w.cursor != 26 {
		t.Fatalf("final cursor = %d, want 26", h.w.cursor)
	}
}

func TestQueuedWithdrawalsWaitUntilCaughtUp(t *testing.T) {
	// Balances are only current once we reach the head, and starting a payout
	// from a stale one could overdraw the wallet.
	h := newHarness(t, 25)
	h.addrs.Add(h.hot.Address, h.hot.ID)
	h.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(h.hot.ID, wei(1000)); err != nil {
			return err
		}
		return tx.PutPending(store.Pending{
			ID: uuid.New(), Wallet: h.hot.ID, Reason: store.ReasonPayout, Destination: addr(0xDD),
			Amount: wei(50), CreatedAt: time.Now(),
		})
	})
	for b := uint64(1); b <= 25; b++ {
		h.chain.put(b, receipt(hash(byte(b)), true))
	}

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The startup pass runs the rules once, so clear what it started to observe
	// the backfill behaviour on its own.
	h.update(func(tx *store.Tx) error {
		return tx.EachFlow(func(f store.Flow) error { return tx.DeleteFlow(f) })
	})

	if _, err := h.w.Step(ctx); err != nil { // mid-backfill
		t.Fatalf("step: %v", err)
	}
	if got := h.flows(); len(got) != 0 {
		t.Fatalf("a withdrawal started while still %d blocks behind: %+v", 25-h.w.cursor, got)
	}

	for {
		caughtUp, err := h.w.Step(ctx)
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if caughtUp {
			break
		}
	}
	if got := h.flows(); len(got) != 1 || got[0].Kind != store.FlowTransfer {
		t.Fatalf("withdrawal did not start once caught up: %+v", got)
	}
}
