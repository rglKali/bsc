package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAppRoundTripAndListing(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	seedApp(t, s, "lkr:acme", 0)

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.App("df")
		if err != nil || !ok {
			t.Fatalf("App: ok=%v err=%v", ok, err)
		}
		if got.Slug != "df" || got.Fee.Flat != 100 {
			t.Fatalf("got %+v", got)
		}
		if _, ok, err := tx.App("nope"); err != nil || ok {
			t.Fatalf("unknown app resolved: ok=%v err=%v", ok, err)
		}
		all, err := tx.Apps()
		if err != nil {
			return err
		}
		if len(all) != 2 || all[0].Slug != "df" || all[1].Slug != "lkr:acme" {
			t.Fatalf("Apps = %v, want slug order", slugs(all))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestPutAppRejectsBadSlug(t *testing.T) {
	s := open(t)
	if err := s.Update(func(tx *Tx) error {
		return tx.PutApp(App{Slug: "Not A Slug", Wallet: uuid.New()})
	}); !errors.Is(err, ErrBadSlug) {
		t.Fatalf("got %v, want ErrBadSlug", err)
	}
}

func TestMutateAppConfiguresFeeAndStampsUpdate(t *testing.T) {
	// Apps configure themselves; there is no admin API and no operator floor on
	// the fee, so this path is the whole configuration mechanism.
	s := open(t)
	app, _ := seedApp(t, s, "df", 0)
	before := app.UpdatedAt

	update(t, s, func(tx *Tx) error {
		got, err := tx.MutateApp("df", func(a *App) error {
			a.Fee.Flat = 200
			a.Fee.Min = 1000
			a.Paused = true
			return nil
		})
		if err != nil {
			return err
		}
		if got.Fee.Flat != 200 || got.Fee.Min != 1000 || !got.Paused {
			t.Fatalf("got %+v", got)
		}
		if !got.UpdatedAt.After(before) {
			t.Fatalf("UpdatedAt not advanced: %v -> %v", before, got.UpdatedAt)
		}
		return nil
	})
}

func TestMutateAppRefusesSlugChangeAndMissingApp(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)

	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateApp("df", func(a *App) error {
			a.Slug = "other"
			return nil
		})
		return err
	})
	if !errors.Is(err, ErrImmutable) {
		t.Fatalf("slug change: got %v, want ErrImmutable", err)
	}

	err = s.Update(func(tx *Tx) error {
		_, err := tx.MutateApp("nope", func(*App) error { return nil })
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app: got %v, want ErrNotFound", err)
	}
}

func TestMutateAppPropagatesCallerError(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	sentinel := errors.New("policy rejected")
	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateApp("df", func(*App) error { return sentinel })
		return err
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the caller's error", err)
	}
}

func TestSetActive(t *testing.T) {
	// Once a wallet has approved the master, later flows skip straight past
	// funding and approving — so this flag decides how much gas a drain costs.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x80, 0)
	if w.Active {
		t.Fatal("a fresh deposit wallet must not be active")
	}
	update(t, s, func(tx *Tx) error {
		got, err := tx.SetActive(w.ID, true)
		if err != nil {
			return err
		}
		if !got.Active {
			t.Fatal("SetActive(true) did not take")
		}
		if got, err = tx.SetActive(w.ID, false); err != nil {
			return err
		}
		if got.Active {
			t.Fatal("SetActive(false) did not take")
		}
		return nil
	})
}

func TestDepositLookupByTransfer(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x81, 0)
	d := putDeposit(t, s, w, 100, 3, 0x92, 7)

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.Deposit(d.TxHash, d.LogIndex)
		if err != nil || !ok {
			t.Fatalf("Deposit: ok=%v err=%v", ok, err)
		}
		if got.AmountWei.Cmp(wei(7)) != 0 || got.Cents != 7 {
			t.Fatalf("amount = %s wei / %d cents, want 7 of each", got.AmountWei, got.Cents)
		}
		// Same transaction, a log index we never recorded.
		if _, ok, err := tx.Deposit(d.TxHash, 4); err != nil || ok {
			t.Fatalf("unknown log index resolved: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestFeePolicyZero(t *testing.T) {
	// An app that sets no policy simply does not charge its users.
	if !(FeePolicy{}).Zero() {
		t.Fatal("empty policy is not zero")
	}
	if !(FeePolicy{Min: 500}).Zero() {
		t.Fatal("a minimum with no fee should still be zero-charge")
	}
	if (FeePolicy{Flat: 100}).Zero() {
		t.Fatal("a flat fee reported as zero")
	}
	if (FeePolicy{BPS: 100}).Zero() {
		t.Fatal("a percentage fee reported as zero")
	}
}

func TestEnumLabels(t *testing.T) {
	// These strings surface in audit output and logs, so a wrong label would
	// mislead whoever is reading them during an incident.
	cases := []struct{ got, want string }{
		{KindTopLevel.String(), "top_level"},
		{KindDeposit.String(), "deposit"},
		{WalletKind(99).String(), "unknown"},
		{FlowDrain.String(), "drain"},
		{FlowWithdrawal.String(), "withdrawal"},
		{FlowPrewarm.String(), "prewarm"},
		{FlowKind(99).String(), "unknown"},
		{StateFunding.String(), "funding"},
		{StateApproving.String(), "approving"},
		{StateSweeping.String(), "sweeping"},
		{StatePaying.String(), "paying"},
		{StateSweepingHouse.String(), "sweeping_house"},
		{StateDone.String(), "done"},
		{StateFailed.String(), "failed"},
		{FlowState(99).String(), "unknown"},
		{DepositPending.String(), "pending"},
		{DepositCredited.String(), "credited"},
		{DepositStatus(99).String(), "unknown"},
		// Two states each, named for what happened to the ledger. There is no
		// withdrawal failure label because there is no failure state (§28).
		{WithdrawalPending.String(), "pending"},
		{WithdrawalDebited.String(), "debited"},
		{WithdrawalStatus(99).String(), "unknown"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("label = %q, want %q", c.got, c.want)
		}
	}
}

func TestFindingStringIncludesKindAndPlace(t *testing.T) {
	f := Finding{Kind: "reserve", Where: "wallet/0xabc", Detail: "stored 5, recomputed 0"}
	got := f.String()
	for _, want := range []string{"reserve", "wallet/0xabc", "recomputed 0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Finding.String() = %q, missing %q", got, want)
		}
	}
}

func TestStorePathIsTheOpenedFile(t *testing.T) {
	s := open(t)
	if !strings.HasSuffix(s.Path(), "bsc.db") {
		t.Fatalf("Path = %q", s.Path())
	}
}

func slugs(as []App) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Slug
	}
	return out
}
