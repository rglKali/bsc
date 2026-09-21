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
// It is build-tagged and skips unless configured, so `task unit` stays offline.
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
	"testing"
	"time"

	"bsc/api"
	"bsc/chain"
	"bsc/cli"
	"bsc/keys"
	"bsc/sender"
	"bsc/store"
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
	envDeposit = "E2E_DEPOSIT" // default 2 whole tokens
	envTimeout = "E2E_TIMEOUT" // default 15m for the whole lifecycle

	envDestination = "E2E_DESTINATION" // payout target; defaults to the funder
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
	// live is set once wat.Start has resolved the cursor. Before that the
	// watcher exists but its cursor is 0, and stepping it would start a backfill
	// from block 0 — a hundred million blocks of history nobody asked for.
	live bool
	mux  *http.ServeMux

	decimals     uint8
	rpcURL       string
	master       common.Address
	masterKey    keys.Key
	funder       keys.Key
	token        common.Address
	destination  common.Address
	chainID      uint64
	deposit      *big.Int
	funderGas    *big.Int
	tokensNeeded *big.Int
	startTokens  *big.Int
	deadline     time.Time
}

// needs declares what a test will actually spend, so one that never deposits
// does not demand a deposit's worth of tokens in the master.
type needs struct {
	deposit bool
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

	// The endpoint is the only chain input, exactly as the service treats it.
	// Everything else — which chain this is, the token, and the
	// mainnet guard below — follows what the endpoint reports once dialled.
	rpcURL := os.Getenv(envRPC)
	if rpcURL == "" {
		rpcURL = chain.TestnetDefaultRPC
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

	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		t.Fatalf("chain id: %v", err)
	}
	// Asking the endpoint rather than trusting a variable is what makes this
	// guard real: the old one compared against a configured chain id, so
	// pointing E2E_RPC_URL at mainnet while leaving the id at 97 sailed
	// straight past it — on the one suite that derives wallets and moves funds
	// it cannot get back.
	if chainID == chain.MainnetChainID && os.Getenv("E2E_I_MEAN_MAINNET") != "yes" {
		t.Fatalf("%s points at mainnet (chain %d). Set E2E_I_MEAN_MAINNET=yes if that is really intended",
			envRPC, chainID)
	}

	// The token defaults to the well-known deployment for the chain; override it
	// when running against your own ERC-20.
	var token common.Address
	switch v := os.Getenv(envToken); {
	case v != "":
		if !common.IsHexAddress(v) {
			t.Fatalf("%s=%q is not a hex address", envToken, v)
		}
		token = common.HexToAddress(v)
	case chainID == chain.MainnetChainID:
		token = usdt.MainnetAddress
	case chainID == chain.TestnetChainID:
		token = usdt.TestnetAddress
	default:
		t.Skipf("%s must be set for chain %d — there is no default token for it", envToken, chainID)
	}

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
	// The token's decimals are read from the contract, exactly as the service
	// does at startup. Nothing scales by them any more (§36) — they are the
	// database's identity and what lets a log line render an amount.
	decimals, err := rpc.TokenDecimals(ctx, token)
	if err != nil {
		t.Fatalf("token decimals: %v", err)
	}
	if err := st.Update(func(tx *store.Tx) error {
		return tx.SetMeta(store.Meta{
			ChainID: chainID, Token: token, Decimals: decimals,
			Master: master.Address,
		})
	}); err != nil {
		t.Fatalf("meta: %v", err)
	}

	snd, err := sender.New(st, rpc, ring, sender.Options{
		ChainID: chainID, Token: token,
		// Real finality is slower than the simulator's instant blocks; give a
		// transaction a fair chance before re-broadcasting it.
		RebroadcastAfter: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	addrs := watcher.NewAddrSet()
	wat := watcher.New(st, rpc, addrs, watcher.Options{
		StartBlock:     0, // the current finalized head; never scan history
		Poll:           2 * time.Second,
		DrainThreshold: whole(1),
		Master:         master.Address, Token: token, Notify: snd.Notify,
	})
	srv := api.New(st, ring, addrs, wat, api.Options{Notify: snd.Notify})
	mux := http.NewServeMux()
	srv.Routes(mux)

	h := &harness{
		t: t, ctx: ctx, store: st, chain: rpc, ring: ring, snd: snd, wat: wat, mux: mux,
		decimals: decimals, rpcURL: rpcURL, master: master.Address, masterKey: master, funder: funder, token: token,
		destination: destination,
		// Enough for the funder's handful of transfers, returned at the end.
		funderGas: big.NewInt(10_000_000_000_000_000), // 0.01
		chainID:   chainID, deposit: deposit, deadline: time.Now().Add(timeout),
	}
	h.tokensNeeded = new(big.Int)
	if n.deposit {
		h.tokensNeeded.Add(h.tokensNeeded, h.deposit)
	}
	h.preflight()
	h.stockFunder()
	t.Cleanup(h.recoverEverything)

	if err := wat.Start(ctx); err != nil {
		t.Fatalf("watcher start: %v", err)
	}
	h.live = true
	t.Logf("chain %d · master %s · funder %s · token %s",
		chainID, master.Address.Hex(), funder.Address.Hex(), token.Hex())
	return h
}

// preflight fails early and legibly rather than letting the suite time out
// twelve minutes later on a wallet that was never funded.
func (h *harness) preflight() {
	h.t.Helper()

	// Sweep first, count second. A previous run that was killed mid-flight
	// leaves tokens in wallets this secret derives again — the addresses are
	// deterministic now (§48) — and those leftovers would otherwise be swept
	// along with this run's deposit, making every amount assertion wrong and
	// tripping a real balance underflow in the watcher. Recovering them also
	// puts them back where the preflight expects to find them.
	h.recoverDerived()

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
		h.drive()
		if time.Since(lastLog) > 20*time.Second {
			lastLog = time.Now()
			h.t.Logf("… waiting for %s (%s elapsed, %d blocks behind)",
				what, time.Since(started).Round(time.Second), h.wat.Behind())
		}
		time.Sleep(time.Second)
	}
}

// drive steps the sender until it runs out of work, then the watcher once. It
// is what turns a wait into progress, and it no-ops before the service exists
// so the funding waits at startup can use it too.
//
// Everything that waits calls this. An earlier version had `pump` drive and
// `awaitBalance` merely poll, which coupled them: a pump whose condition was
// already true did no work, and the awaitBalance after it then waited on a
// balance nothing was moving — for the full 30-minute timeout.
func (h *harness) drive() {
	h.t.Helper()
	// Not "is the watcher constructed" but "has it resolved its cursor": the
	// funder is stocked before Start, and stepping an unstarted watcher would
	// walk the chain from block 0.
	if !h.live {
		return
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
}

// audit runs the offline consistency check against a snapshot of the live
// database, which is the only way to read it while the service holds the lock.
// view runs a read transaction, for the handful of assertions that look at a
// record rather than at the API.
func (h *harness) view(fn func(*store.Tx) error) {
	h.t.Helper()
	if err := h.store.View(fn); err != nil {
		h.t.Fatalf("View: %v", err)
	}
}

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
	return cli.Verify(path)
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
			h.dump()
			h.t.Fatalf("timed out waiting for %s to reach %s (have %s)", what, want, read())
		}
		h.drive()
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
		h.drive()
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
}

// derivedScan is how many wallet indices the recovery walks.
//
// Since §48 a wallet's address is HMAC(master, be64(index)) with indices from
// 1, so EVERY RUN DERIVES THE SAME ADDRESSES. That is the point of the change —
// the money is recoverable from the secret alone — but it means one run's
// leftovers are the next run's starting balance, and a fresh database has no
// record of them. So the recovery derives the sequence rather than reading the
// store, which is also the gap scan §48 describes, at a scale where it is free.
//
// A run uses three wallets; 32 is slack for a suite that grows.
const derivedScan = 32

// recoverDerived pulls tokens back out of the wallets this secret derives.
//
// It runs BEFORE the suite as well as after. A run that is killed mid-flight —
// or one whose teardown could not finish — strands funds in a derived wallet,
// and the next run would then sweep a deposit *plus* those leftovers: the drain
// moves more than was deposited, the watcher debits more custody than it
// recorded (a genuine balance underflow, correctly reported), and every amount
// assertion downstream is wrong by the leftover. That is exactly what happened
// the first time this suite ran end to end.
//
// It works because of the design's own property: the master holds an unlimited
// allowance on every wallet it has activated, so it can pull the balance back
// *and* pay the gas, with no need to fund those wallets first. A wallet that
// was never activated has no allowance — and, never having been drained, is
// where its tokens are stuck until somebody funds it by hand.
func (h *harness) recoverDerived() {
	h.t.Helper()

	abi := usdt.NewUsdt()
	before, pulled := h.tokenBalance(h.master), new(big.Int)
	for i := uint64(1); i <= derivedScan; i++ {
		key, err := h.ring.Derive(i)
		if err != nil {
			h.t.Logf("recovery: derive %d: %v", i, err)
			continue
		}
		held := h.tokenBalance(key.Address)
		if held.Sign() == 0 {
			continue
		}
		// Without an allowance the transferFrom would simply revert and waste
		// the gas, so check before spending it.
		out, err := h.chain.Call(h.ctx, h.token, abi.PackAllowance(key.Address, h.master))
		if err != nil {
			h.t.Logf("recovery: allowance for %s: %v", key.Address.Hex(), err)
			continue
		}
		allowed, err := abi.UnpackAllowance(out)
		h.t.Logf("recovering %s tokens stranded in wallet %d (%s)", fmtToken(held), i, key.Address.Hex())
		if err == nil && allowed.Cmp(held) >= 0 {
			h.signAndSend(h.masterKey, h.token, new(big.Int),
				abi.PackTransferFrom(key.Address, h.master, held))
			pulled.Add(pulled, held)
			continue
		}
		// No allowance: the wallet holds tokens but was never activated, which
		// is what a run killed between the deposit and the drain leaves behind.
		// The master cannot pull from it — but the harness holds its key, so it
		// can push. Gas first, since an unactivated wallet has none.
		h.t.Logf("recovery: wallet %d was never activated (allowance %v); pushing instead", i, allowed)
		if !h.fundGasFor(key.Address) {
			continue
		}
		h.transferTokens(key, h.master, held)
		pulled.Add(pulled, held)
	}
	if pulled.Sign() > 0 {
		// The recovered tokens have to land before anything downstream reads the
		// master's balance, or the preflight would count them twice.
		want := new(big.Int).Add(before, pulled)
		h.waitFor("stranded tokens recovered", func() bool {
			return h.tokenBalance(h.master).Cmp(want) >= 0
		})
	}
}

// fundGasFor gives a derived wallet just enough native currency to send one
// token transfer, for the recovery path where the master cannot pull.
func (h *harness) fundGasFor(addr common.Address) bool {
	h.t.Helper()
	price, err := h.chain.GasPrice(h.ctx)
	if err != nil {
		h.t.Logf("recovery: gas price: %v", err)
		return false
	}
	// An ERC-20 transfer is well under 100k gas; double it and the price, since
	// stranding the recovery itself for want of a few gwei helps nobody.
	need := new(big.Int).Mul(price, big.NewInt(200_000))
	held, err := h.chain.BalanceBNB(h.ctx, addr)
	if err != nil {
		h.t.Logf("recovery: balance of %s: %v", addr.Hex(), err)
		return false
	}
	if held.Cmp(need) >= 0 {
		return true
	}
	h.signAndSend(h.masterKey, addr, new(big.Int).Sub(need, held), nil)
	return h.waitFor("gas for "+addr.Hex(), func() bool {
		got, err := h.chain.BalanceBNB(h.ctx, addr)
		return err == nil && got.Cmp(need) >= 0
	})
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
