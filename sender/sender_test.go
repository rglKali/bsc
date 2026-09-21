package sender

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bsc/flow"
	"bsc/keys"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
)

func addr(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}

func wei(v int64) *big.Int { return big.NewInt(v) }

// fakeChain records what the sender asked for and answers with canned values.
type fakeChain struct {
	mu sync.Mutex

	nonce       uint64
	gasPrice    *big.Int
	gas         uint64
	bnb         map[common.Address]*big.Int
	allowance   map[common.Address]*big.Int // owner -> allowance granted to the master
	allowanceTo map[allowanceKey]*big.Int   // explicit (owner, spender) pairs
	balance     map[common.Address]*big.Int

	sent      [][]byte
	sendErr   error
	nonceFor  []common.Address
	callDatas [][]byte
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		nonce: 7, gasPrice: wei(3_000_000_000), gas: 50_000,
		bnb:         map[common.Address]*big.Int{},
		allowance:   map[common.Address]*big.Int{},
		allowanceTo: map[allowanceKey]*big.Int{},
		balance:     map[common.Address]*big.Int{},
	}
}

func (f *fakeChain) Nonce(_ context.Context, a common.Address) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonceFor = append(f.nonceFor, a)
	return f.nonce, nil
}

func (f *fakeChain) GasPrice(context.Context) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return new(big.Int).Set(f.gasPrice), nil
}

func (f *fakeChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gas, nil
}

func (f *fakeChain) BalanceBNB(_ context.Context, a common.Address) (*big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.bnb[a]; ok {
		return new(big.Int).Set(v), nil
	}
	return new(big.Int), nil
}

// allowanceKey identifies one (owner, spender) pair.
type allowanceKey struct{ owner, spender common.Address }

// Call answers the ERC-20 views, chosen by selector.
func (f *fakeChain) Call(_ context.Context, to common.Address, data []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callDatas = append(f.callDatas, data)

	abi := usdt.NewUsdt()
	if len(data) < 4 {
		return nil, nil
	}
	sel := common.Bytes2Hex(data[:4])

	var out common.Hash
	switch {
	case sel == common.Bytes2Hex(abi.PackAllowance(common.Address{}, common.Address{})[:4]):
		owner := common.BytesToAddress(data[16:36])
		spender := common.BytesToAddress(data[48:68])
		if v, ok := f.allowanceTo[allowanceKey{owner, spender}]; ok {
			v.FillBytes(out[:])
		} else if v, ok := f.allowance[owner]; ok {
			v.FillBytes(out[:])
		}
	case sel == common.Bytes2Hex(abi.PackBalanceOf(common.Address{})[:4]):
		owner := common.BytesToAddress(data[16:36])
		if v, ok := f.balance[owner]; ok {
			v.FillBytes(out[:])
		}
	}
	return out.Bytes(), nil
}

func (f *fakeChain) SendRawTx(_ context.Context, raw []byte) (common.Hash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return common.Hash{}, f.sendErr
	}
	f.sent = append(f.sent, append([]byte(nil), raw...))
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	return tx.Hash(), nil
}

func (f *fakeChain) lastSent(t *testing.T) *types.Transaction {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("nothing was broadcast")
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(f.sent[len(f.sent)-1]); err != nil {
		t.Fatalf("decode broadcast: %v", err)
	}
	return &tx
}

type fixture struct {
	t     *testing.T
	st    *store.Store
	chain *fakeChain
	s     *Sender
	ring  *keys.Ring

	hot   store.Wallet // accumulates
	proxy store.Wallet // forwards into hot
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	master := make([]byte, 32)
	for i := range master {
		master[i] = 0x11
	}
	ring, err := keys.New(master)
	if err != nil {
		t.Fatalf("keys.New: %v", err)
	}
	ch := newFakeChain()
	snd, err := New(st, ch, ring, Options{
		ChainID: 56, Token: usdt.MainnetAddress, Tick: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	f := &fixture{t: t, st: st, chain: ch, s: snd, ring: ring}

	hotID, hotKey, _ := ring.Allocate(1)
	proxyID, proxyKey, _ := ring.Allocate(hotID + 1)
	f.hot = store.Wallet{
		ID: store.WalletID(hotID), Ref: "hot", Address: hotKey.Address,
		Active: true, Balance: new(big.Int), CreatedAt: time.Now(),
	}
	f.proxy = store.Wallet{
		ID: store.WalletID(proxyID), Ref: "cust-1", Address: proxyKey.Address,
		DrainTo: hotKey.Address, Balance: new(big.Int), CreatedAt: time.Now(),
	}
	f.update(func(tx *store.Tx) error {
		if err := tx.PutWallet(f.hot); err != nil {
			return err
		}
		return tx.PutWallet(f.proxy)
	})
	return f
}

func (f *fixture) update(fn func(*store.Tx) error) {
	f.t.Helper()
	if err := f.st.Update(fn); err != nil {
		f.t.Fatalf("Update: %v", err)
	}
}

// begin puts a flow in the given state, ready for the sender to act on.
func (f *fixture) begin(kind store.FlowKind, w store.Wallet, state store.FlowState, p flow.Params) store.Flow {
	f.t.Helper()
	p.Kind, p.Wallet, p.Active = kind, w.ID, true
	if p.To == (common.Address{}) {
		p.To = f.hot.Address
	}
	fl, err := flow.Begin(p)
	if err != nil {
		f.t.Fatalf("Begin: %v", err)
	}
	fl.State = state
	f.update(func(tx *store.Tx) error {
		if err := tx.PutFlow(fl); err != nil {
			return err
		}
		_, err := tx.ClaimWallet(w.ID, fl.ID)
		return err
	})
	return fl
}

func (f *fixture) step() bool {
	f.t.Helper()
	worked, err := f.s.Step(context.Background())
	if err != nil {
		f.t.Fatalf("step: %v", err)
	}
	return worked
}

func (f *fixture) flow(id uuid.UUID) (store.Flow, bool) {
	f.t.Helper()
	var (
		out   store.Flow
		found bool
	)
	if err := f.st.View(func(tx *store.Tx) error {
		var err error
		out, found, err = tx.Flow(id)
		return err
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return out, found
}

func (f *fixture) wallet(id store.WalletID) store.Wallet {
	f.t.Helper()
	var out store.Wallet
	if err := f.st.View(func(tx *store.Tx) error {
		w, ok, err := tx.Wallet(id)
		if err != nil || !ok {
			f.t.Fatalf("wallet: ok=%v err=%v", ok, err)
		}
		out = w
		return nil
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return out
}

func TestFundSendsNativeValueFromTheMaster(t *testing.T) {
	f := newFixture(t)
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateFunding, flow.Params{})

	if !f.step() {
		t.Fatal("sender did nothing")
	}

	tx := f.chain.lastSent(t)
	if *tx.To() != f.proxy.Address {
		t.Fatalf("funded %s, want the deposit wallet", tx.To().Hex())
	}
	if tx.Value().Sign() <= 0 {
		t.Fatalf("funding value = %s", tx.Value())
	}
	if len(tx.Data()) != 0 {
		t.Fatal("a plain BNB transfer must carry no calldata")
	}
	// The master pays for activation, so the master signs it.
	if got := signerOf(t, tx); got != f.s.Master() {
		t.Fatalf("signed by %s, want the master %s", got.Hex(), f.s.Master().Hex())
	}
	// Journalled and linked before broadcast.
	got, _ := f.flow(fl.ID)
	if !got.Waiting() || got.Tx != tx.Hash() {
		t.Fatalf("flow is not waiting on the broadcast transaction: %+v", got)
	}
}

func TestFundIsSkippedWhenTheWalletAlreadyHasGas(t *testing.T) {
	// v1 checked this on-chain before funding; the state model keeps that by
	// letting the sender report an unnecessary action as success.
	f := newFixture(t)
	f.chain.bnb[f.proxy.Address] = wei(1_000_000_000_000_000_000)
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateFunding, flow.Params{})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("funded a wallet that already had gas")
	}
	got, _ := f.flow(fl.ID)
	if got.State != store.StateApproving {
		t.Fatalf("state = %s, want approving", got.State)
	}
}

func TestApproveIsSignedByTheWalletNotTheMaster(t *testing.T) {
	// The allowance must be granted *by* the managed wallet. Signing it with
	// the master would approve the master to itself and leave the real wallet
	// unusable.
	f := newFixture(t)
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateApproving, flow.Params{})

	f.step()

	tx := f.chain.lastSent(t)
	if *tx.To() != usdt.MainnetAddress {
		t.Fatalf("approve sent to %s, want the token", tx.To().Hex())
	}
	if got := signerOf(t, tx); got != f.proxy.Address {
		t.Fatalf("approve signed by %s, want the deposit wallet %s", got.Hex(), f.proxy.Address.Hex())
	}
	got, _ := f.flow(fl.ID)
	if !got.Waiting() {
		t.Fatal("flow not marked as waiting")
	}
}

func TestApproveIsSkippedWhenTheAllowanceExists(t *testing.T) {
	f := newFixture(t)
	f.chain.allowance[f.proxy.Address] = wei(1)
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateApproving, flow.Params{})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("re-approved a wallet that already had an allowance")
	}
	got, _ := f.flow(fl.ID)
	if got.State != store.StateMoving {
		t.Fatalf("state = %s, want sweeping", got.State)
	}
	// Skipping still marks the wallet active: the allowance is what "active"
	// means, however it got there.
	if !f.wallet(f.proxy.ID).Active {
		t.Fatal("wallet not marked active after a skipped approve")
	}
}

func TestSweepMovesTheChainBalanceNotOurRecord(t *testing.T) {
	// The deposit is only a trigger; balanceOf at signing time is the authority,
	// so anything that arrived in the meantime leaves with the same sweep.
	f := newFixture(t)
	f.chain.balance[f.proxy.Address] = wei(777)
	f.update(func(tx *store.Tx) error {
		_, err := tx.Credit(f.proxy.ID, wei(500)) // our record says less
		return err
	})
	f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})

	f.step()

	tx := f.chain.lastSent(t)
	if got := signerOf(t, tx); got != f.s.Master() {
		t.Fatalf("transferFrom signed by %s, want the master", got.Hex())
	}
	from, to, amount := decodeTransferFrom(t, tx.Data())
	if from != f.proxy.Address || to != f.hot.Address {
		t.Fatalf("transferFrom(%s -> %s), want deposit -> top-level", from.Hex(), to.Hex())
	}
	if amount.Cmp(wei(777)) != 0 {
		t.Fatalf("swept %s, want the on-chain 777", amount)
	}
}

func TestSweepOfAnEmptyWalletSpendsNoGas(t *testing.T) {
	f := newFixture(t)
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("broadcast a sweep of an empty wallet")
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("flow should have finished")
	}
	if w := f.wallet(f.proxy.ID); !w.Idle() {
		t.Fatal("wallet not released")
	}
}

func TestPayMovesAnExactAmount(t *testing.T) {
	f := newFixture(t)
	f.chain.balance[f.hot.Address] = wei(1000)
	f.begin(store.FlowTransfer, f.hot, store.StateMoving, flow.Params{
		Amount: wei(250), To: addr(0xDD), Withdrawal: uuid.New(),
	})

	f.step()

	from, to, amount := decodeTransferFrom(t, f.chain.lastSent(t).Data())
	if from != f.hot.Address || to != addr(0xDD) || amount.Cmp(wei(250)) != 0 {
		t.Fatalf("transferFrom(%s -> %s, %s)", from.Hex(), to.Hex(), amount)
	}
}

func TestPayRefusesWhenTheChainHoldsLessThanOurRecords(t *testing.T) {
	// "Verify at the point of spending" — the check that replaces a periodic
	// reconciler, made exactly where drift would cost money.
	f := newFixture(t)
	f.chain.balance[f.hot.Address] = wei(10)

	wd := store.Pending{
		ID: uuid.New(), Wallet: f.hot.ID, Reason: store.ReasonPayout, Destination: addr(0xDD),
		Amount: wei(250), CreatedAt: time.Now(),
	}
	f.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(f.hot.ID, wei(1000)); err != nil { // our record is wrong
			return err
		}
		return tx.PutPending(wd)
	})
	fl := f.begin(store.FlowTransfer, f.hot, store.StateMoving, flow.Params{
		Amount: wei(250), To: wd.Destination, Withdrawal: wd.ID,
	})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("broadcast a payment the chain could not cover")
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("flow should have failed and been removed")
	}
	if err := f.st.View(func(tx *store.Tx) error {
		// Refusing to broadcast is not the same as failing the request: the
		// shortfall is ours, so the withdrawal stays a promise and is retried
		// once custody agrees with the chain again (§28).
		got, ok, err := tx.Pending(wd.ID)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("the withdrawal left the pending keyspace; it should still be owed")
		}
		if !strings.Contains(got.Error, "below") {
			t.Fatalf("withdrawal error = %q, want the shortfall explained", got.Error)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	if err := f.st.View(func(tx *store.Tx) error {
		committed, err := tx.Committed(f.hot.ID)
		if err != nil {
			return err
		}
		// The commitment stands: the payout has not been decided, only
		// deferred, so the wallet still owes it (§28).
		if committed.Cmp(wei(250)) != 0 {
			t.Fatalf("committed = %s, want the 250 still promised", committed)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestJournalSurvivesABroadcastFailure(t *testing.T) {
	// The crash-safety guarantee: the signed transaction is committed before it
	// is sent, so a failure to send leaves it to be retried rather than lost —
	// and never re-signed, which could double-spend.
	f := newFixture(t)
	f.chain.balance[f.proxy.Address] = wei(500)
	f.chain.sendErr = errors.New("connection refused")
	fl := f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})

	f.step()

	got, found := f.flow(fl.ID)
	if !found || !got.Waiting() {
		t.Fatalf("flow lost its in-flight transaction: found=%v %+v", found, got)
	}
	if err := f.st.View(func(tx *store.Tx) error {
		if _, ok, err := tx.TxRefByHash(got.Tx); err != nil || !ok {
			t.Fatalf("watchlist entry missing: ok=%v err=%v", ok, err)
		}
		var journalled int
		return errors.Join(tx.EachSend(func(sd store.Send) error {
			journalled++
			if sd.Hash != got.Tx {
				t.Fatalf("journal holds %s, want %s", sd.Hash.Hex(), got.Tx.Hex())
			}
			return nil
		}), func() error {
			if journalled != 1 {
				t.Fatalf("journal has %d entries, want 1", journalled)
			}
			return nil
		}())
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestSigningIsSequential(t *testing.T) {
	// One transaction in flight at a time is what keeps the pending nonce
	// gapless without a nonce manager.
	f := newFixture(t)
	f.chain.balance[f.proxy.Address] = wei(500)
	f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})
	// A second flow, on a different wallet, also ready to go.
	f.begin(store.FlowTransfer, f.hot, store.StateMoving, flow.Params{
		Amount: wei(1), To: addr(0xDD), Withdrawal: uuid.New(),
	})

	f.step()
	if len(f.chain.sent) != 1 {
		t.Fatalf("broadcast %d transactions in the first step", len(f.chain.sent))
	}
	if f.step() {
		t.Fatal("started a second transaction while one was in flight")
	}
	if len(f.chain.sent) != 1 {
		t.Fatalf("broadcast %d transactions overall, want 1", len(f.chain.sent))
	}
}

func TestNonceComesFromTheChain(t *testing.T) {
	f := newFixture(t)
	f.chain.nonce = 41
	f.chain.balance[f.proxy.Address] = wei(500)
	f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})

	f.step()

	if got := f.chain.lastSent(t).Nonce(); got != 41 {
		t.Fatalf("nonce = %d, want the chain's pending nonce", got)
	}
	f.chain.mu.Lock()
	defer f.chain.mu.Unlock()
	if len(f.chain.nonceFor) == 0 || f.chain.nonceFor[len(f.chain.nonceFor)-1] != f.s.Master() {
		t.Fatalf("nonce was read for %v, want the master", f.chain.nonceFor)
	}
}

func TestStaleTransactionIsRebroadcast(t *testing.T) {
	f := newFixture(t)
	f.s.opts.RebroadcastAfter = time.Millisecond
	f.chain.balance[f.proxy.Address] = wei(500)
	f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})

	f.step()
	if len(f.chain.sent) != 1 {
		t.Fatalf("sent %d", len(f.chain.sent))
	}
	time.Sleep(2 * time.Millisecond)
	f.step()

	if len(f.chain.sent) != 2 {
		t.Fatalf("sent %d, want the journalled transaction re-broadcast", len(f.chain.sent))
	}
	first, second := f.chain.sent[0], f.chain.sent[1]
	if string(first) != string(second) {
		t.Fatal("re-broadcast differed from the original; it must be the same signed bytes")
	}
}

func TestAlreadyBroadcastErrorsAreNotFailures(t *testing.T) {
	// These mean "it is out there, wait for the receipt" — never "sign another".
	for _, msg := range []string{
		"already known", "ALREADY KNOWN", "nonce too low", "known transaction: 0xabc",
	} {
		if !alreadyBroadcast(errors.New(msg)) {
			t.Fatalf("%q treated as a real failure", msg)
		}
	}
	for _, msg := range []string{"connection refused", "insufficient funds for gas"} {
		if alreadyBroadcast(errors.New(msg)) {
			t.Fatalf("%q treated as already broadcast", msg)
		}
	}
}

func TestNewRejectsABadMaster(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := New(st, newFakeChain(), &keys.Ring{}, Options{}); err == nil {
		t.Fatal("New accepted a ring with no usable master")
	}
}

// signerOf recovers the address that signed a transaction.
func signerOf(t *testing.T, tx *types.Transaction) common.Address {
	t.Helper()
	from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(56)), tx)
	if err != nil {
		t.Fatalf("recover signer: %v", err)
	}
	return from
}

// decodeTransferFrom pulls the three arguments back out of the calldata.
func decodeTransferFrom(t *testing.T, data []byte) (from, to common.Address, amount *big.Int) {
	t.Helper()
	if len(data) != 4+32*3 {
		t.Fatalf("transferFrom calldata is %d bytes", len(data))
	}
	from = common.BytesToAddress(data[4+12 : 4+32])
	to = common.BytesToAddress(data[4+32+12 : 4+64])
	amount = new(big.Int).SetBytes(data[4+64 : 4+96])
	return from, to, amount
}

func TestRunDrainsWorkThenIdlesUntilNudged(t *testing.T) {
	f := newFixture(t)
	f.chain.balance[f.proxy.Address] = wei(500)
	f.s.opts.Tick = time.Hour // only a nudge can wake it, so the test is not timing-driven

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx) }()

	// Nothing to do yet.
	time.Sleep(20 * time.Millisecond)
	f.chain.mu.Lock()
	sent := len(f.chain.sent)
	f.chain.mu.Unlock()
	if sent != 0 {
		t.Fatalf("sent %d transactions with no work queued", sent)
	}

	f.begin(store.FlowTransfer, f.proxy, store.StateMoving, flow.Params{})
	f.s.Notify()

	deadline := time.Now().Add(2 * time.Second)
	for {
		f.chain.mu.Lock()
		sent = len(f.chain.sent)
		f.chain.mu.Unlock()
		if sent > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not act on the nudge")
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

func TestNotifyNeverBlocks(t *testing.T) {
	// The watcher calls this after every commit — at ~2.2 blocks a second — so
	// it must never be able to stall the block loop.
	f := newFixture(t)
	for range 100 {
		f.s.Notify()
	}
}
