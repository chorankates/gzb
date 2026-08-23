package zcl

import (
	"bytes"
	"testing"
)

// These are the frames gzb sends and receives once it stops only listening.
// The encodings are pinned to explicit bytes taken from the field layouts in
// the ZCL specification, because an encoder tested against its own decoder is
// free to be wrong in both directions at once.

func TestWriteAttributesRequestEncoding(t *testing.T) {
	got, err := WriteAttributesRequest(0x07, []WriteRecord{
		{ID: 0x0010, Type: TypeCharStr, Value: "hall"},
		{ID: 0x0000, Type: TypeBool, Value: true},
	})
	if err != nil {
		t.Fatalf("WriteAttributesRequest: %v", err)
	}
	want := []byte{
		0x00, 0x07, CmdWriteAttributes,
		0x10, 0x00, byte(TypeCharStr), 0x04, 'h', 'a', 'l', 'l',
		0x00, 0x00, byte(TypeBool), 0x01,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = % X, want % X", got, want)
	}
}

// A write leaves the default response enabled, unlike a read. It is the only
// answer a device that does not implement the command will give.
func TestWriteAttributesRequestAllowsADefaultResponse(t *testing.T) {
	got, err := WriteAttributesRequest(0x01, []WriteRecord{{ID: 0x0000, Type: TypeUint8, Value: uint64(1)}})
	if err != nil {
		t.Fatalf("WriteAttributesRequest: %v", err)
	}
	if got[0]&fcDisableDefaultResponse != 0 {
		t.Errorf("frame control = 0x%02X, want the default response left enabled", got[0])
	}
}

func TestWriteAttributesRequestRejectsAMismatchedValue(t *testing.T) {
	_, err := WriteAttributesRequest(0x01, []WriteRecord{{ID: 0x0000, Type: TypeUint8, Value: "not a number"}})
	if err == nil {
		t.Fatal("encoded a string into a uint8 attribute")
	}
}

// Truncating an oversized value would send a number nobody chose and have the
// device accept it, which is worse than refusing to send at all.
func TestWriteAttributesRequestRejectsAValueThatDoesNotFit(t *testing.T) {
	tooBig := []struct {
		typ   DataType
		value any
	}{
		{TypeUint8, uint64(256)},
		{TypeUint16, uint64(65536)},
		{TypeUint24, uint64(1) << 24},
		{TypeInt8, int64(128)},
		{TypeInt8, int64(-129)},
		{TypeInt16, int64(32768)},
		{TypeEnum8, uint64(300)},
	}
	for _, tc := range tooBig {
		_, err := WriteAttributesRequest(0x01, []WriteRecord{{ID: 0x0000, Type: tc.typ, Value: tc.value}})
		if err == nil {
			t.Errorf("encoded %v into a %s", tc.value, tc.typ)
		}
	}

	// The edges themselves must still encode.
	fits := []struct {
		typ   DataType
		value any
	}{
		{TypeUint8, uint64(255)},
		{TypeInt8, int64(-128)},
		{TypeInt8, int64(127)},
		{TypeBitmap32, uint64(0xFFFFFFFF)},
		{TypeUint48, uint64(1)<<48 - 1},
	}
	for _, tc := range fits {
		if _, err := WriteAttributesRequest(0x01, []WriteRecord{{ID: 0x0000, Type: tc.typ, Value: tc.value}}); err != nil {
			t.Errorf("%v does fit in a %s: %v", tc.value, tc.typ, err)
		}
	}
}

func TestWriteResponseExpandsTheAllSucceededShorthand(t *testing.T) {
	// A device that wrote everything sends one success byte and names nothing.
	frame, err := Decode([]byte{0x18, 0x07, CmdWriteAttributesResponse, StatusSuccess})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.WriteResponse([]uint16{0x0010, 0x0000})
	if err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	want := []WriteResult{{ID: 0x0010}, {ID: 0x0000}}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want one per attribute asked for", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWriteResponseNamesOnlyWhatWasRefused(t *testing.T) {
	frame, err := Decode([]byte{
		0x18, 0x07, CmdWriteAttributesResponse,
		StatusReadOnly, 0x10, 0x00,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.WriteResponse([]uint16{0x0010, 0x0000})
	if err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	// The device listed one failure, so that is all there is to report: the
	// shorthand only applies when everything succeeded.
	if len(got) != 1 || got[0].ID != 0x0010 || got[0].Status != StatusReadOnly {
		t.Fatalf("results = %+v, want attribute 0x0010 read only", got)
	}
}

func TestWriteResponseRejectsATruncatedRecord(t *testing.T) {
	frame, err := Decode([]byte{0x18, 0x07, CmdWriteAttributesResponse, StatusReadOnly, 0x10})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := frame.WriteResponse(nil); err == nil {
		t.Fatal("accepted a record with half an attribute ID")
	}
}

func TestConfigureReportingRequestEncoding(t *testing.T) {
	got, err := ConfigureReportingRequest(0x09, []ReportingConfig{
		{ID: 0x0000, Type: TypeInt16, Min: 60, Max: 3600, Change: uint64(50)},
	})
	if err != nil {
		t.Fatalf("ConfigureReportingRequest: %v", err)
	}
	want := []byte{
		0x00, 0x09, CmdConfigureReporting,
		0x00,       // direction: reports the device sends
		0x00, 0x00, // attribute 0x0000
		byte(TypeInt16),
		0x3C, 0x00, // minimum 60s
		0x10, 0x0E, // maximum 3600s
		0x32, 0x00, // reportable change 50, in the attribute's own type
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = % X, want % X", got, want)
	}
}

// A discrete attribute has no reportable change field at all. Sending one
// anyway would shift every following record by two bytes.
func TestConfigureReportingRequestOmitsChangeForDiscreteTypes(t *testing.T) {
	got, err := ConfigureReportingRequest(0x09, []ReportingConfig{
		{ID: 0x0000, Type: TypeBool, Min: 1, Max: 300, Change: uint64(1)},
	})
	if err != nil {
		t.Fatalf("ConfigureReportingRequest: %v", err)
	}
	want := []byte{
		0x00, 0x09, CmdConfigureReporting,
		0x00,
		0x00, 0x00,
		byte(TypeBool),
		0x01, 0x00, // minimum 1s
		0x2C, 0x01, // maximum 300s
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = % X, want % X", got, want)
	}
}

func TestConfigureReportingRequestRequiresAChangeForAnalogTypes(t *testing.T) {
	_, err := ConfigureReportingRequest(0x09, []ReportingConfig{
		{ID: 0x0000, Type: TypeInt16, Min: 60, Max: 3600},
	})
	if err == nil {
		t.Fatal("encoded an analog configuration with no reportable change")
	}
}

func TestConfigureReportingRequestRejectsInvertedIntervals(t *testing.T) {
	_, err := ConfigureReportingRequest(0x09, []ReportingConfig{
		{ID: 0x0000, Type: TypeBool, Min: 600, Max: 60},
	})
	if err == nil {
		t.Fatal("accepted a minimum interval longer than the maximum")
	}
}

// Turning reporting off is the one case where the maximum is below the
// minimum, because 0xFFFF is a sentinel rather than an interval.
func TestConfigureReportingRequestAllowsTheOffSentinel(t *testing.T) {
	got, err := ConfigureReportingRequest(0x09, []ReportingConfig{
		{ID: 0x0000, Type: TypeBool, Min: 600, Max: MaxIntervalDisabled},
	})
	if err != nil {
		t.Fatalf("ConfigureReportingRequest: %v", err)
	}
	if !bytes.Equal(got[len(got)-2:], []byte{0xFF, 0xFF}) {
		t.Fatalf("request = % X, want it to end in the off sentinel", got)
	}
}

func TestConfigureReportingResponseExpandsTheAllSucceededShorthand(t *testing.T) {
	frame, err := Decode([]byte{0x18, 0x09, CmdConfigureReportingRsp, StatusSuccess})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.ConfigureReportingResponse([]uint16{0x0000, 0x0001})
	if err != nil {
		t.Fatalf("ConfigureReportingResponse: %v", err)
	}
	if len(got) != 2 || got[0].Status != StatusSuccess || got[1].ID != 0x0001 {
		t.Fatalf("results = %+v, want one success per attribute", got)
	}
}

func TestConfigureReportingResponseNamesAnUnreportableAttribute(t *testing.T) {
	frame, err := Decode([]byte{
		0x18, 0x09, CmdConfigureReportingRsp,
		StatusUnreportableAttribute, 0x00, 0x05, 0x00,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.ConfigureReportingResponse([]uint16{0x0005})
	if err != nil {
		t.Fatalf("ConfigureReportingResponse: %v", err)
	}
	if len(got) != 1 || got[0].ID != 0x0005 || got[0].Status != StatusUnreportableAttribute {
		t.Fatalf("results = %+v, want attribute 0x0005 unreportable", got)
	}
}

func TestDefaultResponseCarriesTheCommandItRefused(t *testing.T) {
	// The answer of a device that does not implement Write Attributes at all.
	frame, err := Decode([]byte{0x18, 0x07, CmdDefaultResponse, CmdWriteAttributes, StatusUnsupGeneralCommand})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.DefaultResponse()
	if err != nil {
		t.Fatalf("DefaultResponse: %v", err)
	}
	if got.Command != CmdWriteAttributes || got.Status != StatusUnsupGeneralCommand {
		t.Fatalf("default response = %+v, want write attributes / unsupported general command", got)
	}
}

func TestDefaultResponseRejectsATruncatedFrame(t *testing.T) {
	frame, err := Decode([]byte{0x18, 0x07, CmdDefaultResponse, CmdWriteAttributes})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := frame.DefaultResponse(); err == nil {
		t.Fatal("accepted a default response with no status")
	}
}

func TestResponseParsersRejectTheWrongCommand(t *testing.T) {
	frame, err := Decode([]byte{0x18, 0x07, CmdReportAttributes})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := frame.WriteResponse(nil); err == nil {
		t.Error("WriteResponse accepted a report")
	}
	if _, err := frame.ConfigureReportingResponse(nil); err == nil {
		t.Error("ConfigureReportingResponse accepted a report")
	}
	if _, err := frame.DefaultResponse(); err == nil {
		t.Error("DefaultResponse accepted a report")
	}
}

// The analog/discrete split decides whether a reporting configuration carries
// a reportable change, so getting it wrong misaligns the whole record.
func TestAnalogClassification(t *testing.T) {
	analog := []DataType{TypeUint8, TypeUint16, TypeUint24, TypeUint32, TypeUint48,
		TypeInt8, TypeInt16, TypeInt24, TypeInt32, TypeSingle, TypeUTCTime}
	for _, t2 := range analog {
		if !t2.Analog() {
			t.Errorf("%s should be analog", t2)
		}
	}
	discrete := []DataType{TypeNoData, TypeData8, TypeBool, TypeBitmap8, TypeBitmap16,
		TypeBitmap32, TypeEnum8, TypeEnum16, TypeOctetStr, TypeCharStr, TypeIEEE}
	for _, t2 := range discrete {
		if t2.Analog() {
			t.Errorf("%s should be discrete", t2)
		}
	}
}

// Interpret's names are the keys the device registry stores readings under, so
// renaming one silently orphans everything recorded before the change.
func TestInterpretedNamesAreTheRegistryKeys(t *testing.T) {
	want := map[string]bool{
		"temperature": true, "humidity": true, "pressure": true,
		"illuminance": true, "battery voltage": true, "battery percentage": true,
		"occupancy": true, "on/off": true, "level": true,
		"temperature maximum": true, "temperature minimum": true, "temperature reference": true,
		"humidity maximum": true, "humidity minimum": true, "humidity reference": true,
	}
	got := make(map[string]bool)
	for key, spec := range attributes {
		if spec.scale == 0 {
			continue
		}
		got[spec.name] = true
		if _, ok := want[spec.name]; !ok {
			t.Errorf("cluster 0x%04X attribute 0x%04X interprets as %q, which is not a known registry key", key[0], key[1], spec.name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("nothing interprets as %q any more; readings stored under it are orphaned", name)
		}
	}
}

// A name has to identify one attribute within its cluster, or ParseAttribute
// would return whichever the map happened to yield first.
func TestAttributeNamesAreUniqueWithinACluster(t *testing.T) {
	seen := make(map[[2]any]uint16)
	for key, spec := range attributes {
		id := [2]any{key[0], spec.name}
		if other, ok := seen[id]; ok {
			t.Errorf("cluster 0x%04X calls both 0x%04X and 0x%04X %q", key[0], other, key[1], spec.name)
		}
		seen[id] = key[1]
	}
}

func TestAttributeNamesRoundTrip(t *testing.T) {
	for key := range attributes {
		cluster, attr := key[0], key[1]
		got, ok := ParseAttribute(cluster, AttributeName(cluster, attr))
		if !ok || got != attr {
			t.Errorf("cluster 0x%04X attribute 0x%04X named %q parsed back as 0x%04X (ok=%v)",
				cluster, attr, AttributeName(cluster, attr), got, ok)
		}
	}
}

func TestClusterNamesRoundTrip(t *testing.T) {
	for id := range clusterNames {
		got, ok := ParseCluster(ClusterName(id))
		if !ok || got != id {
			t.Errorf("cluster 0x%04X named %q parsed back as 0x%04X (ok=%v)", id, ClusterName(id), got, ok)
		}
	}
}

func TestParseHex16RequiresItsPrefix(t *testing.T) {
	// Without the prefix there is no telling whether 1000 was meant as hex or
	// as decimal, and they are different attributes.
	if _, ok := ParseHex16("1000"); ok {
		t.Error("accepted a bare number as hex")
	}
	got, ok := ParseHex16("0x0402")
	if !ok || got != 0x0402 {
		t.Errorf("ParseHex16(0x0402) = 0x%04X, %v", got, ok)
	}
	if _, ok := ParseHex16("0xZZZZ"); ok {
		t.Error("accepted a non-hex value")
	}
}

func TestParseDataTypeRoundTrip(t *testing.T) {
	for want := range typeNames {
		got, ok := ParseDataType(want.String())
		if !ok || got != want {
			t.Errorf("%q parsed back as 0x%02X (ok=%v), want 0x%02X", want.String(), uint8(got), ok, uint8(want))
		}
	}
	if _, ok := ParseDataType("uint7"); ok {
		t.Error("accepted a type that does not exist")
	}
	if got, ok := ParseDataType("0x29"); !ok || got != TypeInt16 {
		t.Errorf("ParseDataType(0x29) = 0x%02X, %v; want int16", uint8(got), ok)
	}
}

func TestAttributeTypeIsKnownForEveryNamedAttribute(t *testing.T) {
	// A named attribute that cannot be encoded is only half useful: it can be
	// read, but not written and not configured for reporting.
	for key, spec := range attributes {
		if _, err := encodeValue(spec.typ, sampleValue(spec.typ)); err != nil {
			t.Errorf("cluster 0x%04X attribute 0x%04X (%s) has type %s, which cannot be encoded: %v",
				key[0], key[1], spec.name, spec.typ, err)
		}
	}
}

// sampleValue is a value of the right Go kind for a data type, for checking
// that the type can be encoded at all.
func sampleValue(t DataType) any {
	switch t {
	case TypeBool:
		return true
	case TypeCharStr, TypeOctetStr:
		return "x"
	case TypeSingle:
		return float64(1)
	case TypeInt8, TypeInt16, TypeInt24, TypeInt32:
		return int64(1)
	default:
		return uint64(1)
	}
}

func FuzzDecodeResponses(f *testing.F) {
	f.Add([]byte{0x18, 0x07, CmdWriteAttributesResponse, StatusSuccess})
	f.Add([]byte{0x18, 0x09, CmdConfigureReportingRsp, StatusUnreportableAttribute, 0x00, 0x05, 0x00})
	f.Add([]byte{0x18, 0x07, CmdDefaultResponse, CmdWriteAttributes, StatusUnsupGeneralCommand})
	f.Fuzz(func(t *testing.T, payload []byte) {
		frame, err := Decode(payload)
		if err != nil {
			return
		}
		_, _ = frame.WriteResponse([]uint16{0x0000})
		_, _ = frame.ConfigureReportingResponse([]uint16{0x0000})
		_, _ = frame.DefaultResponse()
	})
}

// A reportable change is stated in the attribute's own units, so being able to
// say what it works out to before sending it is what makes a mistake by a
// factor of a hundred visible.
func TestScaleRendersARawThresholdAsAQuantity(t *testing.T) {
	reading, ok := Scale(ClusterTemperature, 0x0000, 50)
	if !ok {
		t.Fatal("temperature has no interpretation")
	}
	if reading.Value != 0.5 || reading.Unit != "°C" {
		t.Errorf("50 raw = %v %s, want 0.5 °C", reading.Value, reading.Unit)
	}
	if _, ok := Scale(ClusterBasic, 0x0005, 1); ok {
		t.Error("a model string was scaled as a quantity")
	}
	if _, ok := Scale(ClusterTemperature, 0x4242, 1); ok {
		t.Error("an unknown attribute was scaled as a quantity")
	}
}

func TestReadReportingConfigRequestEncoding(t *testing.T) {
	got, err := ReadReportingConfigRequest(0x0A, []uint16{0x0000, 0x0021})
	if err != nil {
		t.Fatalf("ReadReportingConfigRequest: %v", err)
	}
	want := []byte{
		0x00, 0x0A, CmdReadReportingConfig,
		0x00, 0x00, 0x00, // the reports the device sends, attribute 0x0000
		0x00, 0x21, 0x00, // and attribute 0x0021
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = % X, want % X", got, want)
	}
}

// Reading the configuration back is the only way to tell an attribute that has
// been switched off from one that merely has nothing new to say.
func TestReadReportingConfigResponseDistinguishesOffFromConfigured(t *testing.T) {
	frame, err := Decode([]byte{
		0x18, 0x0A, CmdReadReportingConfigRsp,
		// A configured analog attribute: type, both intervals, and a change.
		StatusSuccess, 0x00, 0x00, 0x00, byte(TypeInt16), 0x3C, 0x00, 0x10, 0x0E, 0x32, 0x00,
		// One the device is not reporting at all.
		StatusSuccess, 0x00, 0x01, 0x00, byte(TypeInt16), 0x3C, 0x00, 0xFF, 0xFF, 0x32, 0x00,
		// One it will not answer for.
		StatusUnsupportedAttribute, 0x00, 0x02, 0x00,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.ReadReportingConfigResponse()
	if err != nil {
		t.Fatalf("ReadReportingConfigResponse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("records = %+v, want three", got)
	}
	if !got[0].Reporting() || got[0].Min != 60 || got[0].Max != 3600 {
		t.Errorf("first = %+v, want reporting every 60s to 3600s", got[0])
	}
	if !bytes.Equal(got[0].Change, []byte{0x32, 0x00}) {
		t.Errorf("reportable change = % X, want 32 00", got[0].Change)
	}
	if got[1].Reporting() {
		t.Errorf("second = %+v, want it read as not reported", got[1])
	}
	if got[2].Reporting() || got[2].Status != StatusUnsupportedAttribute {
		t.Errorf("third = %+v, want an unsupported attribute", got[2])
	}
}

// A discrete attribute's record has no reportable change, so reading one as
// though it did would consume the next record's status byte.
func TestReadReportingConfigResponseWalksDiscreteRecords(t *testing.T) {
	frame, err := Decode([]byte{
		0x18, 0x0A, CmdReadReportingConfigRsp,
		StatusSuccess, 0x00, 0x00, 0x00, byte(TypeBool), 0x01, 0x00, 0x2C, 0x01,
		StatusSuccess, 0x00, 0x01, 0x00, byte(TypeBool), 0x02, 0x00, 0x2D, 0x01,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, err := frame.ReadReportingConfigResponse()
	if err != nil {
		t.Fatalf("ReadReportingConfigResponse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %+v, want two", got)
	}
	if got[1].ID != 0x0001 || got[1].Min != 2 || got[1].Max != 301 {
		t.Errorf("second = %+v, want attribute 1 every 2s to 301s", got[1])
	}
	if got[0].Change != nil {
		t.Errorf("a discrete attribute carried a reportable change: % X", got[0].Change)
	}
}
