package zdo

import (
	"strings"
	"testing"
)

// The payloads marked as captured came off the wire from an EFR32 running
// EmberZNet 7.4.4 answering ZDO queries about itself. The rest are constructed
// from the field layouts in the Zigbee specification to reach cases hardware
// did not produce on demand — an end device, a refusal, a truncated frame —
// and are labelled as such, because a constructed vector is only as good as
// the reading of the spec behind it.
//
// Either way each is spelled out byte by byte, so what a failure points at is
// the field that moved rather than a helper that built it.

func TestResponseClusterSetsTheHighBit(t *testing.T) {
	for request, want := range map[uint16]uint16{
		ClusterNodeDescriptorReq:   0x8002,
		ClusterPowerDescriptorReq:  0x8003,
		ClusterSimpleDescriptorReq: 0x8004,
		ClusterActiveEndpointsReq:  0x8005,
	} {
		if got := ResponseCluster(request); got != want {
			t.Errorf("ResponseCluster(0x%04X) = 0x%04X, want 0x%04X", request, got, want)
		}
	}
}

func TestAddressRequestIsLittleEndian(t *testing.T) {
	got := AddressRequest(0x41, 0x90CB)
	want := []byte{0x41, 0xCB, 0x90}
	if string(got) != string(want) {
		t.Errorf("AddressRequest = % 02X, want % 02X", got, want)
	}
}

func TestSimpleDescriptorRequestAppendsTheEndpoint(t *testing.T) {
	got := SimpleDescriptorRequest(0x42, 0x90CB, 1)
	want := []byte{0x42, 0xCB, 0x90, 0x01}
	if string(got) != string(want) {
		t.Errorf("SimpleDescriptorRequest = % 02X, want % 02X", got, want)
	}
}

// AddressRequest must not hand out a buffer that a later append can scribble
// on: SimpleDescriptorRequest builds on it, and both are called in a loop.
func TestAddressRequestDoesNotShareBacking(t *testing.T) {
	first := SimpleDescriptorRequest(0x01, 0x1234, 1)
	second := SimpleDescriptorRequest(0x02, 0x1234, 2)
	if first[0] != 0x01 || first[3] != 0x01 {
		t.Errorf("first request = % 02X, want it unchanged by the second", first)
	}
	if second[3] != 0x02 {
		t.Errorf("second request = % 02X, want endpoint 2", second)
	}
}

// Captured: the coordinator's answer about itself.
func TestParseNodeDescriptorOfTheCoordinator(t *testing.T) {
	payload := []byte{
		0x41,       // transaction sequence
		0x00,       // status: success
		0x00, 0x00, // network address: the coordinator
		0x00,       // logical type 0: coordinator
		0x40,       // APS flags and frequency band: 2.4 GHz
		0x8F,       // MAC capability: mains, always listening, full function
		0xCD, 0xAB, // manufacturer code 0xABCD
		0x52,       // maximum buffer size
		0x80, 0x00, // maximum incoming transfer size
		0x41, 0x2C, // server mask: bit 0 says primary trust centre
		0x80, 0x00, // maximum outgoing transfer size
		0x00, // descriptor capability
	}

	addr, nd, status, err := ParseNodeDescriptor(payload)
	if err != nil {
		t.Fatalf("ParseNodeDescriptor: %v", err)
	}
	if !status.OK() || addr != 0x0000 {
		t.Fatalf("status %s for 0x%04X, want success for 0x0000", status, addr)
	}
	if nd.LogicalType != TypeCoordinator {
		t.Errorf("logical type = %s, want coordinator", nd.LogicalType)
	}
	if !nd.MACCapability.Mains() || nd.MACCapability.Sleepy() {
		t.Errorf("capability 0x%02X = mains %t, sleepy %t; want true, false",
			uint8(nd.MACCapability), nd.MACCapability.Mains(), nd.MACCapability.Sleepy())
	}
	if nd.ManufacturerCode != 0xABCD {
		t.Errorf("manufacturer code = 0x%04X, want 0xABCD", nd.ManufacturerCode)
	}
	if nd.MaxBufferSize != 82 || nd.MaxIncomingSize != 128 || nd.MaxOutgoingSize != 128 {
		t.Errorf("transfer sizes = %d/%d/%d, want 82/128/128",
			nd.MaxBufferSize, nd.MaxIncomingSize, nd.MaxOutgoingSize)
	}
	if nd.ServerMask != 0x2C41 {
		t.Errorf("server mask = 0x%04X, want 0x2C41", nd.ServerMask)
	}
}

// Captured: the coordinator is mains powered and reports a full level, which
// is what fixes the order of the four nibbles. Reading them the other way
// round turns "mains, full" into "no mode, level zero".
func TestParsePowerDescriptorOfTheCoordinator(t *testing.T) {
	payload := []byte{
		0x42, 0x00, 0x00, 0x00,
		0x10, // current mode 0, available sources 1: mains
		0xC1, // current source 1: mains, level 12: full
	}
	_, pd, status, err := ParsePowerDescriptor(payload)
	if err != nil || !status.OK() {
		t.Fatalf("ParsePowerDescriptor: %s %v", status, err)
	}
	if pd.AvailableSrc != PowerMains || pd.CurrentSrc != PowerMains || pd.CurrentLevel != 12 {
		t.Errorf("descriptor = %+v, want mains available and in use, level 12", pd)
	}
	if got := pd.Description(); got != "mains" {
		t.Errorf("Description = %q, want %q", got, "mains")
	}
}

// Captured: the coordinator's own endpoint, registered by this stack. It is
// the widest simple descriptor to hand — seventeen output clusters — which is
// what makes it worth pinning: the output list is parsed with a second count
// byte after a variable-length input list, so a miscount lands here first.
func TestParseSimpleDescriptorOfTheCoordinatorEndpoint(t *testing.T) {
	payload := []byte{
		0x44, 0x00, 0x00, 0x00,
		0x32,       // descriptor length: 50
		0x01,       // endpoint 1
		0x04, 0x01, // profile 0x0104: Home Automation
		0x05, 0x00, // device 0x0005: configuration tool
		0x00, // version
		0x04, // four input clusters
		0x00, 0x00, 0x03, 0x00, 0x0A, 0x00, 0x19, 0x00,
		0x11, // seventeen output clusters
		0x00, 0x00, 0x01, 0x00, 0x03, 0x00, 0x04, 0x00, 0x05, 0x00,
		0x06, 0x00, 0x08, 0x00, 0x20, 0x00, 0x00, 0x03, 0x00, 0x04,
		0x02, 0x04, 0x03, 0x04, 0x05, 0x04, 0x06, 0x04, 0x00, 0x05,
		0x02, 0x07, 0x04, 0x0B,
	}
	_, sd, status, err := ParseSimpleDescriptor(payload)
	if err != nil || !status.OK() {
		t.Fatalf("ParseSimpleDescriptor: %s %v", status, err)
	}
	if sd.Endpoint != 1 || sd.Profile != 0x0104 || sd.Device != 0x0005 {
		t.Errorf("descriptor = %+v, want endpoint 1, profile 0x0104, device 0x0005", sd)
	}
	wantIn := []uint16{0x0000, 0x0003, 0x000A, 0x0019}
	if len(sd.Input) != len(wantIn) {
		t.Fatalf("input clusters = %v, want %v", sd.Input, wantIn)
	}
	for i, id := range wantIn {
		if sd.Input[i] != id {
			t.Errorf("input cluster %d = 0x%04X, want 0x%04X", i, sd.Input[i], id)
		}
	}
	if len(sd.Output) != 17 {
		t.Fatalf("output clusters = %d, want 17: %v", len(sd.Output), sd.Output)
	}
	if sd.Output[0] != 0x0000 || sd.Output[16] != 0x0B04 {
		t.Errorf("output clusters run 0x%04X..0x%04X, want 0x0000..0x0B04",
			sd.Output[0], sd.Output[16])
	}
}

// Captured: the coordinator has one endpoint, and says so.
func TestParseActiveEndpointsOfTheCoordinator(t *testing.T) {
	_, eps, status, err := ParseActiveEndpoints([]byte{0x43, 0x00, 0x00, 0x00, 0x01, 0x01})
	if err != nil || !status.OK() {
		t.Fatalf("ParseActiveEndpoints: %s %v", status, err)
	}
	if len(eps) != 1 || eps[0] != 1 {
		t.Errorf("endpoints = %v, want [1]", eps)
	}
}

// Captured: asking for an endpoint that does not exist. The address is echoed
// and a descriptor length of zero follows the refusal, which must not be read
// as the start of one.
func TestParseSimpleDescriptorOfAnAbsentEndpoint(t *testing.T) {
	addr, sd, status, err := ParseSimpleDescriptor([]byte{0x45, 0x83, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatalf("ParseSimpleDescriptor: %v", err)
	}
	if status != StatusNotActive || addr != 0x0000 {
		t.Errorf("status %s for 0x%04X, want endpoint not active for 0x0000", status, addr)
	}
	if sd.Endpoint != 0 || sd.Profile != 0 || len(sd.Input) != 0 || len(sd.Output) != 0 {
		t.Errorf("descriptor = %+v, want none", sd)
	}
}

func TestParseNodeDescriptorOfASleepySensor(t *testing.T) {
	payload := []byte{
		0x41,       // transaction sequence
		0x00,       // status: success
		0xCB, 0x90, // network address of interest
		0x02,       // logical type 2: end device
		0x40,       // APS flags and frequency band: 2.4 GHz
		0x80,       // MAC capability: allocate address only
		0x86, 0x12, // manufacturer code 0x1286
		0x52,       // maximum buffer size
		0x52, 0x00, // maximum incoming transfer size
		0x00, 0x00, // server mask: an end device serves nothing
		0x52, 0x00, // maximum outgoing transfer size
		0x00, // descriptor capability
	}

	addr, nd, status, err := ParseNodeDescriptor(payload)
	if err != nil {
		t.Fatalf("ParseNodeDescriptor: %v", err)
	}
	if !status.OK() {
		t.Fatalf("status = %s, want success", status)
	}
	if addr != 0x90CB {
		t.Errorf("address = 0x%04X, want 0x90CB", addr)
	}
	if nd.LogicalType != TypeEndDevice {
		t.Errorf("logical type = %s, want end device", nd.LogicalType)
	}
	if nd.MACCapability.Mains() {
		t.Error("a sensor with capability 0x80 reported itself as mains powered")
	}
	if !nd.MACCapability.Sleepy() {
		t.Error("a sensor with RxOnWhenIdle clear reported itself as always listening")
	}
	if nd.ManufacturerCode != 0x1286 {
		t.Errorf("manufacturer code = 0x%04X, want 0x1286", nd.ManufacturerCode)
	}
	if nd.MaxBufferSize != 0x52 || nd.MaxIncomingSize != 0x52 || nd.MaxOutgoingSize != 0x52 {
		t.Errorf("transfer sizes = %d/%d/%d, want 82/82/82",
			nd.MaxBufferSize, nd.MaxIncomingSize, nd.MaxOutgoingSize)
	}
	if nd.ServerMask != 0 {
		t.Errorf("server mask = 0x%04X, want 0", nd.ServerMask)
	}
}

// The logical type is the low three bits of a byte whose upper bits carry the
// complex and user descriptor flags. Masking them off is what keeps a router
// that publishes a user descriptor from decoding as some unknown role.
func TestParseNodeDescriptorMasksTheLogicalTypeNibble(t *testing.T) {
	payload := []byte{
		0x41, 0x00, 0x00, 0x00,
		0x19,       // 0b0001_1001: router, with both descriptor flags set
		0x40, 0x8E, // capability: mains powered and always listening
		0x00, 0x00, 0x52, 0x52, 0x00, 0x01, 0x00, 0x52, 0x00, 0x00,
	}
	_, nd, _, err := ParseNodeDescriptor(payload)
	if err != nil {
		t.Fatalf("ParseNodeDescriptor: %v", err)
	}
	if nd.LogicalType != TypeRouter {
		t.Errorf("logical type = %s (%d), want router", nd.LogicalType, uint8(nd.LogicalType))
	}
	if !nd.MACCapability.Mains() || nd.MACCapability.Sleepy() {
		t.Errorf("capability 0x8E = mains %t, sleepy %t; want true, false",
			nd.MACCapability.Mains(), nd.MACCapability.Sleepy())
	}
}

func TestParseActiveEndpoints(t *testing.T) {
	payload := []byte{
		0x43,
		0x00,       // success
		0xCB, 0x90, // address
		0x02,       // two endpoints
		0x01, 0xF2, // endpoint 1, and the Green Power endpoint
	}
	addr, eps, status, err := ParseActiveEndpoints(payload)
	if err != nil {
		t.Fatalf("ParseActiveEndpoints: %v", err)
	}
	if !status.OK() || addr != 0x90CB {
		t.Fatalf("status %s for 0x%04X, want success for 0x90CB", status, addr)
	}
	if len(eps) != 2 || eps[0] != 0x01 || eps[1] != 0xF2 {
		t.Errorf("endpoints = %v, want [1 242]", eps)
	}
}

// A device with no endpoints is legal and must not be confused with a failure.
func TestParseActiveEndpointsAcceptsAnEmptyList(t *testing.T) {
	_, eps, status, err := ParseActiveEndpoints([]byte{0x43, 0x00, 0xCB, 0x90, 0x00})
	if err != nil {
		t.Fatalf("ParseActiveEndpoints: %v", err)
	}
	if !status.OK() {
		t.Fatalf("status = %s, want success", status)
	}
	if len(eps) != 0 {
		t.Errorf("endpoints = %v, want none", eps)
	}
}

func TestParseSimpleDescriptor(t *testing.T) {
	payload := []byte{
		0x44,
		0x00,       // success
		0xCB, 0x90, // address
		0x14,       // descriptor length
		0x01,       // endpoint 1
		0x04, 0x01, // profile 0x0104: Home Automation
		0x02, 0x03, // device 0x0302: temperature sensor
		0x00,                                           // version
		0x04,                                           // four input clusters
		0x00, 0x00, 0x01, 0x00, 0x03, 0x00, 0x02, 0x04, // Basic, Power, Identify, Temperature
		0x01,       // one output cluster
		0x19, 0x00, // OTA Upgrade
	}
	addr, sd, status, err := ParseSimpleDescriptor(payload)
	if err != nil {
		t.Fatalf("ParseSimpleDescriptor: %v", err)
	}
	if !status.OK() || addr != 0x90CB {
		t.Fatalf("status %s for 0x%04X, want success for 0x90CB", status, addr)
	}
	if sd.Endpoint != 1 || sd.Profile != 0x0104 || sd.Device != 0x0302 {
		t.Errorf("descriptor = %+v, want endpoint 1, profile 0x0104, device 0x0302", sd)
	}
	want := []uint16{0x0000, 0x0001, 0x0003, 0x0402}
	if len(sd.Input) != len(want) {
		t.Fatalf("input clusters = %v, want %v", sd.Input, want)
	}
	for i, id := range want {
		if sd.Input[i] != id {
			t.Errorf("input cluster %d = 0x%04X, want 0x%04X", i, sd.Input[i], id)
		}
	}
	if len(sd.Output) != 1 || sd.Output[0] != 0x0019 {
		t.Errorf("output clusters = %v, want [0x0019]", sd.Output)
	}
}

// The version is the low nibble of a byte whose upper nibble is reserved.
func TestParseSimpleDescriptorMasksTheVersionNibble(t *testing.T) {
	payload := []byte{
		0x44, 0x00, 0xCB, 0x90, 0x08,
		0x01, 0x04, 0x01, 0x02, 0x03,
		0xF1,       // reserved nibble set, version 1
		0x00, 0x00, // no clusters in either direction
	}
	_, sd, _, err := ParseSimpleDescriptor(payload)
	if err != nil {
		t.Fatalf("ParseSimpleDescriptor: %v", err)
	}
	if sd.Version != 1 {
		t.Errorf("version = %d, want 1", sd.Version)
	}
	if len(sd.Input) != 0 || len(sd.Output) != 0 {
		t.Errorf("clusters = %v/%v, want none", sd.Input, sd.Output)
	}
}

func TestParsePowerDescriptorUnpacksFourNibbles(t *testing.T) {
	// Two bytes, four nibbles, least significant octet first: the first byte
	// holds the current mode and the available sources, the second the source
	// in use and its remaining level. Same layout as the captured mains
	// descriptor above, on a battery.
	payload := []byte{
		0x45, 0x00, 0xCB, 0x90,
		0x41, // mode 1, available sources 4 (disposable battery)
		0xC4, // current source 4, level 12 (full)
	}
	addr, pd, status, err := ParsePowerDescriptor(payload)
	if err != nil {
		t.Fatalf("ParsePowerDescriptor: %v", err)
	}
	if !status.OK() || addr != 0x90CB {
		t.Fatalf("status %s for 0x%04X, want success for 0x90CB", status, addr)
	}
	if pd.CurrentMode != 1 || pd.AvailableSrc != 4 || pd.CurrentSrc != 4 || pd.CurrentLevel != 12 {
		t.Errorf("descriptor = %+v, want mode 1, available 4, current 4, level 12", pd)
	}
	if got := pd.Description(); got != "disposable battery" {
		t.Errorf("Description = %q, want %q", got, "disposable battery")
	}
}

func TestPowerDescriptorDescription(t *testing.T) {
	for _, tc := range []struct {
		source uint8
		want   string
	}{
		{PowerMains, "mains"},
		{PowerRechargble, "rechargeable battery"},
		{PowerDisposable, "disposable battery"},
		// Mains wins when a device reports more than one, because that is the
		// one that decides whether it can be reached on demand.
		{PowerMains | PowerDisposable, "mains"},
		{0x08, "power source 0x08"},
	} {
		if got := (PowerDescriptor{CurrentSrc: tc.source}).Description(); got != tc.want {
			t.Errorf("Description(0x%02X) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// A device that answers "no" is not a parse failure. The address must survive
// so the caller knows which question was refused, and no descriptor should be
// invented from bytes that are not there.
func TestParsersReportFailureStatusWithoutError(t *testing.T) {
	refusal := []byte{0x46, byte(StatusDeviceNotFound), 0xCB, 0x90}

	if addr, nd, status, err := ParseNodeDescriptor(refusal); err != nil ||
		status != StatusDeviceNotFound || addr != 0x90CB || nd != (NodeDescriptor{}) {
		t.Errorf("node descriptor refusal = 0x%04X %+v %s %v", addr, nd, status, err)
	}
	if addr, eps, status, err := ParseActiveEndpoints(refusal); err != nil ||
		status != StatusDeviceNotFound || addr != 0x90CB || eps != nil {
		t.Errorf("active endpoints refusal = 0x%04X %v %s %v", addr, eps, status, err)
	}
	if addr, sd, status, err := ParseSimpleDescriptor(refusal); err != nil ||
		status != StatusDeviceNotFound || addr != 0x90CB || sd.Endpoint != 0 {
		t.Errorf("simple descriptor refusal = 0x%04X %+v %s %v", addr, sd, status, err)
	}
	if addr, pd, status, err := ParsePowerDescriptor(refusal); err != nil ||
		status != StatusDeviceNotFound || addr != 0x90CB || pd != (PowerDescriptor{}) {
		t.Errorf("power descriptor refusal = 0x%04X %+v %s %v", addr, pd, status, err)
	}
}

// Asking for an endpoint a device does not have is the everyday failure, and
// it must name the endpoint rather than looking like a broken device.
func TestSimpleDescriptorRejectsAnInactiveEndpoint(t *testing.T) {
	_, _, status, err := ParseSimpleDescriptor([]byte{0x47, byte(StatusNotActive), 0xCB, 0x90})
	if err != nil {
		t.Fatalf("ParseSimpleDescriptor: %v", err)
	}
	if status.OK() || status.String() != "endpoint not active" {
		t.Errorf("status = %s, want endpoint not active", status)
	}
}

// A truncated response must be an error rather than a zero-valued descriptor,
// which would otherwise be recorded as fact.
func TestParsersRejectTruncatedPayloads(t *testing.T) {
	parsers := map[string]func([]byte) error{
		"node descriptor": func(b []byte) error {
			_, _, _, err := ParseNodeDescriptor(b)
			return err
		},
		"active endpoints": func(b []byte) error {
			_, _, _, err := ParseActiveEndpoints(b)
			return err
		},
		"simple descriptor": func(b []byte) error {
			_, _, _, err := ParseSimpleDescriptor(b)
			return err
		},
		"power descriptor": func(b []byte) error {
			_, _, _, err := ParsePowerDescriptor(b)
			return err
		},
	}
	// A success status with nothing behind it: the header promises a
	// descriptor the payload does not contain.
	header := []byte{0x48, 0x00, 0xCB, 0x90}
	for name, parse := range parsers {
		if err := parse(header); err == nil {
			t.Errorf("%s accepted a payload with no descriptor", name)
		}
		for n := 0; n < len(header); n++ {
			if err := parse(header[:n]); err == nil {
				t.Errorf("%s accepted a %d-byte payload", name, n)
			}
		}
	}
}

// A count byte is a promise about bytes that follow, and a device that lies
// about it must not produce a half-filled descriptor.
func TestParsersRejectOverstatedCounts(t *testing.T) {
	if _, _, _, err := ParseActiveEndpoints([]byte{0x49, 0x00, 0xCB, 0x90, 0x05, 0x01}); err == nil {
		t.Error("active endpoints accepted a list shorter than its count")
	}
	overstated := []byte{
		0x49, 0x00, 0xCB, 0x90, 0x14,
		0x01, 0x04, 0x01, 0x02, 0x03, 0x00,
		0x04, 0x00, 0x00, // four input clusters promised, one supplied
	}
	if _, _, _, err := ParseSimpleDescriptor(overstated); err == nil {
		t.Error("simple descriptor accepted fewer clusters than its count")
	}
}

func TestSequence(t *testing.T) {
	if seq, ok := Sequence([]byte{0x4A, 0x00}); !ok || seq != 0x4A {
		t.Errorf("Sequence = 0x%02X, %t; want 0x4A, true", seq, ok)
	}
	if _, ok := Sequence(nil); ok {
		t.Error("Sequence reported a transaction number for an empty payload")
	}
}

func TestStatusStrings(t *testing.T) {
	for status, want := range map[Status]string{
		StatusSuccess:        "success",
		StatusDeviceNotFound: "device not found",
		StatusNotSupported:   "not supported",
		StatusTimeout:        "timeout",
		StatusNoDescriptor:   "no descriptor",
		Status(0x7F):         "zdo status 0x7F",
	} {
		if got := status.String(); got != want {
			t.Errorf("Status(0x%02X) = %q, want %q", uint8(status), got, want)
		}
	}
	if StatusInvalidEP.OK() {
		t.Error("a non-zero status reported OK")
	}
}

func TestLogicalTypeStrings(t *testing.T) {
	for lt, want := range map[LogicalType]string{
		TypeCoordinator: "coordinator",
		TypeRouter:      "router",
		TypeEndDevice:   "end device",
		LogicalType(5):  "logical type 5",
	} {
		if got := lt.String(); got != want {
			t.Errorf("LogicalType(%d) = %q, want %q", uint8(lt), got, want)
		}
	}
}

// Whatever a device sends, a parser may return an error but must never panic:
// these payloads arrive from the network, before anything has vouched for them.
func FuzzParseResponses(f *testing.F) {
	f.Add([]byte{0x41, 0x00, 0xCB, 0x90, 0x02, 0x40, 0x80, 0x86, 0x12, 0x52, 0x52, 0x00, 0x00, 0x00, 0x52, 0x00, 0x00})
	f.Add([]byte{0x43, 0x00, 0xCB, 0x90, 0x02, 0x01, 0xF2})
	f.Add([]byte{0x44, 0x00, 0xCB, 0x90, 0x08, 0x01, 0x04, 0x01, 0x02, 0x03, 0x00, 0x00, 0x00})
	f.Add([]byte{0x45, 0x00, 0xCB, 0x90, 0x41, 0x0C})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		if _, _, _, err := ParseNodeDescriptor(payload); err != nil && !strings.Contains(err.Error(), "zdo:") {
			t.Errorf("node descriptor error is unattributed: %v", err)
		}
		ParseActiveEndpoints(payload)
		ParseSimpleDescriptor(payload)
		ParsePowerDescriptor(payload)
		Sequence(payload)
	})
}
