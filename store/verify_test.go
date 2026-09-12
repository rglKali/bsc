package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// verify runs the audit and fails the test if it cannot run at all.
func verify(t *testing.T, s *Store) Report {
	t.Helper()
	var rep Report
	if err := s.View(func(tx *Tx) error {
		var err error
		rep, err = tx.Verify()
		return err
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

// findingsOfKind returns the findings of one kind, for asserting the audit
// spotted the specific thing we broke.
func findingsOfKind(rep Report, kind string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestVerifyCleanStore(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	w := depositWallet(t, s, "df", "cust-1", 0x70, 0)
	putDeposit(t, s, w, 100, 0, 0x81, 5)

	rep := verify(t, s)
	if !rep.OK() {
		t.Fatalf("clean store reported findings: %v", rep.Findings)
	}
	// One app; three wallets and two deposits because seedApp backs its
	// balance with a real credited deposit on its own address.
	if rep.Apps != 1 || rep.Wallets != 3 || rep.Deposits != 2 {
		t.Fatalf("counts = apps %d wallets %d deposits %d", rep.Apps, rep.Wallets, rep.Deposits)
	}
	if rep.Owed != 1000 {
		t.Fatalf("owed = %d, want the seeded 1000", rep.Owed)
	}
}

func TestVerifyTracksTheLedgerLifecycle(t *testing.T) {
	// The accounting property the whole design rests on: an app's balance is a
	// pure function of its log. Verify recomputes it from credited deposits and
	// settled withdrawals, so a clean report at each step is real evidence that
	// the materialised figure and the history agree (§22).
	s := open(t)
	app, _ := seedApp(t, s, "df", 1000)
	wd := newWithdrawal("df", 10, 1, "k-1") // payout 10, fee 1, debit 11

	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(wd); err != nil {
			return err
		}
		// One reservation covers payout and fee together: they leave the ledger
		// at the same moment now that the fee never moves on its own (§24).
		_, err := tx.ReserveLedger(app.Slug, wd.Debit)
		return err
	})

	if rep := verify(t, s); !rep.OK() {
		t.Fatalf("mid-flight reservation reported findings: %v", rep.Findings)
	}
	if got := appOf(t, s, "df"); got.Reserved != 11 || got.Spendable() != 989 {
		t.Fatalf("reserved %d spendable %d, want 11 and 989", got.Reserved, got.Spendable())
	}

	// The payout settles: release the reservation and debit the ledger by
	// exactly what the record says, so a fee-policy change cannot move it.
	update(t, s, func(tx *Tx) error {
		if _, err := tx.MutateWithdrawal(wd.ID, func(w *Withdrawal) error {
			w.Status = WithdrawalDone
			w.TxHash = hash(1)
			return nil
		}); err != nil {
			return err
		}
		if _, err := tx.ReleaseLedger("df", wd.Debit); err != nil {
			return err
		}
		_, err := tx.DebitLedger("df", wd.Debit)
		return err
	})
	if rep := verify(t, s); !rep.OK() {
		t.Fatalf("after settlement: %v", rep.Findings)
	}

	// Payout and fee are both gone from the ledger. Only the payout moved
	// on-chain; the fee is still in the wallet, now belonging to the house.
	got := appOf(t, s, "df")
	if got.Ledger != 989 || got.Reserved != 0 {
		t.Fatalf("ledger %d reserved %d, want 989 and 0", got.Ledger, got.Reserved)
	}
}

func TestVerifyCatchesDriftedReserve(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	// A reservation with no open withdrawal to justify it — exactly the drift a
	// background reconciler would have hunted for.
	update(t, s, func(tx *Tx) error {
		_, err := tx.ReserveLedger("df", 25)
		return err
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "ledger")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "recomputed 0") {
		t.Fatalf("ledger findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesADriftedLedger(t *testing.T) {
	// The check the old audit could not make at all: the balance itself. A
	// ledger that does not match its own log is drift, and now it is visible.
	s := open(t)
	seedApp(t, s, "df", 1000)
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateApp("df", func(a *App) error {
			a.Ledger += 500 // money from nowhere
			return nil
		})
		return err
	})

	found := findingsOfKind(verify(t, s), "ledger")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "recomputed 1000") {
		t.Fatalf("ledger findings = %v", found)
	}
}

func TestVerifyCatchesReserveExceedingTheLedger(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 10)
	wd := newWithdrawal("df", 10, 0, "k-1")
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(wd); err != nil {
			return err
		}
		if _, err := tx.ReserveLedger("df", 10); err != nil {
			return err
		}
		// Simulate the ledger moving out from under an existing reservation.
		_, err := tx.MutateApp("df", func(a *App) error {
			a.Ledger = 4
			return nil
		})
		return err
	})

	rep := verify(t, s)
	if len(findingsOfKind(rep, "ledger")) == 0 {
		t.Fatalf("audit missed a reservation above the ledger: %v", rep.Findings)
	}
}

// TestVerifyCatchesInsolvency is the check the whole change exists to make
// possible: a hot wallet that holds less than its app is owed. It needs the
// token's decimals, which the service records on first run — here two, so a
// cent is a wei and the arithmetic is visible.
func TestVerifyCatchesInsolvency(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	update(t, s, func(tx *Tx) error {
		return tx.SetMeta(Meta{Token: addr(0x77), Decimals: 2})
	})
	if rep := verify(t, s); !rep.OK() {
		t.Fatalf("a solvent app reported findings: %v", rep.Findings)
	}

	// The money leaves without the ledger being told.
	update(t, s, func(tx *Tx) error {
		app, _, err := tx.App("df")
		if err != nil {
			return err
		}
		_, _, err = tx.Debit(app.Wallet, wei(600))
		return err
	})

	found := findingsOfKind(verify(t, s), "solvency")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "short by 600") {
		t.Fatalf("solvency findings = %v", found)
	}
}

// appOf reads an app back, for asserting on the ledger it carries.
func appOf(t *testing.T, s *Store, slug string) App {
	t.Helper()
	var out App
	if err := s.View(func(tx *Tx) error {
		a, ok, err := tx.App(slug)
		if err != nil || !ok {
			t.Fatalf("App(%q): ok=%v err=%v", slug, ok, err)
		}
		out = a
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	return out
}

func TestVerifyCatchesDanglingWalletOwnership(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x71, 0)
	// Point the wallet at a flow that does not exist.
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWallet(w.ID, func(w *Wallet) error {
			w.Flow = uuid.New()
			return nil
		})
		return err
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "ownership")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "missing flow") {
		t.Fatalf("ownership findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesOneWayOwnership(t *testing.T) {
	// A flow claiming a wallet that does not point back would let a second flow
	// start on the same wallet — the failure the pointer exists to prevent.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x72, 0)
	update(t, s, func(tx *Tx) error {
		return tx.PutFlow(Flow{
			ID: uuid.New(), Kind: FlowDrain, State: StateSweeping, Wallet: w.ID, App: "df",
		})
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "ownership")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "does not point back") {
		t.Fatalf("ownership findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesUndeletedTerminalFlow(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x73, 0)
	f := startFlow(t, s, w, FlowDrain, StateSweeping)
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateFlow(f.ID, func(f *Flow) error {
			f.State = StateDone
			return nil
		})
		return err
	})

	rep := verify(t, s)
	var terminal bool
	for _, fi := range rep.Findings {
		if strings.Contains(fi.Detail, "not deleted") {
			terminal = true
		}
	}
	if !terminal {
		t.Fatalf("audit missed a terminal flow left in the live set: %v", rep.Findings)
	}
}

func TestVerifyCatchesFlowAwaitingAnUnwatchedTx(t *testing.T) {
	// A flow waiting on a hash with no watchlist entry would wait forever: the
	// confirmation could never be routed back to it.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x74, 0)
	f := startFlow(t, s, w, FlowDrain, StateSweeping)
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateFlow(f.ID, func(f *Flow) error {
			f.Tx = hash(0x99)
			return nil
		})
		return err
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "index")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "no watchlist entry") {
		t.Fatalf("index findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesStaleWatchlistEntry(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	update(t, s, func(tx *Tx) error {
		return tx.LinkTx(hash(0x55), TxRef{Flow: uuid.New(), Signer: addr(1), Nonce: 1})
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "index")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "missing flow") {
		t.Fatalf("index findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesOpenSetMismatch(t *testing.T) {
	// Written straight to the record bucket, bypassing index maintenance —
	// simulating exactly the bug hand-rolled indexes invite.
	s := open(t)
	seedApp(t, s, "df", 1000)
	wd := newWithdrawal("df", 10, 1, "k-1")
	wd.Status = WithdrawalDone
	update(t, s, func(tx *Tx) error {
		if err := put(tx, bWithdrawal, wd.ID[:], wd.encode); err != nil {
			return err
		}
		return tx.tx.Bucket(iWdOpen).Put(scoped(wd.App, wd.ID[:]), nil)
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "index")
	if len(found) == 0 || !strings.Contains(found[0].Detail, "open-set membership") {
		t.Fatalf("index findings = %v", rep.Findings)
	}
}

func TestVerifyCatchesDepositMissingFromTheCursorIndex(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x75, 0)
	d := Deposit{
		Wallet: w.ID, App: "df", Block: 100, LogIndex: 0, TxHash: hash(0x91),
		AmountWei: wei(5), Cents: 5, Status: DepositConfirmed, CreatedAt: time.Now().UTC(),
	}
	// Record with no index entries at all.
	update(t, s, func(tx *Tx) error {
		return put(tx, bDeposit, depositKey(d.TxHash, d.LogIndex), d.encode)
	})

	rep := verify(t, s)
	var cursorMissing, openMissing bool
	for _, f := range rep.Findings {
		if strings.Contains(f.Detail, "cursor index") {
			cursorMissing = true
		}
		if strings.Contains(f.Detail, "open-set membership") {
			openMissing = true
		}
	}
	if !cursorMissing || !openMissing {
		t.Fatalf("audit missed unindexed deposit: %v", rep.Findings)
	}
}

func TestVerifyCatchesBrokenAppTopLevel(t *testing.T) {
	s := open(t)
	update(t, s, func(tx *Tx) error {
		return tx.PutApp(App{Slug: "df", Wallet: uuid.New(), CreatedAt: time.Now().UTC()})
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "index")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "missing") {
		t.Fatalf("index findings = %v", rep.Findings)
	}
}
