package ezsp

import (
	"encoding/binary"
	"fmt"
)

// Zigbee is little-endian on the wire throughout, including IEEE addresses,
// which are transmitted least-significant byte first and therefore read
// backwards compared to how they are printed.

// buf is a cursor over a response payload. Every read is bounds-checked and
// the first failure is latched, so a parser can read a whole struct and check
// for an error once at the end rather than after every field.
type buf struct {
	b   []byte
	pos int
	err error
}

func newBuf(b []byte) *buf { return &buf{b: b} }

// need reports whether n more bytes are available, latching an error if not.
func (r *buf) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.pos+n > len(r.b) {
		r.err = fmt.Errorf("ezsp: payload truncated: need %d bytes at offset %d, have %d", n, r.pos, len(r.b))
		return false
	}
	return true
}

func (r *buf) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *buf) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}

func (r *buf) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

// ieee reads an 8-byte EUI-64, correcting for its little-endian transmission
// so the result matches the address printed on the device label.
func (r *buf) ieee() EUI64 {
	var a EUI64
	if !r.need(8) {
		return a
	}
	for i := 0; i < 8; i++ {
		a[7-i] = r.b[r.pos+i]
	}
	r.pos += 8
	return a
}

// remaining reports how many bytes are still unread.
func (r *buf) remaining() int {
	if r.err != nil {
		return 0
	}
	return len(r.b) - r.pos
}

// wbuf accumulates a request payload.
type wbuf struct{ b []byte }

func (w *wbuf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *wbuf) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *wbuf) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }

// ieee writes an EUI-64 in the little-endian order the wire expects.
func (w *wbuf) ieee(a EUI64) {
	for i := 7; i >= 0; i-- {
		w.b = append(w.b, a[i])
	}
}

func (w *wbuf) bytes(v []byte) { w.b = append(w.b, v...) }

// EUI64 is a 64-bit IEEE address, stored most-significant byte first so that
// its String form reads the same way it is printed on a device.
type EUI64 [8]byte

func (a EUI64) String() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X:%02X:%02X",
		a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7])
}

// IsZero reports whether the address is unset.
func (a EUI64) IsZero() bool { return a == EUI64{} }

// MarshalText renders the address for JSON output.
func (a EUI64) MarshalText() ([]byte, error) { return []byte(a.String()), nil }
