//go:build e2e

// Package e2e drives the real service against a real chain.
//
// Everything in the normal test suite runs offline against a simulator. That
// simulator is faithful — it enforces allowances and balances exactly as the
// token does — but it is still our own model of the chain, and a model agreeing
// with itself proves nothing about gas estimation, finality timing, receipt
// shapes, or whether the token behaves as we assume. This package exists to
// close that gap, and it is the only test that spends real money.
//
// It is build-tagged and skips unless configured, so `task test` stays offline.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bsc/api"
	"bsc/app"
	"bsc/chain"
	"bsc/keys"
	"bsc/money"
	"bsc/sender"
	"bsc/store"
	"bsc/swap"
	"bsc/usdt"
	"bsc/watcher"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Configuration, all required except where noted. See README.md in this
// directory for how to obtain each one.
const (
	envRPC     = "E2E_RPC_URL"
	envMaster  = "E2E_MASTER_SECRET"
	envToken   = "E2E_TOKEN_ADDRESS"
	envChainID = "E2E_CHAIN_ID" // default 97 (BSC testnet, Chapel)
	envDeposit = "E2E_DEPOSIT"  // default 2 whole tokens
	envTimeout = "E2E_TIMEOUT"  // default 15m for the whole lifecycle

	envDestination = "E2E_DESTINATION"   // payout target; defaults to the funder
	envCollector   = "E2E_FEE_COLLECTOR" // fee target; defaults to the master
	envRouter      = "E2E_SWAP_ROUTER"   // defaults to PancakeSwap V2 for the chain
	envSwapAmount  = "E2E_SWAP_AMOUNT"   // tokens per gas top-up; default 1 whole token
)

// harness is the real service wired against a real chain: real store, real RPC,
// real signing. Only the HTTP listener is replaced by httptest, and the loops
// are stepped by hand so a failure reports which stage it got stuck in.
type harness struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	chain *chain.Client
	ring  *keys.Ring
	snd   *sender.Sender
	wat   *watcher.Watcher
	mux   *http.ServeMux

	scale        money.Scale
	rpcURL       string
	master       common.Address
	masterKey    keys.Key
	funder       keys.Key
	token        common.Address
	collector    common.Address
	destination  common.Address
	chainID      uint64
	deposit      *big.Int
	funderGas    *big.Int
	tokensNeeded *big.Int
	startTokens  *big.Int
	router       common.Address
	swapAmount   *big.Int
	deadline     time.Time
}

// needs declares what a test will actually spend, so one that never deposits
// does not demand a deposit's worth of tokens in the master.
type needs struct {
	deposit bool
	swap    bool
}

func setup(t *testing.T, n needs) *harness {
	t.Helper()

	need := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			t.Skipf("%s is not set; see e2e/README.md (this test spends real testnet funds)", key)
		}
		return v
	}
	masterSecret := need(envMaster)

	chainID := uint64(chain.TestnetChainID)
	if v := os.Getenv(envChainID); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", envChainID, err)
		}
		chainID = n
	}
	// The token defaults to the well-known deployment for the chain; override it
	// when running against your own ERC-20.
	token := usdt.TestnetAddress
	if chainID == chain.MainnetChainID {
		token = usdt.MainnetAddress
	}
	if v := os.Getenv(envToken); v != "" {
		if !common.IsHexAddress(v) {
			t.Fatalf("%s=%q is not a hex address", envToken, v)
		}
		token = common.HexToAddress(v)
	} else if chainID != chain.MainnetChainID && chainID != chain.TestnetChainID {
		t.Skipf("%s must be set for chain %d — there is no default token for it", envToken, chainID)
	}

	if chainID == chain.MainnetChainID && os.Getenv("E2E_I_MEAN_MAINNET") != "yes" {
		// Refusing by default is cheap insurance: this suite derives fresh
		// wallets, moves funds and cannot undo any of it.
		t.Fatalf("%s=56 is mainnet. Set E2E_I_MEAN_MAINNET=yes if that is really intended", envChainID)
	}
	// The endpoint follows the chain unless named, exactly as the service does.
	rpcURL := os.Getenv(envRPC)
	if rpcURL == "" {
		url, ok := chain.DefaultRPC(chainID)
		if !ok {
			t.Skipf("%s must be set: no default endpoint for chain %d", envRPC, chainID)
		}
		rpcURL = url
	}

	timeout := 15 * time.Minute
	if v := os.Getenv(envTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("%s: %v", envTimeout, err)
		}
		timeout = d
	}
	deposit := whole(2)
	if v := os.Getenv(envDeposit); v != "" {
		n, ok := new(big.Int).SetString(v, 10)
		if !ok || n.Sign() <= 0 {
			t.Fatalf("%s must be a positive integer in wei", envDeposit)
		}
		deposit = n
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	ring, err := keys.ParseHex(masterSecret)
	if err != nil {
		t.Fatalf("%s: %v", envMaster, err)
	}
	master, err := ring.Master()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	// The funder is fresh every run: an outside depositor should not be a
	// long-lived fixture whose leftover state can leak between runs. It is
	// stocked from the master at setup and emptied back into it at teardown, so
	// only the master needs to be funded by hand.
	funderKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate funder: %v", err)
	}
	funder := keys.Key{Priv: funderKey, Address: crypto.PubkeyToAddress(funderKey.PublicKey)}

	rpc, err := chain.Dial(ctx, rpcURL, 20)
	if err != nil {
		t.Fatalf("dial %s: %v", rpcURL, err)
	}
	t.Cleanup(rpc.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// The payout destination defaults to the funder, so the paid-out tokens cycle
	// straight back and a repeat run costs only gas.
	destination := funder.Address
	if v := os.Getenv(envDestination); v != "" && common.IsHexAddress(v) {
		destination = common.HexToAddress(v)
	}
	// The fee collector defaults to the master, matching production: swept fees
	// are meant to accumulate there. Keeping the test faithful to that is worth
	// more than making runs marginally cheaper — and the tokens are not lost,
	// they simply move to the other wallet you hold.
	collector := master.Address
	if v := os.Getenv(envCollector); v != "" && common.IsHexAddress(v) {
		collector = common.HexToAddress(v)
	}

	// The router defaults to the verified PancakeSwap V2 deployment for the
	// chain, the same resolution the service performs.
	router := swap.PancakeV2Testnet
	if chainID == chain.MainnetChainID {
		router = swap.PancakeV2Mainnet
	}
	if v := os.Getenv(envRouter); v != "" && common.IsHexAddress(v) {
		router = common.HexToAddress(v)
	}
	swapAmount := whole(1)
	if v := os.Getenv(envSwapAmount); v != "" {
		n, ok := new(big.Int).SetString(v, 10)
		if !ok || n.Sign() <= 0 {
			t.Fatalf("%s must be a positive integer in wei", envSwapAmount)
		}
		swapAmount = n
	}

	// The ledger's unit follows the token itself, exactly as the service does at
	// startup — asking the contract rather than assuming eighteen decimals.
	decimals, err := rpc.TokenDecimals(ctx, token)
	if err != nil {
		t.Fatalf("token decimals: %v", err)
	}
	scale, err := money.NewScale(decimals)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if err := st.Update(func(tx *store.Tx) error {
		return tx.SetMeta(store.Meta{Token: token, Decimals: decimals})
	}); err != nil {
		t.Fatalf("meta: %v", err)
	}

	snd, err := sender.New(st, rpc, ring, sender.Options{
		ChainID: chainID, Token: token, FeeCollector: collector, Scale: scale,
		Router: router, SlippageBPS: 300, // testnet pools are thin; allow 3%
		// Real finality is slower than the simulator's instant blocks; give a
		// transaction a fair chance before re-broadcasting it.
		RebroadcastAfter: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	addrs := watcher.NewAddrSet()
	wat := watcher.New(st, rpc, addrs, watcher.Options{
		StartBlock: 0, // the current finalized head; never scan history
		Poll:       2 * time.Second,
		Scale:      scale, DrainThreshold: whole(1), HouseSweepMin: whole(1),
		FeeCollector: collector,
		Master:       master.Address, Token: token, Notify: snd.Notify,
	})
	srv := api.New(st, ring, addrs, wat, api.Options{
		DefaultFee: store.FeePolicy{Flat: 100}, // $1.00
		Notify:     snd.Notify,
	})
	mux := http.NewServeMux()
	srv.Routes(mux)

	h := &harness{
		t: t, ctx: ctx, store: st, chain: rpc, ring: ring, snd: snd, wat: wat, mux: mux,
		scale: scale, rpcURL: rpcURL, master: master.Address, masterKey: master, funder: funder, token: token,
		collector: collector, destination: destination,
		router: router, swapAmount: swapAmount,
		// Enough for the funder's handful of transfers, returned at the end.
		funderGas: big.NewInt(10_000_000_000_000_000), // 0.01
		chainID:   chainID, deposit: deposit, deadline: time.Now().Add(timeout),
	}
	h.tokensNeeded = new(big.Int)
	if n.deposit {
		h.tokensNeeded.Add(h.tokensNeeded, h.deposit)
	}
	if n.swap {
		h.tokensNeeded.Add(h.tokensNeeded, h.swapAmount)
	}
	h.preflight()
	h.stockFunder()
	t.Cleanup(h.recoverEverything)

	if err := wat.Start(ctx); err != nil {
		t.Fatalf("watcher start: %v", err)
	}
	t.Logf("chain %d · master %s · funder %s · token %s",
		chainID, master.Address.Hex(), funder.Address.Hex(), token.Hex())
	return h
}

// preflight fails early and legibly rather than letting the suite time out
// twelve minutes later on a wallet that was never funded.
func (h *harness) preflight() {
	h.t.Helper()

	// Everything starts in the master: it stocks the generated funder and
	// collects it back at the end, so it is the only balance to check.
	gas, err := h.chain.BalanceBNB(h.ctx, h.master)
	if err != nil {
		h.t.Fatalf("preflight: master balance: %v", err)
	}
	if gas.Cmp(h.funderGas) <= 0 {
		h.t.Fatalf("preflight: master %s holds %s native — not enough to stock the funder with %s and pay for the run. Fund it from the faucet.",
			h.master.Hex(), gas, h.funderGas)
	}

	// The suite speaks in whole tokens, so anything other than eighteen decimals
	// would make every figure here wrong by orders of magnitude while still
	// "working". The service itself handles any token with at least two.
	if d := h.tokenDecimals(); d != 18 {
		h.t.Fatalf("preflight: token %s reports %d decimals, but this suite is written for 18",
			h.token.Hex(), d)
	}

	// Recorded before anything moves, so teardown can put the master back on
	// the number it started from rather than on a guess about what was spent.
	held := h.tokenBalance(h.master)
	h.startTokens = held
	if held.Cmp(h.tokensNeeded) < 0 {
		h.t.Fatalf("preflight: master holds %s of the token but this test needs %s — check %s is right and the wallet is funded",
			fmtToken(held), fmtToken(h.tokensNeeded), envToken)
	}
	h.t.Logf("preflight ok · master %s · gas %s · tokens %s",
		h.master.Hex(), gas, fmtToken(held))
}

// --- driving the service ---

// pump steps the watcher and sender until cond holds, so a stuck stage reports
// what it was waiting for rather than a bare timeout.
func (h *harness) pump(what string, cond func() bool) {
	h.t.Helper()
	started := time.Now()
	var lastLog time.Time

	for {
		if cond() {
			h.t.Logf("✓ %s (%s)", what, time.Since(started).Round(time.Second))
			return
		}
		if time.Now().After(h.deadline) || h.ctx.Err() != nil {
			h.dump()
			h.t.Fatalf("timed out waiting for %s after %s", what, time.Since(started).Round(time.Second))
		}
		for {
			worked, err := h.snd.Step(h.ctx)
			if err != nil {
				h.t.Fatalf("sender: %v", err)
			}
			if !worked {
				break
			}
		}
		if _, err := h.wat.Step(h.ctx); err != nil {
			h.t.Fatalf("watcher: %v", err)
		}
		if time.Since(lastLog) > 20*time.Second {
			lastLog = time.Now()
			h.t.Logf("… waiting for %s (%s elapsed, %d blocks behind)",
				what, time.Since(started).Round(time.Second), h.wat.Behind())
		}
		time.Sleep(time.Second)
	}
}

// audit runs the offline consistency check against a snapshot of the live
// database, which is the only way to read it while the service holds the lock.
func (h *harness) audit() (store.Report, error) {
	h.t.Helper()
	path := filepath.Join(h.t.TempDir(), "audit.db")
	f, err := os.Create(path)
	if err != nil {
		return store.Report{}, err
	}
	if _, err := h.store.Snapshot(f); err != nil {
		f.Close() //nolint:errcheck
		return store.Report{}, err
	}
	if err := f.Close(); err != nil {
		return store.Report{}, err
	}
	return app.Verify(path)
}

// dump prints the live flows, which is almost always the answer to "why is it stuck".
func (h *harness) dump() {
	h.t.Helper()
	_ = h.store.View(func(tx *store.Tx) error {
		return tx.EachFlow(func(f store.Flow) error {
			h.t.Logf("  flow %s %s/%s wallet=%s tx=%s err=%q",
				f.ID, f.Kind, f.State, f.Wallet, f.Tx.Hex(), f.Error)
			return nil
		})
	})
}

// --- chain helpers ---

// sendTokens moves tokens from the funder, simulating an outside depositor.
func (h *harness) sendTokens(to common.Address, amount *big.Int) common.Hash {
	h.t.Helper()
	return h.transferTokens(h.funder, to, amount)
}

// transferTokens signs an ERC-20 transfer from the given wallet. This is test
// scaffolding, deliberately outside the service's own sender: it moves funds
// the service knows nothing about.
func (h *harness) transferTokens(from keys.Key, to common.Address, amount *big.Int) common.Hash {
	h.t.Helper()
	data := usdt.NewUsdt().PackTransfer(to, amount)
	hash := h.signAndSend(from, h.token, new(big.Int), data)
	h.t.Logf("%s sent %s tokens to %s (tx %s)",
		from.Address.Hex(), fmtToken(amount), to.Hex(), hash.Hex())
	return hash
}

// transferNative moves the chain's own currency.
func (h *harness) transferNative(from keys.Key, to common.Address, amount *big.Int) common.Hash {
	h.t.Helper()
	hash := h.signAndSend(from, to, amount, nil)
	h.t.Logf("%s sent %s native to %s (tx %s)", from.Address.Hex(), amount, to.Hex(), hash.Hex())
	return hash
}

// signAndSend builds, signs and broadcasts one transaction.
func (h *harness) signAndSend(from keys.Key, to common.Address, value *big.Int, data []byte) common.Hash {
	h.t.Helper()
	price, err := h.chain.GasPrice(h.ctx)
	if err != nil {
		h.t.Fatalf("gas price: %v", err)
	}
	gas, err := h.chain.EstimateGas(h.ctx, ethereum.CallMsg{
		From: from.Address, To: &to, Value: value, Data: data,
	})
	if err != nil {
		h.t.Fatalf("estimate: %v", err)
	}
	nonce, err := h.chain.Nonce(h.ctx, from.Address)
	if err != nil {
		h.t.Fatalf("nonce: %v", err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, GasPrice: price, Gas: gas * 12 / 10,
		To: &to, Value: value, Data: data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(new(big.Int).SetUint64(h.chainID)), from.Priv)
	if err != nil {
		h.t.Fatalf("sign: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		h.t.Fatalf("encode: %v", err)
	}
	hash, err := h.chain.SendRawTx(h.ctx, raw)
	if err != nil {
		h.t.Fatalf("send: %v", err)
	}
	return hash
}

// awaitBalance waits for a balance to reach at least want. The service learns
// finality from its watcher; this scaffolding has no watcher of its own, so it
// simply watches the number it cares about.
func (h *harness) awaitBalance(what string, read func() *big.Int, want *big.Int) {
	h.t.Helper()
	started := time.Now()
	for read().Cmp(want) < 0 {
		if time.Now().After(h.deadline) || h.ctx.Err() != nil {
			h.t.Fatalf("timed out waiting for %s to reach %s (have %s)", what, want, read())
		}
		time.Sleep(2 * time.Second)
	}
	h.t.Logf("✓ %s (%s)", what, time.Since(started).Round(time.Second))
}

// waitFor polls until cond holds, reporting whether it made it rather than
// failing. Teardown uses this instead of awaitBalance: a recovery that cannot
// finish must not retroactively fail a test that already passed.
func (h *harness) waitFor(what string, cond func() bool) bool {
	h.t.Helper()
	started := time.Now()
	for !cond() {
		if time.Now().After(h.deadline) || h.ctx.Err() != nil {
			h.t.Logf("recovery: gave up waiting for %s after %s",
				what, time.Since(started).Round(time.Second))
			return false
		}
		time.Sleep(2 * time.Second)
	}
	h.t.Logf("✓ %s (%s)", what, time.Since(started).Round(time.Second))
	return true
}

// stockFunder moves the run's working funds from the master into the freshly
// generated funder wallet.
func (h *harness) stockFunder() {
	h.t.Helper()
	tokens := h.tokensNeeded
	h.t.Logf("stocking funder %s with %s tokens and %s native",
		h.funder.Address.Hex(), fmtToken(tokens), h.funderGas)

	h.transferNative(h.masterKey, h.funder.Address, h.funderGas)
	h.awaitBalance("funder gas", func() *big.Int {
		v, err := h.chain.BalanceBNB(h.ctx, h.funder.Address)
		if err != nil {
			h.t.Fatalf("funder balance: %v", err)
		}
		return v
	}, h.funderGas)

	if tokens.Sign() == 0 {
		return // this test spends no tokens
	}
	h.transferTokens(h.masterKey, h.funder.Address, tokens)
	h.awaitBalance("funder tokens", func() *big.Int {
		return h.tokenBalance(h.funder.Address)
	}, tokens)
}

// recoverEverything returns whatever the funder still holds to the master, so a
// run leaves the master roughly as it found it and the next run can reuse it.
//
// It is best effort by design: a teardown failure must not mask the result of
// the test that just ran, so problems are logged rather than failed.
func (h *harness) recoverEverything() {
	if h.ctx.Err() != nil {
		h.t.Logf("skipping recovery: context already ended")
		return
	}
	h.recoverDerived()
	if tokens := h.tokenBalance(h.funder.Address); tokens.Sign() > 0 {
		want := new(big.Int).Add(h.tokenBalance(h.master), tokens)
		h.t.Logf("returning %s tokens to the master", fmtToken(tokens))
		h.transferTokens(h.funder, h.master, tokens)
		// Wait on the master's balance rising rather than the funder's falling:
		// this is a lower bound, and "at least zero" is always true.
		h.waitFor("tokens returned", func() bool {
			return h.tokenBalance(h.master).Cmp(want) >= 0
		})
	}
	h.returnNative()
	// Last, once every token the run can return has: whatever is still missing
	// was converted into gas, so buy it back.
	h.restoreTokens()
}

// recoverDerived pulls tokens back out of the wallets the service derived
// during the run.
//
// A failed run strands funds in them — a deposit that was swept into an app's
// hot wallet but never paid out, say — and without this the next run's preflight
// fails for want of tokens that are sitting right there. It works because of the
// design's own property: the master holds an unlimited allowance on every
// derived wallet, so it can pull the balance back *and* pay the gas, with no
// need to fund those wallets first.
func (h *harness) recoverDerived() {
	h.t.Helper()

	var wallets []store.Wallet
	if err := h.store.View(func(tx *store.Tx) error {
		return tx.EachWallet(func(w store.Wallet) error {
			if w.Kind != store.KindMaster {
				wallets = append(wallets, w)
			}
			return nil
		})
	}); err != nil {
		h.t.Logf("recovery: listing wallets: %v", err)
		return
	}

	abi := usdt.NewUsdt()
	before, pulled := h.tokenBalance(h.master), new(big.Int)
	for _, w := range wallets {
		held := h.tokenBalance(w.Address)
		if held.Sign() == 0 {
			continue
		}
		// Without an allowance the transferFrom would simply revert and waste
		// the gas, so check before spending it.
		out, err := h.chain.Call(h.ctx, h.token, abi.PackAllowance(w.Address, h.master))
		if err != nil {
			h.t.Logf("recovery: allowance for %s: %v", w.Address.Hex(), err)
			continue
		}
		allowed, err := abi.UnpackAllowance(out)
		if err != nil || allowed.Cmp(held) < 0 {
			h.t.Logf("recovery: %s holds %s but the master may only move %v — leaving it",
				w.Address.Hex(), fmtToken(held), allowed)
			continue
		}
		h.t.Logf("recovering %s tokens stranded in %s (%s)", fmtToken(held), w.Address.Hex(), w.Kind)
		h.signAndSend(h.masterKey, h.token, new(big.Int),
			abi.PackTransferFrom(w.Address, h.master, held))
		pulled.Add(pulled, held)
	}
	if pulled.Sign() > 0 {
		// The recovered tokens have to land before anything downstream reads the
		// master's balance, or the buy-back would purchase them a second time.
		want := new(big.Int).Add(before, pulled)
		h.waitFor("stranded tokens recovered", func() bool {
			return h.tokenBalance(h.master).Cmp(want) >= 0
		})
	}
}

// returnNative sends back everything the funder holds bar the cost of the
// sending transaction itself.
func (h *harness) returnNative() {
	h.t.Helper()
	held, err := h.chain.BalanceBNB(h.ctx, h.funder.Address)
	if err != nil {
		h.t.Logf("recovery: funder balance: %v", err)
		return
	}
	price, err := h.chain.GasPrice(h.ctx)
	if err != nil {
		h.t.Logf("recovery: gas price: %v", err)
		return
	}
	// A plain transfer is 21000 gas; leave exactly that much behind, bumped a
	// little so a price move between here and mining does not strand it.
	cost := new(big.Int).Mul(price, big.NewInt(21_000))
	cost.Mul(cost, big.NewInt(12))
	cost.Div(cost, big.NewInt(10))

	send := new(big.Int).Sub(held, cost)
	if send.Sign() <= 0 {
		h.t.Logf("recovery: funder holds %s, not worth returning after %s of gas", held, cost)
		return
	}
	h.t.Logf("returning %s native to the master (leaving %s for gas)", send, cost)
	h.transferNative(h.funder, h.master, send)
}

// tokenDecimals reads decimals() directly: it is part of IERC20Metadata rather
// than IERC20, so the generated bindings do not carry it.
func (h *harness) tokenDecimals() uint8 {
	h.t.Helper()
	out, err := h.chain.Call(h.ctx, h.token, crypto.Keccak256([]byte("decimals()"))[:4])
	if err != nil {
		h.t.Fatalf("preflight: decimals(): %v — is %s really an ERC-20 on this chain?", err, h.token.Hex())
	}
	if len(out) == 0 {
		h.t.Fatalf("preflight: %s returned nothing for decimals() — wrong address, or not a token",
			h.token.Hex())
	}
	return uint8(new(big.Int).SetBytes(out).Uint64())
}

// custody reads what the service *records* a wallet as holding, which lags the
// chain by one finality: the watcher credits and debits when a block is
// finalized, not when a transaction mines. Assertions about settled state must
// use this rather than balanceOf, or they race ahead of the service.
func (h *harness) custody(addr common.Address) *big.Int {
	h.t.Helper()
	var out *big.Int
	if err := h.store.View(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByAddress(addr)
		if err != nil {
			return err
		}
		if ok {
			out = w.Balance
		}
		return nil
	}); err != nil {
		h.t.Fatalf("custody %s: %v", addr.Hex(), err)
	}
	if out == nil {
		return new(big.Int)
	}
	return out
}

func (h *harness) tokenBalance(addr common.Address) *big.Int {
	h.t.Helper()
	abi := usdt.NewUsdt()
	out, err := h.chain.Call(h.ctx, h.token, abi.PackBalanceOf(addr))
	if err != nil {
		h.t.Fatalf("balanceOf %s: %v", addr.Hex(), err)
	}
	v, err := abi.UnpackBalanceOf(out)
	if err != nil {
		h.t.Fatalf("decode balanceOf: %v", err)
	}
	return v
}

// --- HTTP helpers ---

func (h *harness) call(method, path string, body any, want int, into any) {
	h.t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != want {
		h.t.Fatalf("%s %s → %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if into != nil {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			h.t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
}

// whole returns n tokens assuming 18 decimals.
func whole(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

func fmtToken(v *big.Int) string {
	if v == nil {
		return "0"
	}
	q, r := new(big.Int).QuoRem(v, whole(1), new(big.Int))
	return fmt.Sprintf("%s.%018s", q, r)
}
