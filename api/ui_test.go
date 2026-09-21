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
		addrs: &fakeAddrs{m: map[common.Address]store.WalletID{}},
		sync:  &fakeSync{},
	}
	f.srv = New(st, ring, f.addrs, f.sync, Options{
		Notify: func() { f.notify++ },
		UI:     true,
	})
	f.mux = http.NewServeMux()
	f.srv.Routes(f.mux)
	return f
}

// Off by default is the security boundary, not a preference: this listener has
// no authentication, so a dashboard that appeared without being asked for would
// hand every wallet to whoever could reach the port (§29).
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
	var hot walletView
	f.json(f.do("PUT", "/v1/wallets/"+"hot", createWalletBody{Prewarm: true}),
		http.StatusCreated, &hot)
	f.credit("hot", 1000)
	f.proxy("cust-1", hot.Address)

	var state struct {
		Service struct {
			Version      string `json:"version"`
			MaxLagBlocks uint64 `json:"max_lag_blocks"`
			Lagging      bool   `json:"lagging"`
		} `json:"service"`
		Wallets []struct {
			Ref       string `json:"ref"`
			Address   string `json:"address"`
			DrainTo   string `json:"drain_to"`
			Balance   string `json:"balance"`
			Committed string `json:"committed"`
			Busy      bool   `json:"busy"`
		} `json:"wallets"`
		Flows []struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
			Ref   string `json:"ref"`
		} `json:"flows"`
	}
	f.json(f.do("GET", "/ui/state", nil), http.StatusOK, &state)

	if state.Service.Version != buildinfo.Version {
		t.Errorf("version = %q, want the build stamp %q", state.Service.Version, buildinfo.Version)
	}
	if len(state.Wallets) != 2 {
		t.Fatalf("wallets = %+v, want both", state.Wallets)
	}
	byRef := map[string]int{}
	for i, w := range state.Wallets {
		byRef[w.Ref] = i
	}
	h := state.Wallets[byRef["hot"]]
	if h.Address == "" || h.Balance != "1000" || h.DrainTo != "" {
		t.Errorf("hot wallet = %+v, want custody and no drain target", h)
	}
	p := state.Wallets[byRef["cust-1"]]
	if p.DrainTo != hot.Address {
		t.Errorf("proxy drain_to = %q, want %s", p.DrainTo, hot.Address)
	}
	// The treasury asked to be pre-warmed, so there is real work to show.
	if len(state.Flows) == 0 {
		t.Error("no flows: the operator view is blind to work in flight")
	}
	for _, fl := range state.Flows {
		if fl.Ref == "" {
			t.Errorf("flow %+v has no ref: an operator cannot tell which wallet it is", fl)
		}
	}
}

// The dashboard is additive. Turning it on must not change a single byte of
// what a caller sees, or the contract depends on an operator's config.
func TestDashboardDoesNotChangeTheCallerContract(t *testing.T) {
	paths := []string{"/v1/wallets/hot", "/v1/deposits", "/v1/withdrawals"}

	body := func(f *fixture) map[string]string {
		f.wallet("hot")
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
	for _, key := range []string{"wallets", "flows"} {
		if got := string(raw[key]); got != "[]" {
			t.Errorf("%s = %s on a fresh service, want an empty array", key, got)
		}
	}
}

// The master pays for every transfer and signs everything. It is not derivable
// from anything else on the page and is what an operator actually goes looking
// for, so the read model carries it rather than leaving it to the logs.
func TestUIStateNamesTheMaster(t *testing.T) {
	f := uiFixture(t)

	var state struct {
		Service struct {
			Master string `json:"master"`
		} `json:"service"`
	}
	f.json(f.do("GET", "/ui/state", nil), http.StatusOK, &state)

	if !common.IsHexAddress(state.Service.Master) {
		t.Fatalf("master = %q, want the gas-paying address", state.Service.Master)
	}
}
