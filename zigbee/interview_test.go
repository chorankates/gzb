package zigbee

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
)

// The fixture below answers as a battery-powered temperature and humidity
// sensor: one endpoint, a handful of input clusters, and a Basic cluster that
// names it. Response clusters are spelled out as literals rather than derived
// from the code under test, so a change to the request-to-response mapping has
// to disagree with the tests.
const (
	sensorNode = 0x90CB

	clusterNodeDescriptorRsp   = 0x8002
	clusterPowerDescriptorRsp  = 0x8003
	clusterSimpleDescriptorRsp = 0x8004
	clusterActiveEndpointsRsp  = 0x8005
)

// zdoReply builds the message a device sends in answer to a ZDO request. A
// real device echoes the transaction sequence it was asked with, and this one
// does too, because that echo is what the matching depends on.
func zdoReply(node, cluster uint16, seq uint8, body ...byte) ezsp.IncomingMessage {
	return ezsp.IncomingMessage{
		APS:     ezsp.APSFrame{Profile: ezsp.ProfileZDO, Cluster: cluster},
		Sender:  node,
		LQI:     200,
		RSSI:    -40,
		Payload: append([]byte{seq}, body...),
	}
}

// sensorResponder answers as the sensor at sensorNode.
func sensorResponder(skip map[uint16]bool) func(uint16, ezsp.APSFrame, []byte) (ezsp.IncomingMessage, bool) {
	const lo, hi = byte(sensorNode & 0xFF), byte(sensorNode >> 8)

	return func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		if dest != sensorNode || skip[aps.Cluster] || len(payload) < 2 {
			return ezsp.IncomingMessage{}, false
		}

		if aps.Profile == ezsp.ProfileZDO {
			seq := payload[0]
			switch aps.Cluster {
			case 0x0002:
				return zdoReply(sensorNode, clusterNodeDescriptorRsp, seq,
					0x00, lo, hi, // success, for this address
					0x02,       // logical type 2: end device
					0x40,       // 2.4 GHz
					0x80,       // MAC capability: battery, sleepy
					0x86, 0x12, // manufacturer code 0x1286
					0x52, 0x52, 0x00, 0x00, 0x00, 0x52, 0x00, 0x00), true
			case 0x0003:
				return zdoReply(sensorNode, clusterPowerDescriptorRsp, seq,
					0x00, lo, hi,
					0x41, 0xC4), true // disposable battery, full
			case 0x0005:
				return zdoReply(sensorNode, clusterActiveEndpointsRsp, seq,
					0x00, lo, hi,
					0x01, // one endpoint
					0x01), true
			case 0x0004:
				if len(payload) < 4 || payload[3] != 0x01 {
					// Asking for an endpoint the device does not have.
					return zdoReply(sensorNode, clusterSimpleDescriptorRsp, seq,
						0x83, lo, hi), true
				}
				return zdoReply(sensorNode, clusterSimpleDescriptorRsp, seq,
					0x00, lo, hi,
					0x14,       // descriptor length
					0x01,       // endpoint 1
					0x04, 0x01, // Home Automation profile
					0x02, 0x03, // temperature sensor
					0x00,                                           // version
					0x04,                                           // four input clusters
					0x00, 0x00, 0x01, 0x00, 0x03, 0x00, 0x02, 0x04, // Basic, Power, Identify, Temperature
					0x01,             // one output cluster
					0x19, 0x00), true // OTA Upgrade
			}
			return ezsp.IncomingMessage{}, false
		}

		// A ZCL read of the Basic cluster: the sequence lives behind the frame
		// control byte rather than first.
		if aps.Cluster != 0x0000 {
			return ezsp.IncomingMessage{}, false
		}
		return ezsp.IncomingMessage{
			APS:    ezsp.APSFrame{Profile: ezsp.ProfileHomeAutomation, Cluster: 0x0000},
			Sender: sensorNode,
			Payload: []byte{
				0x18,       // server to client, no default response
				payload[1], // echo the request's sequence
				0x01,       // read attributes response
				0x04, 0x00, 0x00, 0x42, 0x07, 'e', 'W', 'e', 'L', 'i', 'n', 'k',
				0x05, 0x00, 0x00, 0x42, 0x04, 'T', 'H', '0', '1',
			},
		}, true
	}
}

func sensorCoordinator(t *testing.T, skip map[uint16]bool) (*Coordinator, *fakeConnection, *store.Store) {
	t.Helper()
	fake := newFakeConnection()
	fake.responder = sensorResponder(skip)
	db := emptyStore(t)
	return &Coordinator{conn: fake, db: db}, fake, db
}

func TestInterviewAsksEveryQuestionAndRecordsTheAnswer(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())
	device.Name = "bedroom thermo"

	var steps []string
	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout:  time.Second,
		Progress: func(step string) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if len(description.Problems) != 0 {
		t.Fatalf("problems from a device that answered everything: %v", description.Problems)
	}

	if description.IEEE != device.IEEE || description.DeviceName != "bedroom thermo" {
		t.Errorf("identity = %q/%q, want %q/%q",
			description.IEEE, description.DeviceName, device.IEEE, "bedroom thermo")
	}
	if description.Manufacturer != "eWeLink" || description.Model != "TH01" {
		t.Errorf("model = %q %q, want eWeLink TH01", description.Manufacturer, description.Model)
	}

	nd := description.Node
	if nd == nil {
		t.Fatal("no node descriptor")
	}
	if nd.LogicalType != "end device" || nd.Mains || !nd.Sleepy {
		t.Errorf("node = %+v, want a sleepy battery end device", *nd)
	}
	if nd.Capability != 0x80 || nd.ManufacturerCode != 0x1286 {
		t.Errorf("capability 0x%02X, manufacturer 0x%04X; want 0x80, 0x1286",
			nd.Capability, nd.ManufacturerCode)
	}
	if pd := description.Power; pd == nil || pd.Source != "disposable battery" || pd.Level != 12 {
		t.Errorf("power = %+v, want a full disposable battery", pd)
	}

	if len(description.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want one", description.Endpoints)
	}
	ep := description.Endpoints[0]
	if ep.ID != 1 || ep.Profile != 0x0104 || ep.DeviceID != 0x0302 {
		t.Errorf("endpoint = %+v, want 1 / 0x0104 / 0x0302", ep)
	}
	if len(ep.Input) != 4 || ep.Input[3].ID != 0x0402 || ep.Input[3].Name != "temperature" {
		t.Errorf("input clusters = %+v, want four ending in temperature", ep.Input)
	}
	if len(ep.Output) != 1 || ep.Output[0].ID != 0x0019 {
		t.Errorf("output clusters = %+v, want [0x0019]", ep.Output)
	}

	// Identity is asked for first, and answered, so it is not asked again once
	// the endpoint list arrives.
	wantSteps := []string{
		"manufacturer and model", "node descriptor", "power descriptor",
		"active endpoints", "endpoint 1 descriptor",
	}
	if strings.Join(steps, ",") != strings.Join(wantSteps, ",") {
		t.Errorf("progress = %v, want %v", steps, wantSteps)
	}
	// Nothing was inherited: this device answered every question itself.
	if description.InheritedFrom != "" || device.InheritedFrom != "" {
		t.Errorf("inherited from %q/%q, want the device's own answers",
			description.InheritedFrom, device.InheritedFrom)
	}

	// What was learned belongs in the registry, so later output can name the
	// device by what it is rather than by its address.
	if device.Model != "TH01" || device.Manufacturer != "eWeLink" {
		t.Errorf("registry model = %q %q, want eWeLink TH01", device.Manufacturer, device.Model)
	}
	if device.NodeType != "end device" || device.PowerSource != "disposable battery" {
		t.Errorf("registry node = %q, power = %q", device.NodeType, device.PowerSource)
	}
	if device.Capability == nil || *device.Capability != 0x80 {
		t.Errorf("registry capability = %v, want 0x80", device.Capability)
	}
	if len(device.Endpoints) != 1 || len(device.Endpoints[0].Input) != 4 {
		t.Errorf("registry endpoints = %+v", device.Endpoints)
	}
	if device.Interviewed.IsZero() {
		t.Error("registry did not record when the interview happened")
	}
	// A name a person chose outranks anything the interview learned.
	if device.Describe() != "bedroom thermo" {
		t.Errorf("device name = %q, want the chosen name", device.Describe())
	}

	// ZDO travels on endpoint 0 at both ends; the Basic read does not.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, req := range fake.requests {
		if req.aps.Profile != ezsp.ProfileZDO {
			continue
		}
		if req.aps.SourceEP != 0 || req.aps.DestEP != 0 {
			t.Errorf("ZDO request on endpoints %d/%d, want 0/0", req.aps.SourceEP, req.aps.DestEP)
		}
		if req.aps.Options&ezsp.APSOptionRetry == 0 {
			t.Error("ZDO request sent without APS retries")
		}
	}
}

// A sleepy device can answer some questions and sleep through others. What it
// did answer is worth keeping.
func TestInterviewContinuesAfterAStepFails(t *testing.T) {
	c, _, _ := sensorCoordinator(t, map[uint16]bool{0x0003: true})

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.Power != nil {
		t.Error("a power descriptor appeared from a device that never sent one")
	}
	if len(description.Problems) != 1 {
		t.Fatalf("problems = %v, want exactly the unanswered power descriptor", description.Problems)
	}
	if description.Node == nil || len(description.Endpoints) != 1 || description.Model != "TH01" {
		t.Error("one unanswered question lost the rest of the interview")
	}
}

// Without the endpoint list there is nothing to ask about, but what the device
// already said still stands.
func TestInterviewKeepsDescriptorsWhenEndpointsAreUnavailable(t *testing.T) {
	c, _, _ := sensorCoordinator(t, map[uint16]bool{0x0005: true})

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.Node == nil || description.Power == nil {
		t.Error("descriptors were dropped when the endpoint list failed")
	}
	if len(description.Endpoints) != 0 || len(description.Problems) != 1 {
		t.Errorf("endpoints = %+v, problems = %v", description.Endpoints, description.Problems)
	}
}

// A reply that carries the right sequence but comes from somewhere else is
// answering somebody else's question.
func TestZDOQueryIgnoresAnotherDevicesReply(t *testing.T) {
	fake := newFakeConnection()
	fake.responder = func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		reply := zdoReply(sensorNode, clusterNodeDescriptorRsp, payload[0],
			0x00, 0xCB, 0x90, 0x02, 0x40, 0x80, 0x86, 0x12, 0x52, 0x52, 0x00, 0x00, 0x00, 0x52, 0x00, 0x00)
		reply.Sender = 0x1234 // a different device
		return reply, true
	}
	c := &Coordinator{conn: fake, db: emptyStore(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.NodeDescriptor(ctx, sensorNode); err == nil {
		t.Fatal("a reply from 0x1234 was accepted as an answer from 0x90CB")
	}
}

// Sequence numbers are how a reply is matched to its question, so a stale one
// must not be mistaken for the current answer.
func TestZDOQueryIgnoresAStaleSequence(t *testing.T) {
	fake := newFakeConnection()
	fake.responder = func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		return zdoReply(sensorNode, clusterNodeDescriptorRsp, payload[0]-1,
			0x00, 0xCB, 0x90, 0x02, 0x40, 0x80, 0x86, 0x12, 0x52, 0x52, 0x00, 0x00, 0x00, 0x52, 0x00, 0x00), true
	}
	c := &Coordinator{conn: fake, db: emptyStore(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.NodeDescriptor(ctx, sensorNode); err == nil {
		t.Fatal("a reply to an earlier question was accepted")
	}
}

// Every question in an interview must carry its own transaction sequence, or
// the answers cannot be told apart.
func TestInterviewUsesADistinctSequencePerQuery(t *testing.T) {
	c, fake, _ := sensorCoordinator(t, nil)
	if _, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("Interview: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	seen := map[uint8]bool{}
	for _, req := range fake.requests {
		seq := req.payload[0]
		if req.aps.Profile != ezsp.ProfileZDO {
			seq = req.payload[1] // ZCL keeps its sequence behind the frame control
		}
		if seen[seq] {
			t.Errorf("transaction sequence 0x%02X was used twice", seq)
		}
		seen[seq] = true
	}
	if len(seen) != len(fake.requests) || len(fake.requests) != 5 {
		t.Errorf("%d requests, %d distinct sequences", len(fake.requests), len(seen))
	}
}

// A device answering "that endpoint is not active" is a clear answer, and the
// error should say which endpoint rather than blaming the device.
func TestSimpleDescriptorReportsAnInactiveEndpoint(t *testing.T) {
	c, _, _ := sensorCoordinator(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.SimpleDescriptor(ctx, sensorNode, 9)
	if err == nil {
		t.Fatal("endpoint 9 returned a descriptor")
	}
	if !strings.Contains(err.Error(), "endpoint 9") || !strings.Contains(err.Error(), "not active") {
		t.Errorf("error = %q, want it to name endpoint 9 and the status", err)
	}
}

// Every query would otherwise sit waiting for its full timeout on an adapter
// that has no network at all.
func TestInterviewRequiresANetwork(t *testing.T) {
	c, fake, _ := sensorCoordinator(t, nil)
	fake.state = ezsp.NetworkNone

	if _, err := c.Interview(context.Background(), sensorNode, InterviewOptions{}); err == nil {
		t.Fatal("Interview ran against an adapter with no network")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("%d requests went out without a network", len(fake.requests))
	}
}

// An interview can be aimed at any address, including the coordinator itself.
// Those answers must not invent a registry entry with no identity behind it.
func TestInterviewLeavesUnknownAddressesOutOfTheRegistry(t *testing.T) {
	c, _, db := sensorCoordinator(t, nil)

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.Node == nil {
		t.Fatal("the interview learned nothing")
	}
	if description.IEEE != "" || description.DeviceName != "" {
		t.Errorf("identity = %q/%q, want none for an unregistered device",
			description.IEEE, description.DeviceName)
	}
	if len(db.List()) != 0 {
		t.Errorf("registry gained %d devices from interviewing an unknown address", len(db.List()))
	}
}

// Traffic can reach the registry before a join callback supplies a stable
// IEEE address, leaving a record keyed by network address. Interviewing one of
// those may fill in the model, which is a name worth reporting — but the
// placeholder address behind it never is.
func TestInterviewNamesAPlaceholderRecordWithoutExposingIt(t *testing.T) {
	c, _, db := sensorCoordinator(t, nil)
	placeholder, _ := db.ObserveNodeID(sensorNode, time.Now())
	if placeholder.Identified() {
		t.Fatalf("fixture device %q is not a placeholder", placeholder.IEEE)
	}

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.IEEE != "" {
		t.Errorf("IEEE = %q, want none: a placeholder address is not an identity", description.IEEE)
	}
	if description.DeviceName != "eWeLink TH01" {
		t.Errorf("device name = %q, want the model the interview just learned", description.DeviceName)
	}
	if placeholder.Model != "TH01" {
		t.Errorf("registry model = %q, want TH01", placeholder.Model)
	}
}

// A device that answers nothing must not be recorded as freshly seen.
func TestInterviewOfASilentDeviceRecordsNothing(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())
	device.LastSeen = time.Time{}
	c := &Coordinator{conn: fake, db: db}

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if len(description.Problems) == 0 {
		t.Fatal("a device that answered nothing reported no problems")
	}
	if !device.LastSeen.IsZero() || !device.Interviewed.IsZero() {
		t.Errorf("silence was recorded as contact: seen %v, interviewed %v",
			device.LastSeen, device.Interviewed)
	}
	// Identity is still worth reporting: it is what the caller asked about.
	if description.IEEE != device.IEEE {
		t.Errorf("IEEE = %q, want %q", description.IEEE, device.IEEE)
	}
}

func TestInterviewAfterCloseIsRefused(t *testing.T) {
	c, _, _ := sensorCoordinator(t, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Interview(context.Background(), sensorNode, InterviewOptions{}); err == nil {
		t.Fatal("Interview ran on a closed coordinator")
	}
	if _, err := c.NodeDescriptor(context.Background(), sensorNode); err == nil {
		t.Fatal("NodeDescriptor ran on a closed coordinator")
	}
}

// Discovery must not need the readings loop to be stopped: the moment worth
// interviewing a sleepy device is while it is awake and reporting.
func TestInterviewRunsAlongsideAReadingsLoop(t *testing.T) {
	c, _, _ := sensorCoordinator(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	readings, errs := c.Readings(ctx)

	description, err := c.Interview(ctx, sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview during a readings loop: %v", err)
	}
	if description.Node == nil || len(description.Endpoints) != 1 {
		t.Errorf("interview came back incomplete: %+v", description)
	}

	cancel()
	for range readings {
	}
	for err := range errs {
		t.Errorf("unexpected readings error: %v", err)
	}
}

// interviewedPeer adds a device that has already answered for itself: the
// answer key an identical sibling is entitled to inherit from. It is deliberately
// the same make and model the fixture device reports.
func interviewedPeer(t *testing.T, db *store.Store, ieee, name string) *store.Device {
	t.Helper()
	device, _ := db.Observe(ieee, 0xAAAA, time.Now())
	device.Name = name
	device.Manufacturer = "eWeLink"
	device.Model = "TH01"
	device.NodeType = "end device"
	capability := uint8(0x80)
	device.Capability = &capability
	device.PowerSource = "disposable battery"
	device.Endpoints = []store.Endpoint{{
		ID: 1, Profile: 0x0104, Device: 0x0302,
		Input:  []uint16{0x0000, 0x0001, 0x0003, 0x0402},
		Output: []uint16{0x0019},
	}}
	device.Interviewed = time.Now()
	return device
}

func requestCount(t *testing.T, fake *fakeConnection) int {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.requests)
}

// A network is usually several of the same thing bought at the same time. Once
// one unit of a model has been interviewed, the next only has to say what it
// is — the rest of the answers are already on file and cannot differ.
func TestInterviewInheritsStructureFromAnIdenticalDevice(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	peer := interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())

	var steps []string
	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout:  time.Second,
		Progress: func(step string) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if len(description.Problems) != 0 {
		t.Fatalf("problems = %v, want none", description.Problems)
	}

	// One question asked; the other four were answered by the sibling.
	if len(steps) != 1 || steps[0] != "manufacturer and model" {
		t.Errorf("steps = %v, want only the identifying read", steps)
	}
	if got := requestCount(t, fake); got != 1 {
		t.Errorf("%d requests, want 1: the rest were already answered by %s", got, peer.Name)
	}

	// The description says whose answers these are.
	if description.InheritedFrom != peer.IEEE || description.InheritedFromName != "kitchen thermo" {
		t.Errorf("inherited from %q/%q, want %q/kitchen thermo",
			description.InheritedFrom, description.InheritedFromName, peer.IEEE)
	}
	// The model itself was read from this device, not assumed.
	if description.Manufacturer != "eWeLink" || description.Model != "TH01" {
		t.Errorf("model = %q %q, want eWeLink TH01", description.Manufacturer, description.Model)
	}

	if len(description.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want the one the sibling has", description.Endpoints)
	}
	ep := description.Endpoints[0]
	if ep.ID != 1 || ep.Profile != 0x0104 || ep.DeviceID != 0x0302 {
		t.Errorf("endpoint = %+v, want 1 / 0x0104 / 0x0302", ep)
	}
	if len(ep.Input) != 4 || ep.Input[3].ID != 0x0402 || ep.Input[3].Name != "temperature" {
		t.Errorf("input clusters = %+v, want four ending in temperature", ep.Input)
	}
	if len(ep.Output) != 1 || ep.Output[0].ID != 0x0019 {
		t.Errorf("output clusters = %+v, want [0x0019]", ep.Output)
	}

	nd := description.Node
	if nd == nil || nd.LogicalType != "end device" || nd.Mains || !nd.Sleepy {
		t.Errorf("node = %+v, want a sleepy battery end device", nd)
	}
	// The registry never kept the manufacturer code, so nothing may appear here.
	if nd != nil && nd.ManufacturerCode != 0 {
		t.Errorf("manufacturer code = 0x%04X, want none: it was never recorded", nd.ManufacturerCode)
	}
	pd := description.Power
	if pd == nil || pd.Source != "disposable battery" {
		t.Errorf("power = %+v, want the model's supply", pd)
	}
	// Charge is this device's own business and belongs to no model.
	if pd != nil && pd.Level != 0 {
		t.Errorf("power level = %d, want none: a sibling's charge is not this one's", pd.Level)
	}

	// The registry records the structure and where it came from together.
	if device.InheritedFrom != peer.IEEE {
		t.Errorf("registry inherited_from = %q, want %q", device.InheritedFrom, peer.IEEE)
	}
	if len(device.Endpoints) != 1 || len(device.Endpoints[0].Input) != 4 {
		t.Errorf("registry endpoints = %+v", device.Endpoints)
	}
	if device.Interviewed.IsZero() || device.NodeType != "end device" || device.PowerSource != "disposable battery" {
		t.Errorf("registry record = %+v, want the inherited structure", device)
	}
	// The point of all this: commands can now find the cluster without guessing.
	if endpoint, ok := device.HasCluster(0x0402); !ok || endpoint != 1 {
		t.Errorf("HasCluster(temperature) = %d, %t; want 1, true", endpoint, ok)
	}
}

// Inheriting from an inherited record would let one bad match copy itself
// across the registry, leaving nothing that points at a device actually asked.
func TestInterviewWillNotInheritFromAnInheritedRecord(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	peer := interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")
	peer.InheritedFrom = "A4:C1:38:18:BB:BB:BB:BB"
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.InheritedFrom != "" {
		t.Errorf("inherited from %q, which had itself inherited", description.InheritedFrom)
	}
	if got := requestCount(t, fake); got != 5 {
		t.Errorf("%d requests, want the full 5", got)
	}
	if device.InheritedFrom != "" || device.Interviewed.IsZero() {
		t.Errorf("registry record = %+v, want its own interview", device)
	}
}

// An observation outranks a deduction: a device that has answered for itself is
// not improved by being given a sibling's answers instead.
func TestInterviewKeepsADevicesOwnAnswersOverASiblings(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())
	device.Interviewed = time.Now()
	device.Endpoints = []store.Endpoint{{ID: 1, Input: []uint16{0x0000}}}

	if _, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if got := requestCount(t, fake); got != 5 {
		t.Errorf("%d requests, want the full 5: this device had already answered", got)
	}
	if device.InheritedFrom != "" {
		t.Errorf("a device's own answers were replaced by %q's", device.InheritedFrom)
	}
	if len(device.Endpoints) != 1 || len(device.Endpoints[0].Input) != 4 {
		t.Errorf("registry endpoints = %+v, want the fresh interview's", device.Endpoints)
	}
}

// Full is how an inherited record is promoted to one the device gave itself.
func TestFullInterviewReplacesAnInheritedRecord(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	peer := interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())
	device.InheritedFrom = peer.IEEE
	device.Interviewed = time.Now()
	device.Endpoints = []store.Endpoint{{ID: 1, Input: []uint16{0x0000}}}

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout: time.Second,
		Full:    true,
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if got := requestCount(t, fake); got != 5 {
		t.Errorf("%d requests, want the full 5", got)
	}
	if description.InheritedFrom != "" {
		t.Errorf("a full interview reported inheritance from %q", description.InheritedFrom)
	}
	if device.InheritedFrom != "" {
		t.Errorf("registry inherited_from = %q, want it cleared by the real interview", device.InheritedFrom)
	}
	if description.Node == nil || description.Node.ManufacturerCode != 0x1286 {
		t.Error("a full interview did not ask the device for its node descriptor")
	}
}

// The first read of the Basic cluster has to guess an endpoint, because the
// endpoint list is exactly what has not been asked for yet. A wrong guess costs
// one round trip and is not a fault of the device, so the interview asks again
// once the endpoint list says where the cluster actually lives — and reports no
// problem for the guess.
func TestInterviewAsksForTheModelAgainWhenTheGuessedEndpointIsWrong(t *testing.T) {
	fake := newFakeConnection()
	sensor := sensorResponder(nil)
	// The responder is called on the interview's own goroutine, in order, so
	// plain state here is enough to answer differently before and after.
	endpointsKnown := false
	fake.responder = func(dest uint16, aps ezsp.APSFrame, payload []byte) (ezsp.IncomingMessage, bool) {
		if aps.Profile != ezsp.ProfileZDO && !endpointsKnown {
			return ezsp.IncomingMessage{}, false // the guessed endpoint was wrong
		}
		if aps.Profile == ezsp.ProfileZDO && aps.Cluster == 0x0005 {
			endpointsKnown = true
		}
		return sensor(dest, aps, payload)
	}
	db := emptyStore(t)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())
	c := &Coordinator{conn: fake, db: db}

	var steps []string
	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{
		Timeout:  20 * time.Millisecond,
		Progress: func(step string) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.Model != "TH01" {
		t.Errorf("model = %q, want TH01 from the second read", description.Model)
	}
	if len(description.Problems) != 0 {
		t.Errorf("problems = %v, want none: a guess that missed is not a fault", description.Problems)
	}
	wantSteps := []string{
		"manufacturer and model", "node descriptor", "power descriptor",
		"active endpoints", "endpoint 1 descriptor", "manufacturer and model",
	}
	if strings.Join(steps, ",") != strings.Join(wantSteps, ",") {
		t.Errorf("progress = %v, want %v", steps, wantSteps)
	}
	if device.Model != "TH01" {
		t.Errorf("registry model = %q, want TH01", device.Model)
	}
}

// An interview aimed at an address the registry does not know has no record to
// hang an inheritance on, so it asks the device everything.
func TestInterviewOfAnUnknownAddressDoesNotInherit(t *testing.T) {
	c, fake, db := sensorCoordinator(t, nil)
	interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.InheritedFrom != "" {
		t.Errorf("inherited from %q with no record to record it on", description.InheritedFrom)
	}
	if got := requestCount(t, fake); got != 5 {
		t.Errorf("%d requests, want the full 5", got)
	}
}

// An interview is expensive, and whatever ends the run — a dead adapter, a
// second Ctrl-C, a kill — must not be able to take its answers with it. The
// registry is on disk before Interview returns, with no Close involved.
func TestInterviewSavesTheRegistryBeforeReturning(t *testing.T) {
	c, _, db := sensorCoordinator(t, nil)
	db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())

	if _, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("Interview: %v", err)
	}

	// Re-read the file rather than the store that wrote it, which is the only
	// way to tell a saved answer from one still in memory.
	reopened, err := store.Open(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.Get("A4:C1:38:18:56:07:FF:FF")
	if !ok {
		t.Fatal("the interviewed device is not in the saved registry")
	}
	if saved.Model != "TH01" {
		t.Errorf("saved model = %q, want TH01", saved.Model)
	}
	if saved.Interviewed.IsZero() {
		t.Error("saved record has no interview timestamp, so --all would ask it again")
	}
	if len(saved.Endpoints) != 1 {
		t.Errorf("saved endpoints = %d, want 1", len(saved.Endpoints))
	}
}

// The same has to hold for an inherited record, which returns early and so
// takes a different path out of Interview.
func TestInheritedInterviewIsSavedToo(t *testing.T) {
	c, _, db := sensorCoordinator(t, nil)
	peer := interviewedPeer(t, db, "A4:C1:38:18:AA:AA:AA:AA", "kitchen thermo")
	db.Observe("A4:C1:38:18:56:07:FF:FF", sensorNode, time.Now())

	description, err := c.Interview(context.Background(), sensorNode, InterviewOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Interview: %v", err)
	}
	if description.InheritedFrom == "" {
		t.Fatal("nothing was inherited, so this test proves nothing")
	}

	reopened, err := store.Open(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.Get("A4:C1:38:18:56:07:FF:FF")
	if !ok {
		t.Fatal("the inheriting device is not in the saved registry")
	}
	if saved.InheritedFrom != peer.IEEE {
		t.Errorf("saved inherited_from = %q, want %q", saved.InheritedFrom, peer.IEEE)
	}
	if saved.Interviewed.IsZero() {
		t.Error("saved record has no interview timestamp, so --all would ask it again")
	}
}
