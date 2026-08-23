package zcl

import "testing"

// Every frame in this file was captured from a SONOFF temperature/humidity
// sensor (A4:C1:38:18:56:07:FF:FF) reporting to this coordinator. Pinning the
// tests to real bytes rather than to hand-built ones is what catches a decoder
// that is self-consistently wrong.

func TestDecodeTemperatureReport(t *testing.T) {
	// Cluster 0x0402, captured on the wire.
	frame, err := Decode([]byte{0x18, 0xC8, 0x0A, 0x00, 0x00, 0x29, 0x54, 0x0B})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if frame.Type != FrameProfileWide {
		t.Errorf("Type = %d, want profile-wide", frame.Type)
	}
	if frame.Direction != ServerToClient {
		t.Error("a sensor report travels server to client")
	}
	if !frame.DisableDefaultResponse {
		t.Error("frame control 0x18 sets the disable-default-response bit")
	}
	if frame.Command != CmdReportAttributes {
		t.Fatalf("Command = 0x%02X, want report attributes", frame.Command)
	}

	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want 1", len(attrs))
	}
	if attrs[0].ID != 0x0000 || attrs[0].Type != TypeInt16 {
		t.Fatalf("attribute = %+v, want 0x0000 as int16", attrs[0])
	}
	if got, ok := attrs[0].Value.(int64); !ok || got != 2900 {
		t.Fatalf("raw value = %v, want 2900", attrs[0].Value)
	}

	r, ok := Interpret(ClusterTemperature, attrs[0])
	if !ok {
		t.Fatal("temperature attribute was not interpreted")
	}
	if r.Value != 29.0 || r.Unit != "°C" {
		t.Errorf("reading = %s, want 29.00 °C", r)
	}
}

func TestDecodeHumidityReport(t *testing.T) {
	frame, err := Decode([]byte{0x18, 0xC9, 0x0A, 0x00, 0x00, 0x21, 0x7A, 0x0D})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if attrs[0].Type != TypeUint16 {
		t.Errorf("humidity is reported as uint16, got 0x%02X", uint8(attrs[0].Type))
	}
	r, ok := Interpret(ClusterRelativeHumidity, attrs[0])
	if !ok {
		t.Fatal("humidity attribute was not interpreted")
	}
	if r.Value != 34.5 {
		t.Errorf("reading = %s, want 34.50 %%", r)
	}
}

func TestDecodeBatteryReport(t *testing.T) {
	// Cluster 0x0001, attribute 0x0021: battery percentage, in half-percents.
	frame, err := Decode([]byte{0x18, 0xD6, 0x0A, 0x21, 0x00, 0x20, 0xC8})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if attrs[0].ID != 0x0021 {
		t.Fatalf("attribute = 0x%04X, want 0x0021", attrs[0].ID)
	}
	r, ok := Interpret(ClusterPowerConfiguration, attrs[0])
	if !ok {
		t.Fatal("battery attribute was not interpreted")
	}
	if r.Value != 100 {
		t.Errorf("reading = %s, want 100.00 %% (0xC8 is 200 half-percents)", r)
	}
}

func TestDecodeTimeReadRequest(t *testing.T) {
	// The sensor asking the coordinator for the time: a client-to-server read
	// of six attributes on cluster 0x000A.
	frame, err := Decode([]byte{0x00, 0xCC, 0x00, 0x00, 0x00, 0x02, 0x00, 0x07, 0x00, 0x03, 0x00, 0x04, 0x00, 0x05, 0x00})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if frame.Direction != ClientToServer {
		t.Error("a read request travels client to server")
	}
	if frame.Command != CmdReadAttributes {
		t.Fatalf("Command = 0x%02X, want read attributes", frame.Command)
	}
	if len(frame.Payload) != 12 {
		t.Errorf("payload = % X, want six little-endian attribute IDs", frame.Payload)
	}
	// A read request carries no values, so asking for attribute records must
	// fail rather than invent them.
	if _, err := frame.Attributes(); err == nil {
		t.Error("expected an error decoding attributes from a read request")
	}
}

func TestDecodeManufacturerCluster(t *testing.T) {
	// SONOFF's private cluster 0xFC11. The decoder should handle it as an
	// ordinary report even though the cluster has no friendly name.
	frame, err := Decode([]byte{0x18, 0xD9, 0x0A, 0x07, 0x00, 0x21, 0x00, 0x00})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if attrs[0].ID != 0x0007 {
		t.Errorf("attribute = 0x%04X, want 0x0007", attrs[0].ID)
	}
	if _, ok := Interpret(0xFC11, attrs[0]); ok {
		t.Error("a private cluster attribute should have no interpretation")
	}
	if got := ClusterName(0xFC11); got != "manufacturer 0xFC11" {
		t.Errorf("ClusterName(0xFC11) = %q", got)
	}
}

func TestInterpretSonoffTemperatureHumidityStatistics(t *testing.T) {
	// One periodic FC11 burst from a SONOFF display sensor. Temperature values
	// are signed int16; humidity values are unsigned uint16. Both use
	// hundredths of their displayed unit.
	frame, err := Decode([]byte{
		0x18, 0x01, 0x0A,
		0x08, 0x20, 0x29, 0xAC, 0x08, // temperature maximum: 22.20 °C
		0x09, 0x20, 0x29, 0x84, 0x08, // temperature minimum: 21.80 °C
		0x0A, 0x20, 0x29, 0x9A, 0x08, // temperature reference: 22.02 °C
		0x0B, 0x20, 0x21, 0xAE, 0x10, // humidity maximum: 42.70%
		0x0C, 0x20, 0x21, 0x68, 0x10, // humidity minimum: 42.00%
		0x0D, 0x20, 0x21, 0x8E, 0x10, // humidity reference: 42.38%
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}

	want := []Reading{
		{Name: "temperature maximum", Value: 22.20, Unit: "°C"},
		{Name: "temperature minimum", Value: 21.80, Unit: "°C"},
		{Name: "temperature reference", Value: 22.02, Unit: "°C"},
		{Name: "humidity maximum", Value: 42.70, Unit: "%"},
		{Name: "humidity minimum", Value: 42.00, Unit: "%"},
		{Name: "humidity reference", Value: 42.38, Unit: "%"},
	}
	if len(attrs) != len(want) {
		t.Fatalf("got %d attributes, want %d", len(attrs), len(want))
	}
	for i, attr := range attrs {
		got, ok := Interpret(ClusterSonoff, attr)
		if !ok {
			t.Errorf("attribute 0x%04X was not interpreted", attr.ID)
			continue
		}
		if got != want[i] {
			t.Errorf("attribute 0x%04X = %+v, want %+v", attr.ID, got, want[i])
		}
	}
}

func TestDecodeMultipleAttributes(t *testing.T) {
	// Two records in one report, to prove the loop advances correctly.
	frame, err := Decode([]byte{
		0x18, 0x01, 0x0A,
		0x00, 0x00, 0x29, 0x54, 0x0B, // 0x0000 int16 2900
		0x21, 0x00, 0x20, 0xC8, // 0x0021 uint8 200
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("got %d attributes, want 2", len(attrs))
	}
	if attrs[1].ID != 0x0021 || attrs[1].Value.(uint64) != 200 {
		t.Errorf("second attribute = %+v", attrs[1])
	}
}

func TestDecodeManufacturerSpecificHeader(t *testing.T) {
	// Bit 2 of the frame control inserts a two-byte manufacturer code before
	// the sequence number; misreading it shifts every later field.
	frame, err := Decode([]byte{0x1C, 0x34, 0x12, 0x07, 0x0A, 0x00, 0x00, 0x20, 0x2A})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !frame.ManufacturerSpecific {
		t.Fatal("frame control 0x1C marks a manufacturer-specific frame")
	}
	if frame.ManufacturerCode != 0x1234 {
		t.Errorf("ManufacturerCode = 0x%04X, want 0x1234", frame.ManufacturerCode)
	}
	if frame.Sequence != 0x07 || frame.Command != CmdReportAttributes {
		t.Errorf("seq = 0x%02X, command = 0x%02X", frame.Sequence, frame.Command)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if attrs[0].Value.(uint64) != 42 {
		t.Errorf("value = %v, want 42", attrs[0].Value)
	}
}

func TestReadAttributesResponseCarriesStatus(t *testing.T) {
	// Success then unsupported-attribute: the failed record has no type or
	// value and the decoder must not try to read one.
	frame, err := Decode([]byte{
		0x18, 0x02, 0x01,
		0x00, 0x00, 0x00, 0x21, 0x2C, 0x01, // 0x0000 ok, uint16 300
		0x05, 0x00, 0x86, // 0x0005 unsupported
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs, err := frame.Attributes()
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("got %d attributes, want 2", len(attrs))
	}
	if attrs[0].Status != 0 || attrs[0].Value.(uint64) != 300 {
		t.Errorf("first record = %+v", attrs[0])
	}
	if attrs[1].Status != 0x86 || attrs[1].Value != nil {
		t.Errorf("second record = %+v, want status 0x86 and no value", attrs[1])
	}
}

func TestDecodeRejectsTruncatedFrame(t *testing.T) {
	if _, err := Decode([]byte{0x18}); err == nil {
		t.Error("expected an error for a frame with no command byte")
	}
}

func TestUnknownDataTypeStopsCleanly(t *testing.T) {
	// An unknown type has an unknown width, so the record cannot be walked
	// past. It must report that rather than guess.
	frame, err := Decode([]byte{0x18, 0x01, 0x0A, 0x00, 0x00, 0xEE, 0x01, 0x02})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := frame.Attributes(); err == nil {
		t.Error("expected an error for an unsupported data type")
	}
}
