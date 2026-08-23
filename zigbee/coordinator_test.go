package zigbee

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/internal/zcl"
)

type fakeConnection struct {
	state ezsp.NetworkStatus
	msgs  chan ezsp.Message

	mu             sync.Mutex
	allowJoins     int
	permitCalls    int
	permitDuration uint8
	sent           int
	closeOnce      sync.Once
}

func newFakeConnection() *fakeConnection {
	return &fakeConnection{state: ezsp.NetworkJoined, msgs: make(chan ezsp.Message, 8)}
}

func (f *fakeConnection) NetworkState(context.Context) (ezsp.NetworkStatus, error) {
	return f.state, nil
}

func (f *fakeConnection) Subscribe(func(ezsp.Message) bool, int) (<-chan ezsp.Message, func()) {
	return f.msgs, func() {}
}

func (f *fakeConnection) SendUnicast(context.Context, uint16, ezsp.APSFrame, uint8, []byte) (uint8, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	return 1, nil
}

func (f *fakeConnection) AllowJoins(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowJoins++
	return nil
}

func (f *fakeConnection) PermitJoining(_ context.Context, duration uint8) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permitCalls++
	f.permitDuration = duration
	return nil
}

func (f *fakeConnection) Close() error {
	f.closeOnce.Do(func() { close(f.msgs) })
	return nil
}

func TestPermitJoinConfiguresPolicyAndWindow(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}

	if err := c.PermitJoin(context.Background(), 60*time.Second); err != nil {
		t.Fatalf("PermitJoin: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.allowJoins != 1 {
		t.Errorf("AllowJoins calls = %d, want 1", fake.allowJoins)
	}
	if fake.permitDuration != 60 {
		t.Errorf("PermitJoining duration = %d, want 60", fake.permitDuration)
	}
}

func TestPermitJoinRejectsUnrepresentableDuration(t *testing.T) {
	c := &Coordinator{conn: newFakeConnection(), db: emptyStore(t)}
	for _, duration := range []time.Duration{-time.Second, 1500 * time.Millisecond, 256 * time.Second} {
		if err := c.PermitJoin(context.Background(), duration); err == nil {
			t.Errorf("PermitJoin(%s) succeeded, want an error", duration)
		}
	}
}

func TestPermitJoinCanCloseWithoutEnablingPolicy(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	if err := c.PermitJoin(context.Background(), 0); err != nil {
		t.Fatalf("PermitJoin: %v", err)
	}
	if fake.allowJoins != 0 || fake.permitCalls != 1 || fake.permitDuration != 0 {
		t.Errorf("join calls = allow %d, permit %d/%d", fake.allowJoins, fake.permitCalls, fake.permitDuration)
	}
}

func TestPermitJoinRejectsMissingNetwork(t *testing.T) {
	fake := newFakeConnection()
	fake.state = ezsp.NetworkNone
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	if err := c.PermitJoin(context.Background(), 60*time.Second); err == nil {
		t.Fatal("PermitJoin accepted an adapter without a network")
	}
	if fake.allowJoins != 0 || fake.permitCalls != 0 {
		t.Error("join configuration ran without a network")
	}
}

func TestReadingsDecodesAndEnrichesAttributeReport(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	device, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	device.Name = "living room thermo"
	c := &Coordinator{conn: fake, db: db}

	ctx, cancel := context.WithCancel(context.Background())
	readings, errs := c.Readings(ctx)
	fake.msgs <- incomingMessage(zcl.ClusterTemperature,
		[]byte{0x18, 0xC8, 0x0A, 0x00, 0x00, 0x29, 0x54, 0x0B})

	select {
	case reading := <-readings:
		if reading.IEEE != device.IEEE || reading.DeviceName != device.Name {
			t.Errorf("identity = %q/%q, want %q/%q",
				reading.IEEE, reading.DeviceName, device.IEEE, device.Name)
		}
		if reading.Capability != "temperature" || reading.Value != 29 || reading.Unit != "°C" {
			t.Errorf("reading = %+v, want temperature 29 °C", reading)
		}
		if reading.LQI != 200 || reading.RSSI != -40 {
			t.Errorf("link metrics = %d/%d, want 200/-40", reading.LQI, reading.RSSI)
		}
	case err := <-errs:
		t.Fatalf("Readings error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reading")
	}

	cancel()
	for range readings {
	}
	for err := range errs {
		t.Errorf("unexpected shutdown error: %v", err)
	}
	if got := device.Readings["temperature"].Value; got != 29 {
		t.Errorf("stored temperature = %v, want 29", got)
	}
}

func TestReadingsAllowsOnlyOneProtocolLoop(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	ctx, cancel := context.WithCancel(context.Background())
	first, _ := c.Readings(ctx)
	_, secondErrs := c.Readings(context.Background())
	if err := <-secondErrs; !errors.Is(err, ErrReadingsActive) {
		t.Fatalf("second Readings error = %v, want ErrReadingsActive", err)
	}
	cancel()
	for range first {
	}
}

func TestReadingsAnswersTimeRequests(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	ctx, cancel := context.WithCancel(context.Background())
	readings, errs := c.Readings(ctx)
	fake.msgs <- incomingMessage(zcl.ClusterTime,
		[]byte{0x00, 0xCC, 0x00, 0x00, 0x00, 0x02, 0x00, 0x07, 0x00})

	deadline := time.Now().Add(time.Second)
	for {
		fake.mu.Lock()
		sent := fake.sent
		fake.mu.Unlock()
		if sent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator did not answer the Time-cluster request")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	for range readings {
	}
	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadingsPreservesTrafficBeforeDeviceIdentity(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	c := &Coordinator{conn: fake, db: db}
	ctx, cancel := context.WithCancel(context.Background())
	readings, errs := c.Readings(ctx)
	fake.msgs <- incomingMessage(zcl.ClusterTemperature,
		[]byte{0x18, 0xC8, 0x0A, 0x00, 0x00, 0x29, 0x54, 0x0B})

	reading := <-readings
	if reading.IEEE != "" || reading.DeviceName != "" {
		t.Errorf("unidentified reading exposed placeholder identity: %+v", reading)
	}
	device, ok := db.ByNodeID(0x90CB)
	if !ok || device.Identified() || device.Readings["temperature"].Value != 29 {
		t.Errorf("placeholder device = %+v, present = %t", device, ok)
	}
	cancel()
	for range readings {
	}
	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTimeAttributeLocalTimeSupportsNegativeOffset(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	now := time.Date(2026, 8, 16, 12, 30, 0, 0, zone)
	rec := timeAttribute(attrLocalTime, now)
	want := uint64(time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC).Sub(zigbeeEpoch) / time.Second)
	if rec.Value != want {
		t.Fatalf("LocalTime = %v, want %d", rec.Value, want)
	}
}

func TestTimeAttributeClampsBeforeZigbeeEpoch(t *testing.T) {
	rec := timeAttribute(attrTime, zigbeeEpoch.Add(-time.Hour))
	if rec.Value != uint64(0) {
		t.Fatalf("Time = %v, want 0", rec.Value)
	}
}

func emptyStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func incomingMessage(cluster uint16, payload []byte) ezsp.Message {
	params := []byte{
		0x00,       // incoming unicast
		0x04, 0x01, // Home Automation profile
		byte(cluster), byte(cluster >> 8),
		0x01, 0x01, // source and destination endpoints
		0x00, 0x00, // APS options
		0x00, 0x00, // group ID
		0x42,       // APS sequence
		0xC8,       // LQI
		0xD8,       // RSSI (-40)
		0xCB, 0x90, // sender
		0xFF, // binding index
		0xFF, // address table index
		byte(len(payload)),
	}
	params = append(params, payload...)
	return ezsp.Message{ID: ezsp.FrameIncomingMessage, Callback: true, Params: params}
}

// Keep the exported surface usable without importing an internal package.
func TestPublicTypesMarshal(t *testing.T) {
	data, err := json.Marshal(Reading{Capability: "temperature", Value: 21.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty JSON")
	}
}
