package zigbee

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/internal/zcl"
)

func TestDevicesSnapshotsTheRegistry(t *testing.T) {
	db := emptyStore(t)
	older := time.Now().Add(-time.Hour)
	newer := time.Now()

	lamp, _ := db.Observe("00:11:22:33:44:55:66:77", 0x1234, older)
	lamp.Name = "bedroom lamp"

	thermo, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, newer)
	thermo.Model = "SNZB-02B"
	thermo.Manufacturer = "SONOFF"
	thermo.NodeType = "sleepy end device"
	thermo.Endpoints = []store.Endpoint{{ID: 1, Profile: 0x0104, Device: 0x0302, Input: []uint16{0x0402}}}
	thermo.Record("temperature", 23.9, "°C", newer)

	c := &Coordinator{conn: newFakeConnection(), db: db}
	devices := c.Devices()
	if len(devices) != 2 {
		t.Fatalf("Devices() returned %d records, want 2", len(devices))
	}
	// Most recently seen first, so the thermo leads.
	got := devices[0]
	if got.IEEE != thermo.IEEE || got.Model != "SNZB-02B" || got.NodeType != "sleepy end device" {
		t.Errorf("snapshot = %+v", got)
	}
	if got.Describe() != "SONOFF SNZB-02B" {
		t.Errorf("Describe() = %q, want the manufacturer and model", got.Describe())
	}
	if len(got.Endpoints) != 1 || len(got.Endpoints[0].Input) != 1 ||
		got.Endpoints[0].Input[0].Name != "temperature" {
		t.Errorf("endpoints = %+v, want a named temperature cluster", got.Endpoints)
	}
	if r := got.Latest["temperature"]; r.Value != 23.9 || r.Unit != "°C" {
		t.Errorf("latest temperature = %+v", r)
	}

	// A snapshot is a copy: mutating it must not reach the registry.
	got.Latest["temperature"] = LatestReading{Value: -100}
	if db.Devices[thermo.IEEE].Readings["temperature"].Value != 23.9 {
		t.Error("mutating a snapshot reached the registry")
	}
}

func TestDevicesHidesAPlaceholderIdentity(t *testing.T) {
	db := emptyStore(t)
	if _, ok := db.ObserveNodeID(0x90CB, time.Now()); !ok {
		t.Fatal("expected a fresh placeholder record")
	}
	c := &Coordinator{conn: newFakeConnection(), db: db}
	devices := c.Devices()
	if len(devices) != 1 {
		t.Fatalf("Devices() returned %d records, want 1", len(devices))
	}
	if devices[0].IEEE != "" {
		t.Errorf("placeholder identity leaked: %q", devices[0].IEEE)
	}
	if devices[0].Describe() != "0x90CB" {
		t.Errorf("Describe() = %q, want the network address", devices[0].Describe())
	}
}

func TestDeviceResolvesLooselyAndRefusesToGuess(t *testing.T) {
	db := emptyStore(t)
	a, _ := db.Observe("00:11:22:33:44:55:66:77", 0x1234, time.Now())
	a.Name = "bedroom lamp"
	b, _ := db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	b.Name = "bedroom sensor"

	c := &Coordinator{conn: newFakeConnection(), db: db}
	if d, err := c.Device("lamp"); err != nil || d.Name != "bedroom lamp" {
		t.Errorf("Device(lamp) = %+v, %v", d, err)
	}
	if _, err := c.Device("bedroom"); err == nil {
		t.Error("an ambiguous query resolved instead of erroring")
	}
	if _, err := c.Device("garage"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("Device(garage) error = %v, want ErrNoDevice", err)
	}
}

func TestSetNamePersistsAndEnforcesUniqueness(t *testing.T) {
	db := emptyStore(t)
	db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	db.Observe("00:11:22:33:44:55:66:77", 0x1234, time.Now())
	c := &Coordinator{conn: newFakeConnection(), db: db}

	d, err := c.SetName("0x90CB", "living room thermo")
	if err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if d.Name != "living room thermo" {
		t.Errorf("returned device = %+v", d)
	}

	// The name is on disk, not waiting for Close.
	reopened, err := store.Open(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if saved, _ := reopened.Get("A4:C1:38:18:56:07:FF:FF"); saved.Name != "living room thermo" {
		t.Errorf("saved name = %q", saved.Name)
	}

	if _, err := c.SetName("0x1234", "living room thermo"); err == nil {
		t.Error("a duplicate name was accepted")
	}
	if _, err := c.SetName("0x1234", "0xBEEF"); err == nil {
		t.Error("an address-shaped name was accepted")
	}
	if _, err := c.SetName("garage", "anything"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("SetName on a missing device = %v, want ErrNoDevice", err)
	}
}

func TestClearNameReturnsWhatItWas(t *testing.T) {
	db := emptyStore(t)
	db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	c := &Coordinator{conn: newFakeConnection(), db: db}
	if _, err := c.SetName("0x90CB", "living room thermo"); err != nil {
		t.Fatal(err)
	}

	// The existing name is itself an address for the clear.
	d, was, err := c.ClearName("thermo")
	if err != nil {
		t.Fatalf("ClearName: %v", err)
	}
	if was != "living room thermo" || d.Name != "" {
		t.Errorf("cleared = %+v, was %q", d, was)
	}
}

// TestRegistryAPIIsSafeAlongsideReadings exists for the race detector: the
// readings loop records readings while another goroutine lists and renames,
// which is exactly what an application serving a naming API does.
func TestRegistryAPIIsSafeAlongsideReadings(t *testing.T) {
	fake := newFakeConnection()
	db := emptyStore(t)
	db.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	c := &Coordinator{conn: fake, db: db}

	ctx, cancel := context.WithCancel(context.Background())
	readings, errs := c.Readings(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 25 {
			c.Devices()
			if _, err := c.SetName("A4:C1:38:18:56:07:FF:FF", fmt.Sprintf("thermo %d", i)); err != nil {
				t.Errorf("SetName: %v", err)
			}
		}
	}()

	report := []byte{0x18, 0xC8, 0x0A, 0x00, 0x00, 0x29, 0x54, 0x0B}
	for range 25 {
		fake.msgs <- incomingMessage(zcl.ClusterTemperature, report)
		select {
		case <-readings:
		case err := <-errs:
			t.Fatalf("Readings error: %v", err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for a reading")
		}
	}
	<-done
	cancel()
	for range readings {
	}
	for err := range errs {
		t.Errorf("unexpected shutdown error: %v", err)
	}
}
