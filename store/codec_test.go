package store

import (
	"bytes"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWeiRoundTripsAcrossTheFullRange(t *testing.T) {
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	oneUSDT, _ := new(big.Int).SetString("1000000000000000000", 10)

	for _, v := range []*big.Int{nil, big.NewInt(0), big.NewInt(1), oneUSDT, maxUint256} {
		e := &enc{}
		e.wei(v)
		if e.err != nil {
			t.Fatalf("encode %v: %v", v, e.err)
		}
		if len(e.b) != weiLen {
			t.Fatalf("encode %v produced %d bytes, want %d", v, len(e.b), weiLen)
		}
		got := newDec(e.b).wei()
		want := orZero(v)
		if got.Cmp(want) != 0 {
			t.Fatalf("round-trip %v = %v", want, got)
		}
	}
}

func TestWeiRefusesUnrepresentableAmounts(t *testing.T) {
	// Silently truncating or wrapping money is the one thing this codec must
	// never do, so both directions of "does not fit" are latched errors.
	tests := map[string]*big.Int{
		"negative":  big.NewInt(-1),
		"over 256b": new(big.Int).Lsh(big.NewInt(1), 256),
	}
	for name, v := range tests {
		e := &enc{}
		e.wei(v)
		if e.err == nil {
			t.Fatalf("%s: encoded without error", name)
		}
	}
}

func TestDecoderReportsTruncation(t *testing.T) {
	e := &enc{}
	e.u64(1)
	e.wei(big.NewInt(7))
	d := newDec(e.b[:4]) // chop mid-field
	_ = d.u64()
	if d.done() == nil {
		t.Fatal("decoder accepted a truncated buffer")
	}
}

func TestDecoderIgnoresTrailingBytesFromANewerVersion(t *testing.T) {
	// The version-byte contract: an older binary reading a record written by a
	// newer one must read the fields it knows and ignore the rest.
	w := Wallet{
		ID: uuid.New(), App: "df", Kind: KindDeposit, Ref: "cust-1",
		Address: addr(9), Balance: wei(5), CreatedAt: time.Now().UTC(),
	}
	b, err := w.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeWallet(append(b, 0xDE, 0xAD, 0xBE, 0xEF))
	if err != nil {
		t.Fatalf("decode with trailing bytes: %v", err)
	}
	if got.Ref != w.Ref || got.Balance.Cmp(w.Balance) != 0 {
		t.Fatalf("decoded %+v, want ref/balance from %+v", got, w)
	}
}

func TestStringsAndBlobsRoundTrip(t *testing.T) {
	e := &enc{}
	e.str("")
	e.str("lkr:acme")
	e.blob(nil)
	e.blob([]byte{0x02, 0xF8, 0x00})
	if e.err != nil {
		t.Fatalf("encode: %v", e.err)
	}
	d := newDec(e.b)
	if got := d.str(); got != "" {
		t.Fatalf("empty string = %q", got)
	}
	if got := d.str(); got != "lkr:acme" {
		t.Fatalf("string = %q", got)
	}
	if got := d.blob(); len(got) != 0 {
		t.Fatalf("nil blob = %v", got)
	}
	if got := d.blob(); !bytes.Equal(got, []byte{0x02, 0xF8, 0x00}) {
		t.Fatalf("blob = %v", got)
	}
	if err := d.done(); err != nil {
		t.Fatalf("done: %v", err)
	}
}

func TestBlobIsOwnedNotAliased(t *testing.T) {
	// bbolt values are only valid inside their transaction, so a decoded blob
	// must not alias the buffer it came from.
	e := &enc{}
	e.blob([]byte{1, 2, 3})
	buf := e.b
	got := newDec(buf).blob()
	for i := range buf {
		buf[i] = 0xFF
	}
	if !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("blob aliased its source: %v", got)
	}
}

func TestZeroTimeRoundTripsAsZero(t *testing.T) {
	// time.Time{}.UnixNano() is a large negative number, so the zero time needs
	// explicit handling or RetryAfter would decode as a date in 1754.
	e := &enc{}
	e.stamp(time.Time{})
	if got := newDec(e.b).stamp(); !got.IsZero() {
		t.Fatalf("zero time round-tripped to %v", got)
	}

	now := time.Now().UTC().Truncate(time.Nanosecond)
	e2 := &enc{}
	e2.stamp(now)
	if got := newDec(e2.b).stamp(); !got.Equal(now) {
		t.Fatalf("stamp round-trip = %v, want %v", got, now)
	}
}

func TestEveryRecordRoundTrips(t *testing.T) {
	now := time.Now().UTC()
	id, wid := uuid.New(), uuid.New()

	t.Run("app", func(t *testing.T) {
		in := App{
			Slug: "df", Wallet: wid, Paused: true,
			Fee:       FeePolicy{Flat: 100, Min: 500, BPS: 0},
			Ledger:    12_345,
			Reserved:  1_100,
			CreatedAt: now, UpdatedAt: now,
		}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeApp(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Slug != in.Slug || out.Wallet != in.Wallet || !out.Paused ||
			out.Fee != in.Fee || out.Ledger != in.Ledger || out.Reserved != in.Reserved {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

	t.Run("flow", func(t *testing.T) {
		in := Flow{
			ID: id, Kind: FlowWithdrawal, State: StatePaying, Wallet: wid, App: "df",
			Withdrawal: uuid.New(), Amount: wei(77), To: addr(3), Tx: hash(4),
			Attempt: 2, Error: "boom", CreatedAt: now, UpdatedAt: now,
		}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeFlow(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.ID != in.ID || out.Kind != in.Kind || out.State != in.State ||
			out.Amount.Cmp(in.Amount) != 0 || out.To != in.To || out.Tx != in.Tx || out.Error != in.Error {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

	t.Run("txref", func(t *testing.T) {
		in := TxRef{Flow: id, Signer: addr(7), Nonce: 42}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeTxRef(b)
		if err != nil {
			t.Fatal(err)
		}
		if out != in {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

	t.Run("send", func(t *testing.T) {
		in := Send{Flow: id, Signer: addr(7), Nonce: 9, Hash: hash(1), Raw: []byte{0xF8, 0x6C}, CreatedAt: now}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeSend(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Nonce != in.Nonce || out.Hash != in.Hash || !bytes.Equal(out.Raw, in.Raw) {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

	t.Run("deposit", func(t *testing.T) {
		in := Deposit{
			Wallet: wid, App: "df", Block: 48210577, LogIndex: 9, TxHash: hash(2),
			From: addr(5), AmountWei: wei(1000), Cents: 1000, Status: DepositCredited, DrainTx: hash(3), CreatedAt: now,
		}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeDeposit(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Cursor() != in.Cursor() || out.AmountWei.Cmp(in.AmountWei) != 0 ||
			out.Cents != in.Cents || out.Status != in.Status || out.DrainTx != in.DrainTx {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

	t.Run("withdrawal", func(t *testing.T) {
		in := Withdrawal{
			ID: id, App: "df", Destination: addr(6), Amount: 1000, Fee: 100,
			Payout: 900, Debit: 1000, DeductFee: true, Status: WithdrawalPending,
			TxHash: hash(8), IdempotencyKey: "k-1",
			FeeSnapshot: FeePolicy{Flat: 100},
			CreatedAt:   now, UpdatedAt: now,
		}
		b, err := in.encode()
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeWithdrawal(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Payout != in.Payout || out.Debit != in.Debit ||
			!out.DeductFee || out.IdempotencyKey != in.IdempotencyKey ||
			out.FeeSnapshot != in.FeeSnapshot {
			t.Fatalf("got %+v want %+v", out, in)
		}
	})

}
