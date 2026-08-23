package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestObserveReportsFirstSighting(t *testing.T) {
	s := &Store{path: "", Devices: map[string]*Device{}}
	now := time.Now()

	d, isNew := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, now)
	if !isNew {
		t.Fatal("the first sighting of a device must report as new")
	}
	if d.FirstSeen != now || d.NodeID != 0x90CB {
		t.Errorf("device = %+v", d)
	}

	later := now.Add(time.Minute)
	d2, isNew := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x1234, later)
	if isNew {
		t.Error("a second sighting must not report as new")
	}
	if d2 != d {
		t.Error("the same device must merge into the same record")
	}
	if d2.FirstSeen != now {
		t.Error("FirstSeen must survive a later sighting")
	}
	if d2.NodeID != 0x1234 {
		t.Error("a rejoining device gets a new network address, which must be recorded")
	}
}

// TestObservePreservesMergedFields covers the reason Observe merges rather than
// replaces: the three join callbacks each carry a different subset, and a later
// one must not erase what an earlier one established.
func TestObservePreservesMergedFields(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()

	d, _ := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, now)
	cap := uint8(0x80)
	d.Capability = &cap
	d.NodeType = "sleepy end device"

	d2, _ := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, now.Add(time.Second))
	if d2.Capability == nil || *d2.Capability != 0x80 {
		t.Error("capability flags from the announce were lost")
	}
	if d2.NodeType != "sleepy end device" {
		t.Error("node type from the child callback was lost")
	}
}

func TestByNodeID(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())

	if _, ok := s.ByNodeID(0x90CB); !ok {
		t.Error("a recorded device must be findable by its network address")
	}
	if _, ok := s.ByNodeID(0x0001); ok {
		t.Error("an unknown network address must not match")
	}
}

func TestRecordKeepsLatestReading(t *testing.T) {
	d := &Device{IEEE: "A4:C1:38:18:56:07:FF:FF"}
	t0 := time.Now()

	d.Record("temperature", 28.2, "°C", t0)
	d.Record("humidity", 34.6, "%", t0)
	d.Record("temperature", 29.0, "°C", t0.Add(time.Minute))

	if got := d.Readings["temperature"].Value; got != 29.0 {
		t.Errorf("temperature = %v, want the most recent value 29.0", got)
	}
	if names := d.ReadingNames(); len(names) != 2 || names[0] != "humidity" {
		t.Errorf("ReadingNames = %v, want a sorted list of two", names)
	}
}

func TestSaveAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "devices.json")
	now := time.Now().Round(time.Millisecond)

	s := &Store{path: path, Devices: map[string]*Device{}}
	d, _ := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, now)
	d.NodeType = "sleepy end device"
	d.Record("temperature", 28.2, "°C", now)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, ok := reopened.Get("A4:C1:38:18:56:07:FF:FF")
	if !ok {
		t.Fatal("the saved device was not found after reopening")
	}
	if got.NodeID != 0x90CB || got.NodeType != "sleepy end device" {
		t.Errorf("device = %+v", got)
	}
	if got.Readings["temperature"].Value != 28.2 {
		t.Errorf("readings did not survive the round trip: %+v", got.Readings)
	}
	if reopened.Path() != path {
		t.Errorf("Path = %q, want %q", reopened.Path(), path)
	}
}

func TestOpenMigratesLegacyBatteryCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	now := time.Now().Round(time.Millisecond)
	s := &Store{path: path, Devices: map[string]*Device{
		"sensor": {
			IEEE: "sensor",
			Readings: map[string]Reading{
				"battery": {Value: 87.5, Unit: "%", At: now},
			},
		},
	}}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	readings := reopened.Devices["sensor"].Readings
	if _, exists := readings["battery"]; exists {
		t.Error("ambiguous legacy battery capability was retained")
	}
	if got := readings["battery percentage"]; got.Value != 87.5 || got.Unit != "%" {
		t.Errorf("migrated battery percentage = %+v", got)
	}
}

func TestOpenMissingFileIsEmptyNotAnError(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("opening a registry that does not exist yet must succeed: %v", err)
	}
	if len(s.List()) != 0 {
		t.Error("a fresh registry must be empty")
	}
}

func TestListOrdersByMostRecentlySeen(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()
	s.Observe("00:00:00:00:00:00:00:01", 1, now.Add(-time.Hour))
	s.Observe("00:00:00:00:00:00:00:02", 2, now)

	list := s.List()
	if len(list) != 2 || list[0].IEEE != "00:00:00:00:00:00:00:02" {
		t.Errorf("List = %v, want the most recently seen device first", list)
	}
}
