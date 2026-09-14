package api

import (
	"errors"
	"net/http"
	"time"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

type quoteBody struct {
	AmountCents string `json:"amount_cents"`
	DeductFee   bool   `json:"deduct_fee"`
}

type quoteView struct {
	AmountCents string `json:"amount_cents"`
	FeeCents    string `json:"fee_cents"`
	PayoutCents string `json:"payout_cents"`
	DebitCents  string `json:"debit_cents"`
}

type withdrawalBody struct {
	Destination    string `json:"destination"`
	AmountCents    string `json:"amount_cents"`
	DeductFee      bool   `json:"deduct_fee"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// withdrawalView is one payout. `status` is `pending` until the transfer is
// final, then `debited`; there is no failure state, because a request that
// cannot be honoured was refused at creation and anything that goes wrong
// afterwards is retried rather than handed back (§28).
//
// LastError and Attempts are therefore diagnostics, not a verdict. They exist
// because the app API is also the operator's read surface (§13): a payout that
// keeps reverting must be visible to a human without inventing a terminal state
// the app would have to compensate for.
type withdrawalView struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Destination string `json:"destination"`
	AmountCents string `json:"amount_cents"`
	FeeCents    string `json:"fee_cents"`
	PayoutCents string `json:"payout_cents"`
	DeductFee   bool   `json:"deduct_fee"`
	TxHash      string `json:"tx_hash"`
	Attempts    uint32 `json:"attempts"`
	LastError   string `json:"last_error,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// quoteWithdrawal prices a withdrawal without touching anything, so an app can
// show a user the fee before committing to it.
func (s *Server) quoteWithdrawal(w http.ResponseWriter, r *http.Request) error {
	var body quoteBody
	if err := decode(r, &body); err != nil {
		return err
	}
	value, err := cents("amount_cents", body.AmountCents)
	if err != nil {
		return err
	}

	var q store.Quote
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		q, err = a.Fee.Quote(value, body.DeductFee)
		return quoteError(err)
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, quoteView{
		AmountCents: q.Amount.String(), FeeCents: q.Fee.String(),
		PayoutCents: q.Payout.String(), DebitCents: q.Debit.String(),
	})
	return nil
}

// createWithdrawal accepts a payout, reserves the money behind it, and leaves
// the rest to the work rules — the flow starts on the next evaluation rather
// than being enqueued here.
//
// One reservation covers payout and fee together. They used to be held apart
// because they left at different moments; now the fee never leaves at all — it
// is collected by staying in the wallet uncredited — so both are released by the
// same event and one figure is the whole commitment (§24).
func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) error {
	var body withdrawalBody
	if err := decode(r, &body); err != nil {
		return err
	}
	if !common.IsHexAddress(body.Destination) {
		return fail(http.StatusBadRequest, "bad_destination", "destination must be a hex address")
	}
	// The zero address passes IsHexAddress and is the one destination a valid
	// address can be while still being unpayable: the token reverts on it, so
	// the payout could never land however many times it was retried. A
	// withdrawal has no failure state (§28), which means anything permanently
	// unpayable has to be caught here — before it becomes a record holding a
	// reservation forever — rather than settled into one afterwards. It costs
	// no RPC call, so there is no reason not to.
	if common.HexToAddress(body.Destination) == (common.Address{}) {
		return fail(http.StatusBadRequest, "bad_destination",
			"destination is the zero address, which cannot receive tokens")
	}
	value, err := cents("amount_cents", body.AmountCents)
	if err != nil {
		return err
	}
	if len(body.IdempotencyKey) > 128 {
		return fail(http.StatusBadRequest, "bad_idempotency_key", "idempotency_key must be at most 128 characters")
	}

	// The ledger is only current once the watcher reaches the head: a drain that
	// settled in an unprocessed block has not been credited yet. Reserving
	// against a stale ledger would refuse money the app really has, and the
	// honest answer while catching up is "not yet" rather than a wrong number.
	if behind := s.sync.Behind(); behind > s.opts.MaxLagBlocks {
		return fail(http.StatusServiceUnavailable, "syncing",
			"chain sync is %d blocks behind; withdrawals are paused until it catches up", behind)
	}

	destination := common.HexToAddress(body.Destination)
	var (
		wd     store.Withdrawal
		replay bool
	)
	if err := s.store.Update(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		if a.Paused {
			return fail(http.StatusForbidden, "app_paused", "app %q has payouts paused", a.Slug)
		}

		if body.IdempotencyKey != "" {
			prior, ok, err := tx.WithdrawalByKey(a.Slug, body.IdempotencyKey)
			if err != nil {
				return err
			}
			if ok {
				// A retry after a timeout replays the original rather than
				// paying twice — the dedup v1 got for free from the broker.
				if prior.Destination != destination || prior.Amount != value || prior.DeductFee != body.DeductFee {
					return fail(http.StatusConflict, "idempotency_conflict",
						"idempotency_key %q was already used with different parameters", body.IdempotencyKey)
				}
				wd, replay = prior, true
				return nil
			}
		}

		q, err := a.Fee.Quote(value, body.DeductFee)
		if err != nil {
			return quoteError(err)
		}
		now := time.Now().UTC()
		wd = store.Withdrawal{
			ID: uuid.New(), App: a.Slug, Destination: destination,
			Amount: q.Amount, Fee: q.Fee, Payout: q.Payout, Debit: q.Debit,
			DeductFee: body.DeductFee, Status: store.WithdrawalPending,
			IdempotencyKey: body.IdempotencyKey, FeeSnapshot: a.Fee,
			CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.ReserveLedger(a.Slug, q.Debit); err != nil {
			return reserveError(err)
		}
		return tx.PutWithdrawal(wd)
	}); err != nil {
		return err
	}

	if !replay {
		s.opts.Notify()
		s.log.Info("withdrawal accepted",
			"app", wd.App, "id", wd.ID, "payout_cents", wd.Payout, "fee_cents", wd.Fee,
			"to", wd.Destination.Hex())
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, viewWithdrawal(wd))
	return nil
}

func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return fail(http.StatusBadRequest, "bad_id", "withdrawal id must be a UUID")
	}
	var wd store.Withdrawal
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		got, ok, err := tx.Withdrawal(id)
		if err != nil {
			return err
		}
		// Scoped to the app in the path, so one app cannot read another's
		// withdrawal by guessing an id.
		if !ok || got.App != a.Slug {
			return errNotFound
		}
		wd = got
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewWithdrawal(wd))
	return nil
}

// listWithdrawals serves the outstanding set by default. Withdrawals need no
// cursor: the app minted the ids and only wants to know which of its own have
// not settled yet.
func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) error {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}

	var out []withdrawalView
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		var list []store.Withdrawal
		switch status {
		case "pending":
			list, err = tx.OpenWithdrawals(a.Slug)
		case "debited", "all":
			list, err = tx.Withdrawals(a.Slug, 0)
		default:
			return fail(http.StatusBadRequest, "bad_status", "status must be pending, debited or all")
		}
		if err != nil {
			return err
		}
		out = make([]withdrawalView, 0, len(list))
		for _, wd := range list {
			if status == "debited" && wd.Status != store.WithdrawalDebited {
				continue
			}
			out = append(out, viewWithdrawal(wd))
			if len(out) >= limit {
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"withdrawals": out})
	return nil
}

func viewWithdrawal(wd store.Withdrawal) withdrawalView {
	return withdrawalView{
		ID: wd.ID.String(), Status: wd.Status.String(), Destination: wd.Destination.Hex(),
		AmountCents: wd.Amount.String(), FeeCents: wd.Fee.String(),
		PayoutCents: wd.Payout.String(), DeductFee: wd.DeductFee,
		TxHash: hashStr(wd.TxHash), Attempts: wd.Attempts, LastError: wd.Error,
		CreatedAt: stamp(wd.CreatedAt), UpdatedAt: stamp(wd.UpdatedAt),
	}
}

// quoteError maps a pricing refusal to a status the caller can act on.
func quoteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrBelowMinimum):
		return fail(http.StatusUnprocessableEntity, "below_minimum", "%v", err)
	case errors.Is(err, store.ErrFeeExceedsAmount):
		return fail(http.StatusUnprocessableEntity, "fee_exceeds_amount", "%v", err)
	case errors.Is(err, store.ErrUnsupportedPolicy):
		return fail(http.StatusConflict, "unsupported_fee_policy", "%v", err)
	}
	return fail(http.StatusBadRequest, "bad_amount", "%v", err)
}

// reserveError turns an overdraft into a refusal rather than a transfer that
// reverts on-chain after the gas is spent.
func reserveError(err error) error {
	if errors.Is(err, store.ErrInsufficient) {
		return fail(http.StatusUnprocessableEntity, "insufficient_balance", "%v", err)
	}
	return err
}
