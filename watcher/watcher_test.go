package watcher

import (
	"context"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bsc/metrics"
	"bsc/money"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
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
	fetched  []uint64
	headErr  error
	blockErr error
}

func newFakeChain(head uint64) *fakeChain {
	return &fakeChain{head: head, blocks: map[uint64][]*types.Receipt{}, bnb: big.NewInt(5)}
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

	app     store.App
	top     store.Wallet
	deposit store.Wallet
}

const (
	minDeposit    = 100
	houseSweepMin = 100
)

// testScale values a cent at a wei: true for a two-decimal token, and it keeps
// every figure in these tests readable as both units at once.
var testScale = func() money.Scale {
	sc, err := money.NewScale(2)
	if err != nil {
		panic(err)
	}
	return sc
}()

func newHarness(t *testing.T, head uint64) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{t: t, store: st, chain: newFakeChain(head), addrs: NewAddrSet()}
	h.top = store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindTopLevel, Address: addr(0x01),
		Active: true, Balance: new(big.Int), CreatedAt: time.Now(),
	}
	h.deposit = store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindDeposit, Ref: "cust-1", Address: addr(0x02),
		Balance: new(big.Int), CreatedAt: time.Now(),
	}
	h.app = store.App{
		Slug: "df", Wallet: h.top.ID,
		Fee:       store.FeePolicy{Flat: 1},
		CreatedAt: time.Now(),
	}
	h.update(func(tx *store.Tx) error {
		if err := tx.PutWallet(h.top); err != nil {
			return err
		}
		if err := tx.PutWallet(h.deposit); err != nil {
			return err
		}
		return tx.PutApp(h.app)
	})

	h.w = New(st, h.chain, h.addrs, Options{
		StartBlock: 1, Poll: time.Millisecond, BackfillBatch: 10,
		Scale: testScale, DrainThreshold: wei(minDeposit), HouseSweepMin: wei(houseSweepMin),
		FeeCollector: addr(0xFE), Token: usdt.MainnetAddress,
	})
	return h
}

// app2 reads the app back, for asserting on its ledger.
func (h *harness) app2() store.App {
	h.t.Helper()
	var out store.App
	h.view(func(tx *store.Tx) error {
		a, ok, err := tx.App("df")
		if err != nil || !ok {
			h.t.Fatalf("App: ok=%v err=%v", ok, err)
		}
		out = a
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

func (h *harness) wallet(id uuid.UUID) store.Wallet {
	h.t.Helper()
	var out store.Wallet
	h.view(func(tx *store.Tx) error {
		w, ok, err := tx.Wallet(id)
		if err != nil || !ok {
			h.t.Fatalf("wallet %s: ok=%v err=%v", id, ok, err)
		}
		out = w
		return nil
	})
	return out
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
		out, _, err = tx.DepositsSince("df", store.Cursor{}, 0)
		return err
	})
	return out
}

func TestDepositIsCreditedRecordedAndDrained(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.addrs.Add(h.top.Address, h.top.ID)
	h.chain.put(1, receipt(hash(0xA1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(500), 3)))

	h.runOnce()

	w := h.wallet(h.deposit.ID)
	if w.Balance.Cmp(wei(500)) != 0 {
		t.Fatalf("balance = %s, want 500", w.Balance)
	}
	deps := h.deposits()
	if len(deps) != 1 || deps[0].AmountWei.Cmp(wei(500)) != 0 || deps[0].Cents != 500 || deps[0].LogIndex != 3 {
		t.Fatalf("deposits = %+v", deps)
	}
	if deps[0].Cursor() != (store.Cursor{Block: 1, LogIndex: 3}) {
		t.Fatalf("cursor = %v", deps[0].Cursor())
	}
	// The drain rule ran in the same transaction, so the wallet is already busy.
	flows := h.flows()
	if len(flows) != 1 || flows[0].Kind != store.FlowDrain {
		t.Fatalf("flows = %+v", flows)
	}
	if w.Flow != flows[0].ID {
		t.Fatalf("wallet not claimed by the drain: %s vs %s", w.Flow, flows[0].ID)
	}
	// An inactive wallet starts by funding its own activation.
	if flows[0].State != store.StateFunding {
		t.Fatalf("state = %s, want funding", flows[0].State)
	}
	if flows[0].To != h.top.Address {
		t.Fatalf("drain destination = %s, want the top-level", flows[0].To.Hex())
	}
}

func TestTwoDepositsInOneBlockStartExactlyOneDrain(t *testing.T) {
	// The case that started this design: two transfers to a fresh, inactive
	// wallet must not each try to activate it.
	h := newHarness(t, 1)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1,
		receipt(hash(0xB1), true,
			transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(300), 0),
			transferLog(usdt.MainnetAddress, addr(0xF1), h.deposit.Address, wei(400), 1)),
	)

	h.runOnce()

	if got := h.flows(); len(got) != 1 {
		t.Fatalf("started %d flows, want exactly 1: %+v", len(got), got)
	}
	if w := h.wallet(h.deposit.ID); w.Balance.Cmp(wei(700)) != 0 {
		t.Fatalf("balance = %s, want both deposits summed", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 2 {
		t.Fatalf("recorded %d deposits, want 2", len(deps))
	}
}

// TestSubCentTransfersAreIgnoredEntirely: the ledger's resolution is a cent, so
// a transfer worth less than one has no entry to write. It is still credited to
// custody, because the chain says it arrived — it simply belongs to the house
// rather than to the app (§22).
func TestSubCentTransfersAreIgnoredEntirely(t *testing.T) {
	// A cent is a wei at this scale, so half a cent needs a finer token. Use
	// four decimals, where a cent is 100 wei and 99 of them is sub-cent dust.
	sc, err := money.NewScale(4)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, 1)
	h.w.scale, h.w.cfg.Scale = sc, sc
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0xC1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(99), 0)))

	h.runOnce()

	if w := h.wallet(h.deposit.ID); w.Balance.Cmp(wei(99)) != 0 {
		t.Fatalf("custody was not credited: balance = %s", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 0 {
		t.Fatalf("sub-cent dust was recorded: %+v", deps)
	}
}

// TestDepositsBelowTheDrainThresholdAreStillRecorded is the distinction the
// ledger forced: the threshold decides when we spend gas moving money, never
// whether the app is credited for it. Until the drain runs it shows as pending.
func TestDepositsBelowTheDrainThresholdAreStillRecorded(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0xD1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(60), 0)))

	h.runOnce()

	deps := h.deposits()
	if len(deps) != 1 || deps[0].Cents != 60 {
		t.Fatalf("deposits = %+v, want the 60 recorded", deps)
	}
	if got := h.flows(); len(got) != 0 {
		t.Fatalf("spent gas draining less than the threshold: %+v", got)
	}
	// Recorded but not yet credited: it is not in the hot wallet.
	if a := h.app2(); a.Ledger != 0 {
		t.Fatalf("ledger = %d before the drain, want 0", a.Ledger)
	}
}

func TestSmallDepositsAggregatePastTheDrainThreshold(t *testing.T) {
	// Money below the threshold is never lost: it sits in the wallet and leaves
	// once the total is worth a transaction.
	h := newHarness(t, 2)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0xD1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(60), 0)))
	h.chain.put(2, receipt(hash(0xD2), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(60), 0)))

	h.runOnce()

	if w := h.wallet(h.deposit.ID); w.Balance.Cmp(wei(120)) != 0 {
		t.Fatalf("balance = %s, want 120", w.Balance)
	}
	if got := h.flows(); len(got) != 1 || got[0].Kind != store.FlowDrain {
		t.Fatalf("aggregated deposits did not start a drain: %+v", got)
	}
	if deps := h.deposits(); len(deps) != 2 {
		t.Fatalf("recorded %d deposits, want both", len(deps))
	}
}

// TestTransferToATopLevelIsCreditedButNotADeposit: a drain arriving at the
// top-level is our own money moving, and recording it would double-count it in
// the app's feed. Nobody is credited for it, so it reads as house excess — which
// is exactly what an unexpected transfer to a hot wallet is.
func TestTransferToATopLevelIsCreditedButNotADeposit(t *testing.T) {
	h := newHarness(t, 1)
	h.addrs.Add(h.top.Address, h.top.ID)
	h.chain.put(1, receipt(hash(0xE1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.top.Address, wei(900), 0)))

	h.runOnce()

	if w := h.wallet(h.top.ID); w.Balance.Cmp(wei(900)) != 0 {
		t.Fatalf("balance = %s, want 900", w.Balance)
	}
	if deps := h.deposits(); len(deps) != 0 {
		t.Fatalf("top-level credit was recorded as a deposit: %+v", deps)
	}
	if a := h.app2(); a.Ledger != 0 {
		t.Fatalf("ledger = %d; a top-level credit is nobody's deposit", a.Ledger)
	}
	for _, fl := range h.flows() {
		if fl.Kind == store.FlowDrain {
			t.Fatalf("top-level credit started a drain: %+v", fl)
		}
	}
}

func TestOutgoingTransfersAreDebited(t *testing.T) {
	h := newHarness(t, 2)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.addrs.Add(h.top.Address, h.top.ID)
	h.chain.put(1, receipt(hash(0xF1), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(500), 0)))
	// The sweep: out of the deposit wallet, into the top-level.
	h.chain.put(2, receipt(hash(0xF2), true,
		transferLog(usdt.MainnetAddress, h.deposit.Address, h.top.Address, wei(500), 0)))

	h.runOnce()

	if w := h.wallet(h.deposit.ID); w.Balance.Sign() != 0 {
		t.Fatalf("deposit wallet balance = %s, want 0 after the sweep", w.Balance)
	}
	if w := h.wallet(h.top.ID); w.Balance.Cmp(wei(500)) != 0 {
		t.Fatalf("top-level balance = %s, want 500", w.Balance)
	}
}

func TestLogsFromOtherTokensAreIgnored(t *testing.T) {
	// Every BEP-20 shares the Transfer topic, so the emitter is what decides.
	h := newHarness(t, 1)
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0x11), true,
		transferLog(addr(0xBA), addr(0xF0), h.deposit.Address, wei(1_000_000), 0)))

	h.runOnce()

	if w := h.wallet(h.deposit.ID); w.Balance.Sign() != 0 {
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
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0x21), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(500), 0)))

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
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0x81), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(500), 0)))
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
	h.addrs.Add(h.deposit.Address, h.deposit.ID)
	h.chain.put(1, receipt(hash(0x83), true,
		transferLog(usdt.MainnetAddress, addr(0xF0), h.deposit.Address, wei(500), 0)))
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
// master. Native is polled because it cannot be derived; the token balance
// comes from the record the watcher already maintains, and is the same figure
// the gas top-up rule decides on.
func TestMasterGaugesReflectBothBalances(t *testing.T) {
	h := newHarness(t, 1)
	master := store.Wallet{
		ID: uuid.New(), Kind: store.KindMaster, Address: addr(0x99),
		Balance: wei(4242), CreatedAt: time.Now(),
	}
	h.update(func(tx *store.Tx) error { return tx.PutWallet(master) })

	h.w.opts.Master = master.Address
	h.w.opts.MasterWallet = master.ID
	h.w.nextMasterAt = time.Time{} // due now
	h.chain.bnb = wei(7)

	h.w.pollMaster(context.Background())

	if got := testutil.ToFloat64(metrics.MasterUSDT); got != 4242 {
		t.Fatalf("bsc_master_usdt_wei = %v, want the recorded token balance 4242", got)
	}
	if got := testutil.ToFloat64(metrics.MasterBNB); got != 7 {
		t.Fatalf("bsc_master_bnb_wei = %v, want the polled native balance 7", got)
	}
}
