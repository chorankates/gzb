package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// registryWith builds a registry file holding the given devices and returns its
// path, which is what interviewTargets takes.
func registryWith(t *testing.T, devices ...*store.Device) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		recorded, _ := db.Observe(d.IEEE, d.NodeID, d.LastSeen)
		*recorded = *d
	}
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	return path
}

// devicesAt reads a registry file back as the device list commands resolve
// against.
func devicesAt(t *testing.T, path string) []zigbee.Device {
	t.Helper()
	devices, err := zigbee.LoadDevices(path)
	if err != nil {
		t.Fatal(err)
	}
	return devices
}

func uninterviewed(ieee string, node uint16, name string) *store.Device {
	return &store.Device{IEEE: ieee, NodeID: node, Name: name, LastSeen: time.Now()}
}

func interviewed(ieee string, node uint16, name string) *store.Device {
	d := uninterviewed(ieee, node, name)
	d.Interviewed = time.Now()
	d.Endpoints = []store.Endpoint{{ID: 1, Profile: 0x0104, Input: []uint16{0x0000}}}
	return d
}

func targetNames(targets []interviewTarget) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.name)
	}
	return names
}

// An interview is minutes of waiting on a sleepy device and its answers do not
// change, so --all has to mean the devices still without an answer. Asking the
// whole registry again is what --full is for.
func TestAllSkipsDevicesAlreadyInterviewed(t *testing.T) {
	path := registryWith(t,
		interviewed("A4:C1:38:00:00:00:00:01", 0x1111, "living room thermo"),
		uninterviewed("A4:C1:38:00:00:00:00:02", 0x2222, "bedroom thermo"),
		uninterviewed("A4:C1:38:00:00:00:00:03", 0x3333, "door1"),
	)

	targets, done, err := interviewTargets(devicesAt(t, path), nil, true, false)
	if err != nil {
		t.Fatalf("interviewTargets: %v", err)
	}
	if done != 1 {
		t.Errorf("reported %d device(s) already interviewed, want 1", done)
	}
	got := targetNames(targets)
	if len(got) != 2 {
		t.Fatalf("targets = %v, want the two uninterviewed devices", got)
	}
	for _, name := range got {
		if name == "living room thermo" {
			t.Errorf("targets = %v, includes a device already interviewed", got)
		}
	}
}

func TestFullAsksEveryDeviceAgain(t *testing.T) {
	path := registryWith(t,
		interviewed("A4:C1:38:00:00:00:00:01", 0x1111, "living room thermo"),
		uninterviewed("A4:C1:38:00:00:00:00:02", 0x2222, "bedroom thermo"),
	)

	targets, done, err := interviewTargets(devicesAt(t, path), nil, true, true)
	if err != nil {
		t.Fatalf("interviewTargets: %v", err)
	}
	if done != 0 {
		t.Errorf("skipped %d device(s) with --full, want 0", done)
	}
	if len(targets) != 2 {
		t.Errorf("targets = %v, want both devices", targetNames(targets))
	}
}

// Nothing left to ask is a finished job. The caller distinguishes it from an
// empty registry, which is an error, by the count coming back with it.
func TestAllWithEverythingInterviewedHasNothingToAsk(t *testing.T) {
	path := registryWith(t,
		interviewed("A4:C1:38:00:00:00:00:01", 0x1111, "living room thermo"),
		interviewed("A4:C1:38:00:00:00:00:02", 0x2222, "bedroom thermo"),
	)

	targets, done, err := interviewTargets(devicesAt(t, path), nil, true, false)
	if err != nil {
		t.Fatalf("interviewTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("targets = %v, want none", targetNames(targets))
	}
	if done != 2 {
		t.Errorf("reported %d device(s) already interviewed, want 2", done)
	}
}

func TestAllOnAnEmptyRegistryIsAnError(t *testing.T) {
	if _, _, err := interviewTargets(devicesAt(t, registryWith(t)), nil, true, false); err == nil {
		t.Error("interviewTargets on an empty registry returned no error")
	}
}

// Naming a device is the request, whatever the registry already holds — the
// skip belongs to --all, which is a question about which devices are left.
func TestANamedDeviceIsAskedEvenWhenAlreadyInterviewed(t *testing.T) {
	path := registryWith(t, interviewed("A4:C1:38:00:00:00:00:01", 0x1111, "living room thermo"))

	targets, done, err := interviewTargets(devicesAt(t, path), []string{"living room thermo"}, false, false)
	if err != nil {
		t.Fatalf("interviewTargets: %v", err)
	}
	if done != 0 {
		t.Errorf("skipped %d device(s), want 0", done)
	}
	if len(targets) != 1 || targets[0].node != 0x1111 {
		t.Errorf("targets = %+v, want the named device", targets)
	}
}

// A run over several devices ends with a count, because by then the individual
// answers have scrolled away. The counts have to be honest about what was
// actually learned: a device that sat through every question is no more
// recorded than one whose interview never started.
func TestInterviewSummaryCountsWhatWasLearned(t *testing.T) {
	answered := zigbee.Description{Model: "TH01", Endpoints: []zigbee.Endpoint{{ID: 1}}}
	borrowed := zigbee.Description{Model: "TH01", InheritedFrom: "A4:C1:38:00:00:00:00:01",
		Endpoints: []zigbee.Endpoint{{ID: 1}}}
	silent := zigbee.Description{Problems: []string{"timed out"}}

	got := capture(t, func() {
		printInterviewSummary([]zigbee.Description{answered, borrowed, borrowed, silent}, 1)
	})
	want := "3 device(s) interviewed, 2 of them from an identical device; 2 could not be reached.\n" +
		"Re-run to try those again; what succeeded is already recorded.\n"
	if got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

// One device has just printed its own answer in full; counting it up again
// says nothing.
func TestInterviewSummaryStaysQuietForOneDevice(t *testing.T) {
	one := []zigbee.Description{{Model: "TH01", Endpoints: []zigbee.Endpoint{{ID: 1}}}}
	if got := capture(t, func() { printInterviewSummary(one, 0) }); got != "" {
		t.Errorf("summary for a single device = %q, want nothing", got)
	}
}

func TestInterviewSummaryOfACleanRun(t *testing.T) {
	answered := zigbee.Description{Model: "TH01", Endpoints: []zigbee.Endpoint{{ID: 1}}}
	got := capture(t, func() {
		printInterviewSummary([]zigbee.Description{answered, answered}, 0)
	})
	if got != "2 device(s) interviewed.\n" {
		t.Errorf("summary = %q", got)
	}
}
