package flow

import (
	"math/big"
	"testing"
	"time"

	"bsc/store"

	"github.com/google/uuid"
)

var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// proxy is a wallet that forwards what it receives: the drain candidate.
func proxy(balance int64) store.Wallet {
	return store.Wallet{
		ID: uuid.New(), Ref: "cust-1", Kind: store.KindManaged,
		Address: addr(1), DrainTo: addr(2), Active: true,
		Balance: big.NewInt(balance),
	}
}

// hot is a wallet that accumulates: the payout candidate.
func hot(balance int64) store.Wallet {
	return store.Wallet{
		ID: uuid.New(), Ref: "hot", Kind: store.KindManaged,
		Address: addr(2), Active: true, Balance: big.NewInt(balance),
	}
}

// The kind check became a DrainTo check, and it is load-bearing either way: a
// wallet with nowhere to drain to would otherwise be told to move its balance
// to itself, forever (§32).
func TestShouldDrainNeverTouchesAnAccumulatingWallet(t *testing.T) {
	w := hot(1_000)
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("a wallet with no drain_to was told to drain")
	}
}

func TestShouldDrainNeverTouchesTheMaster(t *testing.T) {
	w := proxy(1_000)
	w.Kind = store.KindMaster
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("the master was told to drain")
	}
}

// The threshold is pure gas economics — moving three cents costs more than
// three cents — and decides only whether the move is worth paying for.
func TestShouldDrainThreshold(t *testing.T) {
	threshold := big.NewInt(1_000)
	cases := []struct {
		balance int64
		want    bool
	}{
		{0, false},
		{999, false},
		{1_000, true},
		{5_000, true},
	}
	for _, c := range cases {
		if got := ShouldDrain(proxy(c.balance), threshold, now); got != c.want {
			t.Fatalf("balance %d: ShouldDrain = %v, want %v", c.balance, got, c.want)
		}
	}
}

func TestShouldDrainWaitsForABusyWallet(t *testing.T) {
	w := proxy(5_000)
	w.Flow = uuid.New()
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("a wallet already running a flow was given another")
	}
}

// Without a backoff, a declarative rule plus a flow that can fail is a retry
// loop that burns gas as fast as the chain produces blocks.
func TestShouldDrainHonoursBackoff(t *testing.T) {
	w := proxy(5_000)
	w.RetryAfter = now.Add(time.Minute)
	if ShouldDrain(w, big.NewInt(1), now) {
		t.Fatal("drained before the retry deadline")
	}
	if !ShouldDrain(w, big.NewInt(1), now.Add(2*time.Minute)) {
		t.Fatal("still backed off after the deadline passed")
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	if got := RetryDelay(0); got != 0 {
		t.Fatalf("RetryDelay(0) = %v, want 0", got)
	}
	if got := RetryDelay(1); got != backoffBase {
		t.Fatalf("RetryDelay(1) = %v, want %v", got, backoffBase)
	}
	if got := RetryDelay(2); got != 2*backoffBase {
		t.Fatalf("RetryDelay(2) = %v, want %v", got, 2*backoffBase)
	}
	// The cap matters as much as the growth: a wallet that can never drain must
	// keep trying occasionally rather than hammering or giving up.
	if got := RetryDelay(64); got != backoffMax {
		t.Fatalf("RetryDelay(64) = %v, want the cap %v", got, backoffMax)
	}
}

func TestShouldPay(t *testing.T) {
	cases := map[string]struct {
		wallet     store.Wallet
		hasPending bool
		want       bool
	}{
		"ready":              {hot(10_000), true, true},
		"nothing pending":    {hot(10_000), false, false},
		"paused":             {pausedHot(), true, false},
		"busy":               {busyHot(), true, false},
		"forwards elsewhere": {proxy(10_000), true, false},
		"master":             {masterWallet(), true, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ShouldPay(c.wallet, c.hasPending, now); got != c.want {
				t.Fatalf("ShouldPay = %v, want %v", got, c.want)
			}
		})
	}
}

func pausedHot() store.Wallet {
	w := hot(10_000)
	w.Paused = true
	return w
}

func busyHot() store.Wallet {
	w := hot(10_000)
	w.Flow = uuid.New()
	return w
}

func masterWallet() store.Wallet {
	w := hot(10_000)
	w.Kind = store.KindMaster
	return w
}

// A withdrawal has no failure state: a reverted payout stays pending for this
// rule to pick up again, so without the gate a payout that always reverts would
// be re-signed on every evaluation (§28).
func TestShouldPayWaitsOutTheBackoff(t *testing.T) {
	w := hot(10_000)
	w.RetryAfter = now.Add(time.Minute)
	if ShouldPay(w, true, now) {
		t.Fatal("paid before the retry deadline")
	}
	if !ShouldPay(w, true, now.Add(2*time.Minute)) {
		t.Fatal("still backed off after the deadline passed")
	}
}

func TestRulesTolerateNilAmounts(t *testing.T) {
	w := proxy(0)
	w.Balance = nil
	if ShouldDrain(w, nil, now) {
		t.Fatal("a nil balance was treated as drainable")
	}
	if ShouldDrain(proxy(5), nil, now) != true {
		t.Fatal("a nil threshold should not block a positive balance")
	}
}
