package api

import (
	"net/http"

	"bsc/store"
)

type depositView struct {
	Cursor      string `json:"cursor"`
	Block       uint64 `json:"block"`
	LogIndex    uint32 `json:"log_index"`
	TxHash      string `json:"tx_hash"`
	Ref         string `json:"ref"`
	From        string `json:"from"`
	AmountCents string `json:"amount_cents"`
	AmountWei   string `json:"amount_wei"`
	Status      string `json:"status"`
	DrainTx     string `json:"drain_tx"`
	CreatedAt   string `json:"created_at"`
}

// listDeposits serves the one cursor in the system.
//
// Deposits are unsolicited — an app cannot know one is coming — so it needs
// "what is new since I last looked". The cursor is the chain's own ordering,
// (block, log_index): it sorts identically to the chain, is verifiable against a
// block explorer, and every deposit has one by construction, being a Transfer
// log. Apps should treat it as opaque and ordered: pass it back, compare it,
// don't parse it.
//
// ?status=confirmed instead returns what is still awaiting a drain.
func (s *Server) listDeposits(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if q.Get("status") == "confirmed" {
		return s.listOpenDeposits(w, r, limit)
	}
	if s := q.Get("status"); s != "" {
		return fail(http.StatusBadRequest, "bad_status", "the only supported status filter is confirmed")
	}

	since, err := store.ParseCursor(q.Get("since"))
	if err != nil {
		return fail(http.StatusBadRequest, "bad_cursor", "%v", err)
	}

	var (
		out    []depositView
		cursor store.Cursor
	)
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		list, next, err := tx.DepositsSince(a.Slug, since, limit)
		if err != nil {
			return err
		}
		cursor = next
		out, err = viewDeposits(tx, list)
		return err
	}); err != nil {
		return err
	}
	// The cursor comes back unchanged when nothing is new, so an app that keeps
	// passing it never loses its place.
	writeJSON(w, http.StatusOK, map[string]any{"deposits": out, "cursor": cursor.String()})
	return nil
}

func (s *Server) listOpenDeposits(w http.ResponseWriter, r *http.Request, limit int) error {
	var out []depositView
	if err := s.store.View(func(tx *store.Tx) error {
		a, err := s.app(tx, r)
		if err != nil {
			return err
		}
		list, err := tx.OpenDeposits(a.Slug, limit)
		if err != nil {
			return err
		}
		out, err = viewDeposits(tx, list)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"deposits": out})
	return nil
}

// viewDeposits renders deposits, resolving each wallet's ref so an app sees its
// own handle rather than an address it never chose.
func viewDeposits(tx *store.Tx, list []store.Deposit) ([]depositView, error) {
	out := make([]depositView, 0, len(list))
	refs := make(map[string]string, len(list))
	for _, d := range list {
		ref, ok := refs[d.Wallet.String()]
		if !ok {
			w, found, err := tx.Wallet(d.Wallet)
			if err != nil {
				return nil, err
			}
			if found {
				ref = w.Ref
			}
			refs[d.Wallet.String()] = ref
		}
		out = append(out, depositView{
			Cursor: d.Cursor().String(), Block: d.Block, LogIndex: d.LogIndex,
			TxHash: hashStr(d.TxHash), Ref: ref, From: d.From.Hex(),
			AmountCents: d.Cents.String(), AmountWei: str(d.AmountWei), Status: d.Status.String(),
			DrainTx: hashStr(d.DrainTx), CreatedAt: stamp(d.CreatedAt),
		})
	}
	return out, nil
}
