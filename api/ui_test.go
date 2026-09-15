package api

import (
	"bsc/buildinfo"
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"bsc/keys"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// uiFixture is newFixture with the dashboard turned on, which is the only
// difference that matters here.
func uiFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ring, err := keys.New(bytes.Repeat([]byte{0x11}, 32))
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
		UI:         true,
	})
	f.mux = http.NewServeMux()
	f.srv.Routes(f.mux)
	return f
}

// Off by default is the security boundary, not a preference: this listener has
// no authentication, so a dashboard that appeared without being asked for would
// hand every app's money to whoever could reach the port (§29).
func TestDashboardIsAbsentUnlessEnabled(t *testing.T) {
	f := newFixture(t) // the default fixture, UI not set
	for _, path := range []string{"/ui/", "/ui/state", "/ui/index.html"} {
		if w := f.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d with the dashboard off, want 404", path, w.Code)
		}
	}
}

func TestDashboardMountsWhenEnabled(t *testing.T) {
	f := uiFixture(t)
	if w := f.do("GET", "/ui/", nil); w.Code != http.StatusOK {
		t.Fatalf("/ui/ status = %d", w.Code)
	}
	// /ui without the slash is the address a human types.
	if w := f.do("GET", "/ui", nil); w.Code != http.StatusMovedPermanently {
		t.Fatalf("/ui status = %d, want a redirect", w.Code)
	}
}

// The dashboard's read model is the operator's view and is deliberately
// everything the app contract refuses to carry (§27): wei, block heights, flow
// states, solvency. Keeping it outside /v1 is what lets both be true, so this
// asserts the split rather than trusting it.
func TestUIStateCarriesTheOperatorView(t *testing.T) {
	f := uiFixture(t)
	f.register("df")
	f.credit("df", 1000)
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)

	var state struct {
		Service struct {
			Version      string `json:"version"`
			MaxLagBlocks uint64 `json:"max_lag_blocks"`
			Lagging      bool   `json:"lagging"`
		} `json:"service"`
		Apps []struct {
			Slug       string      `json:"slug"`
			Address    string      `json:"address"`
			CustodyWei string      `json:"custody_wei"`
			Solvent    bool        `json:"solvent"`
			Addresses  int         `json:"addresses"`
			Balance    balanceView `json:"balance"`
		} `json:"apps"`
		Flows []struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"flows"`
	}
	f.json(f.do("GET", "/ui/state", nil), http.StatusOK, &state)

	if state.Service.Version != buildinfo.Version {
		t.Errorf("version = %q, want the build stamp %q", state.Service.Version, buildinfo.Version)
	}
	if len(state.Apps) != 1 || state.Apps[0].Slug != "df" {
		t.Fatalf("apps = %+v", state.Apps)
	}
	a := state.Apps[0]
	if a.Address == "" || a.CustodyWei == "" || !a.Solvent {
		t.Errorf("app view = %+v, want the chain-side truth alongside the ledger", a)
	}
	if a.Addresses != 1 {
		t.Errorf("addresses = %d, want the one just created", a.Addresses)
	}
	if a.Balance.AvailableCents != "1000" {
		t.Errorf("available = %s, want the ledger", a.Balance.AvailableCents)
	}
	// Registering an app pre-warms its wallet, so there is real work to show.
	if len(state.Flows) == 0 {
		t.Error("no flows: the operator view is blind to work in flight")
	}
}

// The dashboard is additive. Turning it on must not change a single byte of
// what an app sees, or the contract depends on an operator's config.
func TestDashboardDoesNotChangeTheAppContract(t *testing.T) {
	paths := []string{"/v1/apps/df", "/v1/apps/df/balance", "/v1/apps/df/deposits"}

	body := func(f *fixture) map[string]string {
		f.register("df")
		out := map[string]string{}
		for _, p := range paths {
			w := f.do("GET", p, nil)
			if w.Code != http.StatusOK {
				f.t.Fatalf("%s: status %d", p, w.Code)
			}
			// Two fixtures mean two databases, so the clock and the derived
			// address differ by construction. Everything else must not.
			var v map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &v); err == nil {
				delete(v, "created_at")
				delete(v, "address")
				norm, _ := json.Marshal(v)
				out[p] = string(norm)
				continue
			}
			out[p] = w.Body.String()
		}
		return out
	}

	off, on := body(newFixture(t)), body(uiFixture(t))
	for _, p := range paths {
		if off[p] != on[p] {
			t.Errorf("%s differs with the dashboard enabled:\n off: %s\n  on: %s", p, off[p], on[p])
		}
	}
}

// encoding/json renders a nil slice as null, and the page does
// state.flows.length the moment it arrives. A service with nothing registered
// and nothing in flight is the first thing anyone loads, so an empty dashboard
// has to be empty rather than a TypeError.
func TestUIStateNeverServesNullCollections(t *testing.T) {
	f := uiFixture(t)

	var raw map[string]json.RawMessage
	f.json(f.do("GET", "/ui/state", nil), http.StatusOK, &raw)
	for _, key := range []string{"apps", "flows"} {
		if got := string(raw[key]); got != "[]" {
			t.Errorf("%s = %s on a fresh service, want an empty array", key, got)
		}
	}
}

// The master pays for every transfer and signs everything, and the collector is
// where the house's money ends up. Neither is derivable from anything else on
// the page, and both are what an operator actually goes looking for — so the
// read model carries them rather than leaving them to the logs.
func TestUIStateNamesTheMasterAndCollector(t *testing.T) {
	f := uiFixture(t)

	var state struct {
		Service struct {
			Master    string `json:"master"`
			Collector string `json:"collector"`
		} `json:"service"`
	}
	f.json(f.do("GET", "/ui/state", nil), http.StatusOK, &state)

	if !common.IsHexAddress(state.Service.Master) {
		t.Fatalf("master = %q, want the gas-paying address", state.Service.Master)
	}
	// This fixture leaves the collector unset, which means the master collects.
	// Empty is how the page is told to say so, rather than repeating an address.
	if state.Service.Collector != "" {
		t.Fatalf("collector = %q, want empty when it is the master", state.Service.Collector)
	}
}
