// Package store keeps a local record of the devices seen on the network.
//
// The coordinator itself remembers very little: it knows the addresses in its
// routing and child tables, but not what a device is, when it was first seen,
// or anything discovered about it since. Single-shot CLI commands would
// otherwise have to rediscover the mesh on every invocation, which for sleepy
// battery devices is not merely slow but frequently impossible — they are only
// reachable while awake.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const unknownIEEEPrefix = "unknown:"

// Device is what we know about one node on the network.
type Device struct {
	IEEE      string    `json:"ieee"`
	NodeID    uint16    `json:"node_id"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`

	NodeType   string  `json:"node_type,omitempty"`
	Parent     *uint16 `json:"parent,omitempty"`
	Capability *uint8  `json:"capability,omitempty"`
	// Label is a user-assigned name. Nothing sets it yet, but it is the field
	// that makes a device list readable, so it is persisted from the start.
	Label string `json:"label,omitempty"`
	// Readings holds the most recent value of each quantity the device
	// reports, keyed by name. Sleepy devices are unreachable most of the time,
	// so the last thing they volunteered is often the only thing available.
	Readings map[string]Reading `json:"readings,omitempty"`
}

// Reading is the latest value of one quantity a device reports.
type Reading struct {
	Value float64   `json:"value"`
	Unit  string    `json:"unit"`
	At    time.Time `json:"at"`
}

func (r Reading) String() string {
	if r.Unit == "" {
		return fmt.Sprintf("%g", r.Value)
	}
	return fmt.Sprintf("%.2f %s", r.Value, r.Unit)
}

// Record stores the latest value of a named quantity.
func (d *Device) Record(name string, value float64, unit string, at time.Time) {
	if d.Readings == nil {
		d.Readings = make(map[string]Reading)
	}
	d.Readings[name] = Reading{Value: value, Unit: unit, At: at}
}

// ReadingNames lists the recorded quantities in a stable order.
func (d *Device) ReadingNames() []string {
	names := make([]string, 0, len(d.Readings))
	for name := range d.Readings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NodeIDHex renders the network address the way Zigbee tools print it.
func (d Device) NodeIDHex() string { return fmt.Sprintf("0x%04X", d.NodeID) }

// Identified reports whether this record has a real IEEE address rather than
// a temporary network-address placeholder.
func (d Device) Identified() bool {
	return !strings.HasPrefix(d.IEEE, unknownIEEEPrefix)
}

// Store is a set of devices persisted to a JSON file.
type Store struct {
	path    string
	Devices map[string]*Device `json:"devices"`
}

// DefaultPath is where the registry lives unless overridden.
func DefaultPath() string {
	if p := os.Getenv("GZB_DB"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "gzb-devices.json"
	}
	return filepath.Join(dir, "gzb", "devices.json")
}

// Open loads the registry, returning an empty one if the file does not exist.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	s := &Store{path: path, Devices: make(map[string]*Device)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("store: parsing %s: %w", path, err)
	}
	if s.Devices == nil {
		s.Devices = make(map[string]*Device)
	}
	s.path = path
	return s, nil
}

// Path reports the file backing this store.
func (s *Store) Path() string { return s.path }

// Observe records a sighting, merging it with anything already known. It
// returns the device and whether this was the first time we saw it.
//
// Fields are merged rather than overwritten because the three join callbacks
// each carry a different subset: losing the capability flags from an announce
// because a trust-centre event arrived afterwards would throw away the only
// signal that says whether a device is battery powered.
func (s *Store) Observe(ieee string, nodeID uint16, now time.Time) (*Device, bool) {
	d, known := s.Devices[ieee]
	if !known {
		placeholder := unknownIEEE(nodeID)
		if pending, ok := s.Devices[placeholder]; ok {
			delete(s.Devices, placeholder)
			d = pending
			d.IEEE = ieee
			known = true
		} else {
			d = &Device{IEEE: ieee, FirstSeen: now}
		}
		s.Devices[ieee] = d
	}
	d.NodeID = nodeID
	d.LastSeen = now
	return d, !known
}

// ObserveNodeID records traffic from a node whose IEEE address is not yet
// known. A later join event for the same network address promotes this
// placeholder into the device's permanent IEEE-keyed record.
func (s *Store) ObserveNodeID(nodeID uint16, now time.Time) (*Device, bool) {
	if d, ok := s.ByNodeID(nodeID); ok {
		d.LastSeen = now
		return d, false
	}
	placeholder := unknownIEEE(nodeID)
	d := &Device{
		IEEE:      placeholder,
		NodeID:    nodeID,
		FirstSeen: now,
		LastSeen:  now,
	}
	s.Devices[placeholder] = d
	return d, true
}

func unknownIEEE(nodeID uint16) string {
	return fmt.Sprintf("%s0x%04X", unknownIEEEPrefix, nodeID)
}

// Get returns a device by IEEE address.
func (s *Store) Get(ieee string) (*Device, bool) {
	d, ok := s.Devices[ieee]
	return d, ok
}

// ByNodeID finds a device by its current 16-bit network address.
//
// Network addresses are not stable — a device that rejoins is issued a new one
// — so this is a best-effort convenience for making live traffic readable, not
// an identity lookup. The IEEE address is the identity.
func (s *Store) ByNodeID(id uint16) (*Device, bool) {
	for _, d := range s.Devices {
		if d.NodeID == id {
			return d, true
		}
	}
	return nil, false
}

// Remove deletes a device from the registry.
func (s *Store) Remove(ieee string) bool {
	if _, ok := s.Devices[ieee]; !ok {
		return false
	}
	delete(s.Devices, ieee)
	return true
}

// List returns the devices in a stable order, most recently seen first.
func (s *Store) List() []*Device {
	out := make([]*Device, 0, len(s.Devices))
	for _, d := range s.Devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].IEEE < out[j].IEEE
	})
	return out
}

// Save writes the registry back to disk.
//
// The write goes to a temporary file in the same directory and is renamed into
// place, so an interrupted save cannot leave a half-written registry behind.
func (s *Store) Save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encoding registry: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return fmt.Errorf("store: creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: replacing %s: %w", s.path, err)
	}
	return nil
}
