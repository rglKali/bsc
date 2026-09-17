package store

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

func TestWalletLookupIndexes(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)

	if err := s.View(func(tx *Tx) error {
		byID, ok, err := tx.Wallet(w.ID)
		if err != nil || !ok {
			t.Fatalf("by id: %v ok=%v", err, ok)
		}
		byAddr, ok, err := tx.WalletByAddress(w.Address)
		if err != nil || !ok {
			t.Fatalf("by address: %v ok=%v", err, ok)
		}
		byRef, ok, err := tx.WalletByRef("cust-1")
		if err != nil || !ok {
			t.Fatalf("by ref: %v ok=%v", err, ok)
		}
		if byID.ID != w.ID || byAddr.ID != w.ID || byRef.ID != w.ID {
			t.Fatal("the three lookups disagree")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Refs are unique across the service now, not per app. Nothing scopes them any
// more, so the caller owns the whole namespace (§37).
func TestRefsAreUniqueAcrossTheWholeService(t *testing.T) {
	s := open(t)
	seedWallet(t, s, "cust-1", 0)

	second := Wallet{
		ID: uuid.New(), Ref: "cust-1", Kind: KindManaged, Address: addr(0x33),
		Balance: new(big.Int), CreatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error { return tx.PutWallet(second) })

	// The later write wins the ref, which is why callers must namespace.
	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.WalletByRef("cust-1")
		if err != nil || !ok {
			t.Fatalf("lookup: %v ok=%v", err, ok)
		}
		if got.ID != second.ID {
			t.Fatal("ref did not resolve to the most recent writer")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPutWalletValidates(t *testing.T) {
	s := open(t)
	cases := map[string]Wallet{
		"no id":           {Ref: "a", Kind: KindManaged, Address: addr(1)},
		"no address":      {ID: uuid.New(), Ref: "a", Kind: KindManaged},
		"no ref":          {ID: uuid.New(), Kind: KindManaged, Address: addr(1)},
		"bad ref":         {ID: uuid.New(), Ref: "Cust 1", Kind: KindManaged, Address: addr(1)},
		"master with ref": {ID: uuid.New(), Ref: "m", Kind: KindMaster, Address: addr(1)},
		"master draining": {ID: uuid.New(), Kind: KindMaster, Address: addr(1), DrainTo: addr(2)},
		"drains to self":  {ID: uuid.New(), Ref: "a", Kind: KindManaged, Address: addr(1), DrainTo: addr(1)},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Update(func(tx *Tx) error { return tx.PutWallet(w) }); err == nil {
				t.Fatal("accepted an invalid wallet")
			}
		})
	}
}

func TestMutateWalletRefusesIndexedFields(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)

	cases := map[string]func(*Wallet){
		"id":      func(x *Wallet) { x.ID = uuid.New() },
		"ref":     func(x *Wallet) { x.Ref = "other" },
		"address": func(x *Wallet) { x.Address = addr(0x44) },
		"kind":    func(x *Wallet) { x.Kind = KindMaster },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(func(tx *Tx) error {
				_, err := tx.MutateWallet(w.ID, func(x *Wallet) error {
					mutate(x)
					return nil
				})
				return err
			})
			if !errors.Is(err, ErrImmutable) {
				t.Fatalf("err = %v, want ErrImmutable", err)
			}
		})
	}
}

// DrainTo is the one piece of configuration a wallet has, so unlike every other
// indexed field it may change — which is exactly why it is validated here.
func TestDrainToIsMutable(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	target := addr(0x55)

	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWallet(w.ID, func(x *Wallet) error {
			x.DrainTo = target
			return nil
		})
		return err
	})
	if err := s.View(func(tx *Tx) error {
		got, _, err := tx.Wallet(w.ID)
		if err != nil {
			return err
		}
		if !got.Proxies() || got.DrainTo != target {
			t.Fatalf("drain_to = %s, want %s", got.DrainTo.Hex(), target.Hex())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

// A→B→A would move the same money round and round, succeeding every hop and
// spending the master's gas forever. The two-level design could not express it;
// making the topology a field is what makes this check necessary (§35).
func TestDrainChainRefusesACycle(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x0A), addr(0x0B), 0)
	b := seedWalletAt(t, s, "b", addr(0x0B), common.Address{}, 0)

	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateWallet(b.ID, func(x *Wallet) error {
			x.DrainTo = a.Address
			return nil
		})
		return err
	})
	if !errors.Is(err, ErrDrainCycle) {
		t.Fatalf("err = %v, want ErrDrainCycle", err)
	}
}

func TestDrainChainRefusesSelfReference(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateWallet(w.ID, func(x *Wallet) error {
			x.DrainTo = x.Address
			return nil
		})
		return err
	})
	if !errors.Is(err, ErrDrainCycle) {
		t.Fatalf("err = %v, want ErrDrainCycle", err)
	}
}

func TestDrainChainRefusesTooManyHops(t *testing.T) {
	s := open(t)
	// A chain one hop longer than the limit, built from the far end backwards
	// so each link is legal when it is written.
	n := MaxDrainDepth + 2
	prev := common.Address{}
	for i := n; i >= 1; i-- {
		at := addr(byte(0x80 + i))
		w := Wallet{
			ID: uuid.New(), Ref: string(rune('a' + i)), Kind: KindManaged,
			Address: at, DrainTo: prev, Balance: new(big.Int), CreatedAt: time.Now().UTC(),
		}
		if err := s.Update(func(tx *Tx) error { return tx.PutWallet(w) }); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
		prev = at
	}
	// Now ask the audit what it thinks of the head of that chain.
	rep := verify(t, s)
	if len(findingsOfKind(rep, "topology")) == 0 {
		t.Fatalf("no topology finding for a %d-hop chain", n)
	}
}

// A chain that ends at a wallet we do not manage is fine: somebody else's
// address cannot point back at us.
func TestDrainChainAcceptsAnExternalDestination(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWallet(w.ID, func(x *Wallet) error {
			x.DrainTo = addr(0xEE) // nothing we derived
			return nil
		})
		return err
	})
	mustBeClean(t, verify(t, s))
}

func TestCreditAndDebit(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)

	update(t, s, func(tx *Tx) error {
		if _, err := tx.Credit(w.ID, wei(500)); err != nil {
			return err
		}
		_, err := tx.Credit(w.ID, wei(250))
		return err
	})
	update(t, s, func(tx *Tx) error {
		got, under, err := tx.Debit(w.ID, wei(300))
		if err != nil {
			return err
		}
		if under {
			t.Fatal("unexpected underflow")
		}
		if got.Balance.Int64() != 450 {
			t.Fatalf("balance = %s, want 450", got.Balance)
		}
		return nil
	})
}

func TestDebitUnderflowClampsAndReports(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	update(t, s, func(tx *Tx) error {
		got, under, err := tx.Debit(w.ID, wei(10))
		if err != nil {
			return err
		}
		if !under {
			t.Fatal("underflow not reported")
		}
		if got.Balance.Sign() != 0 {
			t.Fatalf("balance = %s, want 0", got.Balance)
		}
		return nil
	})
}

func TestClaimWalletIsExclusive(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	first, second := uuid.New(), uuid.New()

	update(t, s, func(tx *Tx) error {
		_, err := tx.ClaimWallet(w.ID, first)
		return err
	})
	if err := s.Update(func(tx *Tx) error {
		_, err := tx.ClaimWallet(w.ID, second)
		return err
	}); err == nil {
		t.Fatal("a second flow claimed a busy wallet")
	}
	// Re-claiming by the same flow is idempotent, which is what makes a retried
	// start harmless.
	update(t, s, func(tx *Tx) error {
		_, err := tx.ClaimWallet(w.ID, first)
		return err
	})
	update(t, s, func(tx *Tx) error {
		got, err := tx.ReleaseWallet(w.ID)
		if err != nil {
			return err
		}
		if !got.Idle() {
			t.Fatal("wallet still busy after release")
		}
		return nil
	})
}

func TestBackoff(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	deadline := time.Now().Add(time.Minute).UTC()

	update(t, s, func(tx *Tx) error {
		got, err := tx.BackOff(w.ID, deadline)
		if err != nil {
			return err
		}
		if got.FailedAttempts != 1 || !got.RetryAfter.Equal(deadline) {
			t.Fatalf("attempts=%d retry=%v", got.FailedAttempts, got.RetryAfter)
		}
		return nil
	})
	update(t, s, func(tx *Tx) error {
		got, err := tx.ClearBackoff(w.ID)
		if err != nil {
			return err
		}
		if got.FailedAttempts != 0 || !got.RetryAfter.IsZero() {
			t.Fatalf("backoff not cleared: %+v", got)
		}
		return nil
	})
}

func TestWalletsPaginateByRef(t *testing.T) {
	s := open(t)
	for i, ref := range []string{"a", "b", "c", "d"} {
		seedWalletAt(t, s, ref, addr(byte(0x10+i)), common.Address{}, 0)
	}

	var first, second []Wallet
	if err := s.View(func(tx *Tx) error {
		var err error
		if first, err = tx.Wallets("", 2); err != nil {
			return err
		}
		second, err = tx.Wallets(first[len(first)-1].Ref, 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := refs(first); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("first page = %v", got)
	}
	if got := refs(second); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("second page = %v", got)
	}
}

// The master has no ref, so it must not appear in a listing the caller reads.
func TestWalletListingExcludesTheMaster(t *testing.T) {
	s := open(t)
	seedWallet(t, s, "cust-1", 0)
	update(t, s, func(tx *Tx) error {
		return tx.PutWallet(Wallet{
			ID: uuid.New(), Kind: KindMaster, Address: addr(0xFF),
			Balance: new(big.Int), CreatedAt: time.Now().UTC(),
		})
	})

	if err := s.View(func(tx *Tx) error {
		list, err := tx.Wallets("", 0)
		if err != nil {
			return err
		}
		if got := refs(list); len(got) != 1 || got[0] != "cust-1" {
			t.Fatalf("listing = %v, want just cust-1", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func refs(ws []Wallet) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Ref)
	}
	return out
}
