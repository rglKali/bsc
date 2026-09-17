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
	"bsc/buildinfo"
	"bsc/engine"
	"bsc/keys"
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

	masterAddr common.Address
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

	snd, err := sender.New(st, sim, ring, sender.Options{
		ChainID: 56, Token: usdt.MainnetAddress,
	})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	addrs := watcher.NewAddrSet()
	wat := watcher.New(st, sim, addrs, watcher.Options{
		StartBlock: 1, BackfillBatch: 50,
		DrainThreshold: ether(1),
		Master:         master.Address, Token: usdt.MainnetAddress,
		Notify: snd.Notify,
	})
	srv := api.New(st, ring, addrs, wat, api.Options{
		// The dashboard is mounted here so its read model is exercised against
		// real money moving, not just against a fixture.
		UI: true,
	})
	mux := http.NewServeMux()
	srv.Routes(mux)

	s := &stack{
		t: t, st: st, sim: sim, snd: snd, wat: wat, mux: mux, addrs: addrs,
		cfg:        engine.Config{DrainThreshold: ether(1)},
		masterAddr: master.Address,
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

// wallet creates one and returns its address.
func (s *stack) wallet(ref, drainTo string) common.Address {
	s.t.Helper()
	var out struct {
		Address string `json:"address"`
	}
	body := map[string]any{}
	if drainTo != "" {
		body["drain_to"] = drainTo
	}
	s.call("PUT", "/v1/wallets/"+ref, body, http.StatusCreated, &out)
	return common.HexToAddress(out.Address)
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

type depositsPage struct {
	Deposits []struct {
		Amount  string `json:"amount"`
		Status  string `json:"status"`
		Wallet  string `json:"wallet"`
		TxHash  string `json:"tx_hash"`
		SweptBy string `json:"swept_by"`
		SweptTx string `json:"swept_tx"`
	} `json:"deposits"`
	Cursor string `json:"cursor"`
}

// TestFullMoneyLifecycle walks a deposit all the way to a payout through every
// package: API → store → work rules → sender → chain → watcher → settlement.
func TestFullMoneyLifecycle(t *testing.T) {
	s := newStack(t)

	// 1. A hot wallet: nothing drains out of it, and payouts come from it.
	hot := s.wallet("hot", "")
	s.settle()
	s.audit()

	// 2. A forwarding wallet for one user, pointed at the hot wallet.
	user := s.wallet("cust-1", hot.Hex())
	s.settle()

	// 3. The user sends 50 USDT to it.
	s.sim.deposit(user, ether(50))
	s.settle()

	// The money forwarded itself, and both ends are on the feed.
	var feed depositsPage
	s.call("GET", "/v1/deposits", nil, http.StatusOK, &feed)
	if len(feed.Deposits) != 2 {
		t.Fatalf("deposits = %+v, want the arrival and the forwarded hop", feed.Deposits)
	}
	var arrival, landed int
	for i, d := range feed.Deposits {
		switch d.Wallet {
		case "cust-1":
			arrival = i
		case "hot":
			landed = i
		}
	}
	if d := feed.Deposits[arrival]; d.Status != "forwarded" || d.Amount != ether(50).String() || d.SweptBy == "" {
		t.Fatalf("arrival = %+v, want it forwarded and linked to its debit", d)
	}
	if d := feed.Deposits[landed]; d.Status != "received" || d.Amount != ether(50).String() {
		t.Fatalf("landing = %+v, want it received on the hot wallet", d)
	}
	// The two ends of one movement are pairable by the debit's transfer.
	if feed.Deposits[arrival].SweptTx != feed.Deposits[landed].TxHash {
		t.Fatalf("swept_tx %s does not match the landing tx %s",
			feed.Deposits[arrival].SweptTx, feed.Deposits[landed].TxHash)
	}

	if got := s.sim.usdtOf(hot); got.Cmp(ether(50)) != 0 {
		t.Fatalf("hot wallet holds %s on-chain, want 50 USDT", got)
	}
	if got := s.sim.usdtOf(user); got.Sign() != 0 {
		t.Fatalf("forwarding wallet still holds %s", got)
	}
	s.audit()

	// 4. A payout from the hot wallet, in the chain's own units.
	dest := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	var created struct {
		Payout struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			Status string `json:"status"`
			Amount string `json:"amount"`
		} `json:"payout"`
	}
	s.call("POST", "/v1/wallets/hot/withdrawals", map[string]any{
		"to": dest.Hex(), "amount": ether(20).String(),
	}, http.StatusCreated, &created)
	wd := created.Payout
	if wd.Status != "pending" || wd.Amount != ether(20).String() || wd.Reason != "payout" {
		t.Fatalf("payout = %+v", wd)
	}

	// While it is pending the wallet has promised it: available is reduced,
	// custody is not (§39).
	var w struct {
		Balance   string `json:"balance"`
		Committed string `json:"committed"`
		Available string `json:"available"`
	}
	s.call("GET", "/v1/wallets/hot", nil, http.StatusOK, &w)
	if w.Balance != ether(50).String() || w.Committed != ether(20).String() ||
		w.Available != ether(30).String() {
		t.Fatalf("wallet before settlement = %+v", w)
	}

	s.settle()

	var settled struct {
		Status string `json:"status"`
		TxHash string `json:"tx_hash"`
	}
	s.call("GET", "/v1/withdrawals/"+wd.ID, nil, http.StatusOK, &settled)
	if settled.Status != "confirmed" || settled.TxHash == "" {
		t.Fatalf("withdrawal = %+v", settled)
	}
	if got := s.sim.usdtOf(dest); got.Cmp(ether(20)) != 0 {
		t.Fatalf("destination holds %s, want 20 USDT", got)
	}
	// Exactly the payout left, and nothing was withheld: bsc charges no fee.
	if got := s.sim.usdtOf(hot); got.Cmp(ether(30)) != 0 {
		t.Fatalf("hot wallet holds %s, want 30 USDT — nothing is kept back", got)
	}

	s.call("GET", "/v1/wallets/hot", nil, http.StatusOK, &w)
	if w.Balance != ether(30).String() || w.Committed != "0" {
		t.Fatalf("wallet after settlement = %+v", w)
	}
	s.audit()
}

// A chain of forwarding wallets is the shape the old two-level design could not
// express: A forwards to B, which forwards to C, and each hop is one transfer
// the master pays for (§32).
func TestFundsForwardAlongAChain(t *testing.T) {
	s := newStack(t)
	vault := s.wallet("vault", "")
	middle := s.wallet("middle", vault.Hex())
	leaf := s.wallet("leaf", middle.Hex())
	s.settle()

	s.sim.deposit(leaf, ether(40))
	s.settle()

	if got := s.sim.usdtOf(vault); got.Cmp(ether(40)) != 0 {
		t.Fatalf("vault holds %s, want the whole 40 USDT", got)
	}
	for name, at := range map[string]common.Address{"leaf": leaf, "middle": middle} {
		if got := s.sim.usdtOf(at); got.Sign() != 0 {
			t.Fatalf("%s still holds %s", name, got)
		}
	}
	s.audit()
}

// Below the threshold, forwarding costs more than it moves — so the money waits
// on the wallet and leaves once the total is worth a transaction. It is
// recorded either way.
func TestSmallDepositsWaitThenForwardTogether(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	user := s.wallet("cust-1", hot.Hex())
	s.settle()

	half := new(big.Int).Div(ether(1), big.NewInt(2))
	s.sim.deposit(user, half)
	s.settle()

	if got := s.sim.usdtOf(user); got.Cmp(half) != 0 {
		t.Fatalf("sub-threshold amount moved: wallet holds %s", got)
	}
	var feed depositsPage
	s.call("GET", "/v1/wallets/cust-1/deposits", nil, http.StatusOK, &feed)
	if len(feed.Deposits) != 1 || feed.Deposits[0].Status != "received" {
		t.Fatalf("deposits = %+v, want one recorded and waiting", feed.Deposits)
	}

	// A second half tips it over, and both leave together.
	s.sim.deposit(user, half)
	s.settle()

	if got := s.sim.usdtOf(hot); got.Cmp(ether(1)) != 0 {
		t.Fatalf("hot wallet holds %s, want the aggregated 1 USDT", got)
	}
	s.call("GET", "/v1/wallets/cust-1/deposits", nil, http.StatusOK, &feed)
	if len(feed.Deposits) != 2 {
		t.Fatalf("deposits = %d, want both", len(feed.Deposits))
	}
	for _, d := range feed.Deposits {
		if d.Status != "forwarded" {
			t.Fatalf("deposit %+v was not forwarded", d)
		}
	}
	s.audit()
}

// A wallet is funded and approves once. The second deposit must skip straight to
// the transfer rather than paying for activation again.
func TestSecondDepositOnAnActiveWalletSkipsActivation(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	user := s.wallet("cust-1", hot.Hex())
	s.settle()

	s.sim.deposit(user, ether(10))
	s.settle()
	first := s.sim.height()

	s.sim.deposit(user, ether(10))
	s.settle()
	second := s.sim.height()

	if (second - first) >= (first) {
		t.Fatalf("the second deposit took %d blocks, the first %d — activation was repeated",
			second-first, first)
	}
	if got := s.sim.usdtOf(hot); got.Cmp(ether(20)) != 0 {
		t.Fatalf("hot wallet holds %s, want 20 USDT", got)
	}
	s.audit()
}

// Creating a wallet must spend nothing. An address book of users who register
// and never deposit would otherwise cost a funding transfer and an approve each,
// paid by the master, for money that never arrives (§41).
func TestCreatingWalletsSpendsNoGas(t *testing.T) {
	s := newStack(t)
	master := s.sim.bnbOf(s.masterAddr)

	for _, ref := range []string{"a", "b", "c", "d", "e"} {
		s.wallet(ref, "")
	}
	s.settle()

	if got := s.sim.bnbOf(s.masterAddr); got.Cmp(master) != 0 {
		t.Fatalf("master gas went from %s to %s creating five wallets that never received anything",
			master, got)
	}
	if !s.idle() {
		t.Fatal("creating wallets started work")
	}
}

// Refusing to overdraw is checked against custody, not against a ledger, and it
// is checked before anything is signed.
func TestOverdraftIsRefusedBeforeSigning(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	s.settle()
	s.sim.deposit(hot, ether(5))
	s.settle()

	dest := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	s.call("POST", "/v1/wallets/hot/withdrawals", map[string]any{
		"to": dest.Hex(), "amount": ether(10).String(),
	}, http.StatusUnprocessableEntity, nil)

	if !s.idle() {
		t.Fatal("a refused withdrawal started a flow")
	}
	if got := s.sim.usdtOf(dest); got.Sign() != 0 {
		t.Fatalf("destination received %s from a refused request", got)
	}
	s.audit()
}

// The operator view carries what the caller contract does not need, and it has
// to agree with the money that actually moved.
func TestDashboardTracksTheMoney(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	s.wallet("cust-1", hot.Hex())
	s.settle()
	s.sim.deposit(hot, ether(7))
	s.settle()

	var state struct {
		Service struct {
			Version string `json:"version"`
			Master  string `json:"master"`
		} `json:"service"`
		Wallets []struct {
			Ref      string `json:"ref"`
			Balance  string `json:"balance"`
			DrainTo  string `json:"drain_to"`
			Active   bool   `json:"active"`
			Deposits int    `json:"deposits"`
		} `json:"wallets"`
		Flows []json.RawMessage `json:"flows"`
	}
	s.call("GET", "/ui/state", nil, http.StatusOK, &state)

	if state.Service.Version != buildinfo.Version {
		t.Errorf("version = %q", state.Service.Version)
	}
	if !common.IsHexAddress(state.Service.Master) {
		t.Errorf("master = %q", state.Service.Master)
	}
	if len(state.Wallets) != 2 {
		t.Fatalf("wallets = %+v", state.Wallets)
	}
	for _, w := range state.Wallets {
		if w.Ref == "hot" {
			// Still inactive, and that is the point: a wallet that has only ever
			// *received* has never needed the master's allowance, so it has cost
			// nothing to activate (§41).
			if w.Balance != ether(7).String() || w.Deposits != 1 {
				t.Fatalf("hot = %+v, want the deposit recorded", w)
			}
			if w.Active {
				t.Fatalf("hot = %+v, want it still inactive — nothing has moved out of it", w)
			}
		}
		if w.Ref == "cust-1" && w.DrainTo != hot.Hex() {
			t.Fatalf("cust-1 drain_to = %q, want %s", w.DrainTo, hot.Hex())
		}
	}
	s.audit()
}

// A fee is syntax over a second debit: bsc routes it to the wallet that pays for
// gas and accepts it with the payout, but the two are ordinary independent
// transfers that settle on their own (§43).
func TestPayoutWithAFeeSettlesAsTwoDebits(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	s.settle()
	s.sim.deposit(hot, ether(100))
	s.settle()

	dest := common.HexToAddress("0x00000000000000000000000000000000000000dd")
	var created struct {
		Payout struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"payout"`
		Fee *struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			To     string `json:"to"`
			PartOf string `json:"part_of"`
		} `json:"fee"`
	}
	s.call("POST", "/v1/wallets/hot/withdrawals", map[string]any{
		"to": dest.Hex(), "amount": ether(20).String(), "fee": ether(1).String(),
	}, http.StatusCreated, &created)
	if created.Fee == nil || created.Fee.PartOf != created.Payout.ID {
		t.Fatalf("fee = %+v, want it linked to the payout", created.Fee)
	}

	s.settle()

	// Both landed, in full, at their own destinations.
	if got := s.sim.usdtOf(dest); got.Cmp(ether(20)) != 0 {
		t.Fatalf("destination holds %s, want 20", got)
	}
	master := common.HexToAddress(created.Fee.To)
	if got := s.sim.usdtOf(master); got.Cmp(ether(1)) != 0 {
		t.Fatalf("fee destination holds %s, want 1", got)
	}
	if got := s.sim.usdtOf(hot); got.Cmp(ether(79)) != 0 {
		t.Fatalf("wallet holds %s, want 79 — payout and fee both left", got)
	}

	// And both are on the settled feed, distinguishable by reason.
	var feed struct {
		Withdrawals []struct {
			Reason string `json:"reason"`
			Status string `json:"status"`
		} `json:"withdrawals"`
	}
	s.call("GET", "/v1/withdrawals", nil, http.StatusOK, &feed)
	reasons := map[string]int{}
	for _, wd := range feed.Withdrawals {
		if wd.Status != "confirmed" {
			t.Fatalf("the settled feed carries a %s debit", wd.Status)
		}
		reasons[wd.Reason]++
	}
	if reasons["payout"] != 1 || reasons["fee"] != 1 {
		t.Fatalf("feed reasons = %v, want one of each", reasons)
	}
	s.audit()
}

// A drain's amount is resolved by the sender from a balance nobody else saw, so
// it has to be carried to settlement or the debit records nothing. Asserting it
// end to end is the only place that gap shows: the engine test can set the
// field by hand and pass while the sender never writes it (§42, §46).
func TestDrainDebitRecordsWhatItActuallyMoved(t *testing.T) {
	s := newStack(t)
	hot := s.wallet("hot", "")
	user := s.wallet("cust-1", hot.Hex())
	s.settle()

	// Two deposits, so the swept figure is their sum rather than either one —
	// a debit that echoed a single deposit would pass a weaker assertion.
	s.sim.deposit(user, ether(3))
	s.sim.deposit(user, ether(4))
	s.settle()

	var feed struct {
		Withdrawals []struct {
			Reason string `json:"reason"`
			Amount string `json:"amount"`
			Wallet string `json:"wallet"`
		} `json:"withdrawals"`
	}
	s.call("GET", "/v1/withdrawals?reason=drain", nil, http.StatusOK, &feed)
	if len(feed.Withdrawals) != 1 {
		t.Fatalf("drain debits = %d, want 1", len(feed.Withdrawals))
	}
	got := feed.Withdrawals[0]
	if got.Wallet != "cust-1" {
		t.Fatalf("debit is on %q, want the forwarding wallet", got.Wallet)
	}
	if got.Amount != ether(7).String() {
		t.Fatalf("drain debit amount = %s, want the 7 it actually swept", got.Amount)
	}
	// And that is the figure the chain moved.
	if held := s.sim.usdtOf(hot); held.Cmp(ether(7)) != 0 {
		t.Fatalf("destination holds %s, want 7", held)
	}
	s.audit()
}
