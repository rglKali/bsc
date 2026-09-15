package api

import (
	"math/big"
	"net/http"
	"time"

	"bsc/buildinfo"
	"bsc/money"
	"bsc/store"
	"bsc/ui"

	"github.com/ethereum/go-ethereum/common"
)

// The dashboard's read model.
//
// This is the operator's view, and it is deliberately the opposite of the app
// contract. §27 keeps wei, block heights and flow states away from apps because
// an app has no use for them and would come to depend on them. An operator has
// nothing *but* use for them: "which flow is wedged", "is that wallet short",
// "how far behind are we" are the only questions worth opening a dashboard for.
//
// Keeping it at /ui/state rather than under /v1 is what lets both be true at
// once. The app contract stays cents-and-a-hash, this stays the truth, and
// api/contract_test.go walks only the former.
type uiState struct {
	Service uiService `json:"service"`
	Apps    []uiApp   `json:"apps"`
	Flows   []uiFlow  `json:"flows"`
	Now     string    `json:"now"`
}

type uiService struct {
	Version      string `json:"version"`
	ChainID      uint64 `json:"chain_id"`
	Token        string `json:"token"`
	Decimals     uint8  `json:"decimals"`
	CentWei      string `json:"cent_wei"`
	Block        uint64 `json:"block"`
	BlocksBehind uint64 `json:"blocks_behind"`
	MaxLagBlocks uint64 `json:"max_lag_blocks"`

	// The two addresses an operator ends up looking for: the one that pays for
	// everything, and the one the house's money lands in. Neither is derivable
	// from anything else on the page.
	Master    string `json:"master"`
	Collector string `json:"collector"`
	// Lagging is the one derived flag worth serving rather than recomputing in
	// the page: it is the exact condition that starts refusing withdrawals.
	Lagging bool `json:"lagging"`
}

type uiApp struct {
	Slug    string      `json:"slug"`
	Address string      `json:"address"`
	Paused  bool        `json:"paused"`
	Fee     feeView     `json:"fee"`
	Balance balanceView `json:"balance"`

	// The chain-side truth behind that balance. CustodyWei is what the hot
	// wallet holds; ExcessWei is custody less the ledger — the house's, and the
	// number that must never go negative (§25).
	CustodyWei string `json:"custody_wei"`
	ExcessWei  string `json:"excess_wei"`
	Solvent    bool   `json:"solvent"`

	Addresses int `json:"addresses"`
}

type uiFlow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	App     string `json:"app"`
	Wallet  string `json:"wallet"`
	Address string `json:"address"`
	TxHash  string `json:"tx_hash"`
	Attempt uint32 `json:"attempt"`
	// RetryAfter is the wallet's backoff, not the flow's: a failed attempt
	// stamps the wallet, which is what the work rules gate on.
	RetryAfter string `json:"retry_after,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// mountUI wires the dashboard and its read model. Both are absent unless the
// operator turned them on; see config.UIEnabled for why that default matters.
func (s *Server) mountUI(mux *http.ServeMux) {
	page, err := ui.Handler()
	if err != nil {
		s.log.Error("could not mount the dashboard", "error", err)
		return
	}
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", page))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.HandleFunc("GET /ui/state", s.handle(s.getUIState))

	s.log.Warn("dashboard enabled",
		"path", "/ui/",
		"note", "this listener has no authentication; anyone who can reach it can move every app's money")
}

func (s *Server) getUIState(w http.ResponseWriter, _ *http.Request) error {
	// Both collections are initialised rather than left nil: encoding/json
	// renders a nil slice as null, and the page does state.flows.length on
	// arrival. An empty dashboard must be empty, not a TypeError.
	out := uiState{Apps: []uiApp{}, Flows: []uiFlow{}}
	if err := s.store.View(func(tx *store.Tx) error {
		behind := s.sync.Behind()
		out.Service = uiService{
			Version:      buildinfo.Version,
			MaxLagBlocks: s.opts.MaxLagBlocks,
			BlocksBehind: behind,
			Lagging:      behind > s.opts.MaxLagBlocks,
			Collector:    addrStr(s.opts.FeeCollector),
		}
		// The master is derived, not stored: it is the master secret used
		// directly rather than a wallet with an id.
		if master, err := s.ring.Master(); err == nil {
			out.Service.Master = master.Address.Hex()
		}
		if m, ok, err := tx.Meta(); err != nil {
			return err
		} else if ok {
			out.Service.ChainID = m.ChainID
			out.Service.Token = m.Token.Hex()
			out.Service.Decimals = m.Decimals
			if scale, err := money.NewScale(m.Decimals); err == nil {
				out.Service.CentWei = scale.CentWei().String()
			}
		}
		if c, known, err := tx.Cursor(); err != nil {
			return err
		} else if known {
			out.Service.Block = c
		}

		apps, err := tx.Apps()
		if err != nil {
			return err
		}
		scale, haveScale := scaleOf(tx)
		out.Apps = make([]uiApp, 0, len(apps))
		for _, a := range apps {
			view := uiApp{
				Slug: a.Slug, Paused: a.Paused,
				Fee: feeView{
					FlatCents: a.Fee.Flat.String(), MinCents: a.Fee.Min.String(),
					BPS: a.Fee.BPS, MaxFeeCents: a.Fee.MaxFee.String(),
				},
				CustodyWei: "0", ExcessWei: "0", Solvent: true,
			}
			top, found, err := tx.Wallet(a.Wallet)
			if err != nil {
				return err
			}
			if found {
				view.Address = top.Address.Hex()
				view.CustodyWei = orZeroStr(top.Balance)
				if haveScale {
					excess := scale.Excess(top.Balance, a.Ledger)
					view.ExcessWei = excess.String()
					view.Solvent = excess.Sign() >= 0
				}
			}
			pending, err := pendingBalance(tx, a.Slug)
			if err != nil {
				return err
			}
			view.Balance = balanceView{
				AvailableCents: a.Spendable().String(),
				ReservedCents:  a.Reserved.String(),
				PendingCents:   pending.String(),
				TotalCents:     (a.Spendable() + a.Reserved + pending).String(),
			}
			wallets, err := tx.DepositWallets(a.Slug, "", 0)
			if err != nil {
				return err
			}
			view.Addresses = len(wallets)
			out.Apps = append(out.Apps, view)
		}

		// Live flows are the whole point of the operator view: this is the work
		// the service believes it owes, and a wedged one shows up here long
		// before it shows up in a balance.
		return tx.EachFlow(func(f store.Flow) error {
			view := uiFlow{
				ID: f.ID.String(), Kind: f.Kind.String(), State: f.State.String(),
				App: f.App, Wallet: f.Wallet.String(), TxHash: hashStr(f.Tx),
				Attempt: f.Attempt, Error: f.Error, CreatedAt: stamp(f.CreatedAt),
			}
			if wl, found, err := tx.Wallet(f.Wallet); err == nil && found {
				view.Address = wl.Address.Hex()
				if !wl.RetryAfter.IsZero() {
					view.RetryAfter = stamp(wl.RetryAfter)
				}
			}
			out.Flows = append(out.Flows, view)
			return nil
		})
	}); err != nil {
		return err
	}
	out.Now = stamp(time.Now().UTC())
	writeJSON(w, http.StatusOK, out)
	return nil
}

// scaleOf reads the token scale this database is denominated in. A database
// that has not met its chain yet has none, and the dashboard simply shows no
// excess rather than inventing one.
func scaleOf(tx *store.Tx) (money.Scale, bool) {
	m, ok, err := tx.Meta()
	if err != nil || !ok {
		return money.Scale{}, false
	}
	scale, err := money.NewScale(m.Decimals)
	if err != nil {
		return money.Scale{}, false
	}
	return scale, true
}

func orZeroStr(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// addrStr renders an address, or empty for the zero value — which on the
// dashboard means "not set" rather than "address zero".
func addrStr(a common.Address) string {
	if a == (common.Address{}) {
		return ""
	}
	return a.Hex()
}
