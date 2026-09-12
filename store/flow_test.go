package store

import (
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// startFlow creates a flow owning w.
func startFlow(t *testing.T, s *Store, w Wallet, kind FlowKind, state FlowState) Flow {
	t.Helper()
	f := Flow{
		ID: uuid.New(), Kind: kind, State: state, Wallet: w.ID, App: w.App,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error {
		if err := tx.PutFlow(f); err != nil {
			return err
		}
		_, err := tx.ClaimWallet(w.ID, f.ID)
		return err
	})
	return f
}

func TestFlowRoundTripAndValidation(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x60, 0)
	f := startFlow(t, s, w, FlowDrain, StateFunding)

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.Flow(f.ID)
		if err != nil || !ok {
			t.Fatalf("Flow: ok=%v err=%v", ok, err)
		}
		if got.Kind != FlowDrain || got.State != StateFunding || got.Wallet != w.ID {
			t.Fatalf("got %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	if err := s.Update(func(tx *Tx) error {
		return tx.PutFlow(Flow{App: "df"}) // no id
	}); err == nil {
		t.Fatal("PutFlow accepted a flow with no id")
	}
	if err := s.Update(func(tx *Tx) error {
		return tx.PutFlow(Flow{ID: uuid.New(), App: "BAD"})
	}); err == nil {
		t.Fatal("PutFlow accepted an invalid slug")
	}
}

func TestDeleteFlowReleasesItsWallet(t *testing.T) {
	// Releasing the wallet is what makes it eligible for the next round of the
	// declarative work rules — the mechanism that picks up a deposit which
	// landed mid-drain.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x61, 0)
	f := startFlow(t, s, w, FlowDrain, StateSweeping)

	update(t, s, func(tx *Tx) error {
		got, _, err := tx.Wallet(w.ID)
		if err != nil {
			return err
		}
		if got.Idle() {
			t.Fatal("wallet is idle while a flow owns it")
		}
		f.State = StateDone
		return tx.DeleteFlow(f)
	})

	if err := s.View(func(tx *Tx) error {
		if _, ok, err := tx.Flow(f.ID); err != nil || ok {
			t.Fatalf("flow still present: ok=%v err=%v", ok, err)
		}
		got, _, err := tx.Wallet(w.ID)
		if err != nil {
			return err
		}
		if !got.Idle() {
			t.Fatalf("wallet still owned by %s", got.Flow)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestDeleteFlowLeavesAWalletOwnedBySomeoneElseAlone(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x62, 0)
	live := startFlow(t, s, w, FlowDrain, StateSweeping)

	// A stale flow record naming the same wallet must not steal the release.
	stale := Flow{ID: uuid.New(), Kind: FlowDrain, State: StateFailed, Wallet: w.ID, App: "df"}
	update(t, s, func(tx *Tx) error {
		if err := tx.PutFlow(stale); err != nil {
			return err
		}
		return tx.DeleteFlow(stale)
	})

	if err := s.View(func(tx *Tx) error {
		got, _, err := tx.Wallet(w.ID)
		if err != nil {
			return err
		}
		if got.Flow != live.ID {
			t.Fatalf("wallet owner = %s, want the live flow %s", got.Flow, live.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestTxWatchlistIsTheConfirmationRouter(t *testing.T) {
	// One structure serves as both the watcher's tx watchlist and the router
	// from a confirmed receipt to its flow and journal entry.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x63, 0)
	f := startFlow(t, s, w, FlowDrain, StateSweeping)
	txh := hash(0x77)
	ref := TxRef{Flow: f.ID, Signer: addr(0x01), Nonce: 12}

	update(t, s, func(tx *Tx) error { return tx.LinkTx(txh, ref) })

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.TxRefByHash(txh)
		if err != nil || !ok {
			t.Fatalf("TxRefByHash: ok=%v err=%v", ok, err)
		}
		if got != ref {
			t.Fatalf("got %+v want %+v", got, ref)
		}
		var seen int
		if err := tx.EachWatchedTx(func(h common.Hash, r TxRef) error {
			seen++
			if h != txh || r != ref {
				t.Fatalf("watchlist entry = (%s, %+v)", h.Hex(), r)
			}
			return nil
		}); err != nil {
			return err
		}
		if seen != 1 {
			t.Fatalf("watchlist has %d entries", seen)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	update(t, s, func(tx *Tx) error { return tx.UnlinkTx(txh) })
	if err := s.View(func(tx *Tx) error {
		if _, ok, err := tx.TxRefByHash(txh); err != nil || ok {
			t.Fatalf("unlinked tx still present: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestSendJournalIsNonceOrderedPerSigner(t *testing.T) {
	// Re-broadcasting a dropped transaction has to go out in nonce order or the
	// gap never closes, so the journal's iteration order is load-bearing.
	s := open(t)
	master, wallet := addr(0x01), addr(0x02)

	update(t, s, func(tx *Tx) error {
		for _, sd := range []Send{
			{Flow: uuid.New(), Signer: master, Nonce: 7, Hash: hash(7), Raw: []byte{7}},
			{Flow: uuid.New(), Signer: wallet, Nonce: 0, Hash: hash(9), Raw: []byte{9}},
			{Flow: uuid.New(), Signer: master, Nonce: 5, Hash: hash(5), Raw: []byte{5}},
			{Flow: uuid.New(), Signer: master, Nonce: 6, Hash: hash(6), Raw: []byte{6}},
		} {
			if err := tx.PutSend(sd); err != nil {
				return err
			}
		}
		return nil
	})

	if err := s.View(func(tx *Tx) error {
		var order []uint64
		if err := tx.EachSend(func(sd Send) error {
			if sd.Signer == master {
				order = append(order, sd.Nonce)
			}
			return nil
		}); err != nil {
			return err
		}
		if len(order) != 3 || order[0] != 5 || order[1] != 6 || order[2] != 7 {
			t.Fatalf("master nonces = %v, want 5,6,7", order)
		}
		got, ok, err := tx.Send(master, 6)
		if err != nil || !ok {
			t.Fatalf("Send: ok=%v err=%v", ok, err)
		}
		if got.Hash != hash(6) || len(got.Raw) != 1 {
			t.Fatalf("journal entry = %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	update(t, s, func(tx *Tx) error { return tx.DeleteSend(master, 6) })
	if err := s.View(func(tx *Tx) error {
		if _, ok, err := tx.Send(master, 6); err != nil || ok {
			t.Fatalf("deleted journal entry still present: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestMutateMissingFlow(t *testing.T) {
	s := open(t)
	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateFlow(uuid.New(), func(*Flow) error { return nil })
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestFlowStateTerminality(t *testing.T) {
	for _, s := range []FlowState{StateFunding, StateApproving, StateSweeping, StatePaying} {
		if s.IsTerminal() {
			t.Fatalf("%s reported terminal", s)
		}
	}
	for _, s := range []FlowState{StateDone, StateFailed} {
		if !s.IsTerminal() {
			t.Fatalf("%s reported non-terminal", s)
		}
	}
}
