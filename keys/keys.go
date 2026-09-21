// Package keys derives every managed wallet's secp256k1 key from one master
// secret.
//
// Derivation is flat: a wallet's private key is HMAC-SHA256(master, index)
// where index is the wallet's sequential number, as 8 bytes big-endian. There
// is no hierarchy in the key material — a wallet's drain_to is metadata the
// store holds, not structure baked into keys — which keeps every wallet
// independent of every other and means a leaked child key reveals nothing about
// its siblings.
//
// HMAC-SHA256 as a KDF and ECDSA signing are unrelated primitives over the same
// secret, and HMAC's PRF security is what makes deriving keys this way sound.
// Its 32-byte output is used directly as the private key; on the ~2⁻¹²⁸ chance
// that it is not a valid secp256k1 scalar, Allocate steps to the next index so
// callers never see a curve error.
//
// The master secret's own key is never part of this sequence. It is the
// operator's wallet — used from scripts, for swaps, by hand — and bsc stores
// nothing about it but its address, in meta, to bind the database to it (§49).
//
// # Why the message is a counter and not a random id
//
// HMAC's security as a PRF does not depend on the message being unpredictable —
// only on the key being secret. The PRF definition gives the adversary
// chosen-message query access, so HMAC(master, 0), HMAC(master, 1) … are
// indistinguishable from independent random 32-byte strings to anyone without
// the master. This is the same reasoning behind BIP-32 hardened derivation,
// which indexes 0, 1, 2 for exactly this reason. The 122 random bits in the
// UUID this replaced were doing no cryptographic work.
//
// What the counter buys is recovery. Under UUIDs, losing the database lost
// every address ever derived even while holding the master secret, because the
// id was half the key material and 122 random bits are not searchable. Now the
// master plus "there were about N of them" is a complete recovery: derive
// 0..N+gap, ask the chain what each holds, sweep. The database stops being the
// second half of the key (§48).
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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// masterLen is the required master-secret width: a raw secp256k1 private key.
const masterLen = 32

// FirstIndex is where wallet numbering starts. Zero is not a wallet and never
// becomes one — Derive refuses it — so a call site that forgot to fill an id in
// fails loudly instead of deriving a key for an address nothing tracks.
const FirstIndex = 1

// allocateAttempts bounds how far Allocate will step looking for a valid
// scalar. Each index fails with probability ~2⁻¹²⁸, so exhausting four means
// the master secret is not what it claims to be, not bad luck.
const allocateAttempts = 4

// ErrInvalidKey means key material is not a valid secp256k1 private scalar.
var ErrInvalidKey = errors.New("keys: invalid secp256k1 key material")

// ErrZeroIndex means something asked to derive index 0. Wallet ids start at 1,
// so a zero here is an unset field rather than a wallet.
var ErrZeroIndex = errors.New("keys: index 0 is not a wallet; ids start at 1")

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

// Derive returns the key for a wallet index.
//
// The message is the index as 8 bytes big-endian — fixed width, so there is no
// way for two spellings of the same number to derive two different keys, and
// the encoding is the same one the store uses for the wallet's key.
//
// Zero is refused rather than derived. It is a perfectly good key mathematically;
// it is just not one this service ever issues, so reaching here with 0 means a
// caller passed an unset field, and a silent key for an address nobody watches
// is the worst possible answer to that.
func (r *Ring) Derive(index uint64) (Key, error) {
	if index == 0 {
		return Key{}, ErrZeroIndex
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], index)
	return toKey(mac(r.master, msg[:]))
}

// Allocate returns the first index at or after `from` whose derivation is a
// valid key, together with that key.
//
// Skipping is deterministic: the same master skips the same indices, so a
// recovery that walks 0, 1, 2 … reproduces exactly the addresses that were
// issued. A skipped index is simply never assigned to a wallet, which is why
// the sequence is allowed to have gaps and why nothing may assume that a
// wallet's index is its position in creation order.
func (r *Ring) Allocate(from uint64) (uint64, Key, error) {
	if from < FirstIndex {
		from = FirstIndex
	}
	for i := range uint64(allocateAttempts) {
		index := from + i
		key, err := r.Derive(index)
		if errors.Is(err, ErrInvalidKey) {
			continue
		}
		if err != nil {
			return 0, Key{}, err
		}
		return index, key, nil
	}
	return 0, Key{}, ErrInvalidKey
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
