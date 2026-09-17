package api

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bsc/keys"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// fakeAddrs records what the API registered for watching.
type fakeAddrs struct {
	mu sync.Mutex
	m  map[common.Address]uuid.UUID
}

func (f *fakeAddrs) Add(a common.Address, id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[a] = id
}

func (f *fakeAddrs) has(a common.Address) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[a]
	return ok
}

// fakeSync lets a test pretend the watcher is behind.
type fakeSync struct{ behind uint64 }

func (f *fakeSync) Behind() uint64 { return f.behind }

type fixture struct {
	t      *testing.T
	st     *store.Store
	srv    *Server
	mux    *http.ServeMux
	addrs  *fakeAddrs
	sync   *fakeSync
	notify int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	master := bytes.Repeat([]byte{0x11}, 32)
	ring, err := keys.New(master)
	if err != nil {
		t.Fatalf("keys.New: %v", err)
	}
	f := &fixture{
		t: t, st: st,
		addrs: &fakeAddrs{m: map[common.Address]uuid.UUID{}},
		sync:  &fakeSync{},
	}
	f.srv = New(st, ring, f.addrs, f.sync, Options{
		Notify: func() { f.notify++ },
	})
	f.mux = http.NewServeMux()
	f.srv.Routes(f.mux)
	return f
}

// do issues a request and returns the recorder.
func (f *fixture) do(method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// json decodes a response, asserting the status.
func (f *fixture) json(w *httptest.ResponseRecorder, want int, into any) {
	f.t.Helper()
	if w.Code != want {
		f.t.Fatalf("status = %d, want %d; body = %s", w.Code, want, w.Body.String())
	}
	if into != nil {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			f.t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
}

// register creates an app and returns its view.
// wallet creates a wallet and returns its view.
func (f *fixture) wallet(ref string) walletView {
	f.t.Helper()
	var out walletView
	f.json(f.do("PUT", "/v1/wallets/"+ref, createWalletBody{}), http.StatusCreated, &out)
	return out
}

// proxy creates a forwarding wallet pointed at drainTo.
func (f *fixture) proxy(ref, drainTo string) walletView {
	f.t.Helper()
	var out walletView
	f.json(f.do("PUT", "/v1/wallets/"+ref, createWalletBody{DrainTo: drainTo}),
		http.StatusCreated, &out)
	return out
}

// credit puts custody on a wallet, as an observed transfer would. Custody is
// the only balance there is now, so this is the whole of "give it money".
func (f *fixture) credit(ref string, v int64) {
	f.t.Helper()
	if err := f.st.Update(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByRef(ref)
		if err != nil || !ok {
			f.t.Fatalf("wallet %q: ok=%v err=%v", ref, ok, err)
		}
		_, err = tx.Credit(w.ID, big.NewInt(v))
		return err
	}); err != nil {
		f.t.Fatalf("credit: %v", err)
	}
}

func TestCreateWalletIsIdempotentOnRef(t *testing.T) {
	f := newFixture(t)
	first := f.wallet("cust-1")
	if first.Address == "" || first.Ref != "cust-1" {
		t.Fatalf("view = %+v", first)
	}
	if !f.addrs.has(common.HexToAddress(first.Address)) {
		t.Fatal("the new address was not registered for watching")
	}

	var again walletView
	f.json(f.do("PUT", "/v1/wallets/"+"cust-1", createWalletBody{}), http.StatusOK, &again)
	if again.Address != first.Address {
		t.Fatalf("ref remapped: %s -> %s", first.Address, again.Address)
	}

	// Exactly one wallet, and no flow at all: creating a wallet spends nothing.
	wallets, flows := f.counts()
	if wallets != 1 || flows != 0 {
		t.Fatalf("wallets=%d flows=%d, want 1 and 0", wallets, flows)
	}
}

// Creating a wallet must not spend anything. An address book of users who
// register and never deposit would otherwise cost one funding transfer and one
// approve each, paid by the master, for money that never arrives (§41).
func TestCreatingAWalletStartsNoWork(t *testing.T) {
	f := newFixture(t)
	before := f.notify
	for _, ref := range []string{"a", "b", "c"} {
		f.wallet(ref)
	}

	wallets, flows := f.counts()
	if wallets != 3 || flows != 0 {
		t.Fatalf("wallets=%d flows=%d, want 3 and 0", wallets, flows)
	}
	if f.notify != before {
		t.Fatal("the sender was nudged for work that does not exist")
	}

	// The wallet is inactive, which is what makes the first real flow start at
	// funding rather than skipping straight to the transfer.
	if err := f.st.View(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByRef("a")
		if err != nil || !ok {
			t.Fatalf("lookup: ok=%v err=%v", ok, err)
		}
		if w.Active {
			t.Fatal("a freshly created wallet is already active")
		}
		if !w.Idle() {
			t.Fatal("a freshly created wallet is already busy")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Prewarming is the opt-out, for a wallet you know will be used — a treasury
// you are about to pay out of, where three transactions of latency on the first
// payout is worse than one activation you were always going to pay for.
func TestPrewarmIsOptIn(t *testing.T) {
	f := newFixture(t)
	before := f.notify

	var w walletView
	f.json(f.do("PUT", "/v1/wallets/"+"treasury", createWalletBody{Prewarm: true}),
		http.StatusCreated, &w)

	wallets, flows := f.counts()
	if wallets != 1 || flows != 1 {
		t.Fatalf("wallets=%d flows=%d, want 1 and 1", wallets, flows)
	}
	if f.notify <= before {
		t.Fatal("the sender was not nudged for the prewarm")
	}
	if err := f.st.View(func(tx *store.Tx) error {
		return tx.EachFlow(func(fl store.Flow) error {
			if fl.Kind != store.FlowPrewarm {
				t.Fatalf("flow kind = %s, want prewarm", fl.Kind)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// counts is (wallets, live flows).
func (f *fixture) counts() (int, int) {
	f.t.Helper()
	var wallets, flows int
	if err := f.st.View(func(tx *store.Tx) error {
		if err := tx.EachWallet(func(store.Wallet) error { wallets++; return nil }); err != nil {
			return err
		}
		return tx.EachFlow(func(store.Flow) error { flows++; return nil })
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return wallets, flows
}

// Idempotent on ref, but only for the same wallet: handing back one configured
// differently from what was asked for would be a silent disagreement about
// where its money goes.
func TestCreateWalletConflictsOnADifferentDrainTo(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)

	w := f.do("PUT", "/v1/wallets/"+"cust-1", createWalletBody{})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
}

func TestCreateWalletRejectsABadRef(t *testing.T) {
	f := newFixture(t)
	// Escaped, so the handler actually sees the ref and rejects it rather than
	// the router refusing a malformed path first.
	for _, ref := range []string{"Bad", "with space", "nul\x00ref", strings.Repeat("x", 200)} {
		path := "/v1/wallets/" + url.PathEscape(ref)
		if w := f.do("PUT", path, createWalletBody{}); w.Code != http.StatusBadRequest {
			t.Fatalf("ref %q: status %d, want 400", ref, w.Code)
		}
	}
	// A ref cannot be empty or contain a slash, and neither is expressible as
	// one path segment — the router refuses both before a handler sees them,
	// which is the same answer by a shorter road.
	for _, path := range []string{"/v1/wallets/", "/v1/wallets/a/b"} {
		if w := f.do("PUT", path, createWalletBody{}); w.Code < 400 {
			t.Fatalf("%s: status %d, want a refusal", path, w.Code)
		}
	}
}

// Refs are a single global namespace now, so the caller has to namespace them
// itself — which is why ':' is allowed in one (§37).
func TestRefsAreGlobalAndMayBeNamespaced(t *testing.T) {
	f := newFixture(t)
	a := f.wallet("acme:cust-1")
	b := f.wallet("globex:cust-1")
	if a.Address == b.Address {
		t.Fatal("two refs produced one address")
	}

	var list struct {
		Wallets []walletView `json:"wallets"`
	}
	f.json(f.do("GET", "/v1/wallets", nil), http.StatusOK, &list)
	if len(list.Wallets) != 2 {
		t.Fatalf("listing = %d wallets, want both", len(list.Wallets))
	}
}

func TestUnknownWalletIsNotFound(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/v1/wallets/nope", "/v1/wallets/nope/deposits", "/v1/wallets/nope/withdrawals",
	} {
		if w := f.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, w.Code)
		}
	}
}

func TestMalformedBodiesAreRejected(t *testing.T) {
	f := newFixture(t)

	r := httptest.NewRequest("PUT", "/v1/wallets/x", strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status %d", w.Code)
	}

	// A misspelled field must not be silently ignored — it usually means the
	// caller thinks it configured something it did not.
	r = httptest.NewRequest("PUT", "/v1/wallets/x", strings.NewReader(`{"drain_too":"x"}`))
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400", w.Code)
	}
}

// DrainTo is the one piece of configuration a wallet has, and retargeting is
// validated rather than trusted: a cycle would move the same money round and
// round, succeeding every hop (§35).
func TestPatchRetargetsAWallet(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	w := f.wallet("cust-1")

	var got walletView
	f.json(f.do("PATCH", "/v1/wallets/cust-1", patchWalletBody{DrainTo: ptr(hot.Address)}),
		http.StatusOK, &got)
	if got.DrainTo != hot.Address {
		t.Fatalf("drain_to = %q, want %s", got.DrainTo, hot.Address)
	}

	// And back to accumulating: an empty string is how that is said. A fresh
	// struct, because drain_to is omitempty and would keep the old value.
	var cleared walletView
	f.json(f.do("PATCH", "/v1/wallets/cust-1", patchWalletBody{DrainTo: ptr("")}),
		http.StatusOK, &cleared)
	if cleared.DrainTo != "" {
		t.Fatalf("drain_to = %q, want it cleared", cleared.DrainTo)
	}
	_ = w
}

func TestPatchRefusesADrainCycle(t *testing.T) {
	f := newFixture(t)
	a := f.wallet("a")
	b := f.proxy("b", a.Address)

	w := f.do("PATCH", "/v1/wallets/a", patchWalletBody{DrainTo: ptr(b.Address)})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "drain_cycle") {
		t.Fatalf("body = %s, want the cycle named", w.Body.String())
	}
}

func TestPatchRefusesSelfReference(t *testing.T) {
	f := newFixture(t)
	a := f.wallet("a")
	w := f.do("PATCH", "/v1/wallets/a", patchWalletBody{DrainTo: ptr(a.Address)})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

// A wallet with money already promised must not start forwarding: the drain
// would race the payout for the same funds.
func TestPatchRefusesToForwardWhileAPayoutIsPending(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.credit("hot", 1_000)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: addrHex(0xDD), Amount: "100",
	}), http.StatusCreated, nil)

	w := f.do("PATCH", "/v1/wallets/hot", patchWalletBody{DrainTo: ptr(addrHex(0xEE))})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	_ = hot
}

func TestPatchPauses(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	yes := true

	var got walletView
	f.json(f.do("PATCH", "/v1/wallets/hot", patchWalletBody{Paused: &yes}), http.StatusOK, &got)
	if !got.Paused {
		t.Fatal("pause was not applied")
	}
}

func TestWalletListPaginates(t *testing.T) {
	f := newFixture(t)
	for _, ref := range []string{"a", "b", "c"} {
		f.wallet(ref)
	}
	var page struct {
		Wallets []walletView `json:"wallets"`
		After   string       `json:"after"`
	}
	f.json(f.do("GET", "/v1/wallets?limit=2", nil), http.StatusOK, &page)
	if len(page.Wallets) != 2 || page.After != "b" {
		t.Fatalf("first page = %+v", page)
	}
	f.json(f.do("GET", "/v1/wallets?limit=2&after="+page.After, nil), http.StatusOK, &page)
	if len(page.Wallets) != 1 || page.Wallets[0].Ref != "c" {
		t.Fatalf("second page = %+v", page.Wallets)
	}
}

// The balance view is custody minus what is already promised. There is no
// ledger behind it: what the chain says the address holds is the whole truth
// the service has (§36, §39).
func TestWalletViewReportsCustodyAndCommitment(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: addrHex(0xDD), Amount: "300",
	}), http.StatusCreated, nil)

	var got walletView
	f.json(f.do("GET", "/v1/wallets/hot", nil), http.StatusOK, &got)
	if got.Balance != "1000" || got.Committed != "300" || got.Available != "700" {
		t.Fatalf("view = %+v, want 1000/300/700", got)
	}
}

func ptr[T any](v T) *T { return &v }

func addrHex(b byte) string { return hexAddr(b).Hex() }

func hexAddr(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}
