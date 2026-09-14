package api

import (
	"net/http"

	"bsc/store"
)

// depositView is one arrival, as a payments provider would describe it: your
// handle, an amount, whether you can spend it, and a hash to look up if you ever
// need to prove it happened.
//
// What is deliberately absent: the block, the log index, the wei amount and the
// hash of the drain that swept it. Those are how the money moved, not what
// happened to the app's balance, and an app that never sees them cannot come to
// depend on them (§27).
type depositView struct {
	ID          string `json:"id"` // opaque, ordered; also the pagination cursor
	TxHash      string `json:"tx_hash"`
	Ref         string `json:"ref"`
	From        string `json:"from"`
	AmountCents string `json:"amount_cents"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// listDeposits serves the one cursor in the system.
//
// Deposits are unsolicited — an app cannot know one is coming — so it needs
// "what is new since I last looked". Every deposit has a unique id by
// construction, and that id doubles as the cursor: pass the last one back as
// ?since. It is opaque and ordered — compare it, pass it back, don't parse it.
//
// ?status=pending instead returns what is not yet spendable.
func (s *Server) listDeposits(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if q.Get("status") == "pending" {
		return s.listOpenDeposits(w, r, limit)
	}
	if s := q.Get("status"); s != "" {
		return fail(http.StatusBadRequest, "bad_status", "the only supported status filter is pending")
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
			ID: d.Cursor().String(), TxHash: hashStr(d.TxHash), Ref: ref,
			From: d.From.Hex(), AmountCents: d.Cents.String(),
			Status: d.Status.String(), CreatedAt: stamp(d.CreatedAt),
		})
	}
	return out, nil
}
