package zigbee

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
)

// The fixture below answers as a temperature sensor with a Basic cluster that
// names it. ZCL command IDs are spelled out as literals rather than taken from
// the package under test, so a change to which command answers which has to
// disagree with these tests.
const (
	zclNode = 0x77A1

	cmdReadAttributes          = 0x00
	cmdReadAttributesResponse  = 0x01
	cmdWriteAttributes         = 0x02
	cmdWriteAttributesResponse = 0x04
	cmdConfigureReporting      = 0x06
	cmdConfigureReportingRsp   = 0x07
	cmdReadReportingConfig     = 0x08
	cmdReadReportingConfigRsp  = 0x09
	cmdDefaultResponse         = 0x0B
)

// zclReply builds the message a device sends in answer to a ZCL command. A
// real device echoes the sequence it was asked with, and this one does too,
// because that echo is what the matching depends on.
func zclReply(cluster uint16, endpoint, seq, command uint8, body ...byte) ezsp.IncomingMessage {
	return ezsp.IncomingMessage{
		APS: ezsp.APSFrame{
			Profile:  ezsp.ProfileHomeAutomation,
			Cluster:  cluster,
			SourceEP: endpoint,
			DestEP:   1,
		},
		Sender: zclNode,
		LQI:    180,
		RSSI:   -55,
		// Server to client, default response suppressed: this is the answer.
		Payload: append([]byte{0x18, seq, command}, body...),
	}
}

// zclResponder answers reads, writes and reporting configurations the way a
// cooperative device does.
func zclResponder(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
	if dest != zclNode || len(payload) < 3 {
		return ezsp.IncomingMessage{}, false
	}
	seq, command := payload[1], payload[2]

	switch command {
	case cmdReadAttributes:
		// A device answers the attributes it was asked for and no others, so
		// the request has to be read rather than assumed.
		var records []byte
		for i := 3; i+1 < len(payload); i += 2 {
			attr := uint16(payload[i]) | uint16(payload[i+1])<<8
			records = append(records, payload[i], payload[i+1])
			switch [2]uint16{aps.Cluster, attr} {
			case [2]uint16{0x0402, 0x0000}:
				records = append(records, 0x00, 0x29, 0x54, 0x0B) // 2900, an int16
			case [2]uint16{0xFC11, 0x2009}:
				// The coldest this sensor remembers being. It is a
				// temperature, and it is not the temperature.
				records = append(records, 0x00, 0x29, 0x96, 0x0A) // 2710, an int16
			case [2]uint16{0x0000, 0x0004}:
				records = append(records, 0x00, 0x42, 0x06, 'S', 'O', 'N', 'O', 'F', 'F')
			case [2]uint16{0x0000, 0x0005}:
				// Padded to a fixed width with NULs, as devices do.
				records = append(records, 0x00, 0x42, 0x08, 'S', 'N', 'Z', 'B', '-', '0', '2', 0x00)
			default:
				records = append(records, 0x86) // unsupported attribute, so no value
			}
		}
		return zclReply(aps.Cluster, aps.DestEP, seq, cmdReadAttributesResponse, records...), true

	case cmdWriteAttributes:
		return zclReply(aps.Cluster, aps.DestEP, seq, cmdWriteAttributesResponse, 0x00), true
	case cmdConfigureReporting:
		return zclReply(aps.Cluster, aps.DestEP, seq, cmdConfigureReportingRsp, 0x00), true

	case cmdReadReportingConfig:
		// Attribute 0x0000 is configured; 0x0001 has been switched off, which
		// is the pair a caller has to be able to tell apart.
		return zclReply(aps.Cluster, aps.DestEP, seq, cmdReadReportingConfigRsp,
			0x00, 0x00, 0x00, 0x00, 0x29, 0x3C, 0x00, 0x10, 0x0E, 0x32, 0x00,
			0x00, 0x00, 0x01, 0x00, 0x29, 0x3C, 0x00, 0xFF, 0xFF, 0x32, 0x00,
		), true
	}
	return ezsp.IncomingMessage{}, false
}

func zclCoordinator(t *testing.T, responder func(uint16, ezsp.APSFrame, []byte) (ezsp.IncomingMessage, bool)) (*Coordinator, *fakeConnection, *store.Store) {
	t.Helper()
	fake := newFakeConnection()
	fake.responder = responder
	db := emptyStore(t)
	return &Coordinator{conn: fake, db: db}, fake, db
}

func temperatureTarget() Target {
	return Target{Node: zclNode, Endpoint: 1, Cluster: 0x0402}
}

func TestReadAttributesReturnsBothTheRawValueAndItsMeaning(t *testing.T) {
	c, _, _ := zclCoordinator(t, zclResponder)

	values, err := c.ReadAttributes(context.Background(), temperatureTarget(), []uint16{0x0000})
	if err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("values = %+v, want one", values)
	}
	got := values[0]
	if got.Name != "temperature" || got.Type != TypeInt16 {
		t.Errorf("value = %s (%s), want temperature (int16)", got.Name, got.Type)
	}
	// The device said 2900. That it means 29 °C is gzb's contribution, and the
	// raw value has to survive alongside it.
	if got.Value != int64(2900) {
		t.Errorf("raw value = %v, want 2900", got.Value)
	}
	if got.Scaled == nil || *got.Scaled != 29 || got.Unit != "°C" {
		t.Errorf("scaled = %v %s, want 29 °C", got.Scaled, got.Unit)
	}
}

func TestReadAttributesReportsWhatTheDeviceDoesNotImplement(t *testing.T) {
	c, _, _ := zclCoordinator(t, zclResponder)

	values, err := c.ReadAttributes(context.Background(), temperatureTarget(), []uint16{0x0000, 0x0003})
	if err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("values = %+v, want two", values)
	}
	refused := values[1]
	if refused.Status != "unsupported attribute" {
		t.Errorf("status = %q, want unsupported attribute", refused.Status)
	}
	if refused.Value != nil || refused.Scaled != nil {
		t.Errorf("refused attribute carries a value: %+v", refused)
	}
}

func TestReadAttributesRecordsMeasurementsInTheRegistry(t *testing.T) {
	c, _, db := zclCoordinator(t, zclResponder)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", zclNode, time.Time{})

	if _, err := c.ReadAttributes(context.Background(), temperatureTarget(), []uint16{0x0000}); err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	// A reading is a reading whether the device volunteered it or was asked.
	reading, ok := device.Readings["temperature"]
	if !ok {
		t.Fatalf("readings = %+v, want a temperature", device.Readings)
	}
	if reading.Value != 29 || reading.Unit != "°C" {
		t.Errorf("recorded %v %s, want 29 °C", reading.Value, reading.Unit)
	}
	if device.LastSeen.IsZero() {
		t.Error("an answer is proof of life and should update LastSeen")
	}
}

func TestReadAttributesTrimsThePaddingFromStrings(t *testing.T) {
	c, _, db := zclCoordinator(t, zclResponder)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", zclNode, time.Time{})

	target := Target{Node: zclNode, Endpoint: 1, Cluster: 0x0000}
	values, err := c.ReadAttributes(context.Background(), target, []uint16{0x0004, 0x0005})
	if err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if values[1].Value != "SNZB-02" {
		t.Errorf("model = %q, want SNZB-02 with the NUL padding gone", values[1].Value)
	}
	// A model learned by a plain read should name the device afterwards, the
	// same as one learned by an interview.
	if device.Manufacturer != "SONOFF" || device.Model != "SNZB-02" {
		t.Errorf("registry has %q %q, want SONOFF SNZB-02", device.Manufacturer, device.Model)
	}
}

func TestReadAttributesAddressesTheEndpointAndClusterItWasGiven(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	target := Target{Node: zclNode, Endpoint: 3, Cluster: 0x0402}
	if _, err := c.ReadAttributes(context.Background(), target, []uint16{0x0000}); err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("sent %d requests, want one", len(fake.requests))
	}
	sent := fake.requests[0]
	if sent.aps.Profile != ezsp.ProfileHomeAutomation || sent.aps.Cluster != 0x0402 {
		t.Errorf("APS = profile 0x%04X cluster 0x%04X, want Home Automation 0x0402", sent.aps.Profile, sent.aps.Cluster)
	}
	if sent.aps.DestEP != 3 || sent.aps.SourceEP != ezsp.DefaultEndpoint.ID {
		t.Errorf("endpoints = %d -> %d, want %d -> 3", sent.aps.SourceEP, sent.aps.DestEP, ezsp.DefaultEndpoint.ID)
	}
	if sent.aps.Options&ezsp.APSOptionRetry == 0 {
		t.Error("a read should be retried by the stack")
	}
	// Frame control, sequence, command, then the attribute asked for.
	if sent.payload[2] != cmdReadAttributes {
		t.Errorf("command = 0x%02X, want read attributes", sent.payload[2])
	}
}

// Sequence numbers are unique per device, not globally, so a reply from
// somewhere else can carry the one being waited for.
func TestReadAttributesIgnoresAnotherDevicesReply(t *testing.T) {
	c, _, _ := zclCoordinator(t, func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		reply := zclReply(aps.Cluster, aps.DestEP, payload[1], cmdReadAttributesResponse,
			0x00, 0x00, 0x00, 0x29, 0x54, 0x0B)
		reply.Sender = zclNode + 1
		return reply, true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ReadAttributes(ctx, temperatureTarget(), []uint16{0x0000}); err == nil {
		t.Fatal("accepted a reply from a different device")
	}
}

func TestReadAttributesIgnoresAStaleSequence(t *testing.T) {
	c, _, _ := zclCoordinator(t, func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		// The answer to an older question, arriving late.
		return zclReply(aps.Cluster, aps.DestEP, payload[1]-1, cmdReadAttributesResponse,
			0x00, 0x00, 0x00, 0x29, 0x54, 0x0B), true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ReadAttributes(ctx, temperatureTarget(), []uint16{0x0000}); err == nil {
		t.Fatal("accepted a reply to a different question")
	}
}

func TestWriteAttributesReportsOnePerAttributeAsked(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	target := Target{Node: zclNode, Endpoint: 1, Cluster: 0x0000}
	results, err := c.WriteAttributes(context.Background(), target, []AttributeWrite{
		{ID: 0x0010, Type: TypeCharStr, Value: "hall"},
		{ID: 0x0012, Type: TypeBool, Value: true},
	})
	if err != nil {
		t.Fatalf("WriteAttributes: %v", err)
	}
	// The device answered with the all-succeeded shorthand, which names no
	// attributes at all; both should still be accounted for.
	if len(results) != 2 {
		t.Fatalf("results = %+v, want one per attribute", results)
	}
	for _, result := range results {
		if !result.OK || result.Status != "" {
			t.Errorf("result = %+v, want success", result)
		}
	}
	if results[0].Name != "location" {
		t.Errorf("name = %q, want location", results[0].Name)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("sent %d requests, want both writes in one message", len(fake.requests))
	}
}

func TestWriteAttributesNamesWhatTheDeviceRefused(t *testing.T) {
	c, _, _ := zclCoordinator(t, func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		return zclReply(aps.Cluster, aps.DestEP, payload[1], cmdWriteAttributesResponse,
			0x88, 0x00, 0x00), true // read only, attribute 0x0000
	})

	results, err := c.WriteAttributes(context.Background(), temperatureTarget(), []AttributeWrite{
		{ID: 0x0000, Type: TypeInt16, Value: int64(2000)},
	})
	if err != nil {
		t.Fatalf("WriteAttributes: %v", err)
	}
	if len(results) != 1 || results[0].OK || results[0].Status != "read only" {
		t.Fatalf("results = %+v, want temperature refused as read only", results)
	}
}

// A device that does not implement the command answers with a Default
// Response. Waiting for a write response that is never coming would turn that
// into a timeout, which says nothing about what went wrong.
func TestARefusedCommandIsReportedAsARefusalNotATimeout(t *testing.T) {
	c, _, _ := zclCoordinator(t, func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		return zclReply(aps.Cluster, aps.DestEP, payload[1], cmdDefaultResponse,
			cmdWriteAttributes, 0x82), true // unsupported general command
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.WriteAttributes(ctx, temperatureTarget(), []AttributeWrite{
		{ID: 0x0000, Type: TypeInt16, Value: int64(2000)},
	})
	if err == nil {
		t.Fatal("a refused write should be an error")
	}
	if !strings.Contains(err.Error(), "unsupported general command") {
		t.Errorf("error = %v, want it to name the reason the device gave", err)
	}
	if ctx.Err() != nil {
		t.Error("the refusal should arrive immediately, not as a timeout")
	}
}

func TestConfigureReportingSendsIntervalsAsWholeSeconds(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	results, err := c.ConfigureReporting(context.Background(), temperatureTarget(), []ReportConfig{
		{ID: 0x0000, Type: TypeInt16, Min: time.Minute, Max: time.Hour, Change: uint64(50)},
	})
	if err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v, want one success", results)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	sent := fake.requests[0].payload
	want := []byte{
		0x00,       // direction: the reports the device sends
		0x00, 0x00, // attribute 0x0000
		0x29,       // int16
		0x3C, 0x00, // minimum 60s
		0x10, 0x0E, // maximum 3600s
		0x32, 0x00, // reportable change 50
	}
	got := sent[3:]
	if len(got) != len(want) {
		t.Fatalf("record = % X, want % X", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record = % X, want % X", got, want)
		}
	}
}

// An analog attribute must carry a reportable change. Left unsaid it means
// "every change", and the wire spells that zero rather than omitting it.
func TestConfigureReportingFillsInAnUnstatedChange(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	if _, err := c.ConfigureReporting(context.Background(), temperatureTarget(), []ReportConfig{
		{ID: 0x0000, Type: TypeInt16, Min: time.Minute, Max: time.Hour},
	}); err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	sent := fake.requests[0].payload
	if len(sent) != 13 {
		t.Fatalf("payload = % X, want a reportable change on the end", sent)
	}
	if sent[11] != 0x00 || sent[12] != 0x00 {
		t.Errorf("reportable change = % X, want zero", sent[11:])
	}
}

func TestConfigureReportingOffUsesTheProtocolSentinel(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	if _, err := c.ConfigureReporting(context.Background(), temperatureTarget(), []ReportConfig{
		{ID: 0x0000, Type: TypeInt16, Min: time.Minute, Max: ReportingOff},
	}); err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	sent := fake.requests[0].payload
	if sent[9] != 0xFF || sent[10] != 0xFF {
		t.Errorf("maximum interval = % X, want the 0xFFFF that means never", sent[9:11])
	}
}

func TestConfigureReportingRejectsIntervalsTheWireCannotCarry(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	for _, config := range []ReportConfig{
		{ID: 0x0000, Type: TypeInt16, Min: 1500 * time.Millisecond, Max: time.Hour},
		{ID: 0x0000, Type: TypeInt16, Min: -time.Second, Max: time.Hour},
		{ID: 0x0000, Type: TypeInt16, Min: time.Minute, Max: 24 * time.Hour},
	} {
		if _, err := c.ConfigureReporting(context.Background(), temperatureTarget(), []ReportConfig{config}); err == nil {
			t.Errorf("accepted min %s max %s", config.Min, config.Max)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("sent %d requests for configurations that cannot be encoded", len(fake.requests))
	}
}

// Each question needs its own sequence number, or one reply answers two of
// them and the second waits forever.
func TestEachAttributeRequestUsesADistinctSequence(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)
	ctx := context.Background()
	target := temperatureTarget()

	if _, err := c.ReadAttributes(ctx, target, []uint16{0x0000}); err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if _, err := c.WriteAttributes(ctx, target, []AttributeWrite{{ID: 0x0000, Type: TypeInt16, Value: int64(1)}}); err != nil {
		t.Fatalf("WriteAttributes: %v", err)
	}
	if _, err := c.ConfigureReporting(ctx, target, []ReportConfig{{ID: 0x0000, Type: TypeInt16, Max: time.Hour}}); err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	seen := make(map[byte]bool)
	for _, request := range fake.requests {
		seq := request.payload[1]
		if seen[seq] {
			t.Errorf("sequence 0x%02X used twice", seq)
		}
		seen[seq] = true
	}
	if len(seen) != 3 {
		t.Errorf("%d distinct sequences across %d requests", len(seen), len(fake.requests))
	}
}

func TestAttributeRequestsRequireANetwork(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)
	fake.state = ezsp.NetworkNone

	if _, err := c.ReadAttributes(context.Background(), temperatureTarget(), []uint16{0x0000}); err == nil {
		t.Fatal("read succeeded with no network")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("sent %d requests with no network to send them on", len(fake.requests))
	}
}

func TestAttributeRequestsAreRefusedAfterClose(t *testing.T) {
	c, _, _ := zclCoordinator(t, zclResponder)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	target := temperatureTarget()
	if _, err := c.ReadAttributes(ctx, target, []uint16{0x0000}); err != ErrClosed {
		t.Errorf("ReadAttributes after close = %v, want %v", err, ErrClosed)
	}
	if _, err := c.WriteAttributes(ctx, target, []AttributeWrite{{ID: 0x0000, Type: TypeInt16, Value: int64(1)}}); err != ErrClosed {
		t.Errorf("WriteAttributes after close = %v, want %v", err, ErrClosed)
	}
	if _, err := c.ConfigureReporting(ctx, target, []ReportConfig{{ID: 0x0000, Type: TypeInt16, Max: time.Hour}}); err != ErrClosed {
		t.Errorf("ConfigureReporting after close = %v, want %v", err, ErrClosed)
	}
}

func TestEmptyAttributeRequestsAreRejectedBeforeSending(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)
	ctx := context.Background()
	target := temperatureTarget()

	if _, err := c.ReadAttributes(ctx, target, nil); err == nil {
		t.Error("read of no attributes was accepted")
	}
	if _, err := c.WriteAttributes(ctx, target, nil); err == nil {
		t.Error("write of no attributes was accepted")
	}
	if _, err := c.ConfigureReporting(ctx, target, nil); err == nil {
		t.Error("reporting configuration of no attributes was accepted")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("sent %d empty requests", len(fake.requests))
	}
}

// A read can be aimed at any address. Recording one for an address nobody has
// identified would fill the registry with entries that mean nothing.
func TestReadAttributesLeavesUnknownAddressesOutOfTheRegistry(t *testing.T) {
	c, _, db := zclCoordinator(t, zclResponder)

	if _, err := c.ReadAttributes(context.Background(), temperatureTarget(), []uint16{0x0000}); err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if len(db.List()) != 0 {
		t.Errorf("registry gained %d devices from reading an unknown address", len(db.List()))
	}
}

// The JSON shape is a documented output of every gzb command, so a type has to
// survive the trip out and back rather than arriving as an opaque number.
func TestDataTypeRoundTripsThroughJSON(t *testing.T) {
	for _, want := range []DataType{TypeInt16, TypeCharStr, TypeBool, DataType(0x4C)} {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", want, err)
		}
		var got DataType
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", encoded, err)
		}
		if got != want {
			t.Errorf("%s marshalled as %s and came back as %s", want, encoded, got)
		}
	}
}

// Turning reporting off and restoring a device's own defaults are different
// operations that differ by one field, and confusing them silences a sensor
// that was reporting perfectly well.
func TestReportDefaultsIsNotTheSameAsReportingOff(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)
	ctx := context.Background()
	target := temperatureTarget()

	if _, err := c.ConfigureReporting(ctx, target, []ReportConfig{ReportDefaults(0x0000, TypeInt16)}); err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}
	if _, err := c.ConfigureReporting(ctx, target, []ReportConfig{
		{ID: 0x0000, Type: TypeInt16, Min: time.Minute, Max: ReportingOff},
	}); err != nil {
		t.Fatalf("ConfigureReporting: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Both intervals at 0xFFFF is the revert; only the maximum is the silence.
	revert := fake.requests[0].payload
	if revert[7] != 0xFF || revert[8] != 0xFF || revert[9] != 0xFF || revert[10] != 0xFF {
		t.Errorf("revert intervals = % X, want both at 0xFFFF", revert[7:11])
	}
	silence := fake.requests[1].payload
	if silence[7] != 0x3C || silence[8] != 0x00 {
		t.Errorf("minimum interval = % X, want the 60s it was given, not the sentinel", silence[7:9])
	}
	if silence[9] != 0xFF || silence[10] != 0xFF {
		t.Errorf("maximum interval = % X, want 0xFFFF", silence[9:11])
	}
}

// Asking a device what it holds is the only way to tell an attribute that has
// been switched off from one that simply has nothing new to say. Both look
// like silence.
func TestReportingConfigurationDistinguishesOffFromConfigured(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	holding, err := c.ReportingConfiguration(context.Background(), temperatureTarget(), []uint16{0x0000, 0x0001})
	if err != nil {
		t.Fatalf("ReportingConfiguration: %v", err)
	}
	if len(holding) != 2 {
		t.Fatalf("statuses = %+v, want two", holding)
	}
	if !holding[0].Reporting || holding[0].Min != time.Minute || holding[0].Max != time.Hour {
		t.Errorf("first = %+v, want reporting every 1m to 1h", holding[0])
	}
	if holding[0].Name != "temperature" {
		t.Errorf("name = %q, want temperature", holding[0].Name)
	}
	if holding[1].Reporting {
		t.Errorf("second = %+v, want it read as switched off", holding[1])
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.requests[0].payload[2]; got != cmdReadReportingConfig {
		t.Errorf("command = 0x%02X, want read reporting configuration", got)
	}
}

// Asking what a device holds must not change what it holds.
func TestReportingConfigurationChangesNothing(t *testing.T) {
	c, fake, _ := zclCoordinator(t, zclResponder)

	if _, err := c.ReportingConfiguration(context.Background(), temperatureTarget(), []uint16{0x0000}); err != nil {
		t.Fatalf("ReportingConfiguration: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, request := range fake.requests {
		if request.payload[2] == cmdConfigureReporting {
			t.Error("reading the configuration sent a configure command")
		}
	}
}

// A registry reading answers "what is it now". A device-kept extreme is a
// temperature that answers a different question, so reading one must not file
// it under the quantity it resembles — while still rendering as that quantity,
// which is the whole reason the two were confused in the first place.
func TestReadAttributesDoesNotRecordAStatisticAsAReading(t *testing.T) {
	c, _, db := zclCoordinator(t, zclResponder)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", zclNode, time.Time{})

	target := Target{Node: zclNode, Endpoint: 1, Cluster: 0xFC11}
	values, err := c.ReadAttributes(context.Background(), target, []uint16{0x2009})
	if err != nil {
		t.Fatalf("ReadAttributes: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("values = %+v, want one", values)
	}
	got := values[0]
	if got.Name != "temperature minimum" {
		t.Errorf("name = %q, want temperature minimum", got.Name)
	}
	if got.Scaled == nil || *got.Scaled < 27.09 || *got.Scaled > 27.11 || got.Unit != "°C" {
		t.Errorf("scaled = %v %s, want 27.10 °C", got.Scaled, got.Unit)
	}
	if got.Current {
		t.Error("a device-kept minimum is marked as the device's current value")
	}
	if len(device.Readings) != 0 {
		t.Errorf("readings = %+v, want none: a minimum is not a measurement of now", device.Readings)
	}
}

// The same distinction has to hold for a report, which arrives by a different
// path. A statistic a device volunteers is still worth seeing, so it surfaces
// as an event rather than being dropped.
func TestReportedStatisticIsAnEventRatherThanAReading(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())

	var events []Event
	c := &Coordinator{conn: fake, db: db, opts: Options{
		OnUnhandled: func(e Event) { events = append(events, e) },
	}}

	// Report attributes: 0x2009 on the SONOFF cluster, an int16 of 2710.
	msg := incomingMessage(0xFC11, []byte{0x18, 0x01, 0x0A, 0x09, 0x20, 0x29, 0x96, 0x0A})
	readings, err := c.handleIncoming(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleIncoming: %v", err)
	}
	if len(readings) != 0 {
		t.Errorf("readings = %+v, want none", readings)
	}
	if len(device.Readings) != 0 {
		t.Errorf("registry readings = %+v, want none", device.Readings)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want the statistic reported as one", events)
	}
	if !strings.Contains(events[0].Description, "temperature minimum") {
		t.Errorf("event described as %q, want it to name the statistic", events[0].Description)
	}
}
