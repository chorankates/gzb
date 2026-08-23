package zcl

import (
	"encoding/binary"
	"fmt"
	"math"
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

// Status codes carried by attribute responses.
//
// Only a handful matter in practice, but the ones that do are the difference
// between "the device is broken" and "you asked for the wrong thing": a write
// to a read-only attribute and a write of the wrong type both come back here
// rather than as a failure to deliver.
const (
	StatusSuccess               uint8 = 0x00
	StatusFailure               uint8 = 0x01
	StatusNotAuthorized         uint8 = 0x7E
	StatusMalformedCommand      uint8 = 0x80
	StatusUnsupClusterCommand   uint8 = 0x81
	StatusUnsupGeneralCommand   uint8 = 0x82
	StatusUnsupportedAttribute  uint8 = 0x86
	StatusInvalidValue          uint8 = 0x87
	StatusReadOnly              uint8 = 0x88
	StatusInsufficientSpace     uint8 = 0x89
	StatusNotFound              uint8 = 0x8B
	StatusUnreportableAttribute uint8 = 0x8C
	StatusInvalidDataType       uint8 = 0x8D
	StatusInvalidSelector       uint8 = 0x8E
	StatusTimeout               uint8 = 0x94
	StatusUnsupportedCluster    uint8 = 0xC3
)

// StatusName renders a status code as the reason it stands for.
func StatusName(status uint8) string {
	switch status {
	case StatusSuccess:
		return "success"
	case StatusFailure:
		return "failure"
	case StatusNotAuthorized:
		return "not authorized"
	case StatusMalformedCommand:
		return "malformed command"
	case StatusUnsupClusterCommand:
		return "unsupported cluster command"
	case StatusUnsupGeneralCommand:
		return "unsupported general command"
	case StatusUnsupportedAttribute:
		return "unsupported attribute"
	case StatusInvalidValue:
		return "invalid value"
	case StatusReadOnly:
		return "read only"
	case StatusInsufficientSpace:
		return "insufficient space"
	case StatusNotFound:
		return "not found"
	case StatusUnreportableAttribute:
		return "unreportable attribute"
	case StatusInvalidDataType:
		return "invalid data type"
	case StatusInvalidSelector:
		return "invalid selector"
	case StatusTimeout:
		return "timeout"
	case StatusUnsupportedCluster:
		return "unsupported cluster"
	default:
		return fmt.Sprintf("status 0x%02X", status)
	}
}

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
		n, err := boundedUint(v, 8)
		return []byte{byte(n)}, err

	case TypeBitmap16, TypeUint16, TypeEnum16:
		n, err := boundedUint(v, 16)
		return binary.LittleEndian.AppendUint16(nil, uint16(n)), err

	case TypeUint24:
		n, err := boundedUint(v, 24)
		return appendUint(nil, n, 3), err

	case TypeBitmap32, TypeUint32, TypeUTCTime:
		n, err := boundedUint(v, 32)
		return binary.LittleEndian.AppendUint32(nil, uint32(n)), err

	case TypeUint48:
		n, err := boundedUint(v, 48)
		return appendUint(nil, n, 6), err

	case TypeInt8:
		n, err := boundedInt(v, 8)
		return []byte{byte(int8(n))}, err

	case TypeInt16:
		n, err := boundedInt(v, 16)
		return binary.LittleEndian.AppendUint16(nil, uint16(int16(n))), err

	case TypeInt24:
		n, err := boundedInt(v, 24)
		return appendUint(nil, uint64(n)&0xFFFFFF, 3), err

	case TypeInt32:
		n, err := boundedInt(v, 32)
		return binary.LittleEndian.AppendUint32(nil, uint32(int32(n))), err

	case TypeSingle:
		f, err := asFloat(v)
		return binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(f))), err

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

// boundedUint reads an unsigned value and checks it fits the width it is about
// to be encoded in.
//
// Truncating instead would send a different number than the one asked for and
// report success, which is the worst way for a write to be wrong: the device
// accepts it, and the value that comes back is one nobody chose.
func boundedUint(v any, bits int) (uint64, error) {
	n, err := asUint(v)
	if err != nil {
		return 0, err
	}
	if bits < 64 && n > uint64(1)<<bits-1 {
		return 0, fmt.Errorf("%d does not fit in %d bits", n, bits)
	}
	return n, nil
}

// boundedInt is boundedUint for the signed widths.
func boundedInt(v any, bits int) (int64, error) {
	n, err := asInt(v)
	if err != nil {
		return 0, err
	}
	limit := int64(1) << (bits - 1)
	if n < -limit || n > limit-1 {
		return 0, fmt.Errorf("%d is outside the range of a %d-bit signed value", n, bits)
	}
	return n, nil
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

// asFloat converts a value to a float, for the one ZCL type that is one.
func asFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case int:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("want a number, got %T", v)
	}
}

// appendUint writes an n-byte little-endian unsigned value, for the widths
// ZCL uses that Go has no type for.
func appendUint(out []byte, v uint64, n int) []byte {
	for i := 0; i < n; i++ {
		out = append(out, byte(v>>(8*i)))
	}
	return out
}

// WriteRecord is one attribute to set on a device.
//
// The type has to be stated because the wire format carries it, and because a
// device checks it: writing 20 to an int16 attribute as a uint8 is rejected
// with StatusInvalidDataType rather than quietly coerced.
type WriteRecord struct {
	ID    uint16
	Type  DataType
	Value any
}

// WriteAttributesRequest builds a Write Attributes command.
func WriteAttributesRequest(seq uint8, records []WriteRecord) ([]byte, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("zcl: a write needs at least one attribute")
	}
	// Client to server, and — unlike a read — the default response is left
	// enabled. A device that does not implement the command at all answers
	// with one, and that is the only thing distinguishing a refusal from a
	// request that never arrived.
	out := []byte{0x00, seq, CmdWriteAttributes}
	for _, rec := range records {
		out = binary.LittleEndian.AppendUint16(out, rec.ID)
		out = append(out, byte(rec.Type))
		encoded, err := encodeValue(rec.Type, rec.Value)
		if err != nil {
			return nil, fmt.Errorf("zcl: attribute 0x%04X: %w", rec.ID, err)
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// MaxIntervalDisabled, as a reporting configuration's maximum interval, tells
// the device not to report that attribute at all.
//
// The same value in *both* intervals means something different: the device
// reverts to its own default reporting configuration. Turning reporting off
// and restoring what the device shipped with are not the same operation, and
// the difference is one field.
const MaxIntervalDisabled uint16 = 0xFFFF

// reportDirectionSend configures the reports a device sends, as opposed to the
// timeout it expects on reports it receives. Only the sending direction is
// useful to a hub that is being reported to.
const reportDirectionSend uint8 = 0x00

// ReportingConfig asks a device to report one attribute on its own initiative.
//
// This is the difference between a hub that polls and one that listens. Min
// and Max bound how often a report may and must be sent, in seconds: Min
// throttles a fast-changing value, and Max is a heartbeat that proves the
// device is still alive even when nothing has changed.
type ReportingConfig struct {
	ID   uint16
	Type DataType
	Min  uint16
	Max  uint16
	// Change is how far the value must move before a report is worth sending.
	// It is carried only for analog types, encoded in the attribute's own
	// type — so a temperature reported in hundredths takes 50 to mean half a
	// degree. Zero asks for a report on every change.
	Change any
}

// ConfigureReportingRequest builds a Configure Reporting command.
func ConfigureReportingRequest(seq uint8, configs []ReportingConfig) ([]byte, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("zcl: a reporting configuration needs at least one attribute")
	}
	// The default response is left enabled for the same reason as a write.
	out := []byte{0x00, seq, CmdConfigureReporting}
	for _, cfg := range configs {
		if cfg.Max != MaxIntervalDisabled && cfg.Min > cfg.Max {
			return nil, fmt.Errorf("zcl: attribute 0x%04X: minimum interval %ds is longer than the maximum %ds", cfg.ID, cfg.Min, cfg.Max)
		}
		out = append(out, reportDirectionSend)
		out = binary.LittleEndian.AppendUint16(out, cfg.ID)
		out = append(out, byte(cfg.Type))
		out = binary.LittleEndian.AppendUint16(out, cfg.Min)
		out = binary.LittleEndian.AppendUint16(out, cfg.Max)
		if !cfg.Type.Analog() {
			continue
		}
		if cfg.Change == nil {
			return nil, fmt.Errorf("zcl: attribute 0x%04X is analog (type 0x%02X), so a reportable change is required", cfg.ID, uint8(cfg.Type))
		}
		encoded, err := encodeValue(cfg.Type, cfg.Change)
		if err != nil {
			return nil, fmt.Errorf("zcl: attribute 0x%04X reportable change: %w", cfg.ID, err)
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// ReadReportingConfigRequest asks a device what reporting it currently has
// configured for some attributes.
//
// This is the question that settles what a configuration actually did. A
// device answers a Configure Reporting command with a status and nothing else,
// so the only way to know what it holds — as opposed to what it was asked for
// — is to ask it.
func ReadReportingConfigRequest(seq uint8, attrs []uint16) ([]byte, error) {
	if len(attrs) == 0 {
		return nil, fmt.Errorf("zcl: a reporting configuration read needs at least one attribute")
	}
	out := []byte{0x00, seq, CmdReadReportingConfig}
	for _, attr := range attrs {
		out = append(out, reportDirectionSend)
		out = binary.LittleEndian.AppendUint16(out, attr)
	}
	return out, nil
}
