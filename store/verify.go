package store

import (
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
	Withdrawals int
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

	wallets := map[uuid.UUID]Wallet{}
	byAddr := map[common.Address]uuid.UUID{}
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
		if !isID(got, id) {
			rep.flag("index", "addr/"+w.Address.Hex(), "does not point back at its wallet")
		}
		if w.Kind == KindMaster {
			continue
		}
		if !isID(t.tx.Bucket(bRef).Get([]byte(w.Ref)), id) {
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

	// Deposits: every record reachable from both indexes, the open set holding
	// exactly the un-forwarded ones on proxy wallets, and every forwarded one
	// pointing at a debit that exists.
	sweptBy := map[uuid.UUID][]common.Hash{}
	openSeen := map[string]bool{}
	if err := scanPrefix(t, iDepOpen, nil, func(k, _ []byte) error {
		openSeen[string(k)] = true
		return nil
	}); err != nil {
		return rep, err
	}
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
		if got := t.tx.Bucket(iDepWallet).Get(join(d.Wallet[:], cur)); string(got) != string(k) {
			rep.flag("index", "dep/"+d.TxHash.Hex(), "missing from the per-wallet index")
		}
		w, known := wallets[d.Wallet]
		if !known {
			rep.flag("ownership", "dep/"+d.TxHash.Hex(), "belongs to a wallet that does not exist")
			return nil
		}
		wantOpen := d.Status == DepositReceived && w.Proxies()
		gotOpen := openSeen[string(join(d.Wallet[:], k))]
		if wantOpen != gotOpen {
			rep.flag("index", "dep/"+d.TxHash.Hex(),
				fmt.Sprintf("status %s on a proxy=%t wallet but open-index membership is %t", d.Status, w.Proxies(), gotOpen))
		}
		// A forwarded credit must name the debit that carried it, and that debit
		// must exist. This is the link that replaces the drain concept, so a
		// broken one is the audit's business (§42).
		switch {
		case d.Status == DepositForwarded && d.SweptBy == uuid.Nil:
			rep.flag("ownership", "dep/"+d.TxHash.Hex(), "forwarded but names no debit")
		case d.Status != DepositForwarded && d.SweptBy != uuid.Nil:
			rep.flag("ownership", "dep/"+d.TxHash.Hex(), "names a debit but is not forwarded")
		case d.SweptBy != uuid.Nil:
			sweptBy[d.SweptBy] = append(sweptBy[d.SweptBy], d.TxHash)
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Withdrawals: indexes both ways, and the promise a wallet has made.
	committed := map[uuid.UUID]*big.Int{}
	debits := map[uuid.UUID]bool{}
	if err := scanPrefix(t, bWithdrawal, nil, func(k, v []byte) error {
		rep.Withdrawals++
		wd, err := decodeWithdrawal(v)
		if err != nil {
			return err
		}
		created := be64(stampNanos(wd.CreatedAt))
		if t.tx.Bucket(iWd).Get(join(created, wd.ID[:])) == nil {
			rep.flag("index", "wd/"+wd.ID.String(), "missing from the history index")
		}
		if t.tx.Bucket(iWdWallet).Get(join(wd.Wallet[:], created, wd.ID[:])) == nil {
			rep.flag("index", "wd/"+wd.ID.String(), "missing from the per-wallet index")
		}
		inOpen := t.tx.Bucket(iWdOpen).Get(wd.ID[:]) != nil
		if inOpen == wd.Status.IsTerminal() {
			rep.flag("index", "wd/"+wd.ID.String(),
				fmt.Sprintf("status %s but open-set membership is %t", wd.Status, inOpen))
		}
		inFeed := t.tx.Bucket(iWdFeed).Get(wd.Settled().Key()) != nil
		if inFeed != wd.Status.IsTerminal() {
			rep.flag("index", "wd/"+wd.ID.String(),
				fmt.Sprintf("status %s but settled-feed membership is %t", wd.Status, inFeed))
		}
		// Go's zero value is a valid uint8, so a construction site that forgets
		// the field writes a debit that explains nothing and compiles silently.
		// Anything else unrecognised is a record nothing in this service could
		// have produced (§42).
		if wd.Reason.String() == "unknown" {
			rep.flag("ownership", "wd/"+wd.ID.String(),
				fmt.Sprintf("reason %d is not one this service writes", wd.Reason))
		}
		// A drain is recorded once it has already happened, so one that is not
		// terminal means something wrote a promise nobody will keep.
		if wd.Reason == ReasonDrain && !wd.Status.IsTerminal() {
			rep.flag("ownership", "wd/"+wd.ID.String(), "a drain is pending, which it can never be")
		}
		if wd.IdempotencyKey != "" && !isID(t.tx.Bucket(iWdIdem).Get([]byte(wd.IdempotencyKey)), wd.ID) {
			rep.flag("index", "wd/"+wd.ID.String(), "idempotency key does not point back at it")
		}
		debits[wd.ID] = true
		if !wd.Status.IsTerminal() {
			if committed[wd.Wallet] == nil {
				committed[wd.Wallet] = new(big.Int)
			}
			committed[wd.Wallet].Add(committed[wd.Wallet], orZero(wd.Amount))
		}
		_ = k
		return nil
	}); err != nil {
		return rep, err
	}

	for id, deposits := range sweptBy {
		if !debits[id] {
			for _, h := range deposits {
				rep.flag("ownership", "dep/"+h.Hex(), "names a debit that does not exist")
			}
		}
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

	return rep, nil
}

func isID(v []byte, want uuid.UUID) bool {
	return len(v) == len(want) && string(v) == string(want[:])
}
