package sender

import (
	"context"
	"math/big"
	"testing"
	"time"

	"bsc/flow"
	"bsc/store"
	"bsc/swap"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

var (
	router = addr(0xA1)
	wbnb   = addr(0xA2)
)

// withRouter configures the fixture for swapping and records the master as a
// wallet, which is what a gas top-up owns.
func (f *fixture) withRouter(rate int64) store.Wallet {
	f.t.Helper()
	f.s.opts.Router = router
	f.s.opts.SlippageBPS = 100
	f.chain.wrapped = wbnb // the router reports its own

	master := store.Wallet{
		ID: uuid.New(), Kind: store.KindMaster, Address: f.s.Master(),
		Balance: new(big.Int), CreatedAt: time.Now(),
	}
	f.update(func(tx *store.Tx) error { return tx.PutWallet(master) })
	f.chain.swapRate = rate
	return master
}

func TestApproveRouterIsSignedByTheMaster(t *testing.T) {
	f := newFixture(t)
	master := f.withRouter(2)
	f.begin(store.FlowGasTopUp, master, store.StateApprovingRouter, flow.Params{Amount: wei(10)})

	f.step()

	tx := f.chain.lastSent(t)
	if got := signerOf(t, tx); got != f.s.Master() {
		t.Fatalf("approve signed by %s, want the master", got.Hex())
	}
	if *tx.To() != f.s.opts.Token {
		t.Fatalf("approve sent to %s, want the token", tx.To().Hex())
	}
	// The spender must be the router, not the master itself.
	spender := common.BytesToAddress(tx.Data()[16:36])
	if spender != router {
		t.Fatalf("approved %s, want the router %s", spender.Hex(), router.Hex())
	}
}

func TestApproveRouterIsSkippedWhenAlreadyGranted(t *testing.T) {
	f := newFixture(t)
	master := f.withRouter(2)
	f.chain.allowanceTo[allowanceKey{f.s.Master(), router}] = wei(1)
	fl := f.begin(store.FlowGasTopUp, master, store.StateApprovingRouter, flow.Params{Amount: wei(10)})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("re-approved a router that already had an allowance")
	}
	got, _ := f.flow(fl.ID)
	if got.State != store.StateSwapping {
		t.Fatalf("state = %s, want swapping", got.State)
	}
}

func TestSwapBoundsThePriceItWillAccept(t *testing.T) {
	// The only transaction whose outcome is a price rather than a yes or no, so
	// the bound is the whole safety story.
	f := newFixture(t)
	master := f.withRouter(3) // the fake router quotes 3 native per token
	f.chain.balance[f.s.Master()] = wei(100)
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	tx := f.chain.lastSent(t)
	if *tx.To() != router {
		t.Fatalf("swap sent to %s, want the router", tx.To().Hex())
	}
	if got := signerOf(t, tx); got != f.s.Master() {
		t.Fatalf("swap signed by %s, want the master", got.Hex())
	}

	args := tx.Data()[4:]
	amountIn := new(big.Int).SetBytes(args[0:32])
	minOut := new(big.Int).SetBytes(args[32:64])
	if amountIn.Cmp(wei(10)) != 0 {
		t.Fatalf("amountIn = %s, want the configured 10", amountIn)
	}
	// 10 tokens × 3 = 30 quoted, less 1% slippage = 29.
	want, err := swap.MinOut(wei(30), 100)
	if err != nil {
		t.Fatal(err)
	}
	if minOut.Cmp(want) != 0 {
		t.Fatalf("amountOutMin = %s, want %s", minOut, want)
	}
	if minOut.Sign() == 0 {
		t.Fatal("a zero minimum accepts any price at all")
	}

	// The recipient is the master: the gas has to land where it is spent from.
	recipient := common.BytesToAddress(args[96+12 : 128])
	if recipient != f.s.Master() {
		t.Fatalf("swap pays out to %s, want the master", recipient.Hex())
	}
	// A deadline stops a transaction that lingers in the mempool from filling
	// later at a price nobody agreed to.
	deadline := new(big.Int).SetBytes(args[128:160])
	if deadline.Int64() <= time.Now().Unix() {
		t.Fatalf("deadline %s is not in the future", deadline)
	}
}

func TestSwapRefusesWithoutTheTokensToTrade(t *testing.T) {
	f := newFixture(t)
	master := f.withRouter(3)
	f.chain.balance[f.s.Master()] = wei(2) // fees were swept elsewhere
	fl := f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("broadcast a swap it could not cover")
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("flow should have failed")
	}
}

func TestGasTopUpJumpsTheQueue(t *testing.T) {
	// The master pays for every other transaction, so anything queued ahead of
	// a top-up would fail for want of the gas it is about to buy.
	f := newFixture(t)
	master := f.withRouter(3)
	f.chain.balance[f.s.Master()] = wei(100)
	f.chain.balance[f.dep.Address] = wei(500)

	// An older drain, then a newer top-up.
	f.begin(store.FlowDrain, f.dep, store.StateSweeping, flow.Params{})
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	tx := f.chain.lastSent(t)
	if *tx.To() != router {
		t.Fatalf("first transaction went to %s, want the router — gas comes first", tx.To().Hex())
	}
}

func TestWrappedTokenComesFromTheRouter(t *testing.T) {
	// Asking the router removes a setting that could name a different token
	// than the router actually routes through — a path that would either
	// revert or, worse, route somewhere unintended.
	f := newFixture(t)
	master := f.withRouter(3)
	f.chain.balance[f.s.Master()] = wei(100)
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	args := f.chain.lastSent(t).Data()[4:]
	// path[1] is the hop the router unwraps from.
	if got := common.BytesToAddress(args[224+12 : 256]); got != wbnb {
		t.Fatalf("path ends at %s, want the router's own wrapped token %s", got.Hex(), wbnb.Hex())
	}
}

func TestConfiguredWrappedTokenOverridesTheRouter(t *testing.T) {
	// An escape hatch for a fork that names the accessor differently.
	f := newFixture(t)
	master := f.withRouter(3)
	override := addr(0xA9)
	f.s.opts.WrappedNative = override
	f.chain.balance[f.s.Master()] = wei(100)
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	args := f.chain.lastSent(t).Data()[4:]
	if got := common.BytesToAddress(args[224+12 : 256]); got != override {
		t.Fatalf("path ends at %s, want the override %s", got.Hex(), override.Hex())
	}
}

func TestSwapFailsIfTheRouterCannotNameItsWrappedToken(t *testing.T) {
	// Rather than routing through the zero address.
	f := newFixture(t)
	master := f.withRouter(3)
	f.chain.wrapped = common.Address{}
	f.chain.balance[f.s.Master()] = wei(100)
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	if _, err := f.s.Step(context.Background()); err == nil {
		t.Fatal("swapped without knowing the wrapped token")
	}
	if len(f.chain.sent) != 0 {
		t.Fatal("broadcast a swap through an unknown path")
	}
}

func TestSwapIsInertWithoutARouter(t *testing.T) {
	f := newFixture(t)
	master := store.Wallet{
		ID: uuid.New(), Kind: store.KindMaster, Address: f.s.Master(),
		Balance: new(big.Int), CreatedAt: time.Now(),
	}
	f.update(func(tx *store.Tx) error { return tx.PutWallet(master) })
	fl := f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatal("swapped with no router configured")
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("flow should have failed rather than hung")
	}
}

func TestMasterSignsWithTheSecretNotADerivation(t *testing.T) {
	// A master wallet record whose id was run through HMAC derivation would
	// produce a key controlling nothing, and the signature would be worthless.
	f := newFixture(t)
	master := f.withRouter(3)
	derived, err := f.ring.Derive(master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if derived.Address == f.s.Master() {
		t.Skip("derivation happened to collide; nothing to prove")
	}
	f.chain.balance[f.s.Master()] = wei(100)
	f.begin(store.FlowGasTopUp, master, store.StateSwapping, flow.Params{Amount: wei(10)})

	f.step()

	if got := signerOf(t, f.chain.lastSent(t)); got != f.s.Master() {
		t.Fatalf("signed by %s, want the master secret's own address %s", got.Hex(), f.s.Master().Hex())
	}
}
