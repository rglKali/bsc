package api

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bsc/keys"
	"bsc/money"
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
		DefaultFee: store.FeePolicy{Flat: 1},
		Notify:     func() { f.notify++ },
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
func (f *fixture) register(slug string) appView {
	f.t.Helper()
	var out appView
	f.json(f.do("PUT", "/v1/apps/"+slug, appBody{}), http.StatusCreated, &out)
	return out
}

// credit gives an app a spendable ledger balance and the custody behind it, as
// a settled drain would. Cents and wei are one-to-one here, which is the truth
// for a two-decimal token and keeps the figures readable as both.
func (f *fixture) credit(slug string, v int64) {
	f.t.Helper()
	if err := f.st.Update(func(tx *store.Tx) error {
		a, _, err := tx.App(slug)
		if err != nil {
			return err
		}
		if _, err := tx.Credit(a.Wallet, big.NewInt(v)); err != nil {
			return err
		}
		_, err = tx.CreditLedger(slug, money.Cents(v))
		return err
	}); err != nil {
		f.t.Fatalf("credit: %v", err)
	}
}

func TestRegisterAppIsIdempotent(t *testing.T) {
	// Registration is public and idempotent by design: calling it on every boot
	// is the intended usage.
	f := newFixture(t)
	first := f.register("df")
	if first.Address == "" || first.Slug != "df" {
		t.Fatalf("view = %+v", first)
	}
	if !f.addrs.has(common.HexToAddress(first.Address)) {
		t.Fatal("the new top-level address was not registered for watching")
	}

	var second appView
	f.json(f.do("PUT", "/v1/apps/df", appBody{}), http.StatusOK, &second)
	if second.Address != first.Address {
		t.Fatalf("re-registration changed the address: %s -> %s", first.Address, second.Address)
	}

	// Exactly one wallet and one prewarm flow, not two.
	var wallets, flows int
	if err := f.st.View(func(tx *store.Tx) error {
		if err := tx.EachWallet(func(store.Wallet) error { wallets++; return nil }); err != nil {
			return err
		}
		return tx.EachFlow(func(fl store.Flow) error {
			flows++
			if fl.Kind != store.FlowPrewarm {
				t.Fatalf("flow kind = %s, want prewarm", fl.Kind)
			}
			return nil
		})
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	if wallets != 1 || flows != 1 {
		t.Fatalf("wallets=%d flows=%d, want 1 and 1", wallets, flows)
	}
}

func TestRegisterRejectsABadSlug(t *testing.T) {
	f := newFixture(t)
	for _, slug := range []string{"Bad", "with%20space", "a:b:c:" + strings.Repeat("x", 70)} {
		if w := f.do("PUT", "/v1/apps/"+slug, appBody{}); w.Code != http.StatusBadRequest {
			t.Fatalf("slug %q: status %d, want 400", slug, w.Code)
		}
	}
}

func TestAppConfiguresItsOwnFee(t *testing.T) {
	// No operator floor: the fee is a charge on the app's own users.
	f := newFixture(t)
	f.register("df")

	var got appView
	f.json(f.do("PUT", "/v1/apps/df", appBody{
		Fee: &feeBody{FlatCents: "200", MinCents: "10"},
	}), http.StatusOK, &got)
	if got.Fee.FlatCents != "200" || got.Fee.MinCents != "10" {
		t.Fatalf("fee = %+v", got.Fee)
	}

	// Zero is a legitimate policy: some apps do not charge at all.
	f.json(f.do("PUT", "/v1/apps/df", appBody{Fee: &feeBody{FlatCents: "0"}}), http.StatusOK, &got)
	if got.Fee.FlatCents != "0" {
		t.Fatalf("fee = %+v, want a free policy", got.Fee)
	}
}

func TestUnknownAppIsNotFound(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/v1/apps/nope", "/v1/apps/nope/balance", "/v1/apps/nope/deposits",
		"/v1/apps/nope/withdrawals", "/v1/apps/nope/addresses",
	} {
		if w := f.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, w.Code)
		}
	}
}

func TestMalformedBodiesAreRejected(t *testing.T) {
	f := newFixture(t)
	f.register("df")

	r := httptest.NewRequest("POST", "/v1/apps/df/addresses", strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status %d", w.Code)
	}

	// A misspelled field must not be silently ignored — it usually means the
	// caller thinks it configured something it did not.
	r = httptest.NewRequest("POST", "/v1/apps/df/addresses", strings.NewReader(`{"reff":"x"}`))
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400", w.Code)
	}
}

func TestDepositAddressIsIdempotentOnRef(t *testing.T) {
	f := newFixture(t)
	f.register("df")

	var first depositAddressView
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}),
		http.StatusCreated, &first)
	if first.Address == "" {
		t.Fatal("no address returned")
	}
	if !f.addrs.has(common.HexToAddress(first.Address)) {
		t.Fatal("new deposit address not registered for watching")
	}

	var again depositAddressView
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}),
		http.StatusOK, &again)
	if again.Address != first.Address {
		t.Fatalf("ref remapped: %s -> %s", first.Address, again.Address)
	}

	var fetched depositAddressView
	f.json(f.do("GET", "/v1/apps/df/addresses/cust-1", nil), http.StatusOK, &fetched)
	if fetched.Address != first.Address {
		t.Fatalf("lookup = %s, want %s", fetched.Address, first.Address)
	}
	if w := f.do("GET", "/v1/apps/df/addresses/nobody", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown ref: status %d", w.Code)
	}
}

func TestDepositAddressesAreScopedPerApp(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.register("lkr:acme")

	var a, b depositAddressView
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, &a)
	f.json(f.do("POST", "/v1/apps/lkr:acme/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, &b)
	if a.Address == b.Address {
		t.Fatal("the same ref under two apps produced one address")
	}

	var list struct {
		Addresses []depositAddressView `json:"addresses"`
	}
	f.json(f.do("GET", "/v1/apps/df/addresses", nil), http.StatusOK, &list)
	if len(list.Addresses) != 1 || list.Addresses[0].Address != a.Address {
		t.Fatalf("listing leaked across apps: %+v", list.Addresses)
	}
}

func TestDepositAddressListPaginates(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	for _, ref := range []string{"a", "b", "c"} {
		f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: ref}), http.StatusCreated, nil)
	}
	var page struct {
		Addresses []depositAddressView `json:"addresses"`
		After     string               `json:"after"`
	}
	f.json(f.do("GET", "/v1/apps/df/addresses?limit=2", nil), http.StatusOK, &page)
	if len(page.Addresses) != 2 || page.After != "b" {
		t.Fatalf("first page = %+v", page)
	}
	f.json(f.do("GET", "/v1/apps/df/addresses?limit=2&after="+page.After, nil), http.StatusOK, &page)
	if len(page.Addresses) != 1 || page.Addresses[0].Ref != "c" {
		t.Fatalf("second page = %+v", page.Addresses)
	}
}
