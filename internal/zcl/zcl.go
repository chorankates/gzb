// Package zcl decodes the Zigbee Cluster Library: the application layer that
// carries actual device data, as opposed to ZDO which carries addressing and
// discovery.
//
// A ZCL frame is a small header followed by a command. The command that
// matters most for a hub is Report Attributes (0x0A), which is how a sensor
// pushes readings without being asked.
package zcl

import (
	"encoding/binary"
	"fmt"
	"math"
)

// FrameType distinguishes commands defined for every cluster from those
// specific to one.
type FrameType uint8

const (
	// FrameProfileWide commands (read, write, report, discover) mean the same
	// thing on every cluster.
	FrameProfileWide FrameType = 0
	// FrameClusterSpecific commands are defined by the cluster itself, so the
	// command byte only has meaning alongside the cluster ID.
	FrameClusterSpecific FrameType = 1
)

// Direction reports which way a frame travels.
type Direction uint8

const (
	// ClientToServer is a hub asking a device to do something.
	ClientToServer Direction = 0
	// ServerToClient is a device reporting to the hub.
	ServerToClient Direction = 1
)

// Profile-wide command IDs.
const (
	CmdReadAttributes            uint8 = 0x00
	CmdReadAttributesResponse    uint8 = 0x01
	CmdWriteAttributes           uint8 = 0x02
	CmdWriteAttributesResponse   uint8 = 0x04
	CmdConfigureReporting        uint8 = 0x06
	CmdConfigureReportingRsp     uint8 = 0x07
	CmdReportAttributes          uint8 = 0x0A
	CmdDefaultResponse           uint8 = 0x0B
	CmdDiscoverAttributes        uint8 = 0x0C
	CmdDiscoverAttributesRespons uint8 = 0x0D
)

// CommandName renders a profile-wide command ID.
func CommandName(cmd uint8) string {
	switch cmd {
	case CmdReadAttributes:
		return "read attributes"
	case CmdReadAttributesResponse:
		return "read attributes response"
	case CmdWriteAttributes:
		return "write attributes"
	case CmdWriteAttributesResponse:
		return "write attributes response"
	case CmdConfigureReporting:
		return "configure reporting"
	case CmdConfigureReportingRsp:
		return "configure reporting response"
	case CmdReportAttributes:
		return "report attributes"
	case CmdDefaultResponse:
		return "default response"
	case CmdDiscoverAttributes:
		return "discover attributes"
	case CmdDiscoverAttributesRespons:
		return "discover attributes response"
	default:
		return fmt.Sprintf("command 0x%02X", cmd)
	}
}

// Frame is a decoded ZCL frame.
type Frame struct {
	Type                   FrameType
	ManufacturerSpecific   bool
	ManufacturerCode       uint16
	Direction              Direction
	DisableDefaultResponse bool
	Sequence               uint8
	Command                uint8
	Payload                []byte
}

// Frame control bits.
const (
	fcTypeMask               = 0x03
	fcManufacturerSpecific   = 0x04
	fcDirection              = 0x08
	fcDisableDefaultResponse = 0x10
)

// Decode parses a ZCL frame from an APS payload.
func Decode(b []byte) (Frame, error) {
	r := &cursor{b: b}
	fc := r.u8()

	f := Frame{
		Type:                   FrameType(fc & fcTypeMask),
		ManufacturerSpecific:   fc&fcManufacturerSpecific != 0,
		Direction:              Direction(fc & fcDirection >> 3),
		DisableDefaultResponse: fc&fcDisableDefaultResponse != 0,
	}
	if f.ManufacturerSpecific {
		f.ManufacturerCode = r.u16()
	}
	f.Sequence = r.u8()
	f.Command = r.u8()
	if r.err != nil {
		return Frame{}, fmt.Errorf("zcl: %w", r.err)
	}
	f.Payload = b[r.pos:]
	return f, nil
}

// Attribute is one attribute value carried by a report or read response.
type Attribute struct {
	ID     uint16
	Type   DataType
	Value  any
	Status uint8
	// Raw holds the encoded bytes, so a value this package cannot interpret is
	// still available to the caller.
	Raw []byte
}

// Attributes decodes the attribute records in a report or read response.
//
// Report Attributes and Read Attributes Response differ by one field: the
// response carries a status per attribute, and omits the type and value when
// that status is not success.
func (f Frame) Attributes() ([]Attribute, error) {
	if f.Type != FrameProfileWide {
		return nil, fmt.Errorf("zcl: command 0x%02X is cluster-specific, not an attribute report", f.Command)
	}
	withStatus := false
	switch f.Command {
	case CmdReportAttributes:
	case CmdReadAttributesResponse:
		withStatus = true
	default:
		return nil, fmt.Errorf("zcl: %s carries no attribute records", CommandName(f.Command))
	}

	r := &cursor{b: f.Payload}
	var out []Attribute
	for r.remaining() > 0 && r.err == nil {
		a := Attribute{ID: r.u16()}
		if withStatus {
			a.Status = r.u8()
			if a.Status != 0 {
				out = append(out, a)
				continue
			}
		}
		a.Type = DataType(r.u8())
		start := r.pos
		a.Value = decodeValue(r, a.Type)
		if r.err != nil {
			break
		}
		a.Raw = f.Payload[start:r.pos]
		out = append(out, a)
	}
	if r.err != nil {
		return out, fmt.Errorf("zcl: decoding attribute records: %w", r.err)
	}
	return out, nil
}

// DataType is a ZCL attribute encoding.
type DataType uint8

const (
	TypeNoData   DataType = 0x00
	TypeData8    DataType = 0x08
	TypeBool     DataType = 0x10
	TypeBitmap8  DataType = 0x18
	TypeBitmap16 DataType = 0x19
	TypeBitmap32 DataType = 0x1B
	TypeUint8    DataType = 0x20
	TypeUint16   DataType = 0x21
	TypeUint24   DataType = 0x22
	TypeUint32   DataType = 0x23
	TypeUint48   DataType = 0x25
	TypeInt8     DataType = 0x28
	TypeInt16    DataType = 0x29
	TypeInt24    DataType = 0x2A
	TypeInt32    DataType = 0x2B
	TypeEnum8    DataType = 0x30
	TypeEnum16   DataType = 0x31
	TypeSingle   DataType = 0x39
	TypeOctetStr DataType = 0x41
	TypeCharStr  DataType = 0x42
	// TypeUTCTime counts seconds from the Zigbee epoch, 2000-01-01 UTC, not
	// the Unix epoch. It is encoded exactly like a uint32.
	TypeUTCTime DataType = 0xE2
	TypeIEEE    DataType = 0xF0
)

// decodeValue reads one attribute value of the given type.
func decodeValue(r *cursor, t DataType) any {
	switch t {
	case TypeNoData:
		return nil
	case TypeBool:
		v := r.u8()
		if v == 0xFF {
			return nil // the ZCL "invalid" marker
		}
		return v != 0
	case TypeData8, TypeBitmap8, TypeUint8, TypeEnum8:
		return uint64(r.u8())
	case TypeBitmap16, TypeUint16, TypeEnum16:
		return uint64(r.u16())
	case TypeUint24:
		return uint64(r.uint(3))
	case TypeBitmap32, TypeUint32, TypeUTCTime:
		return uint64(r.u32())
	case TypeUint48:
		return r.uint(6)
	case TypeInt8:
		return int64(int8(r.u8()))
	case TypeInt16:
		return int64(int16(r.u16()))
	case TypeInt24:
		v := r.uint(3)
		if v&0x800000 != 0 {
			return int64(v) - (1 << 24)
		}
		return int64(v)
	case TypeInt32:
		return int64(int32(r.u32()))
	case TypeSingle:
		return float64(math.Float32frombits(r.u32()))
	case TypeOctetStr, TypeCharStr:
		n := int(r.u8())
		if n == 0xFF {
			return nil
		}
		b := r.bytes(n)
		if t == TypeCharStr {
			return string(b)
		}
		return b
	case TypeIEEE:
		return r.bytes(8)
	default:
		// An unknown type has an unknown length, so the record cannot be
		// walked past this point.
		r.fail(fmt.Errorf("unsupported data type 0x%02X", uint8(t)))
		return nil
	}
}

// cursor is a bounds-checked little-endian reader with a latched error.
type cursor struct {
	b   []byte
	pos int
	err error
}

func (r *cursor) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *cursor) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.pos+n > len(r.b) {
		r.fail(fmt.Errorf("truncated: need %d bytes at offset %d, have %d", n, r.pos, len(r.b)))
		return false
	}
	return true
}

func (r *cursor) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *cursor) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}

func (r *cursor) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

// uint reads an n-byte little-endian unsigned value, for the widths ZCL uses
// that Go has no type for.
func (r *cursor) uint(n int) uint64 {
	if !r.need(n) {
		return 0
	}
	var v uint64
	for i := n - 1; i >= 0; i-- {
		v = v<<8 | uint64(r.b[r.pos+i])
	}
	r.pos += n
	return v
}

func (r *cursor) bytes(n int) []byte {
	if !r.need(n) {
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}

func (r *cursor) remaining() int {
	if r.err != nil {
		return 0
	}
	return len(r.b) - r.pos
}
