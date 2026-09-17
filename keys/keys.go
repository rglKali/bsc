// Package keys derives every managed wallet's secp256k1 key from one master
// secret.
//
// Derivation is flat: a wallet's private key is HMAC-SHA256(master, id) where
// id is the 16-byte UUID the store assigns it. There is no hierarchy in the key
// material — a wallet's drain_to is metadata the store holds, not structure
// baked into keys — which keeps every wallet independent of every other and
// means a leaked child key reveals nothing about its siblings.
//
// HMAC-SHA256 as a KDF and ECDSA signing are unrelated primitives over the same
// secret, and HMAC's PRF security is what makes deriving keys this way sound.
// Its 32-byte output is used directly as the private key; on the ~2⁻¹²⁸ chance
// that it is not a valid secp256k1 scalar, Generate retries with a fresh UUID
// so callers never see a curve error.
//
// The master secret is the whole security model. It signs every transaction,
// pays all gas, and holds a MaxUint256 allowance on every managed wallet — so
// whoever holds it can move every wallet's funds, and losing it loses them all.
// It belongs in a secrets manager, never in the database and never on disk.
package keys

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
)

// masterLen is the required master-secret width: a raw secp256k1 private key.
const masterLen = 32

// generateAttempts bounds Generate's retry on invalid key material. Each
// attempt fails with probability ~2⁻¹²⁸, so exhausting four means something is
// wrong with the entropy source, not bad luck.
const generateAttempts = 4

// ErrInvalidKey means key material is not a valid secp256k1 private scalar.
var ErrInvalidKey = errors.New("keys: invalid secp256k1 key material")

// Key is a derived private key and the address it controls.
type Key struct {
	Priv    *ecdsa.PrivateKey
	Address common.Address
}

// Ring derives all managed-wallet keys from the master secret.
type Ring struct {
	master []byte
}

// New validates that master is a 32-byte secp256k1 private key and returns the
// ring rooted at it.
func New(master []byte) (*Ring, error) {
	if len(master) != masterLen {
		return nil, fmt.Errorf("keys: master secret must be %d bytes (got %d)", masterLen, len(master))
	}
	if _, err := crypto.ToECDSA(master); err != nil {
		return nil, fmt.Errorf("keys: master secret: %w", ErrInvalidKey)
	}
	return &Ring{master: append([]byte(nil), master...)}, nil
}

// ParseHex decodes a master secret from hex, with or without a 0x prefix. This
// is the only place the secret is parsed, so the error messages never echo it.
func ParseHex(s string) (*Ring, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil {
		return nil, errors.New("keys: master secret must be hex")
	}
	return New(b)
}

// Master returns the gas-paying wallet: the master secret used directly as a
// private key. It is the sender of every funding transfer and every
// transferFrom, and the spender every managed wallet approves.
func (r *Ring) Master() (Key, error) {
	return toKey(r.master)
}

// Derive returns the key for a wallet id. The id's raw 16 bytes are the HMAC
// message — not its string form — so there is no way for two spellings of the
// same UUID to derive two different keys.
func (r *Ring) Derive(id uuid.UUID) (Key, error) {
	return toKey(mac(r.master, id[:]))
}

// Generate assigns a fresh wallet id and derives its key, retrying on the
// vanishingly rare invalid-scalar case so the caller never has to handle it.
func (r *Ring) Generate() (uuid.UUID, Key, error) {
	for range generateAttempts {
		id, err := uuid.NewRandom()
		if err != nil {
			return uuid.Nil, Key{}, fmt.Errorf("keys: generate id: %w", err)
		}
		key, err := r.Derive(id)
		if errors.Is(err, ErrInvalidKey) {
			continue
		}
		if err != nil {
			return uuid.Nil, Key{}, err
		}
		return id, key, nil
	}
	return uuid.Nil, Key{}, ErrInvalidKey
}

func mac(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// toKey turns 32 bytes of key material into a Key, or ErrInvalidKey.
func toKey(material []byte) (Key, error) {
	priv, err := crypto.ToECDSA(material)
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	return Key{Priv: priv, Address: crypto.PubkeyToAddress(priv.PublicKey)}, nil
}
