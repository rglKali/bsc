package store

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Buckets. One per logical collection. Keys inside a bucket are byte-sorted,
// which several parts of the design lean on directly: the send journal is
// nonce-ordered per signer, deposit dedup is a property of the key, and the
// deposit cursor is the key itself.
//
// Every per-app prefix is gone. Scoping used to be "<slug>\0<rest>" on five
// indexes, which is what made an app a thing the store had to know about; a
// wallet id is fixed-width, so the composites below need no separator and no
// slug validation to keep them unambiguous (§32).
var (
	bMeta = []byte("meta")  // "version", "token", "decimals", "chain_id"
	bStat = []byte("state") // "cursor"

	bFlow = []byte("state/flow") // flow id          -> Flow
	bTx   = []byte("state/tx")   // tx hash          -> TxRef   (THE tx watchlist)
	bSend = []byte("state/send") // signer ++ nonce  -> Send    (the journal)

	bWallet = []byte("data/wallet") // wallet id -> Wallet
	bAddr   = []byte("data/addr")   // address   -> wallet id
	bRef    = []byte("data/ref")    // ref       -> wallet id

	bDeposit    = []byte("log/deposit")    // tx hash ++ log index -> Deposit
	bWithdrawal = []byte("log/withdrawal") // withdrawal id        -> Withdrawal

	iDep       = []byte("idx/dep")        // block ++ logidx            -> deposit key (the cursor)
	iDepWallet = []byte("idx/dep_wallet") // wallet ++ block ++ logidx  -> deposit key
	iDepOpen   = []byte("idx/dep_open")   // wallet ++ depkey           -> deposit key (awaiting a drain)
	iWd        = []byte("idx/wd")         // created ++ id              -> nil
	iWdFeed    = []byte("idx/wd_feed")    // block ++ id                -> nil (settled only)
	iWdWallet  = []byte("idx/wd_wallet")  // wallet ++ created ++ id    -> nil
	iWdOpen    = []byte("idx/wd_open")    // id                         -> nil
	iWdIdem    = []byte("idx/wd_idem")    // idempotency key            -> withdrawal id
)

// buckets is every bucket the store creates on open.
var buckets = [][]byte{
	bMeta, bStat,
	bFlow, bTx, bSend,
	bWallet, bAddr, bRef,
	bDeposit, bWithdrawal,
	iDep, iDepWallet, iDepOpen, iWd, iWdFeed, iWdWallet, iWdOpen, iWdIdem,
}

var (
	keyVersion  = []byte("version")
	keyCursor   = []byte("cursor")
	keyToken    = []byte("token")    // the token this database is about
	keyDecimals = []byte("decimals") // its decimals(), read from the chain once
	keyChainID  = []byte("chain_id") // the chain it lives on, reported by the endpoint
)

// ErrBadRef is returned for a wallet ref that cannot be used as a key or in a URL.
var ErrBadRef = errors.New("store: invalid wallet ref")

// MaxRef bounds a ref. It is a key in bbolt and a path segment over HTTP, and
// neither wants an unbounded string.
const MaxRef = 128

// ValidRef accepts the conservative subset that stays unambiguous in a key and
// clean in a URL: lowercase letters, digits, '-', '_', '.' and ':' — the last
// so a caller can namespace its own handles (`acme:cust-1`), which it must now
// do for itself, since bsc no longer has an app to scope them by (§37).
func ValidRef(s string) error {
	if s == "" || len(s) > MaxRef {
		return fmt.Errorf("%w: length %d (want 1..%d)", ErrBadRef, len(s), MaxRef)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == ':':
		default:
			return fmt.Errorf("%w: byte %q at %d", ErrBadRef, c, i)
		}
	}
	return nil
}

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

// join concatenates fixed-width key parts. Every composite in this store is
// built from fixed-width pieces, so concatenation is unambiguous without a
// separator.
func join(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	k := make([]byte, 0, n)
	for _, p := range parts {
		k = append(k, p...)
	}
	return k
}

// walletPrefix is the range prefix for one wallet inside a wallet-scoped index.
func walletPrefix(id uuid.UUID) []byte { return id[:] }

// depositKey is tx hash ++ log index: unique per on-chain transfer, so the key
// itself is the dedup constraint — no uniqueness check to forget.
func depositKey(tx common.Hash, logIndex uint32) []byte {
	return join(tx.Bytes(), be32(logIndex))
}

// sendKey is signer ++ nonce, so a scan over one signer's entries is in nonce
// order — which is what makes re-broadcasting a dropped transaction
// deterministic.
func sendKey(signer common.Address, nonce uint64) []byte {
	return join(signer.Bytes(), be64(nonce))
}

// Cursor is a deposit-feed position: the chain's own ordering, (block,
// log_index). Every deposit has one by construction, being a Transfer log, and
// as a 12-byte key it is directly seekable.
//
// The feed is global now rather than per-app. There is one caller — the service
// that keeps the books — and it reads every deposit bsc records, filtering by
// wallet if it wants to. Splitting the feed per app was only ever there to keep
// one tenant from reading another's, which is not a boundary this service draws
// any more (§37).
type Cursor struct {
	Block    uint64
	LogIndex uint32
}

// Key renders the cursor as its 12-byte sortable form.
func (c Cursor) Key() []byte { return join(be64(c.Block), be32(c.LogIndex)) }

// Next returns the smallest cursor strictly greater than c, which is what a
// "give me everything after this" scan seeks to.
func (c Cursor) Next() Cursor {
	if c.LogIndex == ^uint32(0) {
		return Cursor{Block: c.Block + 1}
	}
	return Cursor{Block: c.Block, LogIndex: c.LogIndex + 1}
}

// String is the wire form: the 12-byte sortable key as hex. Two cursors compare
// as strings in the order the chain produced them, so a caller can dedupe and
// resume without parsing one.
func (c Cursor) String() string { return hex.EncodeToString(c.Key()) }

// ParseCursor reads the wire form. An empty string is the zero cursor, i.e.
// "from the beginning".
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 12 {
		return Cursor{}, fmt.Errorf("store: bad cursor %q", s)
	}
	return Cursor{
		Block:    binary.BigEndian.Uint64(raw[:8]),
		LogIndex: binary.BigEndian.Uint32(raw[8:]),
	}, nil
}

// Settled is a position in the finalized-debit feed: the block a withdrawal
// landed in, plus its id to break ties within that block.
//
// Deposits get the chain's own (block, log_index), which every one has by
// construction. A withdrawal's ordering has to be *invented* — a payout is one
// transfer among many in its block and nothing ranks it against the others — so
// this pairs the block with the id. That is total and immutable, which is all a
// cursor needs; it is not meaningful, and a caller must not read one.
type Settled struct {
	Block uint64
	ID    uuid.UUID
}

// Key renders the position as its 24-byte sortable form.
func (s Settled) Key() []byte { return join(be64(s.Block), s.ID[:]) }

// Next is the smallest position strictly greater than s, which is what a
// "give me everything after this" scan seeks to.
func (s Settled) Next() Settled {
	next := s
	for i := len(next.ID) - 1; i >= 0; i-- {
		next.ID[i]++
		if next.ID[i] != 0 {
			return next
		}
	}
	// Every byte wrapped: the largest id in this block, so move to the next.
	return Settled{Block: s.Block + 1}
}

func (s Settled) String() string { return hex.EncodeToString(s.Key()) }

// ParseSettled reads the wire form. An empty string is the zero position.
func ParseSettled(v string) (Settled, error) {
	if v == "" {
		return Settled{}, nil
	}
	raw, err := hex.DecodeString(v)
	if err != nil || len(raw) != 8+16 {
		return Settled{}, fmt.Errorf("store: bad cursor %q", v)
	}
	out := Settled{Block: binary.BigEndian.Uint64(raw[:8])}
	copy(out.ID[:], raw[8:])
	return out, nil
}

func settledFromKey(key []byte) (Settled, bool) {
	if len(key) != 8+16 {
		return Settled{}, false
	}
	out := Settled{Block: binary.BigEndian.Uint64(key[:8])}
	copy(out.ID[:], key[8:])
	return out, true
}

// cursorFromKey recovers the cursor from a 12-byte idx/dep key.
func cursorFromKey(key []byte) (Cursor, bool) {
	if len(key) != 12 {
		return Cursor{}, false
	}
	return Cursor{
		Block:    binary.BigEndian.Uint64(key[:8]),
		LogIndex: binary.BigEndian.Uint32(key[8:12]),
	}, true
}
