package api

import (
	"net/http"

	"bsc/store"
)

// depositView is one recorded incoming transfer.
//
// `id` is the feed cursor: opaque but ordered, so two ids compare in the order
// the chain produced them. Pass the last one back as `since`; never parse one.
//
// `amount` is the chain's own figure. There is no second unit beside it and no
// rounding applied to it, so it is exactly what a block explorer shows (§36).
type depositView struct {
	ID        string `json:"id"`
	TxHash    string `json:"tx_hash"`
	Wallet    string `json:"wallet"` // the ref the money landed on
	From      string `json:"from"`
	Amount    string `json:"amount"`
	CreatedAt string `json:"created_at"`
}

// listDeposits reads the whole feed. Deposits are unsolicited — nobody can know
// one is coming — so the caller asks "what is new since I last looked".
//
// The feed is global. Under the old design each app read its own slice, which
// was a tenancy boundary; there is one caller now and it wants every deposit
// bsc has seen, filtered by wallet on its own side if at all (§37).
func (s *Server) listDeposits(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	since, err := store.ParseCursor(r.URL.Query().Get("since"))
	if err != nil {
		return fail(http.StatusBadRequest, "bad_cursor", "%v", err)
	}

	out := []depositView{}
	var next store.Cursor
	if err := s.store.View(func(tx *store.Tx) error {
		deposits, cursor, err := tx.DepositsSince(since, limit)
		if err != nil {
			return err
		}
		next = cursor
		out, err = viewDeposits(tx, deposits)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"deposits": out, "cursor": next.String()})
	return nil
}

// listWalletDeposits is one wallet's history, oldest first.
func (s *Server) listWalletDeposits(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	out := []depositView{}
	if err := s.store.View(func(tx *store.Tx) error {
		wallet, err := s.wallet(tx, r)
		if err != nil {
			return err
		}
		deposits, err := tx.WalletDeposits(wallet.ID, limit)
		if err != nil {
			return err
		}
		out, err = viewDeposits(tx, deposits)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"deposits": out})
	return nil
}

// viewDeposits renders records, resolving each one's wallet to the ref its
// caller knows it by. The ref is cached per page, so a page of credits on one
// wallet costs one lookup rather than one per row.
func viewDeposits(tx *store.Tx, deposits []store.Deposit) ([]depositView, error) {
	refs := map[store.WalletID]string{}
	out := make([]depositView, 0, len(deposits))
	for _, d := range deposits {
		ref, ok := refs[d.Wallet]
		if !ok {
			w, found, err := tx.Wallet(d.Wallet)
			if err != nil {
				return nil, err
			}
			if found {
				ref = w.Ref
			}
			refs[d.Wallet] = ref
		}
		out = append(out, depositView{
			ID:        d.Cursor().String(),
			TxHash:    d.TxHash.Hex(),
			Wallet:    ref,
			From:      d.From.Hex(),
			Amount:    amountString(d.Amount),
			CreatedAt: stamp(d.CreatedAt),
		})
	}
	return out, nil
}
