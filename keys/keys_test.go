package keys

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// master is a valid, fixed secret so derivations are reproducible in tests.
var master = bytes.Repeat([]byte{0x11}, 32)

func ring(t *testing.T) *Ring {
	t.Helper()
	r, err := New(master)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestNewValidatesTheMaster(t *testing.T) {
	for name, secret := range map[string][]byte{
		"empty": {},
		"short": bytes.Repeat([]byte{1}, 31),
		"long":  bytes.Repeat([]byte{1}, 33),
		"zero":  make([]byte, 32), // not a valid scalar
	} {
		if _, err := New(secret); err == nil {
			t.Fatalf("%s master accepted", name)
		}
	}
	if _, err := New(master); err != nil {
		t.Fatalf("valid master rejected: %v", err)
	}
}

func TestNewCopiesTheMaster(t *testing.T) {
	// The caller's buffer may be reused or zeroed; the ring must not alias it.
	secret := bytes.Repeat([]byte{0x22}, 32)
	r, err := New(secret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before, err := r.Master()
	if err != nil {
		t.Fatalf("Master: %v", err)
	}
	for i := range secret {
		secret[i] = 0
	}
	after, err := r.Master()
	if err != nil {
		t.Fatalf("Master after mutation: %v", err)
	}
	if before.Address != after.Address {
		t.Fatalf("master address changed when the caller's buffer did: %s -> %s",
			before.Address.Hex(), after.Address.Hex())
	}
}

func TestParseHex(t *testing.T) {
	hexSecret := strings.Repeat("11", 32)
	for _, in := range []string{hexSecret, "0x" + hexSecret, "  " + hexSecret + "\n"} {
		r, err := ParseHex(in)
		if err != nil {
			t.Fatalf("ParseHex(%q): %v", in, err)
		}
		got, err := r.Master()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := ring(t).Master()
		if got.Address != want.Address {
			t.Fatalf("ParseHex(%q) derived %s, want %s", in, got.Address.Hex(), want.Address.Hex())
		}
	}
	for _, bad := range []string{"", "zz", strings.Repeat("11", 31), "0x" + strings.Repeat("11", 33)} {
		if _, err := ParseHex(bad); err == nil {
			t.Fatalf("ParseHex(%q) accepted", bad)
		}
	}
}

func TestParseHexErrorDoesNotEchoTheSecret(t *testing.T) {
	// Error strings reach logs; a malformed secret is still a secret.
	secret := strings.Repeat("ab", 31) // wrong length, valid hex
	_, err := ParseHex(secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "abab") {
		t.Fatalf("error echoed the secret: %v", err)
	}
}

func TestDerivationIsDeterministicAndDistinct(t *testing.T) {
	r := ring(t)
	a, b := uint64(1), uint64(2)

	first, err := r.Derive(a)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	again, err := r.Derive(a)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if first.Address != again.Address {
		t.Fatalf("derivation is not deterministic: %s vs %s", first.Address.Hex(), again.Address.Hex())
	}

	other, err := r.Derive(b)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if other.Address == first.Address {
		t.Fatal("distinct ids derived the same address")
	}
}

func TestDerivationIsHMACOfTheFixedWidthIndex(t *testing.T) {
	// The message is 8 bytes big-endian, and the width is the point: a
	// variable-width encoding would let 1 and 256 share a message, which would
	// hand two wallets the same private key.
	r := ring(t)
	key, err := r.Derive(1)
	if err != nil {
		t.Fatal(err)
	}
	want := mac(master, []byte{0, 0, 0, 0, 0, 0, 0, 1})
	if !bytes.Equal(crypto.FromECDSA(key.Priv), want) {
		t.Fatal("derived key is not HMAC(master, be64(index))")
	}

	seen := map[common.Address]uint64{}
	for _, i := range []uint64{1, 255, 256, 257, 65535, 65536, 1 << 32, 1<<64 - 1} {
		k, err := r.Derive(i)
		if err != nil {
			t.Fatalf("Derive(%d): %v", i, err)
		}
		if prev, dup := seen[k.Address]; dup {
			t.Fatalf("indices %d and %d derived the same address", prev, i)
		}
		seen[k.Address] = i
	}
}

// The whole point of a counter: the database is no longer half the key. Walking
// the sequence reproduces every address the service ever issued, from the
// secret alone (§48).
func TestTheSequenceIsRecoverableFromTheSecretAlone(t *testing.T) {
	issued := map[uint64]common.Address{}
	func() {
		r := ring(t)
		for i := uint64(1); i <= 64; i++ {
			k, err := r.Derive(i)
			if err != nil {
				t.Fatal(err)
			}
			issued[i] = k.Address
		}
	}() // the ring, and any notion of what was issued, goes out of scope here

	recovered := ring(t) // a fresh ring, from the same secret and nothing else
	for i := uint64(1); i <= 64; i++ {
		k, err := recovered.Derive(i)
		if err != nil {
			t.Fatal(err)
		}
		if k.Address != issued[i] {
			t.Fatalf("index %d recovered %s, want %s", i, k.Address.Hex(), issued[i].Hex())
		}
	}
}

func TestIndexZeroIsNotAWallet(t *testing.T) {
	r := ring(t)
	if _, err := r.Derive(0); !errors.Is(err, ErrZeroIndex) {
		t.Fatalf("Derive(0) = %v, want ErrZeroIndex", err)
	}
	// Allocate must never hand out 0, even when asked for it.
	id, _, err := r.Allocate(0)
	if err != nil {
		t.Fatal(err)
	}
	if id < FirstIndex {
		t.Fatalf("Allocate(0) handed out %d, want >= %d", id, FirstIndex)
	}
}

func TestDerivationIsMasterSpecific(t *testing.T) {
	const id = 7
	a, _ := New(bytes.Repeat([]byte{0x11}, 32))
	b, _ := New(bytes.Repeat([]byte{0x22}, 32))
	ka, err := a.Derive(id)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := b.Derive(id)
	if err != nil {
		t.Fatal(err)
	}
	if ka.Address == kb.Address {
		t.Fatal("the same id under different masters derived the same address")
	}
}

func TestMasterIsTheSecretItself(t *testing.T) {
	r := ring(t)
	m, err := r.Master()
	if err != nil {
		t.Fatalf("Master: %v", err)
	}
	if !bytes.Equal(crypto.FromECDSA(m.Priv), master) {
		t.Fatal("the master key is not the master secret used directly")
	}
	// The master must not collide with any derived wallet.
	d, err := r.Derive(1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Address == m.Address {
		t.Fatal("a derived wallet collided with the master")
	}
}

func TestAllocateWalksForwardAndAgreesWithDerive(t *testing.T) {
	r := ring(t)
	seen := make(map[uint64]bool)
	for want := uint64(1); want <= 32; want++ {
		id, key, err := r.Allocate(want)
		if err != nil {
			t.Fatalf("Allocate(%d): %v", want, err)
		}
		if id < want {
			t.Fatalf("Allocate(%d) went backwards to %d", want, id)
		}
		if seen[id] {
			t.Fatalf("Allocate repeated id %d", id)
		}
		seen[id] = true

		// The returned key must be exactly what Derive(id) produces, or the
		// store would record an id that cannot reproduce its own address.
		again, err := r.Derive(id)
		if err != nil {
			t.Fatalf("Derive(%d): %v", id, err)
		}
		if again.Address != key.Address {
			t.Fatalf("Allocate/Derive disagree for %d", id)
		}
	}
}

func TestErrInvalidKeyIsMatchable(t *testing.T) {
	_, err := New(make([]byte, 32))
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v, want ErrInvalidKey", err)
	}
}
