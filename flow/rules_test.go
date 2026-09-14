package flow

import (
	"math/big"
	"testing"
	"time"

	"bsc/store"

	"github.com/google/uuid"
)

var now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func depositWallet(balance int64) store.Wallet {
	return store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindDeposit, Ref: "cust-1",
		Address: addr(0x20), Balance: big.NewInt(balance),
	}
}

func topWallet(balance int64) store.Wallet {
	return store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindTopLevel,
		Address: addr(0x01), Balance: big.NewInt(balance),
	}
}

func TestShouldDrainNeverTouchesATopLevelWallet(t *testing.T) {
	// An app's top-level holds its money and also clears the threshold. An
	// unscoped rule would keep trying to drain it to itself, forever.
	top := topWallet(1_000_000)
	if ShouldDrain(top, big.NewInt(1), now) {
		t.Fatal("a top-level wallet was selected for draining")
	}
}

func TestShouldDrainThreshold(t *testing.T) {
	min := big.NewInt(100)
	tests := map[string]struct {
		balance int64
		want    bool
	}{
		"empty":             {0, false},
		"dust":              {1, false},
		"just below":        {99, false},
		"exactly at":        {100, true},
		"comfortably above": {5_000, true},
	}
	for name, tc := range tests {
		if got := ShouldDrain(depositWallet(tc.balance), min, now); got != tc.want {
			t.Fatalf("%s (balance %d): got %v, want %v", name, tc.balance, got, tc.want)
		}
	}
}

func TestShouldDrainAppliesToTheAggregateBalance(t *testing.T) {
	// The threshold gates the wallet's total, not any single transfer, so dust
	// that was too small to record still leaves with the next real deposit.
	w := depositWallet(150) // e.g. three ignored 50-wei transfers
	if !ShouldDrain(w, big.NewInt(100), now) {
		t.Fatal("aggregated dust past the threshold was not drained")
	}
}

func TestShouldDrainWaitsForABusyWallet(t *testing.T) {
	// A deposit landing mid-drain must start nothing; the evaluation after the
	// live flow terminates is what picks up the leftover.
	w := depositWallet(1_000)
	w.Flow = uuid.New()
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("a wallet already owned by a flow was selected again")
	}
}

func TestShouldDrainHonoursBackoff(t *testing.T) {
	// Without this, a declarative rule plus a failing flow is a retry loop that
	// burns gas as fast as the chain allows.
	w := depositWallet(1_000)
	w.RetryAfter = now.Add(time.Minute)
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("drained while still backing off")
	}
	if !ShouldDrain(w, big.NewInt(1), now.Add(time.Minute)) {
		t.Fatal("did not drain once the backoff expired")
	}
	if !ShouldDrain(w, big.NewInt(1), now.Add(time.Hour)) {
		t.Fatal("did not drain well after the backoff expired")
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	if got := RetryDelay(0); got != 0 {
		t.Fatalf("RetryDelay(0) = %v, want 0", got)
	}
	first, second, third := RetryDelay(1), RetryDelay(2), RetryDelay(3)
	if first != backoffBase {
		t.Fatalf("RetryDelay(1) = %v, want %v", first, backoffBase)
	}
	if second != 2*first || third != 4*first {
		t.Fatalf("backoff does not double: %v, %v, %v", first, second, third)
	}
	// A wallet that can never drain must keep retrying occasionally rather than
	// hammering or giving up.
	for _, attempts := range []uint32{20, 50, 1000} {
		if got := RetryDelay(attempts); got != backoffMax {
			t.Fatalf("RetryDelay(%d) = %v, want the cap %v", attempts, got, backoffMax)
		}
	}
}

func TestShouldPay(t *testing.T) {
	app := store.App{Slug: "df"}
	top := topWallet(1_000)

	if !ShouldPay(app, top, true, now) {
		t.Fatal("an idle top-level with a pending withdrawal was not selected")
	}
	if ShouldPay(app, top, false, now) {
		t.Fatal("selected with nothing pending")
	}

	busy := top
	busy.Flow = uuid.New()
	if ShouldPay(app, busy, true, now) {
		t.Fatal("selected while another flow owns the wallet")
	}

	// Withdrawals for one app serialize, which is what keeps the reserve
	// arithmetic obvious.
	paused := store.App{Slug: "df", Paused: true}
	if ShouldPay(paused, top, true, now) {
		t.Fatal("a paused app was allowed to pay out")
	}

	if ShouldPay(app, depositWallet(1_000), true, now) {
		t.Fatal("a deposit wallet was selected to pay out")
	}
}

// A withdrawal has no failure state: a reverted payout stays pending for this
// rule to pick up again (§28). Without the backoff gate that retry fires on
// every evaluation, so a payout that always reverts would burn the master's gas
// as fast as blocks arrive.
func TestShouldPayWaitsOutTheBackoff(t *testing.T) {
	app := store.App{Slug: "df"}
	backedOff := topWallet(1_000)
	backedOff.RetryAfter = now.Add(time.Minute)

	if ShouldPay(app, backedOff, true, now) {
		t.Fatal("re-signed a payout while the wallet was still backed off")
	}
	if !ShouldPay(app, backedOff, true, now.Add(2*time.Minute)) {
		t.Fatal("the backoff never expired")
	}
}

func TestRulesTolerateNilAmounts(t *testing.T) {
	// Records decoded from an older version can carry nil money; predicates
	// must not panic on them.
	var w store.Wallet
	w.Kind = store.KindDeposit
	if ShouldDrain(w, nil, now) {
		t.Fatal("a zero wallet was selected for draining")
	}
}

func masterWallet(tokens int64) store.Wallet {
	return store.Wallet{
		ID: uuid.New(), Kind: store.KindMaster, Address: addr(0x01),
		Balance: big.NewInt(tokens),
	}
}

func TestShouldTopUpGas(t *testing.T) {
	floor, amount := big.NewInt(100), big.NewInt(10)
	master := masterWallet(50) // plenty of collected fees to trade

	if !ShouldTopUpGas(master, big.NewInt(99), floor, amount, true, now) {
		t.Fatal("did not top up below the floor")
	}
	if ShouldTopUpGas(master, big.NewInt(100), floor, amount, true, now) {
		t.Fatal("topped up at the floor; only below it should trigger")
	}
	if ShouldTopUpGas(master, big.NewInt(1_000), floor, amount, true, now) {
		t.Fatal("topped up with plenty of gas")
	}
}

func TestGasTopUpIsOffUnlessEnabled(t *testing.T) {
	// Swapping is the only thing the service does on its own initiative, so it
	// stays off until an operator configures a router deliberately.
	master := masterWallet(50)
	if ShouldTopUpGas(master, big.NewInt(0), big.NewInt(100), big.NewInt(10), false, now) {
		t.Fatal("swapped with swapping disabled")
	}
}

func TestGasTopUpNeedsFeesToTrade(t *testing.T) {
	// Trading more than we hold would simply revert and waste the gas we are
	// short of in the first place.
	poor := masterWallet(5)
	if ShouldTopUpGas(poor, big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now) {
		t.Fatal("tried to swap 10 while holding 5")
	}
	if !ShouldTopUpGas(masterWallet(10), big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now) {
		t.Fatal("holding exactly the swap amount should be enough")
	}
}

func TestGasTopUpHonoursTheCooldown(t *testing.T) {
	// The bound that matters: a swap which succeeds but does not lift the
	// balance above the floor must not re-fire and trade away every fee.
	master := masterWallet(1_000)
	master.RetryAfter = now.Add(time.Hour)
	if ShouldTopUpGas(master, big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now) {
		t.Fatal("swapped during the cooldown")
	}
	if !ShouldTopUpGas(master, big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now.Add(time.Hour)) {
		t.Fatal("did not swap once the cooldown expired")
	}
}

func TestGasTopUpOnlyEverTouchesTheMaster(t *testing.T) {
	// An app's hot wallet also holds tokens. Trading those away would be
	// spending an app's money on our gas.
	app := store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindTopLevel, Address: addr(0x02),
		Balance: big.NewInt(1_000_000),
	}
	if ShouldTopUpGas(app, big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now) {
		t.Fatal("selected an app's wallet for a gas swap")
	}
}

func TestGasTopUpWaitsForABusyMaster(t *testing.T) {
	master := masterWallet(1_000)
	master.Flow = uuid.New()
	if ShouldTopUpGas(master, big.NewInt(0), big.NewInt(100), big.NewInt(10), true, now) {
		t.Fatal("started a swap while another flow owned the master")
	}
}

// TestShouldSweepHouse covers the rule that collects everything nobody is owed.
func TestShouldSweepHouse(t *testing.T) {
	const min = 100
	idle := topWallet(0)

	if !ShouldSweepHouse(idle, big.NewInt(min), big.NewInt(min), false, now) {
		t.Fatal("exactly at the threshold should sweep")
	}
	if ShouldSweepHouse(idle, big.NewInt(min-1), big.NewInt(min), false, now) {
		t.Fatal("below the threshold should not spend a transaction")
	}

	// A queued payout is the more urgent use of a wallet that runs one flow at
	// a time; the excess is ours and can wait.
	if ShouldSweepHouse(idle, big.NewInt(min*10), big.NewInt(min), true, now) {
		t.Fatal("swept while the app had a payout queued")
	}

	// Zero threshold means sweeping is off, not "sweep everything".
	if ShouldSweepHouse(idle, big.NewInt(min*10), big.NewInt(0), false, now) {
		t.Fatal("swept with sweeping disabled")
	}

	// Deposit wallets are drained wholesale by the drain rule; their excess
	// arrives at the top-level wallet and is collected from there.
	if ShouldSweepHouse(depositWallet(0), big.NewInt(min*10), big.NewInt(min), false, now) {
		t.Fatal("house-swept a deposit wallet")
	}

	busy := topWallet(0)
	busy.Flow = uuid.New()
	if ShouldSweepHouse(busy, big.NewInt(min*10), big.NewInt(min), false, now) {
		t.Fatal("swept a wallet another flow owns")
	}

	backedOff := topWallet(0)
	backedOff.RetryAfter = now.Add(time.Minute)
	if ShouldSweepHouse(backedOff, big.NewInt(min*10), big.NewInt(min), false, now) {
		t.Fatal("swept during a backoff")
	}
}

// TestShouldSweepHouseIgnoresAShortfall: a negative excess means we hold less
// than we owe. Sweeping is the last thing to do about that.
func TestShouldSweepHouseIgnoresAShortfall(t *testing.T) {
	if ShouldSweepHouse(topWallet(0), big.NewInt(-500), big.NewInt(100), false, now) {
		t.Fatal("swept while insolvent")
	}
}
