package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chorankates/gzb/zigbee"
)

// The registry these tests resolve against is shaped like the real one: two
// lights with a numbered name, a row of thermos, one of which also ends in a
// digit, and a device nothing has interviewed.

func endpoint(id uint8, clusters ...uint16) zigbee.Endpoint {
	ep := zigbee.Endpoint{ID: id, Profile: 0x0104}
	for _, c := range clusters {
		ep.Input = append(ep.Input, zigbee.Cluster{ID: c, Name: zigbee.ClusterName(c)})
	}
	return ep
}

func lamp(name string, node uint16, ieee string) zigbee.Device {
	return zigbee.Device{
		Name: name, NodeID: node, IEEE: ieee, Interviewed: time.Now(),
		Endpoints: []zigbee.Endpoint{endpoint(1, 0x0000, zigbee.ClusterOnOff, zigbee.ClusterLevelControl, zigbee.ClusterColorControl)},
	}
}

func thermo(name string, node uint16, ieee string) zigbee.Device {
	return zigbee.Device{
		Name: name, NodeID: node, IEEE: ieee, Interviewed: time.Now(),
		Endpoints: []zigbee.Endpoint{endpoint(1, 0x0000, 0x0001, 0x0402, 0x0405)},
	}
}

func fixtureDevices() []zigbee.Device {
	return []zigbee.Device{
		lamp("light1", 0xA489, "28:2C:02:BF:FF:E9:8B:01"),
		lamp("light2", 0x919A, "28:2C:02:BF:FF:E9:8B:02"),
		thermo("living room thermo", 0xCF83, "A4:C1:38:00:00:00:00:01"),
		thermo("outside #1 thermo", 0x584D, "A4:C1:38:00:00:00:00:02"),
		thermo("emylo1", 0xACA3, "A4:C1:38:00:00:00:00:03"),
		{Name: "door1", NodeID: 0x99FB, IEEE: "A4:C1:38:00:00:00:00:04"},
	}
}

// "1" is ambiguous across the registry — light1, emylo1, outside #1 thermo —
// and unambiguous among the lights, which is the whole point of scoping.
func TestLightResolvesByTrailingNumberAmongLights(t *testing.T) {
	devices := fixtureDevices()
	if _, err := findDevice(devices, "1"); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Fatalf("registry-wide, \"1\" resolved (%v); want an ambiguity, or the scope proves nothing", err)
	}
	light, name, err := resolveLight(devices, "1", 0)
	if err != nil {
		t.Fatalf("resolveLight(1): %v", err)
	}
	if light.Node != 0xA489 || light.Endpoint != 1 || name != "light1" {
		t.Errorf("light 1 = %s %q, want light1 at 0xA489 endpoint 1", light, name)
	}
}

// A scope narrows the search; it does not refuse a device outside it. A
// thermo can still be told "on" — it will refuse, which is its business —
// and a cluster an interview did not list can still be read.
func TestScopeFallsBackToTheWholeRegistry(t *testing.T) {
	target, name, err := resolveTarget(fixtureDevices(), "living", zigbee.ClusterOnOff, 0)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if target.Node != 0xCF83 || name != "living room thermo" {
		t.Errorf("resolved %q at 0x%04X, want the living room thermo", name, target.Node)
	}
	// The thermo has no on/off endpoint, so the address is a guess at 1.
	if target.Endpoint != 1 {
		t.Errorf("endpoint = %d, want the guess of 1", target.Endpoint)
	}
}

// An ambiguity inside the scope is reported, not fallen back from: the
// candidates named are exactly the ones the person has to choose between.
func TestAmbiguityInScopeIsAnErrorNamingTheCandidates(t *testing.T) {
	_, _, err := resolveTarget(fixtureDevices(), "thermo", 0x0402, 0)
	if err == nil {
		t.Fatal("\"thermo\" resolved to one of three thermos")
	}
	for _, want := range []string{"living room thermo", "outside #1 thermo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "light1") {
		t.Errorf("error %q names a light, which is outside the scope", err)
	}
}

func TestAddressesMatchExactlyAndBypassTheRegistry(t *testing.T) {
	devices := fixtureDevices()
	target, name, err := resolveTarget(devices, "0x1234", zigbee.ClusterOnOff, 0)
	if err != nil {
		t.Fatalf("a bare address the registry does not know: %v", err)
	}
	if target.Node != 0x1234 || target.Endpoint != 1 || name != "0x1234" {
		t.Errorf("bare address resolved to %s %q", target, name)
	}

	// An address the registry does know resolves to its record, so it gets
	// the interviewed endpoint and the name.
	target, name, err = resolveTarget(devices, "0xa489", zigbee.ClusterOnOff, 0)
	if err != nil || target.Node != 0xA489 || name != "light1" {
		t.Errorf("0xa489 = %s %q (%v), want light1", target, name, err)
	}
	if _, err := findDevice(devices, "a4:c1:38:00:00:00:00:02"); err != nil {
		t.Errorf("an IEEE address in lower case did not match: %v", err)
	}

	// A name that only happens to contain an address is still a name.
	if _, err := findDevice(devices, "nothing"); !errors.Is(err, zigbee.ErrNoDevice) {
		t.Errorf("an unknown name gave %v, want ErrNoDevice", err)
	}
}

func TestEndpointComesFromTheInterviewUnlessOverridden(t *testing.T) {
	sensor := zigbee.Device{
		Name: "two-part sensor", NodeID: 0x0011, IEEE: "00:00:00:00:00:00:00:11", Interviewed: time.Now(),
		Endpoints: []zigbee.Endpoint{endpoint(1, 0x0000), endpoint(2, 0x0402)},
	}
	devices := []zigbee.Device{sensor}
	target, _, err := resolveTarget(devices, "two-part", 0x0402, 0)
	if err != nil || target.Endpoint != 2 {
		t.Errorf("temperature resolved to endpoint %d (%v), want 2 as interviewed", target.Endpoint, err)
	}
	target, _, err = resolveTarget(devices, "two-part", 0x0402, 5)
	if err != nil || target.Endpoint != 5 {
		t.Errorf("-endpoint 5 gave endpoint %d (%v)", target.Endpoint, err)
	}
	if _, _, err := resolveTarget(devices, "two-part", 0x0402, 300); err == nil {
		t.Error("endpoint 300 was accepted; it is outside the application range")
	}
}

// A lamp with a sensor on its first endpoint is a lamp on its second, and the
// light clusters say so.
func TestResolveLightFindsTheEndpointWithTheLightClusters(t *testing.T) {
	lamp := zigbee.Device{
		Name: "lamp with sensor", NodeID: 0x0022, IEEE: "00:00:00:00:00:00:00:22", Interviewed: time.Now(),
		Endpoints: []zigbee.Endpoint{endpoint(1, 0x0000, 0x0406), endpoint(2, zigbee.ClusterOnOff, zigbee.ClusterLevelControl)},
	}
	light, _, err := resolveLight([]zigbee.Device{lamp}, "lamp", 0)
	if err != nil || light.Endpoint != 2 {
		t.Errorf("light = %s (%v), want endpoint 2", light, err)
	}
}

// A device nothing has interviewed is not offered under a scope — nothing is
// known about it — but it can still be addressed, at the usual guess.
func TestUninterviewedDevicesResolveButAreNotInAnyScope(t *testing.T) {
	devices := fixtureDevices()
	for _, d := range withCluster(devices, zigbee.ClusterOnOff) {
		if d.Name == "door1" {
			t.Error("door1 is in the on/off scope without an interview")
		}
	}
	target, name, err := resolveTarget(devices, "door1", zigbee.ClusterOnOff, 0)
	if err != nil || name != "door1" || target.Endpoint != 1 {
		t.Errorf("door1 = %s %q (%v)", target, name, err)
	}
}

func TestNameTiersPreferTheCloserMatch(t *testing.T) {
	devices := []zigbee.Device{
		{Name: "bedroom lamp", NodeID: 1, IEEE: "00:00:00:00:00:00:00:01"},
		{Name: "second bedroom lamp", NodeID: 2, IEEE: "00:00:00:00:00:00:00:02"},
		{Name: "lamp", NodeID: 3, IEEE: "00:00:00:00:00:00:00:03"},
	}
	for query, want := range map[string]uint16{
		"lamp":         3, // exact beats the two that merely end in it
		"bedroom":      1, // prefix beats substring
		"second":       2,
		"Bedroom Lamp": 1, // case does not matter
	} {
		d, err := findDevice(devices, query)
		if err != nil || d.NodeID != want {
			t.Errorf("findDevice(%q) = %d (%v), want %d", query, d.NodeID, err, want)
		}
	}
}
