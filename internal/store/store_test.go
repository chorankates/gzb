package store

import (
	"os"
	"path/filepath"
	"strings"
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

func TestObserveNodeIDPromotesPlaceholderOnJoin(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()
	pending, isNew := s.ObserveNodeID(0x90CB, now)
	pending.Record("temperature", 28.2, "°C", now)
	if !isNew || pending.Identified() {
		t.Fatalf("placeholder = %+v, isNew = %t", pending, isNew)
	}

	joined, isNew := s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, now.Add(time.Minute))
	if isNew {
		t.Error("promoting a previously observed node must not count it twice")
	}
	if joined != pending || !joined.Identified() {
		t.Fatalf("joined record = %+v, want promoted placeholder", joined)
	}
	if joined.Readings["temperature"].Value != 28.2 {
		t.Error("readings collected before the join were lost")
	}
	if len(s.Devices) != 1 {
		t.Fatalf("registry contains %d records, want 1", len(s.Devices))
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

func TestOpenRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted corrupt JSON")
	}
}

func TestRemove(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	const ieee = "A4:C1:38:18:56:07:FF:FF"
	s.Observe(ieee, 0x90CB, time.Now())
	if !s.Remove(ieee) {
		t.Fatal("Remove returned false for a known device")
	}
	if s.Remove(ieee) {
		t.Fatal("Remove returned true after the device was already removed")
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

// peer builds an interviewed record of the given model, the kind an identical
// sibling is entitled to copy.
func peer(s *Store, ieee, model string, interviewed time.Time) *Device {
	d, _ := s.Observe(ieee, 0x1000, interviewed)
	d.Manufacturer = "eWeLink"
	d.Model = model
	d.Endpoints = []Endpoint{{ID: 1, Input: []uint16{0x0000, 0x0402}}}
	d.Interviewed = interviewed
	return d
}

func TestPeerOfModelFindsAnInterviewedSibling(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()
	want := peer(s, "A4:C1:38:18:AA:AA:AA:AA", "TH01", now)
	peer(s, "A4:C1:38:18:CC:CC:CC:CC", "SNZB-04", now) // a different model

	got, ok := s.PeerOfModel("eWeLink", "TH01", "A4:C1:38:18:56:07:FF:FF")
	if !ok || got != want {
		t.Fatalf("peer = %+v, %t; want the TH01", got, ok)
	}
	if _, ok := s.PeerOfModel("eWeLink", "SNZB-02D", "A4:C1:38:18:56:07:FF:FF"); ok {
		t.Error("a model nothing on the network reports found a peer")
	}
	// Model strings are not unique across vendors, so the maker has to agree.
	if _, ok := s.PeerOfModel("Tuya", "TH01", "A4:C1:38:18:56:07:FF:FF"); ok {
		t.Error("another vendor's TH01 was accepted as the same device")
	}
	// A device is not its own answer key.
	if _, ok := s.PeerOfModel("eWeLink", "TH01", want.IEEE); ok {
		t.Error("a device was offered itself to inherit from")
	}
}

// Only a device that answered for itself may be copied. Inheriting from an
// inherited record would let one bad match spread with nothing left pointing at
// a device that was actually asked.
func TestPeerOfModelRequiresAFirsthandInterview(t *testing.T) {
	now := time.Now()
	const asking = "A4:C1:38:18:56:07:FF:FF"

	secondhand := &Store{Devices: map[string]*Device{}}
	peer(secondhand, "A4:C1:38:18:AA:AA:AA:AA", "TH01", now).InheritedFrom = "A4:C1:38:18:BB:BB:BB:BB"
	if _, ok := secondhand.PeerOfModel("eWeLink", "TH01", asking); ok {
		t.Error("an inherited record was offered as an answer key")
	}

	// A record keyed by network address has a placeholder identity that a later
	// join replaces, so a reference to it would not survive.
	placeholder := &Store{Devices: map[string]*Device{}}
	unknown, _ := placeholder.ObserveNodeID(0x90CB, now)
	unknown.Manufacturer, unknown.Model = "eWeLink", "TH01"
	unknown.Endpoints = []Endpoint{{ID: 1}}
	unknown.Interviewed = now
	if _, ok := placeholder.PeerOfModel("eWeLink", "TH01", asking); ok {
		t.Error("a placeholder record was offered as an answer key")
	}

	// Structure is the point; a record that has a model but no endpoints has
	// nothing worth copying.
	thin := &Store{Devices: map[string]*Device{}}
	bare := peer(thin, "A4:C1:38:18:AA:AA:AA:AA", "TH01", now)
	bare.Endpoints = nil
	if _, ok := thin.PeerOfModel("eWeLink", "TH01", asking); ok {
		t.Error("a record with no endpoints was offered as an answer key")
	}

	// An empty model matches everything, which is exactly what it must not do.
	blank := &Store{Devices: map[string]*Device{}}
	peer(blank, "A4:C1:38:18:AA:AA:AA:AA", "", now)
	if _, ok := blank.PeerOfModel("eWeLink", "", asking); ok {
		t.Error("a device that never said what it is was matched by model")
	}
}

// The same registry must always nominate the same device, or two runs of the
// same command would disagree about where a record came from.
func TestPeerOfModelPicksTheSameDeviceEveryTime(t *testing.T) {
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()
	peer(s, "A4:C1:38:18:AA:AA:AA:AA", "TH01", now.Add(-time.Hour))
	newest := peer(s, "A4:C1:38:18:BB:BB:BB:BB", "TH01", now)
	peer(s, "A4:C1:38:18:CC:CC:CC:CC", "TH01", now.Add(-2*time.Hour))

	for range 10 {
		got, ok := s.PeerOfModel("eWeLink", "TH01", "A4:C1:38:18:56:07:FF:FF")
		if !ok || got != newest {
			t.Fatalf("peer = %+v, want the most recently interviewed", got)
		}
	}
}

// Interviewed is a struct, so it needs omitzero rather than omitempty to stay
// out of the file. A registry full of "0001-01-01T00:00:00Z" reads as though
// every device were interviewed at the dawn of the calendar.
func TestUninterviewedDeviceOmitsTheInterviewTimestamp(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Observe("A4:C1:38:18:56:07:FF:FF", 0x90CB, time.Now())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "interviewed") {
		t.Errorf("a device that was never interviewed carries a timestamp:\n%s", data)
	}
}

// gzb used to record any attribute it could scale, which swept the extremes a
// display sensor keeps about itself into the readings map, where a timestamp
// made them read as measurements. Opening a registry written then has to shed
// them, or every one of those sensors keeps six wrong readings forever.
func TestOpenDropsStatisticsRecordedAsReadings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	now := time.Now().Round(time.Millisecond)
	s := &Store{path: path, Devices: map[string]*Device{
		"sensor": {
			IEEE: "sensor",
			Readings: map[string]Reading{
				"temperature":           {Value: 26.9, Unit: "°C", At: now},
				"humidity":              {Value: 28, Unit: "%", At: now},
				"battery percentage":    {Value: 100, Unit: "%", At: now},
				"temperature maximum":   {Value: 27.9, Unit: "°C", At: now},
				"temperature minimum":   {Value: 27.1, Unit: "°C", At: now},
				"temperature reference": {Value: 27.6, Unit: "°C", At: now},
				"humidity maximum":      {Value: 28.2, Unit: "%", At: now},
				"humidity minimum":      {Value: 27.2, Unit: "%", At: now},
				"humidity reference":    {Value: 27.59, Unit: "%", At: now},
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
	want := []string{"battery percentage", "humidity", "temperature"}
	if got := reopened.Devices["sensor"].ReadingNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("readings = %v, want %v", got, want)
	}
	// The real values are untouched — this drops entries, it does not rewrite
	// the ones that belong there.
	if got := readings["temperature"]; got.Value != 26.9 || got.Unit != "°C" {
		t.Errorf("temperature = %+v, want 26.9 °C", got)
	}
}

// Illuminance was recorded as though the wire value were already lux, so a
// registry written before the fix holds figures like "15564 lx" for a room
// that was at 36. The entry cannot be corrected in place — a converted value
// is indistinguishable from a raw one and would be converted again on the next
// start — so it is dropped and the next report replaces it.
func TestOpenDropsIlluminanceRecordedAsRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	now := time.Now().Round(time.Millisecond)
	s := &Store{path: path, Devices: map[string]*Device{
		"light": {
			IEEE: "light",
			Readings: map[string]Reading{
				"illuminance": {Value: 15564, Unit: "lx", At: now},
				"on/off":      {Value: 0, At: now},
				"temperature": {Value: 21.5, Unit: "°C", At: now},
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
	readings := reopened.Devices["light"].Readings
	if got, ok := readings["illuminance"]; ok {
		t.Errorf("illuminance survived as %+v, and %v lx is not a reading of anything", got, got.Value)
	}
	// Only that one entry goes. A migration that took the neighbouring
	// readings with it would be a worse bug than the one being fixed.
	if got := readings["temperature"]; got.Value != 21.5 {
		t.Errorf("temperature = %+v, want 21.5 °C", got)
	}
	if _, ok := readings["on/off"]; !ok {
		t.Error("on/off was dropped along with the illuminance")
	}
}
