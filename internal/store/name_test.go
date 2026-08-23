package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// named builds a registry of devices with the given names, in the order given.
func named(t *testing.T, names ...string) *Store {
	t.Helper()
	s := &Store{Devices: map[string]*Device{}}
	now := time.Now()
	for i, name := range names {
		ieee := "00:00:00:00:00:00:00:0" + string(rune('1'+i))
		d, _ := s.Observe(ieee, uint16(0x1000+i), now.Add(time.Duration(i)*time.Second))
		if name != "" {
			if _, err := s.SetName(ieee, name); err != nil {
				t.Fatalf("SetName(%q): %v", name, err)
			}
		}
		_ = d
	}
	return s
}

func TestSetNameNormalizesWhitespace(t *testing.T) {
	s := named(t, "")
	d, err := s.SetName("00:00:00:00:00:00:00:01", "  living   room  thermo ")
	if err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if d.Name != "living room thermo" {
		t.Errorf("Name = %q, want the whitespace collapsed", d.Name)
	}
}

// Names address devices, so a duplicate would make a lookup unanswerable.
func TestSetNameRejectsDuplicates(t *testing.T) {
	s := named(t, "living room thermo", "")

	_, err := s.SetName("00:00:00:00:00:00:00:02", "Living Room Thermo")
	if err == nil {
		t.Fatal("a name differing only in case must not be accepted twice")
	}
	if !strings.Contains(err.Error(), "00:00:00:00:00:00:00:01") {
		t.Errorf("the error must say which device holds the name, got: %v", err)
	}

	// Renaming a device to what it is already called is not a conflict.
	if _, err := s.SetName("00:00:00:00:00:00:00:01", "living room thermo"); err != nil {
		t.Errorf("renaming a device to its own name: %v", err)
	}
}

// A name that looks like an address would be shadowed by Resolve's address
// matching, and would mean two different things depending on what was paired.
func TestSetNameRejectsAddressShapedNames(t *testing.T) {
	s := named(t, "")
	for _, name := range []string{"0x90CB", "0x0000", "A4:C1:38:18:56:07:FF:FF", "", "   ", strings.Repeat("x", MaxNameLen+1), "bell\aname"} {
		if _, err := s.SetName("00:00:00:00:00:00:00:01", name); err == nil {
			t.Errorf("SetName(%q) must be rejected", name)
		}
	}
	// A name that merely contains hex is fine.
	if _, err := s.SetName("00:00:00:00:00:00:00:01", "sensor 0x1"); err != nil {
		t.Errorf("SetName(\"sensor 0x1\"): %v", err)
	}
	// A newline is whitespace, and whitespace is normalized rather than
	// refused: a name pasted out of a log is still the name that was meant.
	d, err := s.SetName("00:00:00:00:00:00:00:01", "two\nlines")
	if err != nil || d.Name != "two lines" {
		t.Errorf("SetName(\"two\\nlines\") = %q, %v; want it folded to one line", d.Name, err)
	}
}

func TestClearName(t *testing.T) {
	s := named(t, "living room thermo")

	d, was, err := s.ClearName("00:00:00:00:00:00:00:01")
	if err != nil {
		t.Fatalf("ClearName: %v", err)
	}
	if was != "living room thermo" || d.Name != "" {
		t.Errorf("ClearName = %q, device name now %q", was, d.Name)
	}
	// The name is free again afterwards.
	if _, err := s.SetName("00:00:00:00:00:00:00:01", "living room thermo"); err != nil {
		t.Errorf("reusing a cleared name: %v", err)
	}
}

func TestSetNameUnknownDevice(t *testing.T) {
	s := named(t)
	if _, err := s.SetName("00:00:00:00:00:00:00:09", "ghost"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("err = %v, want ErrNoDevice", err)
	}
}

func TestResolve(t *testing.T) {
	s := named(t, "living room thermo", "back door sensor")

	for _, tc := range []struct {
		query string
		want  string // IEEE
		why   string
	}{
		{"00:00:00:00:00:00:00:01", "00:00:00:00:00:00:00:01", "IEEE address"},
		{"00:00:00:00:00:00:00:02", "00:00:00:00:00:00:00:02", "IEEE address"},
		{"0x1000", "00:00:00:00:00:00:00:01", "network address"},
		{"0X1001", "00:00:00:00:00:00:00:02", "network address, any case"},
		{"living room thermo", "00:00:00:00:00:00:00:01", "exact name"},
		{"LIVING ROOM THERMO", "00:00:00:00:00:00:00:01", "name ignoring case"},
		{"living", "00:00:00:00:00:00:00:01", "name prefix"},
		{"thermo", "00:00:00:00:00:00:00:01", "part of a name"},
		{"door", "00:00:00:00:00:00:00:02", "part of a name"},
		{"  back door  sensor ", "00:00:00:00:00:00:00:02", "name with stray whitespace"},
	} {
		d, err := s.Resolve(tc.query)
		if err != nil {
			t.Errorf("Resolve(%q) by %s: %v", tc.query, tc.why, err)
			continue
		}
		if d.IEEE != tc.want {
			t.Errorf("Resolve(%q) by %s = %s, want %s", tc.query, tc.why, d.IEEE, tc.want)
		}
	}
}

// An address must mean the same thing however devices are named, so it is
// matched before names and never loosely.
func TestResolvePrefersAddressesOverNames(t *testing.T) {
	s := named(t, "thermo", "")
	// The second device is named after the first device's IEEE address, which
	// SetName would refuse; only a hand-edited registry can produce it.
	s.Devices["00:00:00:00:00:00:00:02"].Name = "00:00:00:00:00:00:00:01"

	d, err := s.Resolve("00:00:00:00:00:00:00:01")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.IEEE != "00:00:00:00:00:00:00:01" {
		t.Error("an IEEE address must resolve to the device that holds it, not to a device named after it")
	}
}

func TestResolveAmbiguousNameIsAnError(t *testing.T) {
	s := named(t, "bedroom lamp", "bedroom sensor")

	_, err := s.Resolve("bedroom")
	if err == nil {
		t.Fatal("a query matching two devices must not resolve to either")
	}
	if errors.Is(err, ErrNoDevice) {
		t.Error("an ambiguous query is not a missing device; the two need different advice")
	}
	for _, want := range []string{"bedroom lamp", "bedroom sensor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must list the candidates, missing %q: %v", want, err)
		}
	}
}

// A prefix is a stronger match than a substring, so an exact-ish query is not
// made ambiguous by a longer name that happens to contain it.
func TestResolvePrefersPrefixOverSubstring(t *testing.T) {
	s := named(t, "bedroom lamp", "second bedroom lamp")

	d, err := s.Resolve("bedroom")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.Name != "bedroom lamp" {
		t.Errorf("Resolve = %q, want the device whose name starts with the query", d.Name)
	}
}

func TestResolveUnknown(t *testing.T) {
	s := named(t, "living room thermo")

	for _, query := range []string{"kitchen", "0x9999", "00:00:00:00:00:00:00:99", ""} {
		if _, err := s.Resolve(query); !errors.Is(err, ErrNoDevice) {
			t.Errorf("Resolve(%q) = %v, want ErrNoDevice", query, err)
		}
	}
}

func TestParseNodeIDRequiresPrefix(t *testing.T) {
	if _, ok := ParseNodeID("1234"); ok {
		t.Error("a bare number must not be read as an address; it may be a name")
	}
	if _, ok := ParseNodeID("0xZZZZ"); ok {
		t.Error("0xZZZZ is not an address")
	}
	if id, ok := ParseNodeID("0x90cb"); !ok || id != 0x90CB {
		t.Errorf("ParseNodeID(0x90cb) = %#x, %v", id, ok)
	}
}

func TestDescribePrefersTheNameAPersonChose(t *testing.T) {
	d := &Device{IEEE: "A4:C1:38:18:56:07:FF:FF"}
	if got := d.Describe(); got != d.IEEE {
		t.Errorf("Describe = %q, want the address when nothing else is known", got)
	}

	d.Manufacturer, d.Model = "SONOFF", "SNZB-02"
	if got := d.Describe(); got != "SONOFF SNZB-02" {
		t.Errorf("Describe = %q, want the interviewed model", got)
	}

	d.Name = "living room thermo"
	if got := d.Describe(); got != "living room thermo" {
		t.Errorf("Describe = %q, want the name to win over the model", got)
	}
}

func TestNameSurvivesSaveAndReopen(t *testing.T) {
	s := named(t, "living room thermo")
	s.path = t.TempDir() + "/devices.json"
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(s.path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d, err := reopened.Resolve("thermo")
	if err != nil {
		t.Fatalf("Resolve after reopening: %v", err)
	}
	if d.Name != "living room thermo" {
		t.Errorf("Name = %q after a round trip", d.Name)
	}
}
