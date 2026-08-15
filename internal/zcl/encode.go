package zcl

import (
	"encoding/binary"
	"fmt"
)

// ReadRequest lists the attribute IDs a Read Attributes command asks for.
func (f Frame) ReadRequest() ([]uint16, error) {
	if f.Type != FrameProfileWide || f.Command != CmdReadAttributes {
		return nil, fmt.Errorf("zcl: %s is not a read request", CommandName(f.Command))
	}
	if len(f.Payload)%2 != 0 {
		return nil, fmt.Errorf("zcl: read request payload is %d bytes, want a whole number of attribute IDs", len(f.Payload))
	}
	ids := make([]uint16, 0, len(f.Payload)/2)
	for i := 0; i < len(f.Payload); i += 2 {
		ids = append(ids, binary.LittleEndian.Uint16(f.Payload[i:]))
	}
	return ids, nil
}

// ReadAttributesRequest builds a Read Attributes command.
func ReadAttributesRequest(seq uint8, ids []uint16) []byte {
	// Client to server, and a default response is not wanted: a read either
	// gets its answer or times out.
	out := []byte{byte(fcDisableDefaultResponse), seq, CmdReadAttributes}
	for _, id := range ids {
		out = binary.LittleEndian.AppendUint16(out, id)
	}
	return out
}

// Status codes used in attribute responses.
const (
	StatusSuccess              uint8 = 0x00
	StatusUnsupportedAttribute uint8 = 0x86
)

// Record is one attribute to encode into a response.
//
// A Status other than success is encoded alone, without a type or value, which
// is how the reader on the other end knows to skip it.
type Record struct {
	ID     uint16
	Status uint8
	Type   DataType
	Value  any
}

// ReadAttributesResponse builds a reply to a Read Attributes command.
//
// The sequence must echo the request's, since that is how the asking device
// matches a reply to its outstanding question.
func ReadAttributesResponse(seq uint8, records []Record) ([]byte, error) {
	// Server to client, and no default response wanted: this is already the
	// answer to a question.
	out := []byte{byte(fcDirection | fcDisableDefaultResponse), seq, CmdReadAttributesResponse}

	for _, rec := range records {
		out = binary.LittleEndian.AppendUint16(out, rec.ID)
		out = append(out, rec.Status)
		if rec.Status != StatusSuccess {
			continue
		}
		out = append(out, byte(rec.Type))
		encoded, err := encodeValue(rec.Type, rec.Value)
		if err != nil {
			return nil, fmt.Errorf("zcl: attribute 0x%04X: %w", rec.ID, err)
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// encodeValue renders one attribute value in its wire form.
func encodeValue(t DataType, v any) ([]byte, error) {
	switch t {
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("want a bool for type 0x%02X, got %T", uint8(t), v)
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil

	case TypeData8, TypeBitmap8, TypeUint8, TypeEnum8:
		n, err := asUint(v)
		return []byte{byte(n)}, err

	case TypeBitmap16, TypeUint16, TypeEnum16:
		n, err := asUint(v)
		return binary.LittleEndian.AppendUint16(nil, uint16(n)), err

	case TypeBitmap32, TypeUint32, TypeUTCTime:
		n, err := asUint(v)
		return binary.LittleEndian.AppendUint32(nil, uint32(n)), err

	case TypeInt8:
		n, err := asInt(v)
		return []byte{byte(int8(n))}, err

	case TypeInt16:
		n, err := asInt(v)
		return binary.LittleEndian.AppendUint16(nil, uint16(int16(n))), err

	case TypeInt32:
		n, err := asInt(v)
		return binary.LittleEndian.AppendUint32(nil, uint32(int32(n))), err

	case TypeCharStr, TypeOctetStr:
		var b []byte
		switch s := v.(type) {
		case string:
			b = []byte(s)
		case []byte:
			b = s
		default:
			return nil, fmt.Errorf("want a string or bytes for type 0x%02X, got %T", uint8(t), v)
		}
		if len(b) > 254 {
			return nil, fmt.Errorf("string of %d bytes exceeds the ZCL limit", len(b))
		}
		return append([]byte{byte(len(b))}, b...), nil

	default:
		return nil, fmt.Errorf("cannot encode data type 0x%02X", uint8(t))
	}
}

func asUint(v any) (uint64, error) {
	switch n := v.(type) {
	case uint64:
		return n, nil
	case uint32:
		return uint64(n), nil
	case uint16:
		return uint64(n), nil
	case uint8:
		return uint64(n), nil
	case int:
		if n < 0 {
			return 0, fmt.Errorf("negative value %d in an unsigned attribute", n)
		}
		return uint64(n), nil
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("negative value %d in an unsigned attribute", n)
		}
		return uint64(n), nil
	default:
		return 0, fmt.Errorf("want an unsigned integer, got %T", v)
	}
}

func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("want a signed integer, got %T", v)
	}
}
