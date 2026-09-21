package store

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

func TestCursorKeysSortLikeTheChain(t *testing.T) {
	// The entire deposit feed rests on this: byte order over the 12-byte key
	// must equal numeric order over (block, log_index). If it ever didn't, a
	// range scan would silently skip or replay deposits.
	in := []Cursor{
		{Block: 2, LogIndex: 0},
		{Block: 1, LogIndex: 300},
		{Block: 1, LogIndex: 2},
		{Block: 48210577, LogIndex: 9},
		{Block: 1, LogIndex: 0},
		{Block: 0, LogIndex: 0},
		{Block: 256, LogIndex: 1},
	}
	byKey := append([]Cursor(nil), in...)
	sort.Slice(byKey, func(i, j int) bool { return bytes.Compare(byKey[i].Key(), byKey[j].Key()) < 0 })

	byValue := append([]Cursor(nil), in...)
	sort.Slice(byValue, func(i, j int) bool {
		if byValue[i].Block != byValue[j].Block {
			return byValue[i].Block < byValue[j].Block
		}
		return byValue[i].LogIndex < byValue[j].LogIndex
	})

	for i := range byKey {
		if byKey[i] != byValue[i] {
			t.Fatalf("byte order diverges from chain order at %d: %v vs %v", i, byKey, byValue)
		}
	}
}

func TestCursorWireFormRoundTrips(t *testing.T) {
	for _, c := range []Cursor{{}, {Block: 1, LogIndex: 0}, {Block: 48210577, LogIndex: 9}} {
		got, err := ParseCursor(c.String())
		if err != nil {
			t.Fatalf("ParseCursor(%q): %v", c.String(), err)
		}
		if got != c {
			t.Fatalf("round-trip %v -> %q -> %v", c, c.String(), got)
		}
	}
	if got, err := ParseCursor(""); err != nil || got != (Cursor{}) {
		t.Fatalf(`ParseCursor("") = (%v, %v), want (zero, nil)`, got, err)
	}
	// The old "<block>-<logindex>" form is no longer a cursor: it leaked the
	// chain's position to apps that have no use for it (§27).
	for _, bad := range []string{"nope", "1", "1-", "-1", "1-x", "x-1", "48210577-9", "00", ""[:0] + "0000000000000064000000"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Fatalf("ParseCursor(%q) accepted", bad)
		}
	}
}

// The cursor is opaque to apps but still ordered, which is the one property an
// app is allowed to rely on: it may compare two ids to know which came first
// without being able to read a block height out of either (§27).
func TestCursorStringsSortLikeTheChain(t *testing.T) {
	ordered := []Cursor{
		{Block: 100, LogIndex: 3},
		{Block: 100, LogIndex: 4},
		{Block: 101, LogIndex: 0},
		{Block: 48210577, LogIndex: 9},
	}
	for i := 1; i < len(ordered); i++ {
		prev, next := ordered[i-1].String(), ordered[i].String()
		if prev >= next {
			t.Fatalf("cursor strings out of order: %q then %q", prev, next)
		}
	}
	// And nothing human-readable survives: no separator, no decimal block.
	if s := (Cursor{Block: 48210577, LogIndex: 9}).String(); strings.Contains(s, "-") ||
		strings.Contains(s, "48210577") {
		t.Fatalf("cursor %q still reads as a block position", s)
	}
}

func TestCursorNextCrossesBlockBoundary(t *testing.T) {
	if got := (Cursor{Block: 5, LogIndex: 7}).Next(); got != (Cursor{Block: 5, LogIndex: 8}) {
		t.Fatalf("Next within block = %v", got)
	}
	// A block cannot really hold 2^32 logs, but the successor must stay
	// monotonic regardless, or a feed could stall at the boundary.
	if got := (Cursor{Block: 5, LogIndex: ^uint32(0)}).Next(); got != (Cursor{Block: 6, LogIndex: 0}) {
		t.Fatalf("Next at index overflow = %v", got)
	}
}

func TestCursorRecoveryFromIndexKey(t *testing.T) {
	want := Cursor{Block: 48210577, LogIndex: 9}
	got, ok := cursorFromKey(want.Key())
	if !ok || got != want {
		t.Fatalf("cursorFromKey = (%v, %v), want (%v, true)", got, ok, want)
	}
	if _, ok := cursorFromKey([]byte("short")); ok {
		t.Fatal("cursorFromKey accepted a key of the wrong width")
	}
}

func TestValidRef(t *testing.T) {
	for _, ok := range []string{"df", "lkr:acme", "a-b_c", "dot.ref", "x", "0"} {
		if err := ValidRef(ok); err != nil {
			t.Fatalf("ValidRef(%q) = %v, want nil", ok, err)
		}
	}
	long := make([]byte, MaxRef+1)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "DF", "with space", "slash/ref", "nul\x00ref", string(long)} {
		if err := ValidRef(bad); err == nil {
			t.Fatalf("ValidRef(%q) accepted", bad)
		}
	}
}

// Every composite key is built from fixed-width parts now, so concatenation is
// unambiguous without the separator the slug scoping needed (§32).
func TestWalletScopedKeysAreFixedWidth(t *testing.T) {
	a, b := nextID(), nextID()
	ka := join(a.Key(), Cursor{Block: 1, LogIndex: 2}.Key())
	kb := join(b.Key(), Cursor{Block: 1, LogIndex: 2}.Key())
	if bytes.Equal(ka, kb) {
		t.Fatal("two wallets produced the same index key")
	}
	if !bytes.HasPrefix(ka, walletPrefix(a)) || bytes.HasPrefix(ka, walletPrefix(b)) {
		t.Fatal("wallet prefixes leak between wallets")
	}
	if len(ka) != 8+12 {
		t.Fatalf("key width = %d, want 20", len(ka))
	}
}

func TestPrefixEnd(t *testing.T) {
	if got := prefixEnd([]byte{1, 2, 3}); !bytes.Equal(got, []byte{1, 2, 4}) {
		t.Fatalf("prefixEnd = %v", got)
	}
	if got := prefixEnd([]byte{1, 0xFF}); !bytes.Equal(got, []byte{2}) {
		t.Fatalf("prefixEnd with trailing 0xFF = %v", got)
	}
	if got := prefixEnd([]byte{0xFF, 0xFF}); got != nil {
		t.Fatalf("prefixEnd of all-0xFF = %v, want nil", got)
	}
}

func TestSendKeysOrderByNoncePerSigner(t *testing.T) {
	// Re-broadcast has to go out in nonce order, which this key layout provides
	// for free rather than through a sort.
	s1, s2 := addr(1), addr(2)
	keys := [][]byte{
		sendKey(s2, 0),
		sendKey(s1, 300),
		sendKey(s1, 2),
		sendKey(s1, 0),
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	want := [][]byte{sendKey(s1, 0), sendKey(s1, 2), sendKey(s1, 300), sendKey(s2, 0)}
	for i := range want {
		if !bytes.Equal(keys[i], want[i]) {
			t.Fatalf("send keys out of order at %d", i)
		}
	}
}
