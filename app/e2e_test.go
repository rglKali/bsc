package app

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"bsc/api"
	"bsc/engine"
	"bsc/keys"
	"bsc/money"
	"bsc/sender"
	"bsc/store"
	"bsc/usdt"
	"bsc/watcher"

	"github.com/ethereum/go-ethereum/common"
)

// stack is every package wired together against the simulated chain — the same
// composition Run builds, minus the HTTP listener and the signal handling.
type stack struct {
	t     *testing.T
	st    *store.Store
	sim   *chainSim
	snd   *sender.Sender
	wat   *watcher.Watcher
	mux   *http.ServeMux
	addrs *watcher.AddrSet
	cfg   engine.Config
	scale money.Scale
}

func newStack(t *testing.T) *stack {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ring, err := keys.New(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	master, _ := ring.Master()

	sim := newChainSim(usdt.MainnetAddress)
	sim.fund(master.Address, ether(100)) // the operator has topped up gas
	collector := common.HexToAddress("0x00000000000000000000000000000000000000fe")

	// Real USDT on BSC has 18 decimals, so a cent is 10^16 wei and the tests
	// below can speak in whole tokens exactly as an integrator would.
	scale, err := money.NewScale(18)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	snd, err := sender.New(st, sim, ring, sender.Options{
		ChainID: 56, Token: usdt.MainnetAddress, FeeCollector: collector, Scale: scale,
	})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	addrs := watcher.NewAddrSet()
	wat := watcher.New(st, sim, addrs, watcher.Options{
		StartBlock: 1, BackfillBatch: 50,
		Scale: scale, DrainThreshold: ether(1), HouseSweepMin: ether(1),
		FeeCollector: collector,
		Master:       master.Address, Token: usdt.MainnetAddress,
		Notify: snd.Notify,
	})
	srv := api.New(st, ring, addrs, wat, api.Options{
		DefaultFee: store.FeePolicy{Flat: 100}, // $1.00
	})
	mux := http.NewServeMux()
	srv.Routes(mux)

	s := &stack{
		t: t, st: st, sim: sim, snd: snd, wat: wat, mux: mux, addrs: addrs,
		cfg: engine.Config{
			Scale: scale, DrainThreshold: ether(1), HouseSweepMin: ether(1),
			FeeCollector: collector,
		},
		scale: scale,
	}
	if err := wat.Start(context.Background()); err != nil {
		t.Fatalf("watcher start: %v", err)
	}
	return s
}

// settle runs the loop the process runs: seal a block, let the watcher apply it,
// let the sender act, repeat until nothing is left to do.
func (s *stack) settle() {
	s.t.Helper()
	ctx := context.Background()
	for range 40 {
		// Drain the sender first so anything it signs lands in the next block.
		for {
			worked, err := s.snd.Step(ctx)
			if err != nil {
				s.t.Fatalf("sender: %v", err)
			}
			if !worked {
				break
			}
		}
		s.sim.seal(s.t)
		for {
			caughtUp, err := s.wat.Step(ctx)
			if err != nil {
				s.t.Fatalf("watcher: %v", err)
			}
			if caughtUp {
				break
			}
		}
		if s.idle() {
			return
		}
	}
	s.t.Fatalf("did not settle: %s", s.sim)
}

// idle reports that no flow is outstanding.
func (s *stack) idle() bool {
	s.t.Helper()
	n := 0
	if err := s.st.View(func(tx *store.Tx) error {
		return tx.EachFlow(func(store.Flow) error { n++; return nil })
	}); err != nil {
		s.t.Fatalf("View: %v", err)
	}
	return n == 0
}

func (s *stack) call(method, path string, body any, want int, into any) {
	s.t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != want {
		s.t.Fatalf("%s %s → %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if into != nil {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			s.t.Fatalf("decode: %v", err)
		}
	}
}

func (s *stack) audit() {
	s.t.Helper()
	if err := s.st.View(func(tx *store.Tx) error {
		rep, err := tx.Verify()
		if err != nil {
			return err
		}
		if !rep.OK() {
			s.t.Fatalf("audit findings: %v", rep.Findings)
		}
		return nil
	}); err != nil {
		s.t.Fatalf("View: %v", err)
	}
}

func ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// TestFullMoneyLifecycle walks a deposit all the way to a payout through every
// package: API → store → work rules → sender → chain → watcher → settlement.
func TestFullMoneyLifecycle(t *testing.T) {
	s := newStack(t)

	// 1. An app registers. This derives its hot wallet and pre-warms it.
	var app struct {
		Address string `json:"address"`
	}
	s.call("PUT", "/v1/apps/df", map[string]any{}, http.StatusCreated, &app)
	top := common.HexToAddress(app.Address)
	s.settle()
	s.audit()

	// 2. It asks for a deposit address for one of its users.
	var da struct {
		Address string `json:"address"`
	}
	s.call("POST", "/v1/apps/df/addresses", map[string]any{"ref": "cust-1"}, http.StatusCreated, &da)
	deposit := common.HexToAddress(da.Address)

	// 3. The user sends 50 USDT to it.
	s.sim.deposit(deposit, ether(50))
	s.settle()

	// The deposit is recorded, credited, and the money is on the hot wallet.
	var feed struct {
		Deposits []struct {
			AmountCents string `json:"amount_cents"`
			Status      string `json:"status"`
			Ref         string `json:"ref"`
			TxHash      string `json:"tx_hash"`
		} `json:"deposits"`
	}
	s.call("GET", "/v1/apps/df/deposits", nil, http.StatusOK, &feed)
	if len(feed.Deposits) != 1 {
		t.Fatalf("deposits = %+v", feed.Deposits)
	}
	// Cents and a hash, and nothing about how the money moved: 50 USDT is 5000
	// cents, and the hash is the app's one handle on the chain (§27).
	if d := feed.Deposits[0]; d.Status != "credited" || d.Ref != "cust-1" ||
		d.AmountCents != "5000" || d.TxHash == "" {
		t.Fatalf("deposit = %+v", d)
	}
	if got := s.sim.usdtOf(top); got.Cmp(ether(50)) != 0 {
		t.Fatalf("hot wallet holds %s on-chain, want 50 USDT", got)
	}
	if got := s.sim.usdtOf(deposit); got.Sign() != 0 {
		t.Fatalf("deposit wallet still holds %s", got)
	}

	var balance struct {
		AvailableCents string `json:"available_cents"`
		PendingCents   string `json:"pending_cents"`
	}
	s.call("GET", "/v1/apps/df/balance", nil, http.StatusOK, &balance)
	if balance.AvailableCents != "5000" || balance.PendingCents != "0" {
		t.Fatalf("balance = %+v", balance)
	}
	s.audit()

	// 4. The app pays a user out, with the default 1 USDT fee on top.
	dest := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	var wd struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FeeCents    string `json:"fee_cents"`
		PayoutCents string `json:"payout_cents"`
	}
	s.call("POST", "/v1/apps/df/withdrawals", map[string]any{
		"destination": dest.Hex(), "amount_cents": "2000",
	}, http.StatusCreated, &wd)
	if wd.Status != "pending" || wd.PayoutCents != "2000" || wd.FeeCents != "100" {
		t.Fatalf("withdrawal = %+v", wd)
	}
	s.settle()

	// The destination has the money, and the withdrawal is done.
	if got := s.sim.usdtOf(dest); got.Cmp(ether(20)) != 0 {
		t.Fatalf("destination holds %s, want 20 USDT", got)
	}
	var settled struct {
		Status string `json:"status"`
		TxHash string `json:"tx_hash"`
	}
	s.call("GET", "/v1/apps/df/withdrawals/"+wd.ID, nil, http.StatusOK, &settled)
	if settled.Status != "debited" || settled.TxHash == "" {
		t.Fatalf("withdrawal = %+v", settled)
	}

	// The fee never moved with the payout: it was charged in the books and left
	// sitting in the hot wallet as the house's, and the house sweep then
	// collected it because it cleared the threshold (§24, §25).
	collector := common.HexToAddress("0x00000000000000000000000000000000000000fe")
	if got := s.sim.usdtOf(collector); got.Cmp(ether(1)) != 0 {
		t.Fatalf("collector holds %s, want the 1 USDT fee swept", got)
	}
	var after struct {
		Balance struct {
			AvailableCents string `json:"available_cents"`
			ReservedCents  string `json:"reserved_cents"`
		} `json:"balance"`
	}
	s.call("GET", "/v1/apps/df", nil, http.StatusOK, &after)
	if after.Balance.ReservedCents != "0" {
		t.Fatalf("reserved = %s, want nothing held once the payout settled", after.Balance.ReservedCents)
	}
	if after.Balance.AvailableCents != "2900" {
		t.Fatalf("available = %s, want 5000-2000-100", after.Balance.AvailableCents)
	}
	// And the wallet holds exactly what the app is owed — no more, because the
	// house's share has been collected.
	if got := s.sim.usdtOf(top); got.Cmp(ether(29)) != 0 {
		t.Fatalf("hot wallet holds %s, want exactly the app's 29 USDT", got)
	}

	s.audit()
}

// TestFeesAccumulateAsExcessAndSweep checks the fee model across a run of
// withdrawals: each charges its fee in the books only, so the house's share
// builds up in the wallet and is collected by one sweep rather than by a
// transfer per withdrawal (§24).
func TestFeesAccumulateAsExcessAndSweep(t *testing.T) {
	s := newStack(t)
	var app struct {
		Address string `json:"address"`
	}
	s.call("PUT", "/v1/apps/df", map[string]any{}, http.StatusCreated, &app)
	s.settle()

	var da struct {
		Address string `json:"address"`
	}
	s.call("POST", "/v1/apps/df/addresses", map[string]any{"ref": "cust-1"}, http.StatusCreated, &da)
	s.sim.deposit(common.HexToAddress(da.Address), ether(500))
	s.settle()

	dest := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	collector := common.HexToAddress("0x00000000000000000000000000000000000000fe")
	for i := range 5 {
		s.call("POST", "/v1/apps/df/withdrawals", map[string]any{
			"destination": dest.Hex(), "amount_cents": "500",
		}, http.StatusCreated, nil)
		s.settle()

		// Each fee is collected as excess once it clears the sweep threshold,
		// so the collector grows in step — but by a sweep, not by a second leg
		// of the withdrawal.
		want := ether(int64(i) + 1)
		if got := s.sim.usdtOf(collector); got.Cmp(want) != 0 {
			t.Fatalf("after withdrawal %d the collector holds %s, want %s", i+1, got, want)
		}
		var view struct {
			Balance struct {
				ReservedCents string `json:"reserved_cents"`
			} `json:"balance"`
		}
		s.call("GET", "/v1/apps/df", nil, http.StatusOK, &view)
		if view.Balance.ReservedCents != "0" {
			t.Fatalf("after withdrawal %d: reserved=%s", i+1, view.Balance.ReservedCents)
		}
	}

	if got := s.sim.usdtOf(dest); got.Cmp(ether(25)) != 0 {
		t.Fatalf("destination holds %s, want 5 payouts of 5", got)
	}
	s.audit()
}

// TestSecondDepositOnAnActiveWalletSkipsActivation shows the activation prefix
// being paid once per wallet, not once per deposit.
func TestSecondDepositOnAnActiveWalletSkipsActivation(t *testing.T) {
	s := newStack(t)
	s.call("PUT", "/v1/apps/df", map[string]any{}, http.StatusCreated, nil)
	s.settle()

	var da struct {
		Address string `json:"address"`
	}
	s.call("POST", "/v1/apps/df/addresses", map[string]any{"ref": "cust-1"}, http.StatusCreated, &da)
	deposit := common.HexToAddress(da.Address)

	s.sim.deposit(deposit, ether(10))
	s.settle()

	before := s.sim.head
	s.sim.deposit(deposit, ether(10))
	s.settle()
	// The first drain needed fund + approve + sweep; the second only a sweep, so
	// it settles in far fewer blocks.
	if blocks := s.sim.head - before; blocks > 4 {
		t.Fatalf("second drain took %d blocks; activation was probably repeated", blocks)
	}

	var app struct {
		Balance struct {
			AvailableCents string `json:"available_cents"`
		} `json:"balance"`
	}
	s.call("GET", "/v1/apps/df", nil, http.StatusOK, &app)
	if app.Balance.AvailableCents != "2000" {
		t.Fatalf("available = %s, want both deposits", app.Balance.AvailableCents)
	}
	s.audit()
}
