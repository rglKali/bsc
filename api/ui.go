package api

import (
	"math/big"
	"net/http"
	"time"

	"bsc/buildinfo"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
)

// The dashboard's read model.
//
// This is the operator's view, and it is deliberately the opposite of the
// caller contract. §27 keeps gas, block heights and flow states away from
// callers because a caller has no use for them and would come to depend on
// them. An operator has nothing *but* use for them: "which flow is wedged",
// "is that wallet short", "how far behind are we" are the only questions worth
// opening a dashboard for.
//
// Keeping it at /ui/state rather than under /v1 is what lets both be true at
// once. The caller contract stays wallets-and-amounts, this stays the whole
// truth, and api/contract_test.go walks only the former.
type uiState struct {
	Service uiService  `json:"service"`
	Wallets []uiWallet `json:"wallets"`
	Flows   []uiFlow   `json:"flows"`
	Now     string     `json:"now"`
}

type uiService struct {
	Version      string `json:"version"`
	ChainID      uint64 `json:"chain_id"`
	Token        string `json:"token"`
	Decimals     uint8  `json:"decimals"`
	Block        uint64 `json:"block"`
	BlocksBehind uint64 `json:"blocks_behind"`
	MaxLagBlocks uint64 `json:"max_lag_blocks"`

	// The address an operator ends up looking for: the one that pays for
	// everything. It is derived, not stored, so it appears nowhere else.
	Master string `json:"master"`
	// Lagging is the one derived flag worth serving rather than recomputing in
	// the page: it is the exact condition that starts refusing withdrawals.
	Lagging bool `json:"lagging"`
}

// uiWallet is the operator's view of one wallet, and it deliberately carries
// what the caller contract does not need: the raw custody figure, the pending
// commitment against it, and where this wallet forwards to.
type uiWallet struct {
	Ref       string `json:"ref"`
	Address   string `json:"address"`
	DrainTo   string `json:"drain_to,omitempty"`
	Balance   string `json:"balance"`
	Committed string `json:"committed"`
	Paused    bool   `json:"paused"`
	Active    bool   `json:"active"`
	Busy      bool   `json:"busy"`
	Deposits  int    `json:"deposits"`
}

type uiFlow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Ref     string `json:"ref"`
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
	page, err := dashboard()
	if err != nil {
		s.log.Error("could not mount the dashboard", "error", err)
		return
	}
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", page))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.HandleFunc("GET /ui/state", s.handle(s.getUIState))

	s.log.Warn("dashboard enabled",
		"path", "/ui/",
		"note", "this listener has no authentication; anyone who can reach it can move every wallet's money")
}

func (s *Server) getUIState(w http.ResponseWriter, _ *http.Request) error {
	// Both collections are initialised rather than left nil: encoding/json
	// renders a nil slice as null, and the page does state.flows.length on
	// arrival. An empty dashboard must be empty, not a TypeError.
	out := uiState{Wallets: []uiWallet{}, Flows: []uiFlow{}}
	if err := s.store.View(func(tx *store.Tx) error {
		behind := s.sync.Behind()
		out.Service = uiService{
			Version:      buildinfo.Version,
			MaxLagBlocks: s.opts.MaxLagBlocks,
			BlocksBehind: behind,
			Lagging:      behind > s.opts.MaxLagBlocks,
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
		}
		if c, known, err := tx.Cursor(); err != nil {
			return err
		} else if known {
			out.Service.Block = c
		}

		wallets, err := tx.Wallets("", 0)
		if err != nil {
			return err
		}
		out.Wallets = make([]uiWallet, 0, len(wallets))
		for _, wl := range wallets {
			committed, err := tx.Committed(wl.ID)
			if err != nil {
				return err
			}
			deposits, err := tx.WalletDeposits(wl.ID, 0)
			if err != nil {
				return err
			}
			view := uiWallet{
				Ref: wl.Ref, Address: wl.Address.Hex(),
				Balance: orZeroStr(wl.Balance), Committed: committed.String(),
				Paused: wl.Paused, Active: wl.Active, Busy: !wl.Idle(),
				Deposits: len(deposits),
			}
			if wl.Proxies() {
				view.DrainTo = wl.DrainTo.Hex()
			}
			out.Wallets = append(out.Wallets, view)
		}

		// Live flows are the whole point of the operator view: this is the work
		// the service believes it owes, and a wedged one shows up here long
		// before it shows up in a balance.
		return tx.EachFlow(func(f store.Flow) error {
			view := uiFlow{
				ID: f.ID.String(), Kind: f.Label(), State: f.State.String(),
				Wallet: f.Wallet.String(), TxHash: hashStr(f.Tx),
				Attempt: f.Attempt, Error: f.Error, CreatedAt: stamp(f.CreatedAt),
			}
			if wl, found, err := tx.Wallet(f.Wallet); err == nil && found {
				view.Address = wl.Address.Hex()
				view.Ref = wl.Ref
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
