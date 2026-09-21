package api

import (
	"testing"

	"bsc/keys"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
)

// Wallet ids are sequential because that is what makes the money recoverable
// from the master secret alone: a restore walks 1, 2, 3 … and asks the chain,
// where 122 random bits were unsearchable (§48).
//
// The ids never reach the caller, so this reads them from the store — which is
// the point of the test: the property lives below the wire contract and nothing
// above would notice if it broke.
func TestWalletIDsAreAllocatedSequentially(t *testing.T) {
	f := newFixture(t)

	refs := []string{"cust-1", "cust-2", "cust-3"}
	for _, ref := range refs {
		f.wallet(ref)
	}

	var got []store.WalletID
	if err := f.st.View(func(tx *store.Tx) error {
		for _, ref := range refs {
			w, ok, err := tx.WalletByRef(ref)
			if err != nil || !ok {
				t.Fatalf("wallet %s: ok=%v err=%v", ref, ok, err)
			}
			got = append(got, w.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Strictly increasing and starting at 1. Not necessarily contiguous:
	// keys.Allocate steps past any index that is not a valid scalar.
	for i, id := range got {
		if id == 0 {
			t.Fatalf("%s got id 0, which is not a wallet", refs[i])
		}
		if i > 0 && id <= got[i-1] {
			t.Fatalf("ids went backwards: %v", got)
		}
	}
}

// The recovery the sequence buys: walk the ids, derive from the secret alone,
// and get back the addresses the service actually issued.
//
// What this pins is that a wallet's recorded id is the one its recorded address
// was derived from. It cannot prove Derive itself is "right" — both sides call
// it — but that is covered in keys/, and this is the half that recovery depends
// on: an id that does not reproduce its own address makes the whole sequence
// worthless. Verified by breaking it: recording id+1 fails this test.
func TestIssuedAddressesAreDerivableFromTheSecretAlone(t *testing.T) {
	f := newFixture(t)
	f.wallet("cust-1")
	f.wallet("cust-2")

	issued := map[store.WalletID]common.Address{}
	if err := f.st.View(func(tx *store.Tx) error {
		return tx.EachWallet(func(w store.Wallet) error {
			issued[w.ID] = w.Address
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(issued) != 2 {
		t.Fatalf("issued %d wallets, want 2", len(issued))
	}

	// A bare ring over the same secret, with no access to the store at all.
	ring, err := keys.New(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range issued {
		key, err := ring.Derive(uint64(id))
		if err != nil {
			t.Fatalf("Derive(%d): %v", id, err)
		}
		if key.Address != want {
			t.Fatalf("index %d derives %s, but the service issued %s",
				id, key.Address.Hex(), want.Hex())
		}
	}
}
