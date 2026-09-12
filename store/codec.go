// Package store is bsc's single datastore: one bbolt file holding every record
// the service keeps, in three namespaces (state/, data/, log/) plus their
// indexes. See docs/REWRITE.md §3.
//
// Records are hand-packed binary rather than JSON: a 20-byte address is 20
// bytes, a hash is 32, and a uint256 amount is its native 32-byte big-endian
// form. That keeps common.Address / common.Hash / *big.Int conversions free
// (BytesToAddress, SetBytes, FillBytes) with no struct tags, no reflection and
// no generated code.
//
// Every record starts with a version byte. Decoders switch on it and read only
// the fields that version carried, so a new field is an append plus a bumped
// constant — never a migration.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// weiLen is the width of an encoded uint256 amount.
const weiLen = 32

var errShort = errors.New("store: record truncated")

// enc appends packed fields to a growing buffer. Encoding errors (an amount
// that is negative or wider than 256 bits) are latched rather than returned per
// call, so callers check err once at the end.
type enc struct {
	b   []byte
	err error
}

func (e *enc) u8(v uint8)   { e.b = append(e.b, v) }
func (e *enc) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *enc) u64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }

// i64 carries a Cents amount. Ledger figures are machine integers rather than
// big-endian big.Ints because they are exact by construction and never need to
// be sorted as keys.
func (e *enc) i64(v int64) { e.u64(uint64(v)) }

func (e *enc) boolean(v bool) {
	if v {
		e.u8(1)
		return
	}
	e.u8(0)
}

// raw appends fixed-width bytes with no length prefix — the reader knows the width.
func (e *enc) raw(p []byte) { e.b = append(e.b, p...) }

func (e *enc) addr(a common.Address) { e.raw(a.Bytes()) }
func (e *enc) hash(h common.Hash)    { e.raw(h.Bytes()) }
func (e *enc) id(u uuid.UUID)        { e.raw(u[:]) }

// str appends a uint16-length-prefixed string. 64KiB is far above any slug,
// ref, idempotency key or error message we store.
func (e *enc) str(s string) {
	if len(s) > 0xFFFF {
		e.err = fmt.Errorf("store: string too long (%d bytes)", len(s))
		return
	}
	e.b = binary.BigEndian.AppendUint16(e.b, uint16(len(s)))
	e.b = append(e.b, s...)
}

// blob appends a uint32-length-prefixed byte slice (signed transactions).
func (e *enc) blob(p []byte) {
	e.b = binary.BigEndian.AppendUint32(e.b, uint32(len(p)))
	e.b = append(e.b, p...)
}

// wei appends an amount as 32-byte big-endian. A nil amount encodes as zero;
// anything negative or wider than 256 bits is a programming error and is
// latched, because silently truncating money is not an option.
func (e *enc) wei(v *big.Int) {
	var buf [weiLen]byte
	if v != nil {
		if v.Sign() < 0 {
			e.err = fmt.Errorf("store: negative amount %s", v)
			return
		}
		if v.BitLen() > 256 {
			e.err = fmt.Errorf("store: amount exceeds uint256 (%d bits)", v.BitLen())
			return
		}
		v.FillBytes(buf[:])
	}
	e.raw(buf[:])
}

// stamp appends a timestamp as unix nanoseconds, with the zero time as 0 (not
// time.Time{}.UnixNano(), which is a large negative number).
func (e *enc) stamp(t time.Time) {
	if t.IsZero() {
		e.u64(0)
		return
	}
	e.u64(uint64(t.UnixNano()))
}

// dec reads packed fields. A short read latches errShort and yields zero
// values, so a decoder can read straight through and check err once.
type dec struct {
	b   []byte
	err error
}

func newDec(b []byte) *dec { return &dec{b: b} }

func (d *dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if len(d.b) < n {
		d.err = errShort
		return nil
	}
	p := d.b[:n]
	d.b = d.b[n:]
	return p
}

func (d *dec) u8() uint8 {
	p := d.take(1)
	if p == nil {
		return 0
	}
	return p[0]
}

func (d *dec) u32() uint32 {
	p := d.take(4)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint32(p)
}

func (d *dec) u64() uint64 {
	p := d.take(8)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint64(p)
}

func (d *dec) i64() int64 { return int64(d.u64()) }

func (d *dec) boolean() bool { return d.u8() == 1 }

func (d *dec) addr() common.Address { return common.BytesToAddress(d.take(common.AddressLength)) }
func (d *dec) hash() common.Hash    { return common.BytesToHash(d.take(common.HashLength)) }

func (d *dec) id() uuid.UUID {
	var u uuid.UUID
	copy(u[:], d.take(len(u)))
	return u
}

func (d *dec) str() string {
	p := d.take(2)
	if p == nil {
		return ""
	}
	return string(d.take(int(binary.BigEndian.Uint16(p))))
}

// blob returns an owned copy: bbolt values are only valid for the life of the
// transaction that read them, so nothing may alias the mmap.
func (d *dec) blob() []byte {
	n := d.u32()
	p := d.take(int(n))
	if p == nil {
		return nil
	}
	return append([]byte(nil), p...)
}

// wei returns a non-nil amount (zero when the field is zero), so callers never
// have to nil-check money.
func (d *dec) wei() *big.Int {
	p := d.take(weiLen)
	if p == nil {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(p)
}

func (d *dec) stamp() time.Time {
	v := d.u64()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(v)).UTC()
}

// done reports the first error hit while decoding.
func (d *dec) done() error { return d.err }
