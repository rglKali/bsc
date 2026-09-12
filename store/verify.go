package store

import (
	"fmt"
	"math/big"

	"bsc/money"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Finding is one internal inconsistency.
type Finding struct {
	Kind   string // reserve | index | ownership
	Where  string
	Detail string
}

func (f Finding) String() string { return fmt.Sprintf("[%s] %s: %s", f.Kind, f.Where, f.Detail) }

// Report is the result of Verify.
type Report struct {
	Apps        int
	Wallets     int
	Flows       int
	Deposits    int
	Withdrawals int
	Owed        money.Cents // the whole ledger: what every app is owed
	Findings    []Finding
}

// OK reports a clean audit.
func (r Report) OK() bool { return len(r.Findings) == 0 }

func (r *Report) flag(kind, where, detail string) {
	r.Findings = append(r.Findings, Finding{Kind: kind, Where: where, Detail: detail})
}

// Verify recomputes everything the store maintains incrementally and reports
// drift. It replaces a reconciler *process*: run on demand against a snapshot
// rather than forever in the background (§5).
//
// Since v2.1 that includes the balances. The ledger is a pure function of the
// log — credited deposits less settled withdrawals — so the materialised figure
// can be recomputed and compared, which is exactly what the old
// chain-materialised balance could never support (§22). What it still cannot do
// offline is compare against the token itself; `bsc inspect --rpc` adds that,
// and the solvency margin below is what makes that comparison meaningful.
func (t *Tx) Verify() (Report, error) {
	var rep Report

	apps, err := t.Apps()
	if err != nil {
		return rep, err
	}
	rep.Apps = len(apps)

	// An app's reserve lives entirely on its top-level wallet.
	topLevel := make(map[string]uuid.UUID, len(apps))
	for _, a := range apps {
		topLevel[a.Slug] = a.Wallet
		w, ok, err := t.Wallet(a.Wallet)
		if err != nil {
			return rep, err
		}
		switch {
		case !ok:
			rep.flag("index", "app/"+a.Slug, fmt.Sprintf("top-level wallet %s missing", a.Wallet))
		case w.Kind != KindTopLevel:
			rep.flag("index", "app/"+a.Slug, fmt.Sprintf("wallet %s is %s, not top_level", a.Wallet, w.Kind))
		case w.App != a.Slug:
			rep.flag("index", "app/"+a.Slug, fmt.Sprintf("wallet %s belongs to app %q", a.Wallet, w.App))
		}
	}

	// Recompute each app's ledger from the log. Credits are the deposits whose
	// drain has landed; debits are the withdrawals that settled. Reservations
	// are whatever is still open. All three are derived from records that are
	// never rewritten, so a disagreement here is drift in the materialised
	// figure and nothing else.
	type recomputed struct{ credited, debited, reserved money.Cents }
	ledger := make(map[string]*recomputed, len(apps))
	for _, a := range apps {
		ledger[a.Slug] = &recomputed{}
	}
	want := func(slug string) *recomputed {
		r, ok := ledger[slug]
		if !ok {
			r = &recomputed{}
			ledger[slug] = r
		}
		return r
	}

	if err := t.EachWallet(func(w Wallet) error {
		rep.Wallets++

		// The ownership pointer must agree with the flow it names.
		if !w.Idle() {
			f, ok, err := t.Flow(w.Flow)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				rep.flag("ownership", "wallet/"+w.Address.Hex(),
					fmt.Sprintf("points at missing flow %s", w.Flow))
			case f.Wallet != w.ID:
				rep.flag("ownership", "wallet/"+w.Address.Hex(),
					fmt.Sprintf("flow %s owns wallet %s", f.ID, f.Wallet))
			}
		}

		// Both lookup indexes must resolve back to this wallet.
		got := t.tx.Bucket(bAddr).Get(w.Address.Bytes())
		if !isID(got, w.ID) {
			rep.flag("index", "addr/"+w.Address.Hex(), "address index does not resolve to this wallet")
		}
		if w.Kind == KindDeposit {
			got := t.tx.Bucket(bRef).Get(scoped(w.App, []byte(w.Ref)))
			if !isID(got, w.ID) {
				rep.flag("index", "ref/"+w.App+"/"+w.Ref, "ref index does not resolve to this wallet")
			}
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Every live flow must be owned by the wallet it claims.
	if err := t.EachFlow(func(f Flow) error {
		rep.Flows++
		if f.State.IsTerminal() {
			rep.flag("ownership", "flow/"+f.ID.String(),
				fmt.Sprintf("terminal (%s) but not deleted", f.State))
		}
		w, ok, err := t.Wallet(f.Wallet)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			rep.flag("ownership", "flow/"+f.ID.String(), fmt.Sprintf("wallet %s missing", f.Wallet))
		case w.Flow != f.ID:
			rep.flag("ownership", "flow/"+f.ID.String(),
				fmt.Sprintf("wallet %s does not point back (points at %s)", w.Address.Hex(), w.Flow))
		}
		if f.Waiting() {
			if _, ok, err := t.TxRefByHash(f.Tx); err != nil {
				return err
			} else if !ok {
				rep.flag("index", "flow/"+f.ID.String(),
					fmt.Sprintf("awaiting tx %s with no watchlist entry", f.Tx.Hex()))
			}
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// The watchlist must name live flows.
	if err := t.EachWatchedTx(func(h common.Hash, ref TxRef) error {
		if _, ok, err := t.Flow(ref.Flow); err != nil {
			return err
		} else if !ok {
			rep.flag("index", "tx/"+h.Hex(), fmt.Sprintf("names missing flow %s", ref.Flow))
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Deposit indexes must resolve, and the open set must hold only uncredited rows.
	if err := scanPrefix(t, bDeposit, nil, func(_, v []byte) error {
		d, err := decodeDeposit(v)
		if err != nil {
			return err
		}
		rep.Deposits++
		if d.Cents <= 0 {
			rep.flag("ledger", "deposit/"+d.TxHash.Hex(),
				fmt.Sprintf("recorded with %d cents; sub-cent transfers must not be recorded at all", d.Cents))
		}
		if d.Status == DepositCredited {
			want(d.App).credited += d.Cents
		}
		key := depositKey(d.TxHash, d.LogIndex)
		if t.tx.Bucket(iDep).Get(scoped(d.App, d.Cursor().Key())) == nil {
			rep.flag("index", "deposit/"+d.TxHash.Hex(), "missing from the cursor index")
		}
		inOpen := t.tx.Bucket(iDepOpen).Get(scoped(d.App, d.Wallet[:], key)) != nil
		if want := d.Status == DepositConfirmed; inOpen != want {
			rep.flag("index", "deposit/"+d.TxHash.Hex(),
				fmt.Sprintf("status %s but open-set membership is %v", d.Status, inOpen))
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Withdrawal indexes must resolve, and the open set must exclude terminals.
	if err := scanPrefix(t, bWithdrawal, nil, func(_, v []byte) error {
		wd, err := decodeWithdrawal(v)
		if err != nil {
			return err
		}
		rep.Withdrawals++
		switch wd.Status {
		case WithdrawalDone:
			want(wd.App).debited += wd.Debit
		case WithdrawalQueued, WithdrawalPending:
			want(wd.App).reserved += wd.Debit
		}
		inOpen := t.tx.Bucket(iWdOpen).Get(scoped(wd.App, wd.ID[:])) != nil
		if want := !wd.Status.IsTerminal(); inOpen != want {
			rep.flag("index", "withdrawal/"+wd.ID.String(),
				fmt.Sprintf("status %s but open-set membership is %v", wd.Status, inOpen))
		}
		if wd.IdempotencyKey != "" {
			got := t.tx.Bucket(iWdIdem).Get(scoped(wd.App, []byte(wd.IdempotencyKey)))
			if !isID(got, wd.ID) {
				rep.flag("index", "withdrawal/"+wd.ID.String(), "idempotency key does not resolve back")
			}
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// The ledger comparison happens last, once both logs have been walked.
	for _, a := range apps {
		r := want(a.Slug)
		if got, expect := a.Ledger, r.credited-r.debited; got != expect {
			rep.flag("ledger", "app/"+a.Slug,
				fmt.Sprintf("ledger %d, recomputed %d (credited %d, debited %d)",
					got, expect, r.credited, r.debited))
		}
		if a.Reserved != r.reserved {
			rep.flag("ledger", "app/"+a.Slug,
				fmt.Sprintf("reserved %d, recomputed %d from open withdrawals", a.Reserved, r.reserved))
		}
		if a.Reserved > a.Ledger {
			rep.flag("ledger", "app/"+a.Slug,
				fmt.Sprintf("reserved %d exceeds the ledger %d", a.Reserved, a.Ledger))
		}
		if a.Ledger < 0 {
			rep.flag("ledger", "app/"+a.Slug, fmt.Sprintf("negative ledger %d", a.Ledger))
		}
		rep.Owed += a.Ledger
	}

	// Solvency: the hot wallet must hold at least what its app is owed. This is
	// the check the design exists to make possible — the difference is the
	// house's claim, and it must never be negative (§25). It needs the token's
	// decimals, which the service records on first run.
	if m, ok, err := t.Meta(); err != nil {
		return rep, err
	} else if ok {
		scale, err := money.NewScale(m.Decimals)
		if err != nil {
			return rep, err
		}
		for _, a := range apps {
			w, found, err := t.Wallet(a.Wallet)
			if err != nil {
				return rep, err
			}
			if !found {
				continue // already flagged above
			}
			if excess := scale.Excess(w.Balance, a.Ledger); excess.Sign() < 0 {
				rep.flag("solvency", "app/"+a.Slug,
					fmt.Sprintf("wallet %s holds %s wei but the app is owed %s (%s cents) — short by %s",
						w.Address.Hex(), orZero(w.Balance), scale.Wei(a.Ledger), a.Ledger,
						new(big.Int).Neg(excess)))
			}
		}
	}

	return rep, nil
}

// isID compares an index value against an expected id without assuming the
// stored value is well-formed — an audit must survive the corruption it hunts.
func isID(v []byte, want uuid.UUID) bool {
	return len(v) == len(want) && uuid.UUID(v[:len(want)]) == want
}
