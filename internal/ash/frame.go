// Package ash implements Silicon Labs' Asynchronous Serial Host protocol, the
// reliable-delivery layer that carries EZSP between a host and an EmberZNet
// network coprocessor over a UART.
//
// ASH is a small sliding-window protocol. Every frame is:
//
//	+---------+-----------+---------+---------+------+
//	| Control | Data...   | CRC hi  | CRC lo  | 0x7E |
//	+---------+-----------+---------+---------+------+
//
// with three transformations applied on the way out, in this order:
//
//  1. The Data field of DATA frames is XORed with a pseudo-random sequence,
//     so a payload of repeated bytes cannot imitate a flag or XON/XOFF.
//  2. A CRC-16/CCITT is appended, covering the control byte and data.
//  3. Reserved bytes are escaped, so the flag byte 0x7E appears only as a
//     frame terminator.
//
// Getting that order wrong is the classic ASH bug: the CRC must cover the
// randomized bytes but not the escaping.
package ash

import (
	"errors"
	"fmt"
)

// Reserved bytes. Any of these appearing inside a frame must be escaped.
const (
	FlagByte   byte = 0x7E // terminates a frame
	EscapeByte byte = 0x7D // next byte is XORed with 0x20
	XOnByte    byte = 0x11
	XOffByte   byte = 0x13
	SubstByte  byte = 0x18 // marks a frame corrupted by a low-level error
	CancelByte byte = 0x1A // discard the partial frame in progress
)

// escapeMask is XORed into a byte to escape or unescape it.
const escapeMask byte = 0x20

// MaxFrameLen bounds a decoded frame. EZSP payloads stay well under this.
const MaxFrameLen = 256

// FrameType identifies what a control byte means.
type FrameType int

const (
	// FrameData carries an EZSP payload and must be acknowledged.
	FrameData FrameType = iota
	// FrameACK acknowledges receipt of DATA frames.
	FrameACK
	// FrameNAK rejects a frame and asks for retransmission.
	FrameNAK
	// FrameRST asks the NCP to reset. Host to NCP only.
	FrameRST
	// FrameRSTACK announces that the NCP has reset. NCP to host only.
	FrameRSTACK
	// FrameError reports that the NCP has halted and needs a reset.
	FrameError
)

func (t FrameType) String() string {
	switch t {
	case FrameData:
		return "DATA"
	case FrameACK:
		return "ACK"
	case FrameNAK:
		return "NAK"
	case FrameRST:
		return "RST"
	case FrameRSTACK:
		return "RSTACK"
	case FrameError:
		return "ERROR"
	default:
		return fmt.Sprintf("FrameType(%d)", int(t))
	}
}

// Frame is a decoded ASH frame.
type Frame struct {
	Type FrameType
	// FrmNum is the sender's sequence number. DATA frames only.
	FrmNum uint8
	// AckNum is the next frame number the sender expects. DATA, ACK and NAK.
	AckNum uint8
	// ReTx marks a DATA frame as a retransmission.
	ReTx bool
	// NotReady means the sender cannot accept more DATA for now. ACK and NAK.
	NotReady bool
	// Data is the de-randomized payload of a DATA frame, or the two status
	// bytes of RSTACK and ERROR.
	Data []byte
}

func (f Frame) String() string {
	switch f.Type {
	case FrameData:
		retx := ""
		if f.ReTx {
			retx = " reTx"
		}
		return fmt.Sprintf("DATA(frm=%d ack=%d%s, %d bytes)", f.FrmNum, f.AckNum, retx, len(f.Data))
	case FrameACK, FrameNAK:
		nrdy := ""
		if f.NotReady {
			nrdy = " nRdy"
		}
		return fmt.Sprintf("%s(ack=%d%s)", f.Type, f.AckNum, nrdy)
	case FrameRSTACK:
		if len(f.Data) == 2 {
			return fmt.Sprintf("RSTACK(version=%d, reset=%s)", f.Data[0], ResetCode(f.Data[1]))
		}
	case FrameError:
		if len(f.Data) == 2 {
			return fmt.Sprintf("ERROR(version=%d, code=%s)", f.Data[0], ResetCode(f.Data[1]))
		}
	}
	return f.Type.String()
}

// ResetCode explains why the NCP reset or halted.
type ResetCode byte

const (
	ResetUnknown    ResetCode = 0x00
	ResetExternal   ResetCode = 0x01
	ResetPowerOn    ResetCode = 0x02
	ResetWatchdog   ResetCode = 0x03
	ResetAssert     ResetCode = 0x06
	ResetBootloader ResetCode = 0x09
	ResetSoftware   ResetCode = 0x0B
	ErrExceededMax  ResetCode = 0x51 // too many ACK timeouts
	ErrChipSpecific ResetCode = 0x80
)

func (c ResetCode) String() string {
	switch c {
	case ResetUnknown:
		return "unknown"
	case ResetExternal:
		return "external"
	case ResetPowerOn:
		return "power-on"
	case ResetWatchdog:
		return "watchdog"
	case ResetAssert:
		return "assert failure"
	case ResetBootloader:
		return "bootloader"
	case ResetSoftware:
		return "software"
	case ErrExceededMax:
		return "exceeded maximum ACK timeouts"
	case ErrChipSpecific:
		return "chip-specific error"
	default:
		return fmt.Sprintf("code 0x%02X", byte(c))
	}
}

// Errors surfaced while parsing frames.
var (
	ErrBadCRC     = errors.New("ash: frame CRC mismatch")
	ErrShortFrame = errors.New("ash: frame too short")
	ErrLongFrame  = errors.New("ash: frame exceeds maximum length")
	// ErrSubstitute marks a frame the NCP flagged as corrupt in transit; it
	// must be discarded without being parsed.
	ErrSubstitute = errors.New("ash: frame marked corrupt by substitute byte")
)

// crc16 computes CRC-16/CCITT-FALSE: polynomial 0x1021, initial value 0xFFFF,
// no reflection, no final XOR. ASH transmits it high byte first.
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// randomize XORs data with ASH's pseudo-random sequence, in place on a copy.
//
// The generator is an 8-bit LFSR seeded at 0x42: shift right, and XOR in 0xB8
// whenever the bit shifted out was set. Applying it twice restores the
// original, so the same function both scrambles and unscrambles.
func randomize(data []byte) []byte {
	out := make([]byte, len(data))
	rand := byte(0x42)
	for i, b := range data {
		out[i] = b ^ rand
		if rand&1 == 0 {
			rand >>= 1
		} else {
			rand = rand>>1 ^ 0xB8
		}
	}
	return out
}

// needsEscape reports whether b is reserved and must be escaped.
func needsEscape(b byte) bool {
	switch b {
	case FlagByte, EscapeByte, XOnByte, XOffByte, SubstByte, CancelByte:
		return true
	}
	return false
}

// stuff escapes reserved bytes and appends the terminating flag.
func stuff(frame []byte) []byte {
	out := make([]byte, 0, len(frame)+8)
	for _, b := range frame {
		if needsEscape(b) {
			out = append(out, EscapeByte, b^escapeMask)
			continue
		}
		out = append(out, b)
	}
	return append(out, FlagByte)
}

// unstuff reverses escaping on a frame body with the flag already stripped.
func unstuff(body []byte) ([]byte, error) {
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i++ {
		if body[i] != EscapeByte {
			out = append(out, body[i])
			continue
		}
		i++
		if i >= len(body) {
			return nil, errors.New("ash: escape byte at end of frame")
		}
		out = append(out, body[i]^escapeMask)
	}
	return out, nil
}

// controlByte renders a frame's header byte.
func (f Frame) controlByte() byte {
	switch f.Type {
	case FrameData:
		c := f.FrmNum&0x07<<4 | f.AckNum&0x07
		if f.ReTx {
			c |= 0x08
		}
		return c
	case FrameACK:
		c := 0x80 | f.AckNum&0x07
		if f.NotReady {
			c |= 0x10
		}
		return c
	case FrameNAK:
		c := 0xA0 | f.AckNum&0x07
		if f.NotReady {
			c |= 0x10
		}
		return c
	case FrameRST:
		return 0xC0
	case FrameRSTACK:
		return 0xC1
	case FrameError:
		return 0xC2
	default:
		return 0
	}
}

// Encode serialises a frame into its on-the-wire bytes, terminating flag
// included. RST frames are conventionally preceded by a cancel byte, which
// Conn adds; Encode emits the frame alone.
func Encode(f Frame) ([]byte, error) {
	if len(f.Data) > MaxFrameLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrLongFrame, len(f.Data))
	}

	body := []byte{f.controlByte()}
	if f.Type == FrameData {
		// Randomize first: the CRC must cover the bytes as transmitted.
		body = append(body, randomize(f.Data)...)
	} else {
		body = append(body, f.Data...)
	}

	crc := crc16(body)
	body = append(body, byte(crc>>8), byte(crc))
	return stuff(body), nil
}

// Decode parses one frame body: the bytes up to but excluding the flag, still
// escaped. It verifies the CRC and de-randomizes DATA payloads.
func Decode(raw []byte) (Frame, error) {
	body, err := unstuff(raw)
	if err != nil {
		return Frame{}, err
	}
	// Control byte plus a two-byte CRC is the minimum viable frame.
	if len(body) < 3 {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrShortFrame, len(body))
	}

	payload, sum := body[:len(body)-2], body[len(body)-2:]
	want := uint16(sum[0])<<8 | uint16(sum[1])
	if got := crc16(payload); got != want {
		return Frame{}, fmt.Errorf("%w: computed 0x%04X, frame carries 0x%04X", ErrBadCRC, got, want)
	}

	ctrl, data := payload[0], payload[1:]
	f := Frame{}
	switch {
	case ctrl&0x80 == 0:
		f.Type = FrameData
		f.FrmNum = ctrl >> 4 & 0x07
		f.ReTx = ctrl&0x08 != 0
		f.AckNum = ctrl & 0x07
		f.Data = randomize(data) // symmetric: unscrambles
	case ctrl&0xE0 == 0x80:
		f.Type = FrameACK
		f.AckNum = ctrl & 0x07
		f.NotReady = ctrl&0x10 != 0
	case ctrl&0xE0 == 0xA0:
		f.Type = FrameNAK
		f.AckNum = ctrl & 0x07
		f.NotReady = ctrl&0x10 != 0
	case ctrl == 0xC0:
		f.Type = FrameRST
	case ctrl == 0xC1:
		f.Type = FrameRSTACK
		f.Data = append([]byte(nil), data...)
	case ctrl == 0xC2:
		f.Type = FrameError
		f.Data = append([]byte(nil), data...)
	default:
		return Frame{}, fmt.Errorf("ash: unrecognised control byte 0x%02X", ctrl)
	}
	return f, nil
}
