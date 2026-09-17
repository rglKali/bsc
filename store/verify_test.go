package store

import (
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// verify runs the audit and fails the test if it cannot run at all. Findings
// are returned rather than failed on, because most tests here are about
// provoking one deliberately.
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

func findingsOfKind(rep Report, kind string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func mustBeClean(t *testing.T, rep Report) {
	t.Helper()
	for _, f := range rep.Findings {
		t.Errorf("unexpected finding: %s", f)
	}
}

func TestVerifyCleanStore(t *testing.T) {
	s := open(t)
	hot := seedWallet(t, s, "hot", 1000)
	seedProxy(t, s, "cust-1", hot.Address, 250)

	rep := verify(t, s)
	mustBeClean(t, rep)
	if rep.Wallets != 2 || rep.Proxies != 1 {
		t.Fatalf("wallets = %d (%d proxies), want 2 (1)", rep.Wallets, rep.Proxies)
	}
	if rep.Deposits != 2 {
		t.Fatalf("deposits = %d, want 2", rep.Deposits)
	}
	if rep.Held.Int64() != 1250 {
		t.Fatalf("held = %s, want 1250", rep.Held)
	}
}

func TestVerifyCountsCommittedAcrossPendingWithdrawals(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	update(t, s, func(tx *Tx) error {
		for _, amount := range []int64{100, 250} {
			if err := tx.PutWithdrawal(Withdrawal{
				ID: uuid.New(), Wallet: w.ID, Reason: ReasonPayout, Destination: addr(0x99),
				Amount: wei(amount), Status: WithdrawalPending, CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
		return nil
	})

	rep := verify(t, s)
	mustBeClean(t, rep)
	if rep.Committed.Int64() != 350 {
		t.Fatalf("committed = %s, want 350", rep.Committed)
	}
}

// A wallet that owes more than it holds is the invariant that replaced the
// per-app solvency margin. It is the same question asked of the thing that
// actually holds the money.
func TestVerifyFlagsAWalletOwingMoreThanItHolds(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 100)
	update(t, s, func(tx *Tx) error {
		return tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, Reason: ReasonPayout, Destination: addr(0x99),
			Amount: wei(500), Status: WithdrawalPending, CreatedAt: time.Now().UTC(),
		})
	})

	rep := verify(t, s)
	found := findingsOfKind(rep, "solvency")
	if len(found) != 1 {
		t.Fatalf("solvency findings = %d, want 1 (%v)", len(found), rep.Findings)
	}
	if !strings.Contains(found[0].Detail, "owes 500") {
		t.Fatalf("finding does not name the amount: %s", found[0].Detail)
	}
}

// A confirmed withdrawal is not a promise any more, so it must stop counting
// against the wallet.
func TestVerifyIgnoresConfirmedWithdrawals(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 100)
	update(t, s, func(tx *Tx) error {
		return tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, Reason: ReasonPayout, Destination: addr(0x99),
			Amount: wei(500), Status: WithdrawalConfirmed, TxHash: hash(0x11),
			CreatedAt: time.Now().UTC(),
		})
	})
	mustBeClean(t, verify(t, s))
}

func TestVerifyFlagsABrokenRefIndex(t *testing.T) {
	s := open(t)
	seedWallet(t, s, "hot", 0)
	update(t, s, func(tx *Tx) error {
		return tx.tx.Bucket(bRef).Delete([]byte("hot"))
	})

	if found := findingsOfKind(verify(t, s), "index"); len(found) != 1 {
		t.Fatalf("index findings = %d, want 1", len(found))
	}
}

func TestVerifyFlagsAWalletClaimingAFlowThatDoesNotExist(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 0)
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWallet(w.ID, func(mw *Wallet) error {
			mw.Flow = uuid.New()
			return nil
		})
		return err
	})

	found := findingsOfKind(verify(t, s), "ownership")
	if len(found) != 1 {
		t.Fatalf("ownership findings = %d, want 1 (%v)", len(found), verify(t, s).Findings)
	}
	if !strings.Contains(found[0].Detail, "does not exist") {
		t.Fatalf("unexpected detail: %s", found[0].Detail)
	}
}

func TestVerifyFlagsAFlowWhoseWalletClaimsAnother(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 0)
	other := uuid.New()
	update(t, s, func(tx *Tx) error {
		if err := tx.PutFlow(Flow{
			ID: other, Kind: FlowPrewarm, State: StateFunding, Wallet: w.ID,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		// The wallet is left idle: claiming is the half that was skipped.
		return nil
	})

	if found := findingsOfKind(verify(t, s), "ownership"); len(found) != 1 {
		t.Fatalf("ownership findings = %d, want 1", len(found))
	}
}

// A drain cycle is refused at write time, so reaching the audit means a record
// was edited outside the service. It is the failure that costs money silently:
// every hop succeeds, forever.
func TestVerifyFlagsADrainCycleWrittenBehindItsBack(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x0A), addr(0x0B), 0)
	b := seedWalletAt(t, s, "b", addr(0x0B), common.Address{}, 0)

	// Close the loop by writing the record directly, bypassing MutateWallet.
	update(t, s, func(tx *Tx) error {
		b.DrainTo = a.Address
		return put(tx, bWallet, b.ID[:], b.encode)
	})

	found := findingsOfKind(verify(t, s), "topology")
	if len(found) == 0 {
		t.Fatalf("no topology finding for a cycle (%v)", verify(t, s).Findings)
	}
}

func TestVerifyFlagsADepositMissingFromTheOpenSet(t *testing.T) {
	s := open(t)
	hot := seedWallet(t, s, "hot", 0)
	p := seedProxy(t, s, "cust-1", hot.Address, 500)

	// Drop the open-index entry without changing the record's status.
	update(t, s, func(tx *Tx) error {
		return scanPrefix(tx, iDepOpen, walletPrefix(p.ID), func(k, _ []byte) error {
			return tx.tx.Bucket(iDepOpen).Delete(append([]byte(nil), k...))
		})
	})

	if found := findingsOfKind(verify(t, s), "index"); len(found) != 1 {
		t.Fatalf("index findings = %d, want 1 (%v)", len(found), verify(t, s).Findings)
	}
}

// A deposit on an accumulating wallet is already where it belongs, so it must
// never join the open set — nothing would ever take it out again.
func TestVerifyAcceptsDepositsOnAnAccumulatingWalletOutsideTheOpenSet(t *testing.T) {
	s := open(t)
	seedWallet(t, s, "hot", 750)

	rep := verify(t, s)
	mustBeClean(t, rep)
	if rep.Deposits != 1 {
		t.Fatalf("deposits = %d, want 1", rep.Deposits)
	}
}

func TestVerifyFlagsAnIdempotencyKeyPointingElsewhere(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, Reason: ReasonPayout, Destination: addr(0x99), Amount: wei(10),
			Status: WithdrawalPending, IdempotencyKey: "order-1", CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		other := uuid.New()
		return tx.tx.Bucket(iWdIdem).Put([]byte("order-1"), other[:])
	})

	if found := findingsOfKind(verify(t, s), "index"); len(found) != 1 {
		t.Fatalf("index findings = %d, want 1", len(found))
	}
}
