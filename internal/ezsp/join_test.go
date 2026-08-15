package ezsp

import "testing"

func TestDecodeTrustCenterJoin(t *testing.T) {
	// newNodeId(2) newNodeEui64(8) status(1) policyDecision(1) parent(2)
	params := []byte{
		0xF1, 0xA3, // node 0xA3F1
		0xCD, 0xAB, 0xC0, 0x24, 0x00, 0x4B, 0x12, 0x00, // IEEE, little-endian
		0x01,       // unsecured join
		0x00,       // use preconfigured key
		0x00, 0x00, // parent: the coordinator
	}
	ev, err := decodeTrustCenterJoin(params)
	if err != nil {
		t.Fatalf("decodeTrustCenterJoin: %v", err)
	}
	if ev.NodeID != 0xA3F1 {
		t.Errorf("NodeID = 0x%04X, want 0xA3F1", ev.NodeID)
	}
	if got, want := ev.IEEE.String(), "00:12:4B:00:24:C0:AB:CD"; got != want {
		t.Errorf("IEEE = %s, want %s", got, want)
	}
	if ev.Update == nil || *ev.Update != UpdateUnsecuredJoin {
		t.Errorf("Update = %v, want an unsecured join", ev.Update)
	}
	if ev.Decision == nil || *ev.Decision != JoinUsePreconfiguredKey {
		t.Errorf("Decision = %v, want the preconfigured key", ev.Decision)
	}
	if ev.Leaving {
		t.Error("a join was reported as a departure")
	}
	if ev.Parent == nil || *ev.Parent != 0x0000 {
		t.Errorf("Parent = %v, want 0x0000", ev.Parent)
	}
}

func TestDecodeTrustCenterJoinReportsDeparture(t *testing.T) {
	params := []byte{
		0xF1, 0xA3,
		0xCD, 0xAB, 0xC0, 0x24, 0x00, 0x4B, 0x12, 0x00,
		0x02, // device left
		0x03, // no action
		0x00, 0x00,
	}
	ev, err := decodeTrustCenterJoin(params)
	if err != nil {
		t.Fatalf("decodeTrustCenterJoin: %v", err)
	}
	if !ev.Leaving {
		t.Error("status 0x02 is EMBER_DEVICE_LEFT and must set Leaving")
	}
}

func TestDecodeChildJoin(t *testing.T) {
	// index(1) joining(1) childId(2) childEui64(8) childType(1)
	params := []byte{
		0x00,       // child table index
		0x01,       // joining
		0xF1, 0xA3, // node 0xA3F1
		0xCD, 0xAB, 0xC0, 0x24, 0x00, 0x4B, 0x12, 0x00,
		0x04, // sleepy end device
	}
	ev, err := decodeChildJoin(params)
	if err != nil {
		t.Fatalf("decodeChildJoin: %v", err)
	}
	if ev.NodeID != 0xA3F1 {
		t.Errorf("NodeID = 0x%04X, want 0xA3F1", ev.NodeID)
	}
	if ev.NodeType == nil || *ev.NodeType != NodeSleepyEnd {
		t.Errorf("NodeType = %v, want a sleepy end device", ev.NodeType)
	}
	if ev.Leaving {
		t.Error("joining=1 must not report a departure")
	}
}

func TestDecodeDeviceAnnounce(t *testing.T) {
	// transactionSeq(1) nwkAddr(2) ieeeAddr(8) capability(1)
	payload := []byte{
		0x42,       // ZDO transaction sequence
		0xF1, 0xA3, // node 0xA3F1
		0xCD, 0xAB, 0xC0, 0x24, 0x00, 0x4B, 0x12, 0x00,
		0x80, // allocate address only: battery, sleepy, not a router
	}
	ev, err := decodeDeviceAnnounce(payload)
	if err != nil {
		t.Fatalf("decodeDeviceAnnounce: %v", err)
	}
	if ev.NodeID != 0xA3F1 {
		t.Errorf("NodeID = 0x%04X, want 0xA3F1", ev.NodeID)
	}
	if got, want := ev.IEEE.String(), "00:12:4B:00:24:C0:AB:CD"; got != want {
		t.Errorf("IEEE = %s, want %s", got, want)
	}
	cap := ev.Capability
	if cap == nil {
		t.Fatal("capability flags were not decoded")
	}
	if cap.Mains() {
		t.Error("capability 0x80 has no mains-power bit")
	}
	if !cap.Sleepy() {
		t.Error("capability 0x80 has no rx-on-when-idle bit, so the device is sleepy")
	}
	if cap.Router() {
		t.Error("capability 0x80 has no full-function bit, so the device cannot route")
	}
}

func TestCapabilityMainsRouter(t *testing.T) {
	// A mains-powered router: full function, mains, always listening.
	c := CapFullFunction | CapMainsPowered | CapRxOnWhenIdle
	if !c.Mains() || !c.Router() || c.Sleepy() {
		t.Errorf("capability 0x%02X decoded as %q", uint8(c), c)
	}
}

// buildIncoming assembles an incomingMessageHandler payload in the EZSP v13
// layout this adapter uses.
func buildIncoming(profile, cluster uint16, payload []byte) []byte {
	frame := []byte{
		0x00,                              // EMBER_INCOMING_UNICAST
		byte(profile), byte(profile >> 8), // profile
		byte(cluster), byte(cluster >> 8), // cluster
		0x00, 0x00, // source, destination endpoint
		0x00, 0x00, // APS options
		0x00, 0x00, // group ID
		0x42,       // APS sequence
		0xC8,       // last hop LQI
		0xD8,       // last hop RSSI, -40 dBm
		0xF1, 0xA3, // sender
		0xFF, // binding index
		0xFF, // address table index
	}
	frame = append(frame, byte(len(payload)))
	return append(frame, payload...)
}

func TestDecodeIncomingMessage(t *testing.T) {
	payload := []byte{0x42, 0xF1, 0xA3, 0xCD, 0xAB, 0xC0, 0x24, 0x00, 0x4B, 0x12, 0x00, 0x80}
	msg, err := decodeIncomingMessage(buildIncoming(ProfileZDO, ZDODeviceAnnounce, payload))
	if err != nil {
		t.Fatalf("decodeIncomingMessage: %v", err)
	}
	if msg.APS.Profile != ProfileZDO || msg.APS.Cluster != ZDODeviceAnnounce {
		t.Errorf("APS = %+v, want the ZDO device announce", msg.APS)
	}
	if msg.Sender != 0xA3F1 {
		t.Errorf("Sender = 0x%04X, want 0xA3F1", msg.Sender)
	}
	if msg.LQI != 0xC8 {
		t.Errorf("LQI = %d, want 200", msg.LQI)
	}
	if msg.RSSI != -40 {
		t.Errorf("RSSI = %d dBm, want -40", msg.RSSI)
	}
	if len(msg.Payload) != len(payload) {
		t.Fatalf("payload = % X, want % X", msg.Payload, payload)
	}
}

// TestDecodeIncomingMessageRejectsWrongLayout guards the assumption that this
// firmware uses the pre-v14 layout. EZSP v14 replaces the middle of the frame
// with an EmberRxPacketInfo struct; the length byte then lands elsewhere and
// must be reported rather than parsed as data.
func TestDecodeIncomingMessageRejectsWrongLayout(t *testing.T) {
	frame := buildIncoming(ProfileZDO, ZDODeviceAnnounce, []byte{0x01, 0x02, 0x03})
	frame[incomingPrefixLen] = 0x7F // a length that cannot be right
	if _, err := decodeIncomingMessage(frame); err == nil {
		t.Error("expected an error when the length byte disagrees with the frame")
	}
}

func TestDecodeJoinMessageIgnoresOtherClusters(t *testing.T) {
	// An ordinary ZCL report must not be mistaken for a device arriving.
	frame := buildIncoming(0x0104, 0x0402, []byte{0x18, 0x01, 0x0A})
	ev, err := decodeJoinMessage(Message{ID: FrameIncomingMessage, Callback: true, Params: frame})
	if err != nil {
		t.Fatalf("decodeJoinMessage: %v", err)
	}
	if ev != nil {
		t.Errorf("a temperature report decoded as a join event: %+v", ev)
	}
}
