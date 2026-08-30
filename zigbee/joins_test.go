package zigbee

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
)

var testIEEE = ezsp.EUI64{0xA4, 0xC1, 0x38, 0x18, 0x56, 0x07, 0xFF, 0xFF}

func nextJoinEvent(t *testing.T, events <-chan JoinEvent) JoinEvent {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a join event")
		return JoinEvent{}
	}
}

func TestJoinsRecordsArrivalAndSaves(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	c := &Coordinator{conn: fake, db: db}
	events, errs, cancel := c.Joins(8)
	defer cancel()

	update := ezsp.UpdateUnsecuredJoin
	decision := ezsp.JoinSendKeyInClear
	parent := uint16(0x0000)
	fake.joinEvents <- ezsp.JoinEvent{
		Kind:     ezsp.EventTrustCenter,
		At:       time.Now(),
		NodeID:   0x90CB,
		IEEE:     testIEEE,
		Parent:   &parent,
		Update:   &update,
		Decision: &decision,
	}

	ev := nextJoinEvent(t, events)
	if ev.Kind != EventTrustCenter || ev.NodeID != 0x90CB || ev.IEEE != testIEEE.String() {
		t.Errorf("event = %+v, want trust-centre 0x90CB %s", ev, testIEEE)
	}
	if !ev.New || ev.Leaving {
		t.Errorf("event new/leaving = %t/%t, want true/false", ev.New, ev.Leaving)
	}
	if want := "joined, accepted (key sent in the clear), via 0x0000"; ev.Description != want {
		t.Errorf("description = %q, want %q", ev.Description, want)
	}

	// The arrival must be on disk already, not waiting for a clean exit.
	reopened, err := store.Open(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := reopened.Get(testIEEE.String())
	if !ok || d.NodeID != 0x90CB || d.Parent == nil || *d.Parent != 0x0000 {
		t.Errorf("saved device = %+v, present = %t", d, ok)
	}

	select {
	case err := <-errs:
		t.Errorf("unexpected error: %v", err)
	default:
	}
}

func TestJoinsMergesCallbacksIntoOneRecord(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	c := &Coordinator{conn: fake, db: db}
	events, _, cancel := c.Joins(8)
	defer cancel()

	nodeType := ezsp.NodeSleepyEnd
	capability := ezsp.Capability(0x80) // battery, sleepy, end device
	fake.joinEvents <- ezsp.JoinEvent{Kind: ezsp.EventTrustCenter, At: time.Now(), NodeID: 0x90CB, IEEE: testIEEE}
	fake.joinEvents <- ezsp.JoinEvent{Kind: ezsp.EventChild, At: time.Now(), NodeID: 0x90CB, IEEE: testIEEE, NodeType: &nodeType}
	fake.joinEvents <- ezsp.JoinEvent{Kind: ezsp.EventAnnounce, At: time.Now(), NodeID: 0x90CB, IEEE: testIEEE, Capability: &capability}

	first := nextJoinEvent(t, events)
	second := nextJoinEvent(t, events)
	third := nextJoinEvent(t, events)
	if !first.New || second.New || third.New {
		t.Errorf("new flags = %t/%t/%t, want true/false/false", first.New, second.New, third.New)
	}
	if second.Kind != EventChild || second.Description != "sleepy end device" {
		t.Errorf("child event = %+v", second)
	}
	if third.Kind != EventAnnounce || third.Description != "end device, battery, sleepy" {
		t.Errorf("announce event = %+v", third)
	}

	d, ok := db.Get(testIEEE.String())
	if !ok {
		t.Fatal("device not recorded")
	}
	if d.NodeType != "sleepy end device" || d.Capability == nil || *d.Capability != 0x80 {
		t.Errorf("merged record = %+v", d)
	}
}

func TestJoinsNamesAReturningDevice(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	if _, ok := db.Observe(testIEEE.String(), 0x90CB, time.Now()); !ok {
		t.Fatal("expected a fresh device")
	}
	if _, err := db.SetName(testIEEE.String(), "living room thermo"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{conn: fake, db: db}
	events, _, cancel := c.Joins(8)
	defer cancel()

	fake.joinEvents <- ezsp.JoinEvent{Kind: ezsp.EventTrustCenter, At: time.Now(), NodeID: 0x4BCD, IEEE: testIEEE}
	ev := nextJoinEvent(t, events)
	if ev.New || ev.DeviceName != "living room thermo" {
		t.Errorf("event = %+v, want known device named living room thermo", ev)
	}
	if d, _ := db.Get(testIEEE.String()); d.NodeID != 0x4BCD {
		t.Errorf("rejoin did not update the network address: %+v", d)
	}
}

func TestJoinsLooksUpButDoesNotRecordADeparture(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	c := &Coordinator{conn: fake, db: db}
	events, _, cancel := c.Joins(8)
	defer cancel()

	update := ezsp.UpdateDeviceLeft
	decision := ezsp.JoinNoAction
	fake.joinEvents <- ezsp.JoinEvent{
		Kind: ezsp.EventTrustCenter, At: time.Now(), NodeID: 0x90CB, IEEE: testIEEE,
		Update: &update, Decision: &decision, Leaving: true,
	}
	ev := nextJoinEvent(t, events)
	if !ev.Leaving || ev.New {
		t.Errorf("event = %+v, want a departure of an unknown device", ev)
	}
	if _, ok := db.Get(testIEEE.String()); ok {
		t.Error("a departure invented a registry record")
	}
}

func TestJoinsReportsWindowStateChanges(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	events, _, cancel := c.Joins(8)
	defer cancel()

	fake.msgs <- ezsp.Message{ID: ezsp.FrameStackStatusHandler, Callback: true, Params: []byte{byte(ezsp.StatusNetworkOpened)}}
	fake.msgs <- ezsp.Message{ID: ezsp.FrameStackStatusHandler, Callback: true, Params: []byte{byte(ezsp.StatusNetworkClosed)}}

	opened := nextJoinEvent(t, events)
	closed := nextJoinEvent(t, events)
	if opened.Kind != EventWindowOpened || opened.Description == "" {
		t.Errorf("opened event = %+v", opened)
	}
	if closed.Kind != EventWindowClosed || closed.Description == "" {
		t.Errorf("closed event = %+v", closed)
	}
	if opened.NodeID != 0 || opened.IEEE != "" {
		t.Errorf("window event carries a device identity: %+v", opened)
	}
}

func TestJoinsForwardsDecodeErrors(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	events, errs, cancel := c.Joins(8)
	defer cancel()

	fake.joinErrs <- errors.New("ezsp: trustCenterJoinHandler: short read")
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("nil error delivered")
		}
	case ev := <-events:
		t.Fatalf("error arrived as an event: %+v", ev)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the error")
	}
}

func TestJoinsRefusesAClosedCoordinator(t *testing.T) {
	c := &Coordinator{conn: newFakeConnection(), db: emptyStore(t), closed: true}
	events, errs, cancel := c.Joins(8)
	defer cancel()
	if err := <-errs; !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want ErrClosed", err)
	}
	if _, ok := <-events; ok {
		t.Fatal("events channel open on a closed coordinator")
	}
}

func TestJoinsEndsWhenTheConnectionCloses(t *testing.T) {
	fake := newFakeConnection()
	c := &Coordinator{conn: fake, db: emptyStore(t)}
	events, errs, cancel := c.Joins(8)
	defer cancel()

	fake.Close()
	for range events {
	}
	for err := range errs {
		t.Errorf("unexpected error during shutdown: %v", err)
	}
}

// TestJoinsIsSafeAlongsideReadings exists for the race detector: a pairing
// watch, the readings loop and the registry API all run at once, exactly the
// way an application holding the port would run them.
func TestJoinsIsSafeAlongsideReadings(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	db.Observe(testIEEE.String(), 0x90CB, time.Now())
	c := &Coordinator{conn: fake, db: db}

	events, joinErrs, cancelJoins := c.Joins(64)
	defer cancelJoins()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 25 {
			fake.joinEvents <- ezsp.JoinEvent{Kind: ezsp.EventTrustCenter, At: time.Now(), NodeID: 0x90CB, IEEE: testIEEE}
			c.Devices()
			if _, err := c.SetName(testIEEE.String(), fmt.Sprintf("thermo %d", i)); err != nil {
				t.Errorf("SetName: %v", err)
			}
			if _, err := c.Device("0x90CB"); err != nil {
				t.Errorf("Device: %v", err)
			}
		}
	}()

	var got int
	for got < 25 {
		select {
		case <-events:
			got++
		case err := <-joinErrs:
			t.Fatalf("join error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d events", got)
		}
	}
	<-done
}
