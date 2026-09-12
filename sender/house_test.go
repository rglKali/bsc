package sender

import (
	"math/big"
	"testing"
	"time"

	"bsc/flow"
	"bsc/money"
	"bsc/store"
)

// seedApp records an app owing `owed` cents, with `held` wei actually in its hot
// wallet. At this fixture's scale a cent is a wei, so the gap between the two
// figures is the house's claim, readable directly (§25).
func (f *fixture) seedApp(owed int64, held int64) {
	f.t.Helper()
	f.update(func(tx *store.Tx) error {
		if err := tx.PutApp(store.App{
			Slug: "df", Wallet: f.top.ID, CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		_, err := tx.CreditLedger("df", money.Cents(owed))
		return err
	})
	f.chain.balance[f.top.Address] = big.NewInt(held)
}

func TestSweepHouseMovesOnlyTheExcess(t *testing.T) {
	f := newFixture(t)
	f.seedApp(1000, 1150) // owed $10.00, holding $11.50
	f.begin(store.FlowHouseSweep, f.top, store.StateSweepingHouse,
		flow.Params{To: f.s.opts.FeeCollector})

	f.step()

	from, to, amount := decodeTransferFrom(t, f.chain.lastSent(t).Data())
	if from != f.top.Address || to != f.s.opts.FeeCollector {
		t.Fatalf("transferFrom(%s -> %s), want the hot wallet to the collector", from.Hex(), to.Hex())
	}
	if amount.Cmp(big.NewInt(150)) != 0 {
		t.Fatalf("swept %s, want only the 150 nobody is owed", amount)
	}
}

func TestSweepHouseSkipsWhenNothingIsOver(t *testing.T) {
	// The wallet holds exactly what the app is owed. Spending gas to prove it
	// would be a transfer that moves nothing.
	f := newFixture(t)
	f.seedApp(1000, 1000)
	fl := f.begin(store.FlowHouseSweep, f.top, store.StateSweepingHouse,
		flow.Params{To: f.s.opts.FeeCollector})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatalf("broadcast a sweep with no excess: %+v", f.chain.sent)
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("a skipped sweep should have advanced to completion")
	}
}

// TestSweepHouseRefusesWhileInsolvent is the guard that matters most here: if a
// wallet holds less than its app is owed, taking money out of it is the last
// thing to do. The sweep is the one operation whose amount is computed over
// somebody else's money, so it fails loudly rather than guessing (§25).
func TestSweepHouseRefusesWhileInsolvent(t *testing.T) {
	f := newFixture(t)
	f.seedApp(1000, 900)
	fl := f.begin(store.FlowHouseSweep, f.top, store.StateSweepingHouse,
		flow.Params{To: f.s.opts.FeeCollector})

	f.step()

	if len(f.chain.sent) != 0 {
		t.Fatalf("swept while insolvent: %+v", f.chain.sent)
	}
	if _, found := f.flow(fl.ID); found {
		t.Fatal("the flow should have failed rather than waiting")
	}
}

// TestSweepHouseReadsTheChainNotOurRecord: custody is whatever balanceOf says at
// signing time. Our materialised figure is a cache, and acting on it here would
// mean sweeping money that is no longer there — or leaving money that is.
func TestSweepHouseReadsTheChainNotOurRecord(t *testing.T) {
	f := newFixture(t)
	f.seedApp(1000, 1150)
	// Our record disagrees with the chain: it thinks the wallet is empty.
	f.update(func(tx *store.Tx) error {
		_, _, err := tx.Debit(f.top.ID, big.NewInt(1150))
		return err
	})
	f.begin(store.FlowHouseSweep, f.top, store.StateSweepingHouse,
		flow.Params{To: f.s.opts.FeeCollector})

	f.step()

	_, _, amount := decodeTransferFrom(t, f.chain.lastSent(t).Data())
	if amount.Cmp(big.NewInt(150)) != 0 {
		t.Fatalf("swept %s, want the 150 the chain actually holds over the ledger", amount)
	}
}
