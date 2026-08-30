package zigbee

// The registry API: what the coordinator knows about its devices, and the
// names people give them. None of it touches hardware, which matters more
// than it sounds — the moment to name a device is right after it appears in a
// readings stream, and by then a battery sensor is asleep and unreachable.
//
// These methods are safe to call from any goroutine, alongside an active
// Readings loop and a Joins watch. That is the point of them: the process
// holding the serial port holds it exclusively, so whatever wants to list or
// name devices — an HTTP handler, a UI — has to go through the process that
// is already listening.

import (
	"fmt"
	"time"

	"github.com/chorankates/gzb/internal/store"
)

// ErrNoDevice reports that nothing in the registry matched a query. Callers
// use errors.Is to tell "you named a device I do not know" apart from an
// ambiguous or invalid name, which need different advice.
var ErrNoDevice = store.ErrNoDevice

// LatestReading is the last value recorded for one capability, however it
// arrived — reported by the device or read on demand.
type LatestReading struct {
	Value float64   `json:"value"`
	Unit  string    `json:"unit,omitempty"`
	At    time.Time `json:"at"`
}

// Device is one registry record: everything gzb has learned about a device.
// It is a snapshot — mutating it changes nothing.
type Device struct {
	// IEEE is the stable identity. It is empty for a device seen only by
	// network address, whose traffic arrived before any join callback said
	// who it was.
	IEEE string `json:"ieee,omitempty"`
	// NodeID is the network address, which may change whenever the device
	// rejoins.
	NodeID uint16 `json:"node_id"`
	// Name is what a person called the device, or empty.
	Name string `json:"name,omitempty"`

	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	// NodeType is the device's role as the network reported it, for example
	// "sleepy end device".
	NodeType    string `json:"node_type,omitempty"`
	PowerSource string `json:"power_source,omitempty"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Interviewed is when discovery last filled in the endpoints, or zero.
	Interviewed time.Time `json:"interviewed,omitzero"`
	// InheritedFrom is the IEEE address of the identical device whose
	// interview answers this record carries, when it was not asked itself.
	InheritedFrom string `json:"inherited_from,omitempty"`

	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Latest holds the most recent reading per capability.
	Latest map[string]LatestReading `json:"latest,omitempty"`
}

// Describe names the device as well as what is known allows: the name a
// person gave it, otherwise its model, otherwise its address. It is never
// empty, so it is always safe to print.
func (d Device) Describe() string {
	switch {
	case d.Name != "":
		return d.Name
	case d.Manufacturer != "" && d.Model != "":
		return d.Manufacturer + " " + d.Model
	case d.Model != "":
		return d.Model
	case d.IEEE != "":
		return d.IEEE
	default:
		return fmt.Sprintf("0x%04X", d.NodeID)
	}
}

// Devices lists the registry, most recently seen first.
func (c *Coordinator) Devices() []Device {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	list := c.db.List()
	devices := make([]Device, 0, len(list))
	for _, d := range list {
		devices = append(devices, snapshotDevice(d))
	}
	return devices
}

// Device finds the device a query means, from an IEEE address, a network
// address in hex, or a name. Names match loosely — any unambiguous part will
// do — and a query matching several devices is an error naming them, never an
// arbitrary pick.
func (c *Coordinator) Device(query string) (Device, error) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	d, err := c.db.Resolve(query)
	if err != nil {
		return Device{}, err
	}
	return snapshotDevice(d), nil
}

// SetName gives a device a human-friendly name and saves the registry. The
// device may be given by address or by its existing name; the new name must
// be unique, because a name is also an address, and cannot look like one.
func (c *Coordinator) SetName(query, name string) (Device, error) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	d, err := c.db.Resolve(query)
	if err != nil {
		return Device{}, err
	}
	if _, err := c.db.SetName(d.IEEE, name); err != nil {
		return Device{}, err
	}
	if err := c.db.Save(); err != nil {
		return Device{}, err
	}
	return snapshotDevice(d), nil
}

// ClearName removes a device's name, saves the registry, and returns the
// record along with what the device used to be called.
func (c *Coordinator) ClearName(query string) (Device, string, error) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	d, err := c.db.Resolve(query)
	if err != nil {
		return Device{}, "", err
	}
	_, was, err := c.db.ClearName(d.IEEE)
	if err != nil {
		return Device{}, "", err
	}
	if err := c.db.Save(); err != nil {
		return Device{}, "", err
	}
	return snapshotDevice(d), was, nil
}

// snapshotDevice copies a registry record into its public shape. The caller
// holds dbMu.
func snapshotDevice(d *store.Device) Device {
	device := Device{
		NodeID:        d.NodeID,
		Name:          d.Name,
		Manufacturer:  d.Manufacturer,
		Model:         d.Model,
		NodeType:      d.NodeType,
		PowerSource:   d.PowerSource,
		FirstSeen:     d.FirstSeen,
		LastSeen:      d.LastSeen,
		Interviewed:   d.Interviewed,
		InheritedFrom: d.InheritedFrom,
		Endpoints:     endpointsOf(d),
	}
	// A placeholder record is keyed by an address nothing identified; that
	// key is bookkeeping, not an identity to report.
	if d.Identified() {
		device.IEEE = d.IEEE
	}
	if len(d.Readings) > 0 {
		device.Latest = make(map[string]LatestReading, len(d.Readings))
		for name, r := range d.Readings {
			device.Latest[name] = LatestReading{Value: r.Value, Unit: r.Unit, At: r.At}
		}
	}
	return device
}
