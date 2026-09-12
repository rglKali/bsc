package store

import (
	"bytes"
	"sort"
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
	for _, bad := range []string{"nope", "1", "1-", "-1", "1-x", "x-1"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Fatalf("ParseCursor(%q) accepted", bad)
		}
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
	got, ok := cursorFromScoped(scoped("lkr:acme", want.Key()))
	if !ok || got != want {
		t.Fatalf("cursorFromScoped = (%v, %v), want (%v, true)", got, ok, want)
	}
	if _, ok := cursorFromScoped([]byte("no-separator")); ok {
		t.Fatal("cursorFromScoped accepted a key with no separator")
	}
	if _, ok := cursorFromScoped(scoped("df", []byte{1, 2, 3})); ok {
		t.Fatal("cursorFromScoped accepted a short suffix")
	}
}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"df", "lkr:acme", "a-b_c", "x", "0"} {
		if err := ValidSlug(ok); err != nil {
			t.Fatalf("ValidSlug(%q) = %v, want nil", ok, err)
		}
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "DF", "with space", "dot.slug", "slash/slug", "nul\x00slug", string(long)} {
		if err := ValidSlug(bad); err == nil {
			t.Fatalf("ValidSlug(%q) accepted", bad)
		}
	}
}

func TestScopedKeysCannotCollideAcrossApps(t *testing.T) {
	// The separator is what keeps a variable-length slug unambiguous: without
	// it, app "a" with ref "bc" and app "ab" with ref "c" would share a key.
	a := scoped("a", []byte("bc"))
	b := scoped("ab", []byte("c"))
	if bytes.Equal(a, b) {
		t.Fatalf("keys collide: %q == %q", a, b)
	}
	if !bytes.HasPrefix(a, scopePrefix("a")) || bytes.HasPrefix(b, scopePrefix("a")) {
		t.Fatalf("prefix scoping leaks between apps: %q %q", a, b)
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
