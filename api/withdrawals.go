package api

import (
	"math/big"
	"net/http"
	"time"

	"bsc/money"
	"bsc/store"

	"github.com/google/uuid"
)

type withdrawalBody struct {
	To     string `json:"to"`
	Amount string `json:"amount"`

	// Fee is an extra amount debited from the same wallet to the one that pays
	// for gas, asked for in the same call. bsc does not decide what a fee is or
	// how it is computed — the caller has already done that and is telling us a
	// number. All bsc adds is that the two are accepted together and that it
	// knows where to send it (§43).
	Fee string `json:"fee,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// createdView is what a create returns: the payout, and the fee if one was
// asked for. They are two records because they are two transfers — nothing can
// make two ERC-20 transfers atomic without a contract — so the response names
// both rather than pretending they are one thing (§43).
type createdView struct {
	Payout withdrawalView  `json:"payout"`
	Fee    *withdrawalView `json:"fee,omitempty"`
}

// withdrawalView is one payout. `status` is `pending` until the transfer is
// final, then `confirmed`; there is no failure state, because a request that
// cannot be honoured was refused at creation and anything that goes wrong
// afterwards is retried rather than handed back (§28).
//
// LastError and Attempts are therefore diagnostics, not a verdict. A payout
// whose attempts climb into double digits is something a human must look at.
type withdrawalView struct {
	ID     string `json:"id"`
	Wallet string `json:"wallet"`
	// Reason is why the money left: `payout` for one a caller asked for, `fee`
	// for the charge that accompanied it, `drain` for one bsc made on its own.
	// A drain is never pending — it is recorded once it has already happened.
	Reason string `json:"reason"`
	// PartOf links a fee back to its payout.
	PartOf string `json:"part_of,omitempty"`
	Status string `json:"status"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	TxHash string `json:"tx_hash,omitempty"`
	// Cursor is this debit's position in the settled feed, present once it has
	// settled. Pass the last one back as `since`.
	Cursor    string `json:"cursor,omitempty"`
	Attempts  uint32 `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// createWithdrawal accepts a payout from one wallet and leaves the rest to the
// work rules — the flow starts on the next evaluation rather than being
// enqueued here.
//
// The overdraft guard is what replaced the reservation ledger, and it is a
// comparison rather than a balance of its own: what this wallet already owes on
// pending withdrawals, plus what is being asked for now, against what the chain
// says it holds. Both sides are records that already exist, so there is nothing
// to keep in step and nothing to recompute (§39).
func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) error {
	var body withdrawalBody
	if err := decode(r, &body); err != nil {
		return err
	}
	dest, err := address("to", body.To)
	if err != nil {
		return err
	}
	value, err := amount("amount", body.Amount)
	if err != nil {
		return err
	}
	var fee *big.Int
	if body.Fee != "" {
		if fee, err = amount("fee", body.Fee); err != nil {
			return err
		}
	}
	if len(body.IdempotencyKey) > 128 {
		return fail(http.StatusBadRequest, "bad_idempotency_key", "idempotency_key must be at most 128 characters")
	}
	// Balances are only current at the head, and accepting a payout against a
	// stale one could overdraw the wallet. Reads keep working throughout; only
	// spending is held back (§21).
	if behind := s.sync.Behind(); behind > s.opts.MaxLagBlocks {
		return fail(http.StatusServiceUnavailable, "syncing",
			"chain sync is %d blocks behind; withdrawals are paused until it catches up", behind)
	}
	// Where a fee goes is not a policy decision, so it is not configurable: it
	// goes to the wallet that pays for every transfer bsc makes. That is also
	// what refills the gas the service spends and earns nothing back (§38).
	master, err := s.ring.Master()
	if err != nil {
		return err
	}

	var out createdView
	if err := s.store.Update(func(tx *store.Tx) error {
		wallet, err := s.wallet(tx, r)
		if err != nil {
			return err
		}
		// A forwarding wallet's balance is on its way somewhere else. Paying out
		// of it would race the drain for the same funds, which is why the two
		// configurations are mutually exclusive rather than merely unusual (§32).
		if wallet.Proxies() {
			return fail(http.StatusUnprocessableEntity, "wallet_forwards",
				"wallet %q forwards to %s; it cannot pay out. Clear drain_to first",
				wallet.Ref, wallet.DrainTo.Hex())
		}
		if wallet.Paused {
			return fail(http.StatusForbidden, "paused", "wallet %q has payouts paused", wallet.Ref)
		}

		if body.IdempotencyKey != "" {
			existing, ok, err := tx.WithdrawalByKey(body.IdempotencyKey)
			if err != nil {
				return err
			}
			if ok {
				// A retry after a timeout replays the original. The same key
				// with different parameters is a conflict, not a second payout.
				if existing.Destination != dest || existing.Amount.Cmp(value) != 0 || existing.Wallet != wallet.ID {
					return fail(http.StatusConflict, "idempotency_conflict",
						"idempotency_key %q was used with different parameters", body.IdempotencyKey)
				}
				out, err = replay(tx, existing, wallet.Ref)
				return err
			}
		}

		// Payout and fee are checked together, so a wallet can never end up
		// having accepted one and refused the other. That joint acceptance is
		// the whole of what bsc adds over two separate calls — it cannot make
		// the two transfers land together (§43).
		total := new(big.Int).Set(value)
		if fee != nil {
			total.Add(total, fee)
		}
		committed, err := tx.Committed(wallet.ID)
		if err != nil {
			return err
		}
		want := new(big.Int).Add(committed, total)
		held := money.OrZero(wallet.Balance)
		if want.Cmp(held) > 0 {
			return fail(http.StatusUnprocessableEntity, "insufficient_balance",
				"wallet %q holds %s and already owes %s; %s more cannot be promised",
				wallet.Ref, held, committed, total)
		}

		now := time.Now().UTC()
		payout := store.Withdrawal{
			ID: uuid.New(), Wallet: wallet.ID, Reason: store.ReasonPayout,
			Destination: dest, Amount: value,
			Status: store.WithdrawalPending, IdempotencyKey: body.IdempotencyKey,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.PutWithdrawal(payout); err != nil {
			return err
		}
		out = createdView{Payout: viewWithdrawal(payout, wallet.Ref)}

		if fee == nil {
			return nil
		}
		charge := store.Withdrawal{
			ID: uuid.New(), Wallet: wallet.ID, Reason: store.ReasonFee,
			PartOf: payout.ID, Destination: master.Address, Amount: fee,
			// A moment later, so oldest-first runs the payout before its fee.
			// Nothing depends on that order; it is simply the one that reads
			// right if somebody is watching.
			Status: store.WithdrawalPending, CreatedAt: now.Add(time.Millisecond),
			UpdatedAt: now,
		}
		if err := tx.PutWithdrawal(charge); err != nil {
			return err
		}
		view := viewWithdrawal(charge, wallet.Ref)
		out.Fee = &view
		return nil
	}); err != nil {
		return err
	}
	s.opts.Notify()
	writeJSON(w, http.StatusCreated, out)
	return nil
}

// replay rebuilds the response for an idempotent retry, finding the fee that
// was created alongside the payout. The fee has no key of its own — one request
// is one key — so it is found by the link back to its payout.
func replay(tx *store.Tx, payout store.Withdrawal, ref string) (createdView, error) {
	out := createdView{Payout: viewWithdrawal(payout, ref)}
	siblings, err := tx.WalletWithdrawals(payout.Wallet, 0)
	if err != nil {
		return out, err
	}
	for _, wd := range siblings {
		if wd.PartOf == payout.ID {
			view := viewWithdrawal(wd, ref)
			out.Fee = &view
			break
		}
	}
	return out, nil
}

func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return fail(http.StatusBadRequest, "bad_id", "withdrawal id must be a uuid")
	}
	var out withdrawalView
	if err := s.store.View(func(tx *store.Tx) error {
		wd, ok, err := tx.Withdrawal(id)
		if err != nil {
			return err
		}
		if !ok {
			return fail(http.StatusNotFound, "unknown_withdrawal", "no withdrawal %s", id)
		}
		ref, err := refOf(tx, wd.Wallet)
		if err != nil {
			return err
		}
		out = viewWithdrawal(wd, ref)
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// listWithdrawals is the settled-debit feed: everything that has left a managed
// wallet, in the order the chain settled it.
//
// It is the mirror of the deposit feed, and reads the same way — `since` a
// cursor, opaque and ordered. Pending payouts are deliberately absent: this is a
// record of what happened, and a promise has not happened yet. A caller tracking
// one it asked for holds the id and reads it directly; a caller watching for
// completions walks this feed.
//
// Drains and fees are included by default. They are real movements between real
// addresses, and a caller reconciling a wallet needs them — leaving them out
// would put back the special case that treating every movement as a debit
// removed (§42). `?reason=payout` narrows it.
func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	reason, err := reasonParam(r)
	if err != nil {
		return err
	}
	since, err := store.ParseSettled(r.URL.Query().Get("since"))
	if err != nil {
		return fail(http.StatusBadRequest, "bad_cursor", "%v", err)
	}

	out := []withdrawalView{}
	var next store.Settled
	if err := s.store.View(func(tx *store.Tx) error {
		records, cursor, err := tx.SettledSince(since, limit)
		if err != nil {
			return err
		}
		next = cursor
		out, err = viewWithdrawals(tx, filterReason(records, reason), 0)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"withdrawals": out, "cursor": next.String()})
	return nil
}

// reasonParam reads the reason filter. Everything by default: the feed is what
// left, whoever decided it should.
func reasonParam(r *http.Request) (string, error) {
	switch got := r.URL.Query().Get("reason"); got {
	case "", "all":
		return "all", nil
	case "payout", "fee", "drain":
		return got, nil
	default:
		return "", fail(http.StatusBadRequest, "bad_reason", "reason must be payout, fee, drain or all")
	}
}

func filterReason(records []store.Withdrawal, reason string) []store.Withdrawal {
	if reason == "all" {
		return records
	}
	kept := records[:0]
	for _, wd := range records {
		if wd.Reason.String() == reason {
			kept = append(kept, wd)
		}
	}
	return kept
}

func (s *Server) listWalletWithdrawals(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	status, err := statusParam(r)
	if err != nil {
		return err
	}
	reason, err := reasonParam(r)
	if err != nil {
		return err
	}
	out := []withdrawalView{}
	if err := s.store.View(func(tx *store.Tx) error {
		wallet, err := s.wallet(tx, r)
		if err != nil {
			return err
		}
		var records []store.Withdrawal
		if status == "pending" {
			records, err = tx.WalletOpenWithdrawals(wallet.ID)
		} else {
			records, err = tx.WalletWithdrawals(wallet.ID, limit)
		}
		if err != nil {
			return err
		}
		out, err = viewWithdrawals(tx, filterReason(filterStatus(records, status), reason), limit)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"withdrawals": out})
	return nil
}

// statusParam reads the status filter for a single wallet's history. Pending is
// the default: the settled ones are on the feed, so what is worth asking a
// wallet directly is what it still owes.
func statusParam(r *http.Request) (string, error) {
	switch got := r.URL.Query().Get("status"); got {
	case "":
		return "pending", nil
	case "pending", "confirmed", "all":
		return got, nil
	default:
		return "", fail(http.StatusBadRequest, "bad_status", "status must be pending, confirmed or all")
	}
}

func filterStatus(records []store.Withdrawal, status string) []store.Withdrawal {
	if status == "all" {
		return records
	}
	kept := records[:0]
	for _, wd := range records {
		if wd.Status.String() == status {
			kept = append(kept, wd)
		}
	}
	return kept
}

func viewWithdrawals(tx *store.Tx, records []store.Withdrawal, limit int) ([]withdrawalView, error) {
	refs := map[string]string{}
	out := make([]withdrawalView, 0, len(records))
	for _, wd := range records {
		if limit > 0 && len(out) >= limit {
			break
		}
		ref, ok := refs[string(wd.Wallet[:])]
		if !ok {
			var err error
			if ref, err = refOf(tx, wd.Wallet); err != nil {
				return nil, err
			}
			refs[string(wd.Wallet[:])] = ref
		}
		out = append(out, viewWithdrawal(wd, ref))
	}
	return out, nil
}

func refOf(tx *store.Tx, id uuid.UUID) (string, error) {
	w, ok, err := tx.Wallet(id)
	if err != nil || !ok {
		return "", err
	}
	return w.Ref, nil
}

func viewWithdrawal(wd store.Withdrawal, ref string) withdrawalView {
	v := withdrawalView{
		ID: wd.ID.String(), Wallet: ref,
		Reason: wd.Reason.String(), Status: wd.Status.String(),
		To: wd.Destination.Hex(), Amount: money.String(wd.Amount),
		TxHash: hashStr(wd.TxHash), Attempts: wd.Attempts, LastError: wd.Error,
		CreatedAt: stamp(wd.CreatedAt), UpdatedAt: stamp(wd.UpdatedAt),
	}
	if wd.PartOf != uuid.Nil {
		v.PartOf = wd.PartOf.String()
	}
	if wd.Status.IsTerminal() {
		v.Cursor = wd.Settled().String()
	}
	return v
}
