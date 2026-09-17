package api

import (
	"math/big"
	"net/http"
	"strconv"
	"time"

	"bsc/flow"
	"bsc/money"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
)

type createWalletBody struct {
	// DrainTo makes this a forwarding wallet: everything that lands on it is
	// swept to that address once it is worth the gas. Omitted or empty means
	// the wallet accumulates and can be paid out of.
	DrainTo string `json:"drain_to,omitempty"`

	// Prewarm activates the wallet now instead of on first use. It costs one
	// native-currency transfer and one approve, both paid by the master, and
	// it is off by default for exactly that reason (§41).
	//
	// Ask for it only on a wallet you know will be used — a treasury you are
	// about to pay out of. Never on a per-user address, where most of the
	// address book may never receive anything.
	Prewarm bool `json:"prewarm,omitempty"`
}

type patchWalletBody struct {
	// A pointer so that "" is distinguishable from absent: sending an empty
	// string is how a caller turns a forwarding wallet back into one that
	// accumulates.
	DrainTo *string `json:"drain_to,omitempty"`
	Paused  *bool   `json:"paused,omitempty"`
}

// walletView is what a caller sees. The balance is custody in the token's own
// base units — what the chain says this address holds, and the only balance the
// service has an opinion about.
type walletView struct {
	Ref       string `json:"ref"`
	Address   string `json:"address"`
	DrainTo   string `json:"drain_to,omitempty"`
	Balance   string `json:"balance"`
	Committed string `json:"committed"` // promised by pending withdrawals
	Available string `json:"available"` // balance − committed
	Paused    bool   `json:"paused"`
	CreatedAt string `json:"created_at"`
}

// putWallet derives an address for the ref in the path. It is idempotent, and
// the mapping is permanent: an address handed to a user is never reused for
// anyone else or rotated away.
//
// **Creating a wallet spends nothing.** Deriving is HMAC over the master secret
// and a write to bbolt; no transaction is signed and no gas is paid. Activation
// — one funding transfer and one approve — happens as the prefix of whatever
// flow first needs to move money, so an address that never receives anything
// never costs anything (§41). `prewarm: true` opts out of that for a wallet you
// know will be used.
func (s *Server) putWallet(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("ref")
	if err := store.ValidRef(ref); err != nil {
		return fail(http.StatusBadRequest, "bad_ref", "%v", err)
	}
	var body createWalletBody
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	var drainTo common.Address
	if body.DrainTo != "" {
		got, err := address("drain_to", body.DrainTo)
		if err != nil {
			return err
		}
		drainTo = got
	}

	var (
		wallet    store.Wallet
		created   bool
		prewarmed bool
	)
	if err := s.store.Update(func(tx *store.Tx) error {
		existing, ok, err := tx.WalletByRef(ref)
		if err != nil {
			return err
		}
		if ok {
			// Idempotent on ref, but only for the same wallet: returning a
			// wallet configured differently from what was asked for would be a
			// silent disagreement about where its money goes.
			if existing.DrainTo != drainTo {
				return fail(http.StatusConflict, "ref_conflict",
					"ref %q already exists with a different drain_to", ref)
			}
			wallet = existing
			return nil
		}
		id, key, err := s.ring.Generate()
		if err != nil {
			return err
		}
		wallet = store.Wallet{
			ID: id, Ref: ref, Kind: store.KindManaged, Address: key.Address,
			DrainTo: drainTo, Balance: new(big.Int),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := tx.CheckDrainChain(id, key.Address, drainTo); err != nil {
			return drainError(err)
		}
		if err := tx.PutWallet(wallet); err != nil {
			return err
		}
		created = true
		if !body.Prewarm {
			return nil
		}
		// Opted in: activate now so the wallet's first real movement is one
		// transaction rather than three. The master pays for it either way —
		// this only decides when.
		prewarmed = true
		f, err := flow.Begin(flow.Params{
			Kind: store.FlowPrewarm, Wallet: id, Active: false, Now: time.Now(),
		})
		if err != nil {
			return err
		}
		if err := tx.PutFlow(f); err != nil {
			return err
		}
		_, err = tx.ClaimWallet(id, f.ID)
		return err
	}); err != nil {
		return err
	}

	if created {
		// The watchlist must know about it before the first transfer arrives —
		// there is no history back-scan. Only after the write commits, so the
		// watcher never learns of an address a rollback removed.
		s.addrs.Add(wallet.Address, wallet.ID)
		s.log.Info("wallet created", "ref", wallet.Ref, "address", wallet.Address.Hex(),
			"drain_to", wallet.DrainTo.Hex(), "prewarm", prewarmed)
	}
	if prewarmed {
		s.opts.Notify()
	}
	view, err := s.walletView(wallet.Ref)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, view)
	return nil
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("ref")
	if err := store.ValidRef(ref); err != nil {
		return fail(http.StatusBadRequest, "bad_ref", "%v", err)
	}
	view, err := s.walletView(ref)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

// patchWallet retargets or pauses a wallet. Retargeting is validated rather
// than trusted: a drain chain that loops would move the same money round and
// round, succeeding every time and costing gas on every hop (§35).
func (s *Server) patchWallet(w http.ResponseWriter, r *http.Request) error {
	var body patchWalletBody
	if err := decode(r, &body); err != nil {
		return err
	}
	var ref string
	if err := s.store.Update(func(tx *store.Tx) error {
		wallet, err := s.wallet(tx, r)
		if err != nil {
			return err
		}
		ref = wallet.Ref
		var drainTo common.Address
		if body.DrainTo != nil && *body.DrainTo != "" {
			got, err := address("drain_to", *body.DrainTo)
			if err != nil {
				return err
			}
			drainTo = got
		}
		_, err = tx.MutateWallet(wallet.ID, func(mw *store.Wallet) error {
			if body.DrainTo != nil {
				// A wallet with money already promised must not become a proxy:
				// the drain would race the payout for the same funds.
				if drainTo != (common.Address{}) {
					committed, err := tx.Committed(mw.ID)
					if err != nil {
						return err
					}
					if committed.Sign() > 0 {
						return fail(http.StatusConflict, "has_pending_withdrawals",
							"wallet %q has %s committed to pending withdrawals; it cannot start forwarding until they settle",
							mw.Ref, committed)
					}
				}
				mw.DrainTo = drainTo
			}
			if body.Paused != nil {
				mw.Paused = *body.Paused
			}
			return nil
		})
		return drainError(err)
	}); err != nil {
		return err
	}
	s.opts.Notify()
	view, err := s.walletView(ref)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) listWallets(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	after := r.URL.Query().Get("after")

	var out []walletView
	if err := s.store.View(func(tx *store.Tx) error {
		wallets, err := tx.Wallets(after, limit)
		if err != nil {
			return err
		}
		out = make([]walletView, 0, len(wallets))
		for _, wl := range wallets {
			committed, err := tx.Committed(wl.ID)
			if err != nil {
				return err
			}
			out = append(out, viewWallet(wl, committed))
		}
		return nil
	}); err != nil {
		return err
	}
	body := map[string]any{"wallets": out}
	if len(out) == limit && limit > 0 {
		body["after"] = out[len(out)-1].Ref // pass back to continue
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func (s *Server) walletView(ref string) (walletView, error) {
	var out walletView
	err := s.store.View(func(tx *store.Tx) error {
		wl, ok, err := tx.WalletByRef(ref)
		if err != nil {
			return err
		}
		if !ok {
			return fail(http.StatusNotFound, "unknown_wallet", "no wallet with ref %q", ref)
		}
		committed, err := tx.Committed(wl.ID)
		if err != nil {
			return err
		}
		out = viewWallet(wl, committed)
		return nil
	})
	return out, err
}

func viewWallet(w store.Wallet, committed *big.Int) walletView {
	balance := money.OrZero(w.Balance)
	available := new(big.Int).Sub(balance, money.OrZero(committed))
	if available.Sign() < 0 {
		available = new(big.Int)
	}
	v := walletView{
		Ref: w.Ref, Address: w.Address.Hex(),
		Balance: balance.String(), Committed: money.String(committed),
		Available: available.String(), Paused: w.Paused,
		CreatedAt: stamp(w.CreatedAt),
	}
	if w.Proxies() {
		v.DrainTo = w.DrainTo.Hex()
	}
	return v
}

// drainError maps the store's topology refusals onto statuses. They are the
// caller's input being wrong, not our failure.
func drainError(err error) error {
	switch {
	case err == nil:
		return nil
	case isErr(err, store.ErrDrainCycle):
		return fail(http.StatusUnprocessableEntity, "drain_cycle", "%v", err)
	case isErr(err, store.ErrDrainDepth):
		return fail(http.StatusUnprocessableEntity, "drain_too_deep", "%v", err)
	case isErr(err, store.ErrBadRef):
		return fail(http.StatusBadRequest, "bad_ref", "%v", err)
	}
	return err
}

func limitParam(r *http.Request, def, max int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fail(http.StatusBadRequest, "bad_limit", "limit must be a positive integer")
	}
	return min(n, max), nil
}
