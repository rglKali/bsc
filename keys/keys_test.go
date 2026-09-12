package keys

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
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
	a, b := uuid.New(), uuid.New()

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

func TestDerivationUsesRawIDBytes(t *testing.T) {
	// Keying on the raw 16 bytes rather than a string removes any chance that
	// two spellings of the same UUID derive two different wallets.
	r := ring(t)
	id := uuid.MustParse("0f7c3e2a-1b4d-4c8e-9a6f-2d5b8c1e4f70")
	upper := uuid.MustParse(strings.ToUpper(id.String()))
	lower, err := r.Derive(id)
	if err != nil {
		t.Fatal(err)
	}
	upperKey, err := r.Derive(upper)
	if err != nil {
		t.Fatal(err)
	}
	if lower.Address != upperKey.Address {
		t.Fatal("case of the UUID's string form changed the derived key")
	}

	// And it really is HMAC(master, id[:]).
	want := mac(master, id[:])
	if !bytes.Equal(crypto.FromECDSA(lower.Priv), want) {
		t.Fatal("derived key is not HMAC(master, raw id bytes)")
	}
}

func TestDerivationIsMasterSpecific(t *testing.T) {
	id := uuid.New()
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
	d, err := r.Derive(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if d.Address == m.Address {
		t.Fatal("a derived wallet collided with the master")
	}
}

func TestGenerateReturnsAUsableWallet(t *testing.T) {
	r := ring(t)
	seen := make(map[uuid.UUID]bool)
	for range 32 {
		id, key, err := r.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if id == uuid.Nil {
			t.Fatal("Generate returned the nil UUID")
		}
		if seen[id] {
			t.Fatalf("Generate repeated id %s", id)
		}
		seen[id] = true

		// The returned key must be exactly what Derive(id) produces, or the
		// store would record an id that cannot reproduce its own address.
		again, err := r.Derive(id)
		if err != nil {
			t.Fatalf("Derive(%s): %v", id, err)
		}
		if again.Address != key.Address {
			t.Fatalf("Generate/Derive disagree for %s", id)
		}
	}
}

func TestErrInvalidKeyIsMatchable(t *testing.T) {
	_, err := New(make([]byte, 32))
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v, want ErrInvalidKey", err)
	}
}
