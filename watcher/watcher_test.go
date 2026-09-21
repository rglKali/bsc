package watcher

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bsc/metrics"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// fakeChain serves canned blocks. Keeping the chain behind an interface is what
// lets the whole block loop be tested without a node.
type fakeChain struct {
	mu       sync.Mutex
	head     uint64
	blocks   map[uint64][]*types.Receipt
	bnb      *big.Int
	usdt     *big.Int
	fetched  []uint64
	headErr  error
	blockErr error
}

func newFakeChain(head uint64) *fakeChain {
	return &fakeChain{head: head, blocks: map[uint64][]*types.Receipt{}, bnb: big.NewInt(5), usdt: new(big.Int)}
}

func (f *fakeChain) Finalized(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

func (f *fakeChain) BlockReceipts(_ context.Context, n uint64) ([]*types.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockErr != nil {
		return nil, f.blockErr
	}
	f.fetched = append(f.fetched, n)
	return f.blocks[n], nil
}

func (f *fakeChain) BalanceBNB(context.Context, common.Address) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bnb, nil
}

func (f *fakeChain) TokenBalance(context.Context, common.Address, common.Address) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return new(big.Int).Set(f.usdt), nil
}

func (f *fakeChain) put(block uint64, receipts ...*types.Receipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks[block] = receipts
}

// transferLog builds a USDT Transfer log as the contract would emit it.
func transferLog(token, from, to common.Address, amount *big.Int, index uint) *types.Log {
	var value common.Hash
	amount.FillBytes(value[:])
	return &types.Log{
		Address: token,
		Topics:  []common.Hash{transferTopic, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())},
		Data:    value.Bytes(),
		Index:   index,
	}
}

func receipt(txHash common.Hash, ok bool, logs ...*types.Log) *types.Receipt {
	status := types.ReceiptStatusSuccessful
	if !ok {
		status = types.ReceiptStatusFailed
	}
	return &types.Receipt{TxHash: txHash, Status: status, Logs: logs}
}

func addr(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}

func hash(b byte) common.Hash {
	var h common.Hash
	h[common.HashLength-1] = b
	return h
}

func wei(v int64) *big.Int { return big.NewInt(v) }

// harness wires a real store to a fake chain.
type harness struct {
	t     *testing.T
	store *store.Store
	chain *fakeChain
	w     *Watcher
	addrs *AddrSet

	hot   store.Wallet // accumulates: drain_to unset
	proxy store.Wallet // forwards into hot
}

// drainThreshold is pure gas economics: below it, forwarding costs more than
// it moves. It has no bearing on what gets recorded.
const drainThreshold = 100

func newHarness(t *testing.T, head uint64) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{t: t, store: st, chain: newFakeChain(head), addrs: NewAddrSet()}
	h.hot = store.Wallet{
		ID: 1, Ref: "hot", Address: addr(0x01),
		Active: true, Balance: new(big.Int), CreatedAt: time.Now(),
	}
	h.proxy = store.Wallet{
		ID: 2, Ref: "cust-1", Address: addr(0x02),
		DrainTo: h.hot.Address, Balance: new(big.Int), CreatedAt: time.Now(),
	}
	h.update(func(tx *store.Tx) error {
		if err := tx.PutWallet(h.hot); err != nil {
			return err
		}
		return tx.PutWallet(h.proxy)
	})

	h.w = New(st, h.chain, h.addrs, Options{
		StartBlock: 1, Poll: time.Millisecond, BackfillBatch: 10,
		DrainThreshold: wei(drainThreshold), Token: usdt.MainnetAddress,
	})
	return h
}

// walletOf reads a wallet back, for asserting on custody.
func (h *harness) walletOf(id store.WalletID) store.Wallet {
	h.t.Helper()
	var out store.Wallet
	h.view(func(tx *store.Tx) error {
		w, ok, err := tx.Wallet(id)
		if err != nil || !ok {
			h.t.Fatalf("Wallet: ok=%v err=%v", ok, err)
		}
		out = w
		return nil
	})
	return out
}

func (h *harness) update(fn func(*store.Tx) error) {
	h.t.Helper()
	if err := h.store.Update(fn); err != nil {
		h.t.Fatalf("Update: %v", err)
	}
}

func (h *harness) view(fn func(*store.Tx) error) {
	h.t.Helper()
	if err := h.store.View(fn); err != nil {
		h.t.Fatalf("View: %v", err)
	}
}

// runOnce starts the watcher and lets it drain the fake chain.
func (h *harness) runOnce() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.w.Start(ctx); err != nil {
		h.t.Fatalf("start: %v", err)
	}
	for {
		caughtUp, err := h.w.Step(ctx)
		if err != nil {
			h.t.Fatalf("step: %v", err)
		}
		if caughtUp {
			return
		}
	}
}

func (h *harness) flows() []store.Flow {
	h.t.Helper()
	var out []store.Flow
	h.view(func(tx *store.Tx) error {
		return tx.EachFlow(func(f store.Flow) error {
			out = append(out, f)
			return nil
		})
	})
	return out
}

func (h *harness) deposits() []store.Deposit {
	h.t.Helper()
	var out []store.Deposit
	h.view(func(tx *store.Tx) error {
		var err error
		out, _, err = tx.DepositsSince(store.Cursor{}, 0)
		return err
	})
	return out
}

func TestDepositIsCreditedRecordedAndDrained(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.addrs.Add(h.hot.Address, h.hot.ID)
	h.chain.put(1, receipt(hash(0xA1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(500), 3)))

	h.runOnce()

	w := h.walletOf(h.proxy.ID)
	if w.Balance.Cmp(wei(500)) != 0 {
		t.Fatalf("balance = %s, want 500", w.Balance)
	}
	deps := h.deposits()
	if len(deps) != 1 || deps[0].Amount.Cmp(wei(500)) != 0 || deps[0].LogIndex != 3 {
		t.Fatalf("deposits = %+v", deps)
	}
	if deps[0].Cursor() != (store.Cursor{Block: 1, LogIndex: 3}) {
		t.Fatalf("cursor = %v", deps[0].Cursor())
	}
	// The drain rule ran in the same transaction, so the wallet is already busy.
	flows := h.flows()
	if len(flows) != 1 || flows[0].Kind != store.FlowTransfer {
		t.Fatalf("flows = %+v", flows)
	}
	if w.Flow != flows[0].ID {
		t.Fatalf("wallet not claimed by the drain: %s vs %s", w.Flow, flows[0].ID)
	}
	// An inactive wallet starts by funding its own activation.
	if flows[0].State != store.StateFunding {
		t.Fatalf("state = %s, want funding", flows[0].State)
	}
	if flows[0].To != h.hot.Address {
		t.Fatalf("drain destination = %s, want the top-level", flows[0].To.Hex())
	}
}

func TestTwoDepositsInOneBlockStartExactlyOneDrain(t *testing.T) {
	// The case that started this design: two transfers to a fresh, inactive
	// wallet must not each try to activate it.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1,
		receipt(hash(0xB1), true,
			transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(300), 0),
			transferLog(usdt.MainnetAddress, addr(0xF1), h.proxy.Address, wei(400), 1)),
	)

	h.runOnce()

	if got := h.flows(); len(got) != 1 {
		t.Fatalf("started %d flows, want exactly 1: %+v", len(got), got)
	}
	if w := h.walletOf(h.proxy.ID); w.Balance.Cmp(wei(700)) != 0 {
		t.Fatalf("balance = %s, want both deposits summed", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 2 {
		t.Fatalf("recorded %d deposits, want 2", len(deps))
	}
}

// With the chain's own units there is no amount too small to record: the floor
// existed only because a sub-cent credit was a ledger entry that changed
// nothing, and there is no ledger now (§36).
func TestEvenTheSmallestTransferIsRecorded(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0xC1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(1), 0)))

	h.runOnce()

	if w := h.walletOf(h.proxy.ID); w.Balance.Cmp(wei(1)) != 0 {
		t.Fatalf("custody was not credited: balance = %s", w.Balance)
	}
	deps := h.deposits()
	if len(deps) != 1 || deps[0].Amount.Cmp(wei(1)) != 0 {
		t.Fatalf("the smallest transfer was not recorded: %+v", deps)
	}
}

// The threshold decides when we spend gas moving money, never whether the
// transfer is recorded. Until the drain runs the deposit reads as `received`.
func TestDepositsBelowTheDrainThresholdAreStillRecorded(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0xD1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(60), 0)))

	h.runOnce()

	deps := h.deposits()
	if len(deps) != 1 || deps[0].Amount.Cmp(wei(60)) != 0 {
		t.Fatalf("deposits = %+v, want the 60 recorded", deps)
	}
	if got := h.flows(); len(got) != 0 {
		t.Fatalf("spent gas draining less than the threshold: %+v", got)
	}
}

func TestSmallDepositsAggregatePastTheDrainThreshold(t *testing.T) {
	// Money below the threshold is never lost: it sits in the wallet and leaves
	// once the total is worth a transaction.
	h := newHarness(t, 2)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0xD1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(60), 0)))
	h.chain.put(2, receipt(hash(0xD2), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(60), 0)))

	h.runOnce()

	if w := h.walletOf(h.proxy.ID); w.Balance.Cmp(wei(120)) != 0 {
		t.Fatalf("balance = %s, want 120", w.Balance)
	}
	if got := h.flows(); len(got) != 1 || got[0].Kind != store.FlowTransfer {
		t.Fatalf("aggregated deposits did not start a drain: %+v", got)
	}
	if deps := h.deposits(); len(deps) != 2 {
		t.Fatalf("recorded %d deposits, want both", len(deps))
	}
}

// A transfer arriving at an accumulating wallet is a deposit like any other.
// Under the ledger it had to be suppressed to avoid double-counting a drain
// into an app's feed; with no ledger there is nothing to double-count, and the
// caller can pair the two ends by the drain's tx hash (§34).
func TestTransferToAnAccumulatingWalletIsRecorded(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.hot.Address, h.hot.ID)
	h.chain.put(1, receipt(hash(0xE1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.hot.Address, wei(900), 0)))

	h.runOnce()

	if w := h.walletOf(h.hot.ID); w.Balance.Cmp(wei(900)) != 0 {
		t.Fatalf("balance = %s, want 900", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 1 {
		t.Fatalf("deposits = %d, want 1", len(deps))
	}
	for _, fl := range h.flows() {
		if fl.Kind == store.FlowTransfer {
			t.Fatalf("an accumulating wallet started a drain: %+v", fl)
		}
	}
}

func TestOutgoingTransfersAreDebited(t *testing.T) {
	h := newHarness(t, 2)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.addrs.Add(h.hot.Address, h.hot.ID)
	h.chain.put(1, receipt(hash(0xF1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(500), 0)))
	// The sweep: out of the deposit wallet, into the top-level.
	h.chain.put(2, receipt(hash(0xF2), true,
		transferLog(usdt.MainnetAddress, h.proxy.Address, h.hot.Address, wei(500), 0)))

	h.runOnce()

	if w := h.walletOf(h.proxy.ID); w.Balance.Sign() != 0 {
		t.Fatalf("deposit wallet balance = %s, want 0 after the sweep", w.Balance)
	}
	if w := h.walletOf(h.hot.ID); w.Balance.Cmp(wei(500)) != 0 {
		t.Fatalf("top-level balance = %s, want 500", w.Balance)
	}
}

func TestLogsFromOtherTokensAreIgnored(t *testing.T) {
	// Every BEP-20 shares the Transfer topic, so the emitter is what decides.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0x11), true,
		transferLog(addr(0xBA), addr(0xF0), h.proxy.Address, wei(1_000_000), 0)))

	h.runOnce()

	if w := h.walletOf(h.proxy.ID); w.Balance.Sign() != 0 {
		t.Fatalf("a foreign token credited the balance: %s", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 0 {
		t.Fatalf("a foreign token was recorded: %+v", deps)
	}
}

func TestReprocessingABlockDoesNotDuplicate(t *testing.T) {
	// Exactly-once falls out of the key layout and the single transaction, with
	// no dedup window to tune.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0x21), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(500), 0)))

	h.runOnce()
	// Rewind the cursor and replay, as a crash between commit and cursor would.
	h.update(func(tx *store.Tx) error { return tx.SetCursor(1) })
	h.w.cursor = 1
	h.runOnce()

	if deps := h.deposits(); len(deps) != 1 {
		t.Fatalf("replay produced %d deposits, want 1", len(deps))
	}
	// The balance does double, because credits are not idempotent — which is
	// exactly why the cursor is committed in the same transaction as the data.
	if got := h.flows(); len(got) != 1 {
		t.Fatalf("replay started %d flows, want 1 (the wallet is already claimed)", len(got))
	}
}

func TestRunProcessesBlocksAndStopsOnCancel(t *testing.T) {
	h := newHarness(t, 2)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0x81), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(500), 0)))
	h.chain.put(2, receipt(hash(0x82), true))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	// Wait for the deposit to land rather than for a fixed delay.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.deposits()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not process the block")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunSurvivesTransientChainErrors(t *testing.T) {
	// An RPC hiccup must not end the loop: the watcher is what observes
	// finality, so if it stops, every in-flight transfer stops with it.
	h := newHarness(t, 1)
	h.addrs.Add(h.proxy.Address, h.proxy.ID)
	h.chain.put(1, receipt(hash(0x83), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.proxy.Address, wei(500), 0)))
	h.chain.mu.Lock()
	h.chain.headErr = context.DeadlineExceeded
	h.chain.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	if got := h.deposits(); len(got) != 0 {
		t.Fatalf("processed a block while the chain was failing: %+v", got)
	}

	// Recover: the loop must pick up where it left off.
	h.chain.mu.Lock()
	h.chain.headErr = nil
	h.chain.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for len(h.deposits()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not recover after the chain came back")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestBehindTracksTheGapToTheHead(t *testing.T) {
	// The API reads this to refuse withdrawals rather than reserve against a
	// balance that is still catching up.
	h := newHarness(t, 25)
	for b := uint64(1); b <= 25; b++ {
		h.chain.put(b, receipt(hash(byte(b)), true))
	}
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := h.w.Behind(); got != 0 {
		t.Fatalf("Behind before any step = %d", got)
	}
	if _, err := h.w.Step(ctx); err != nil { // one batch of 10
		t.Fatalf("step: %v", err)
	}
	if got := h.w.Behind(); got != 25 {
		t.Fatalf("Behind mid-backfill = %d, want the gap measured before the batch", got)
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
	if got := h.w.Behind(); got != 0 {
		t.Fatalf("Behind once caught up = %d, want 0", got)
	}
}

// TestMasterGaugesReflectBothBalances: the operator's two numbers for the
// master. Both are asked of the chain, because the master is not a wallet in
// this store — it is the operator's own key, spent from scripts and by hand, so
// there is no record to read and no reason to watch transfers touching it (§49).
func TestMasterGaugesReflectBothBalances(t *testing.T) {
	h := newHarness(t, 1)

	h.w.opts.Master = addr(0x99)
	h.w.nextMasterAt = time.Time{} // due now
	h.chain.bnb = wei(7)
	h.chain.usdt = wei(4242)

	h.w.pollMaster(context.Background())

	if got := testutil.ToFloat64(metrics.MasterUSDT); got != 4242 {
		t.Fatalf("bsc_master_usdt_wei = %v, want the chain's 4242", got)
	}
	if got := testutil.ToFloat64(metrics.MasterBNB); got != 7 {
		t.Fatalf("bsc_master_bnb_wei = %v, want the polled native balance 7", got)
	}
}

// A watcher that has not resolved its cursor must not step. Zero means "block
// 0", so stepping an unstarted watcher begins a backfill from genesis — on
// mainnet a hundred million blocks — and the only symptom is a service that
// looks busy and never catches up. This cost an e2e run a thirty-minute hang,
// with "132260842 blocks behind" as the only clue (§52).
func TestSteppingBeforeStartIsRefused(t *testing.T) {
	h := newHarness(t, 100)

	if _, err := h.w.Step(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Step before Start = %v, want ErrNotStarted", err)
	}
	if got := h.w.Behind(); got != 0 {
		t.Fatalf("an unstarted watcher reported %d blocks behind; it must claim nothing", got)
	}
	if len(h.chain.fetched) != 0 {
		t.Fatalf("an unstarted watcher fetched blocks %v", h.chain.fetched)
	}

	if err := h.w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.w.Step(context.Background()); err != nil {
		t.Fatalf("Step after Start: %v", err)
	}
}
