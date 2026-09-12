package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Buckets. One per logical collection; the name mirrors the namespace path in
// docs/REWRITE.md §3. Keys inside a bucket are byte-sorted, which several parts
// of the design lean on directly: the timer due-queue is an ordered scan, the
// send journal is nonce-ordered per signer, deposit dedup is a property of the
// key, and the deposit cursor is the key itself.
var (
	bMeta = []byte("meta")  // "version"
	bStat = []byte("state") // "cursor"

	bFlow = []byte("state/flow") // flow id            -> Flow
	bTx   = []byte("state/tx")   // tx hash            -> TxRef   (THE tx watchlist)
	bSend = []byte("state/send") // signer ++ nonce -> Send (the journal)

	bApp    = []byte("data/app")    // slug        -> App (carries the ledger)
	bWallet = []byte("data/wallet") // wallet id   -> Wallet
	bAddr   = []byte("data/addr")   // address     -> wallet id
	bRef    = []byte("data/ref")    // slug \0 ref -> wallet id

	bDeposit    = []byte("log/deposit")    // tx hash ++ log index -> Deposit
	bWithdrawal = []byte("log/withdrawal") // withdrawal id        -> Withdrawal

	iDep     = []byte("idx/dep")      // slug \0 block ++ logidx    -> deposit key (the cursor)
	iDepOpen = []byte("idx/dep_open") // slug \0 wallet ++ depkey   -> deposit key (awaiting drain)
	iWd      = []byte("idx/wd")       // slug \0 created ++ id      -> nil
	iWdOpen  = []byte("idx/wd_open")  // slug \0 id                 -> nil
	iWdIdem  = []byte("idx/wd_idem")  // slug \0 key                -> withdrawal id
)

// buckets is every bucket the store creates on open.
var buckets = [][]byte{
	bMeta, bStat,
	bFlow, bTx, bSend,
	bApp, bWallet, bAddr, bRef,
	bDeposit, bWithdrawal,
	iDep, iDepOpen, iWd, iWdOpen, iWdIdem,
}

var (
	keyVersion  = []byte("version")
	keyCursor   = []byte("cursor")
	keyToken    = []byte("token")    // the token this database is about
	keyDecimals = []byte("decimals") // its decimals(), read from the chain once
	keyChainID  = []byte("chain_id") // the chain it lives on, reported by the endpoint
)

// ErrBadSlug is returned for an app slug that cannot be used in a key.
var ErrBadSlug = errors.New("store: invalid app slug")

// slugSep separates a variable-length slug from the fixed-width remainder of a
// composite key. Slugs may not contain it, which validSlug enforces.
const slugSep = 0x00

// ValidSlug accepts the conservative subset that keeps composite keys
// unambiguous and URLs clean: lowercase letters, digits, '-', '_' and ':' (so
// namespaced ids like "lkr:acme" work), 1..64 bytes. It is exported because the
// HTTP layer must reject a bad slug before it reaches a key.
func ValidSlug(s string) error {
	if s == "" || len(s) > 64 {
		return fmt.Errorf("%w: length %d", ErrBadSlug, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == ':':
		default:
			return fmt.Errorf("%w: byte %q at %d", ErrBadSlug, c, i)
		}
	}
	return nil
}

// scoped builds "<slug>\0<rest...>" — the prefix form every per-app index uses.
func scoped(slug string, rest ...[]byte) []byte {
	k := make([]byte, 0, len(slug)+1+8)
	k = append(k, slug...)
	k = append(k, slugSep)
	for _, r := range rest {
		k = append(k, r...)
	}
	return k
}

// scopePrefix is the range prefix for one app inside a scoped bucket.
func scopePrefix(slug string) []byte { return scoped(slug) }

func be64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// depositKey is tx hash ++ log index: unique per on-chain transfer, so the key
// itself is the dedup constraint — no uniqueness check to forget.
func depositKey(tx common.Hash, logIndex uint32) []byte {
	k := make([]byte, 0, common.HashLength+4)
	k = append(k, tx.Bytes()...)
	return append(k, be32(logIndex)...)
}

// sendKey is signer ++ nonce, so a scan over one signer's entries is in nonce
// order — which is what makes re-broadcasting a dropped transaction
// deterministic (§4).
func sendKey(signer common.Address, nonce uint64) []byte {
	k := make([]byte, 0, common.AddressLength+8)
	k = append(k, signer.Bytes()...)
	return append(k, be64(nonce)...)
}

// Cursor is a deposit-feed position: the chain's own ordering, (block,
// log_index). It sorts identically to the chain, is verifiable against a block
// explorer, and as a 12-byte key is directly seekable (§9).
type Cursor struct {
	Block    uint64
	LogIndex uint32
}

// Key renders the cursor as its 12-byte sortable form.
func (c Cursor) Key() []byte { return append(be64(c.Block), be32(c.LogIndex)...) }

// Next returns the smallest cursor strictly greater than c, which is what a
// "give me everything after this" scan seeks to.
func (c Cursor) Next() Cursor {
	if c.LogIndex == ^uint32(0) {
		return Cursor{Block: c.Block + 1}
	}
	return Cursor{Block: c.Block, LogIndex: c.LogIndex + 1}
}

// String is the wire form handed to apps: "<block>-<logindex>". Apps are told
// to treat it as opaque and ordered — pass it back, compare it, don't parse it.
func (c Cursor) String() string {
	return strconv.FormatUint(c.Block, 10) + "-" + strconv.FormatUint(uint64(c.LogIndex), 10)
}

// ParseCursor reads the wire form. An empty string is the zero cursor, i.e.
// "from the beginning".
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	blk, idx, ok := strings.Cut(s, "-")
	if !ok {
		return Cursor{}, fmt.Errorf("store: bad cursor %q", s)
	}
	b, err := strconv.ParseUint(blk, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("store: bad cursor block in %q: %w", s, err)
	}
	i, err := strconv.ParseUint(idx, 10, 32)
	if err != nil {
		return Cursor{}, fmt.Errorf("store: bad cursor index in %q: %w", s, err)
	}
	return Cursor{Block: b, LogIndex: uint32(i)}, nil
}

// cursorFromScoped recovers the cursor from an idx/dep key ("<slug>\0<block><logidx>").
func cursorFromScoped(key []byte) (Cursor, bool) {
	i := bytes.IndexByte(key, slugSep)
	if i < 0 || len(key)-i-1 != 12 {
		return Cursor{}, false
	}
	rest := key[i+1:]
	return Cursor{
		Block:    binary.BigEndian.Uint64(rest[:8]),
		LogIndex: binary.BigEndian.Uint32(rest[8:12]),
	}, true
}
