package store

import (
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Finding is one internal inconsistency.
type Finding struct {
	Kind   string // index | ownership | topology | solvency
	Where  string
	Detail string
}

func (f Finding) String() string { return fmt.Sprintf("[%s] %s: %s", f.Kind, f.Where, f.Detail) }

// Report is the result of Verify.
type Report struct {
	Wallets     int
	Proxies     int
	Flows       int
	Deposits    int
	Withdrawals int      // settled
	Pending     int      // promises still outstanding
	Held        *big.Int // custody across every managed wallet
	Committed   *big.Int // the total promised by pending withdrawals
	Findings    []Finding
}

// OK reports a clean audit.
func (r Report) OK() bool { return len(r.Findings) == 0 }

func (r *Report) flag(kind, where, detail string) {
	r.Findings = append(r.Findings, Finding{Kind: kind, Where: where, Detail: detail})
}

// Verify recomputes everything the store maintains incrementally and reports
// drift. It replaces a reconciler *process*: run on demand against a snapshot
// rather than forever in the background.
//
// What it checks changed with the ledger's removal. There is no longer a
// balance to recompute from the log — custody comes from the chain and `bsc
// inspect --rpc` is what compares it against the token — so the offline audit
// is now about structure: that every index points both ways, that no wallet
// claims a flow that does not exist, that no drain chain loops, and that no
// wallet has promised more than it holds (§40).
func (t *Tx) Verify() (Report, error) {
	rep := Report{Held: new(big.Int), Committed: new(big.Int)}

	wallets := map[WalletID]Wallet{}
	byAddr := map[common.Address]WalletID{}
	if err := t.EachWallet(func(w Wallet) error {
		rep.Wallets++
		wallets[w.ID] = w
		byAddr[w.Address] = w.ID
		rep.Held.Add(rep.Held, orZero(w.Balance))
		if w.Proxies() {
			rep.Proxies++
		}
		if orZero(w.Balance).Sign() < 0 {
			rep.flag("solvency", "wallet/"+w.Address.Hex(),
				fmt.Sprintf("negative custody %s", orZero(w.Balance)))
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Both lookup indexes must resolve to the record that claims them.
	for id, w := range wallets {
		got := t.tx.Bucket(bAddr).Get(w.Address.Bytes())
		if !isWalletID(got, id) {
			rep.flag("index", "addr/"+w.Address.Hex(), "does not point back at its wallet")
		}
		if !isWalletID(t.tx.Bucket(bRef).Get([]byte(w.Ref)), id) {
			rep.flag("index", "ref/"+w.Ref, "does not point back at its wallet")
		}
	}

	// A wallet may only claim a flow that exists, and a flow may only own a
	// wallet that claims it back. This pair is the "one live flow per wallet"
	// invariant, which is structural rather than checked at runtime.
	flows := map[uuid.UUID]Flow{}
	if err := t.EachFlow(func(f Flow) error {
		rep.Flows++
		flows[f.ID] = f
		w, ok := wallets[f.Wallet]
		if !ok {
			rep.flag("ownership", "flow/"+f.ID.String(), "references a wallet that does not exist")
			return nil
		}
		if w.Flow != f.ID {
			rep.flag("ownership", "flow/"+f.ID.String(),
				fmt.Sprintf("owns wallet %s, which claims flow %s", w.Address.Hex(), w.Flow))
		}
		return nil
	}); err != nil {
		return rep, err
	}
	for _, w := range wallets {
		if w.Idle() {
			continue
		}
		if _, ok := flows[w.Flow]; !ok {
			rep.flag("ownership", "wallet/"+w.Address.Hex(),
				fmt.Sprintf("claims flow %s, which does not exist", w.Flow))
		}
	}

	// Drain chains. A cycle is refused at write time, so finding one here means
	// either a bug in that check or a record edited outside the service — and it
	// is the failure that costs money silently, since every hop succeeds (§35).
	for id, w := range wallets {
		if !w.Proxies() {
			continue
		}
		if err := t.CheckDrainChain(id, w.Address, w.DrainTo); err != nil {
			rep.flag("topology", "wallet/"+w.Address.Hex(), err.Error())
		}
	}

	// Deposits: every record reachable from both indexes, and belonging to a
	// wallet that exists. There is nothing else to check — a deposit has no
	// lifecycle and points at nothing (§50).
	if err := scanPrefix(t, bDeposit, nil, func(k, v []byte) error {
		rep.Deposits++
		d, err := decodeDeposit(v)
		if err != nil {
			return err
		}
		cur := d.Cursor().Key()
		if got := t.tx.Bucket(iDep).Get(cur); string(got) != string(k) {
			rep.flag("index", "dep/"+d.TxHash.Hex(), "missing from the cursor index")
		}
		if got := t.tx.Bucket(iDepWallet).Get(join(d.Wallet.Key(), cur)); string(got) != string(k) {
			rep.flag("index", "dep/"+d.TxHash.Hex(), "missing from the per-wallet index")
		}
		if _, known := wallets[d.Wallet]; !known {
			rep.flag("ownership", "dep/"+d.TxHash.Hex(), "belongs to a wallet that does not exist")
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Debits, in two keyspaces. A promise must not also be a fact, a fact must
	// be in the feed and the per-wallet index, and every wallet's promises must
	// be coverable by what it holds (§51).
	committed := map[WalletID]*big.Int{}
	if err := scanPrefix(t, bPending, nil, func(_, v []byte) error {
		p, err := decodePending(v)
		if err != nil {
			return err
		}
		rep.Pending++
		if p.Reason.String() == "unknown" {
			rep.flag("ownership", "wd/"+p.ID.String(),
				fmt.Sprintf("reason %d is not one this service writes", p.Reason))
		}
		// A drain is recorded once it has already happened, so one waiting here
		// means something wrote a promise nobody will keep.
		if p.Reason == ReasonDrain {
			rep.flag("ownership", "wd/"+p.ID.String(), "a drain is pending, which it can never be")
		}
		// The two keyspaces are exclusive by construction — Settle deletes then
		// writes, in one transaction — so a record in both means one of those
		// halves did not happen.
		if _, both, err := t.Withdrawal(p.ID); err != nil {
			return err
		} else if both {
			rep.flag("ownership", "wd/"+p.ID.String(), "is both a promise and a fact")
		}
		if p.IdempotencyKey != "" && !isID(t.tx.Bucket(iWdIdem).Get([]byte(p.IdempotencyKey)), p.ID) {
			rep.flag("index", "wd/"+p.ID.String(), "idempotency key does not point back at it")
		}
		if committed[p.Wallet] == nil {
			committed[p.Wallet] = new(big.Int)
		}
		committed[p.Wallet].Add(committed[p.Wallet], orZero(p.Amount))
		return nil
	}); err != nil {
		return rep, err
	}

	if err := scanPrefix(t, bWithdrawal, nil, func(_, v []byte) error {
		rep.Withdrawals++
		wd, err := decodeWithdrawal(v)
		if err != nil {
			return err
		}
		created := be64(stampNanos(wd.CreatedAt))
		if t.tx.Bucket(iWdWallet).Get(join(wd.Wallet.Key(), created, wd.ID[:])) == nil {
			rep.flag("index", "wd/"+wd.ID.String(), "missing from the per-wallet index")
		}
		if t.tx.Bucket(iWdFeed).Get(wd.Settled().Key()) == nil {
			rep.flag("index", "wd/"+wd.ID.String(), "missing from the settled feed")
		}
		// Go's zero value is a valid uint8, so a construction site that forgets
		// the field writes a debit that explains nothing and compiles silently.
		// Anything else unrecognised is a record nothing in this service could
		// have produced (§42).
		if wd.Reason.String() == "unknown" {
			rep.flag("ownership", "wd/"+wd.ID.String(),
				fmt.Sprintf("reason %d is not one this service writes", wd.Reason))
		}
		// Being here means it happened, so it must say what happened.
		if wd.TxHash == (common.Hash{}) {
			rep.flag("ownership", "wd/"+wd.ID.String(), "is a settled debit with no transaction")
		}
		if wd.IdempotencyKey != "" && !isID(t.tx.Bucket(iWdIdem).Get([]byte(wd.IdempotencyKey)), wd.ID) {
			rep.flag("index", "wd/"+wd.ID.String(), "idempotency key does not point back at it")
		}
		if _, known := wallets[wd.Wallet]; !known {
			rep.flag("ownership", "wd/"+wd.ID.String(), "belongs to a wallet that does not exist")
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// A wallet must never owe more than it holds. This is what the per-app
	// solvency margin became: the same question, asked of the thing that
	// actually holds the money rather than of an abstraction above it.
	for id, total := range committed {
		rep.Committed.Add(rep.Committed, total)
		w, ok := wallets[id]
		if !ok {
			rep.flag("ownership", "wd/wallet/"+id.String(), "pending withdrawals on a wallet that does not exist")
			continue
		}
		if total.Cmp(orZero(w.Balance)) > 0 {
			rep.flag("solvency", "wallet/"+w.Address.Hex(),
				fmt.Sprintf("owes %s but holds %s", total, orZero(w.Balance)))
		}
	}

	if err := t.checkIndexes(&rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// checkIndexes walks every index bucket and checks each entry resolves to a
// record that agrees with it.
//
// This is the other direction. Everything above starts from a record and asks
// whether the indexes know about it, which catches a missing entry — but an
// entry pointing at a record that was never written, or at the wrong one, is
// invisible from that side. A hand-rolled index can fail either way, and the
// failure this misses is the worse one: a lookup that succeeds and returns
// somebody else's row.
func (t *Tx) checkIndexes(rep *Report) error {
	// idx/dep: the caller's cursor. Its key must be the deposit's own position.
	if err := scanPrefix(t, iDep, nil, func(k, v []byte) error {
		d, err := decodeDeposit(t.tx.Bucket(bDeposit).Get(v))
		if err != nil {
			rep.flag("index", "idx/dep/"+hex.EncodeToString(k), "points at no deposit")
			return nil //nolint:nilerr // a dangling entry is a finding, not a read failure
		}
		if string(d.Cursor().Key()) != string(k) {
			rep.flag("index", "idx/dep/"+hex.EncodeToString(k), "indexed under a cursor that is not the deposit's")
		}
		return nil
	}); err != nil {
		return err
	}

	// idx/dep_wallet: wallet ++ cursor.
	if err := scanPrefix(t, iDepWallet, nil, func(k, v []byte) error {
		d, err := decodeDeposit(t.tx.Bucket(bDeposit).Get(v))
		if err != nil {
			rep.flag("index", "idx/dep_wallet/"+hex.EncodeToString(k), "points at no deposit")
			return nil //nolint:nilerr // a dangling entry is a finding, not a read failure
		}
		if string(join(d.Wallet.Key(), d.Cursor().Key())) != string(k) {
			rep.flag("index", "idx/dep_wallet/"+hex.EncodeToString(k), "indexed under the wrong wallet or cursor")
		}
		return nil
	}); err != nil {
		return err
	}

	// idx/wd_feed: block ++ id, settled debits only.
	if err := scanPrefix(t, iWdFeed, nil, func(k, _ []byte) error {
		pos, ok := settledFromKey(k)
		if !ok {
			rep.flag("index", "idx/wd_feed/"+hex.EncodeToString(k), "malformed key")
			return nil
		}
		wd, found, err := t.Withdrawal(pos.ID)
		if err != nil {
			return err
		}
		switch {
		case !found:
			rep.flag("index", "wd/"+pos.ID.String(), "on the settled feed but not in the log")
		case wd.Block != pos.Block:
			rep.flag("index", "wd/"+pos.ID.String(), "on the settled feed under the wrong block")
		}
		return nil
	}); err != nil {
		return err
	}

	// idx/wd_wallet: wallet ++ created ++ id.
	if err := scanPrefix(t, iWdWallet, nil, func(k, _ []byte) error {
		if len(k) != 8+8+16 {
			rep.flag("index", "idx/wd_wallet/"+hex.EncodeToString(k), "malformed key")
			return nil
		}
		var id uuid.UUID
		copy(id[:], k[16:])
		wd, found, err := t.Withdrawal(id)
		if err != nil {
			return err
		}
		if !found {
			rep.flag("index", "wd/"+id.String(), "in the per-wallet index but not in the log")
			return nil
		}
		if string(join(wd.Wallet.Key(), be64(stampNanos(wd.CreatedAt)), id[:])) != string(k) {
			rep.flag("index", "wd/"+id.String(), "in the per-wallet index under the wrong wallet or time")
		}
		return nil
	}); err != nil {
		return err
	}

	// idx/wd_idem: the key a retry resolves through. It spans both keyspaces, so
	// either half may answer — but exactly one must, and it must claim the key.
	if err := scanPrefix(t, iWdIdem, nil, func(k, v []byte) error {
		var id uuid.UUID
		copy(id[:], v)
		if p, ok, err := t.Pending(id); err != nil {
			return err
		} else if ok {
			if p.IdempotencyKey != string(k) {
				rep.flag("index", "wd/"+id.String(), "idempotency key "+string(k)+" resolves to a promise that does not claim it")
			}
			return nil
		}
		wd, found, err := t.Withdrawal(id)
		if err != nil {
			return err
		}
		switch {
		case !found:
			rep.flag("index", "idem/"+string(k), "points at no debit")
		case wd.IdempotencyKey != string(k):
			rep.flag("index", "wd/"+id.String(), "idempotency key "+string(k)+" resolves to a debit that does not claim it")
		}
		return nil
	}); err != nil {
		return err
	}

	// data/addr and data/ref: the two wallet lookups. A stale entry here is the
	// one that resolves an address or a handle to the wrong wallet entirely.
	if err := scanPrefix(t, bAddr, nil, func(k, v []byte) error {
		w, found, err := t.Wallet(walletIDFromKey(v))
		if err != nil {
			return err
		}
		if !found || w.Address != common.BytesToAddress(k) {
			rep.flag("index", "addr/"+common.BytesToAddress(k).Hex(), "resolves to a wallet with a different address")
		}
		return nil
	}); err != nil {
		return err
	}
	return scanPrefix(t, bRef, nil, func(k, v []byte) error {
		w, found, err := t.Wallet(walletIDFromKey(v))
		if err != nil {
			return err
		}
		if !found || w.Ref != string(k) {
			rep.flag("index", "ref/"+string(k), "resolves to a wallet with a different ref")
		}
		return nil
	})
}

func isID(v []byte, want uuid.UUID) bool {
	return len(v) == len(want) && string(v) == string(want[:])
}

func isWalletID(v []byte, want WalletID) bool {
	return len(v) == 8 && string(v) == string(want.Key())
}
