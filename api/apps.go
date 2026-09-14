package api

import (
	"math/big"
	"net/http"
	"strconv"
	"time"

	"bsc/flow"
	"bsc/money"
	"bsc/store"
)

// appBody is the configuration an app may set for itself. Apps are all
// first-party, so each sets its own fee with no operator floor — the fee is a
// charge on the app's own users, not a gas recovery.
type appBody struct {
	Fee *feeBody `json:"fee,omitempty"`

	// Paused stops payouts for this app. Drains and fee sweeps continue — those
	// are us collecting our own money, not the app spending its users'.
	Paused *bool `json:"paused,omitempty"`
}

type feeBody struct {
	FlatCents string `json:"flat_cents"`
	MinCents  string `json:"min_cents"`
}

type feeView struct {
	FlatCents   string `json:"flat_cents"`
	MinCents    string `json:"min_cents"`
	BPS         uint32 `json:"bps"`
	MaxFeeCents string `json:"max_fee_cents"`
}

// balanceView is the app's ledger in cents, split by what the app can do with
// each part right now. Every deposit and every withdrawal sits in exactly one of
// the three, so they answer "where is my money" without remainder:
//
//	available  spendable this second
//	reserved   committed to payouts that have not settled
//	pending    arrived, not yet moved into the wallet payouts are drawn from
//	total      the three added up — everything that is or will be the app's
//
// None of it is a chain balance. The hot wallet also holds our fees and the
// sub-cent remainders, and none of that is the app's (§22) — which is the whole
// reason the ledger is reported instead of a `balanceOf`.
type balanceView struct {
	AvailableCents string `json:"available_cents"`
	ReservedCents  string `json:"reserved_cents"`
	PendingCents   string `json:"pending_cents"`
	TotalCents     string `json:"total_cents"`
}

type appView struct {
	Slug      string      `json:"slug"`
	Address   string      `json:"address"`
	Paused    bool        `json:"paused"`
	Fee       feeView     `json:"fee"`
	Balance   balanceView `json:"balance"`
	CreatedAt string      `json:"created_at"`
}

// putApp registers an app or updates its configuration. Registration is public
// and idempotent — the service is loopback-only, so calling it on every boot is
// the intended usage, exactly as v1's tenant provisioning worked.
func (s *Server) putApp(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("slug")
	if err := store.ValidSlug(slug); err != nil {
		return fail(http.StatusBadRequest, "bad_slug", "%v", err)
	}
	var body appBody
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}

	var (
		created   bool
		newWallet store.Wallet
	)
	if err := s.store.Update(func(tx *store.Tx) error {
		existing, ok, err := tx.App(slug)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		if !ok {
			// A new app needs its top-level hot wallet before anything else can
			// reference it.
			id, key, err := s.ring.Generate()
			if err != nil {
				return err
			}
			newWallet = store.Wallet{
				ID: id, App: slug, Kind: store.KindTopLevel, Address: key.Address,
				Balance: new(big.Int), CreatedAt: now,
			}
			if err := tx.PutWallet(newWallet); err != nil {
				return err
			}
			existing = store.App{Slug: slug, Wallet: id, Fee: s.opts.DefaultFee, CreatedAt: now}
			created = true
		}

		if body.Paused != nil {
			existing.Paused = *body.Paused
		}
		if body.Fee != nil {
			fee, err := parseFee(*body.Fee)
			if err != nil {
				return err
			}
			existing.Fee = fee
		}
		existing.UpdatedAt = now
		if err := tx.PutApp(existing); err != nil {
			return err
		}
		if !created {
			return nil
		}
		// Pre-warm the new wallet off the hot path, so the app's first
		// withdrawal does not also pay for activation.
		f, err := flow.Begin(flow.Params{
			Kind: store.FlowPrewarm, Wallet: newWallet.ID, App: slug, Active: false, Now: now,
		})
		if err != nil {
			return err
		}
		if err := tx.PutFlow(f); err != nil {
			return err
		}
		_, err = tx.ClaimWallet(newWallet.ID, f.ID)
		return err
	}); err != nil {
		return err
	}

	if created {
		// Only after the write commits, so the watcher never learns of an
		// address that a rollback removed.
		s.addrs.Add(newWallet.Address, newWallet.ID)
		s.opts.Notify()
		s.log.Info("app registered", "app", slug, "address", newWallet.Address.Hex())
	}

	view, err := s.appView(slug)
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

func parseFee(b feeBody) (store.FeePolicy, error) {
	flat, err := cents("fee.flat_cents", orDefault(b.FlatCents))
	if err != nil {
		return store.FeePolicy{}, err
	}
	min, err := cents("fee.min_cents", orDefault(b.MinCents))
	if err != nil {
		return store.FeePolicy{}, err
	}
	return store.FeePolicy{Flat: flat, Min: min}, nil
}

func orDefault(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) error {
	view, err := s.appView(r.PathValue("slug"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) error {
	view, err := s.appView(r.PathValue("slug"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view.Balance)
	return nil
}

// appView assembles an app's public state, including the pending balance —
// money detected on deposit addresses but not yet swept, which is real but not
// yet spendable.
func (s *Server) appView(slug string) (appView, error) {
	var out appView
	err := s.store.View(func(tx *store.Tx) error {
		if err := store.ValidSlug(slug); err != nil {
			return fail(http.StatusBadRequest, "bad_slug", "%v", err)
		}
		a, ok, err := tx.App(slug)
		if err != nil {
			return err
		}
		if !ok {
			return fail(http.StatusNotFound, "unknown_app", "app %q is not registered", slug)
		}
		top, ok, err := tx.Wallet(a.Wallet)
		if err != nil {
			return err
		}
		if !ok {
			return fail(http.StatusInternalServerError, "internal", "app %q has no wallet", slug)
		}
		pending, err := pendingBalance(tx, slug)
		if err != nil {
			return err
		}
		out = appView{
			Slug: a.Slug, Address: top.Address.Hex(), Paused: a.Paused,
			Fee: feeView{
				FlatCents: a.Fee.Flat.String(), MinCents: a.Fee.Min.String(),
				BPS: a.Fee.BPS, MaxFeeCents: a.Fee.MaxFee.String(),
			},
			Balance: balanceView{
				AvailableCents: a.Spendable().String(),
				ReservedCents:  a.Reserved.String(),
				PendingCents:   pending.String(),
				TotalCents:     (a.Spendable() + a.Reserved + pending).String(),
			},
			CreatedAt: stamp(a.CreatedAt),
		}
		return nil
	})
	return out, err
}

// pendingBalance sums the deposits this app has been shown but cannot spend yet:
// recorded, awaiting the drain that moves them into the hot wallet.
//
// It is summed from the deposit records rather than from what the deposit
// wallets hold on-chain, and the difference matters: those wallets also carry
// sub-cent dust that was never credited to anyone, and reporting it as the
// app's pending money would promise a balance that will never arrive (§22).
func pendingBalance(tx *store.Tx, slug string) (money.Cents, error) {
	open, err := tx.OpenDeposits(slug, 0)
	if err != nil {
		return 0, err
	}
	var total money.Cents
	for _, d := range open {
		total += d.Cents
	}
	return total, nil
}

// --- deposit addresses ---

type depositAddressBody struct {
	Ref string `json:"ref"`
}

type depositAddressView struct {
	Ref       string `json:"ref"`
	Address   string `json:"address"`
	CreatedAt string `json:"created_at"`
}

// createDepositAddress derives an address for the app's own handle. It is
// idempotent on ref, and the mapping is permanent: an address handed to a user
// is never reused for anyone else or rotated away.
func (s *Server) createDepositAddress(w http.ResponseWriter, r *http.Request) error {
	var body depositAddressBody
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.Ref == "" || len(body.Ref) > 128 {
		return fail(http.StatusBadRequest, "bad_ref", "ref must be 1..128 characters")
	}

	var (
		wallet  store.Wallet
		created bool
	)
	if err := s.store.Update(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		existing, ok, err := tx.WalletByRef(a.Slug, body.Ref)
		if err != nil {
			return err
		}
		if ok {
			wallet = existing
			return nil
		}
		id, key, err := s.ring.Generate()
		if err != nil {
			return err
		}
		wallet = store.Wallet{
			ID: id, App: a.Slug, Kind: store.KindDeposit, Ref: body.Ref, Address: key.Address,
			Balance: new(big.Int), CreatedAt: time.Now().UTC(),
		}
		created = true
		return tx.PutWallet(wallet)
	}); err != nil {
		return err
	}

	if created {
		// The watchlist must know about it before the first transfer arrives —
		// there is no history back-scan.
		s.addrs.Add(wallet.Address, wallet.ID)
		s.log.Info("deposit address created", "app", wallet.App, "ref", wallet.Ref, "address", wallet.Address.Hex())
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, viewAddress(wallet))
	return nil
}

func (s *Server) getDepositAddress(w http.ResponseWriter, r *http.Request) error {
	var wallet store.Wallet
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		got, ok, err := tx.WalletByRef(a.Slug, r.PathValue("ref"))
		if err != nil {
			return err
		}
		if !ok {
			return fail(http.StatusNotFound, "unknown_ref", "no deposit address for that ref")
		}
		wallet = got
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewAddress(wallet))
	return nil
}

func (s *Server) listDepositAddresses(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	after := r.URL.Query().Get("after")

	var out []depositAddressView
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		wallets, err := tx.DepositWallets(a.Slug, after, limit)
		if err != nil {
			return err
		}
		out = make([]depositAddressView, 0, len(wallets))
		for _, wl := range wallets {
			out = append(out, viewAddress(wl))
		}
		return nil
	}); err != nil {
		return err
	}
	body := map[string]any{"addresses": out}
	if len(out) == limit {
		body["after"] = out[len(out)-1].Ref // pass back to continue
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

func viewAddress(w store.Wallet) depositAddressView {
	return depositAddressView{Ref: w.Ref, Address: w.Address.Hex(), CreatedAt: stamp(w.CreatedAt)}
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
