package zigbee

// Pairing is watched, not just permitted. PermitJoin opens the window; this
// file is how an application sees what happens during it — devices arriving,
// devices leaving, and the window itself opening and closing — and how those
// arrivals reach the device registry. A device that joins while nothing is
// watching is still on the network, but nothing local knows it exists.

import (
	"fmt"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
)

// JoinEventKind says which signal produced a JoinEvent.
//
// A joining device produces up to three: the trust centre reports any join
// anywhere in the mesh, the child event fires for devices parented directly
// to the coordinator and uniquely carries the node type, and the announce is
// broadcast by the device itself and is the one signal that says whether it
// is mains powered or a sleepy battery node. They are complementary rather
// than redundant, so all three are delivered and merged into the registry
// instead of letting the last one win.
type JoinEventKind string

const (
	EventTrustCenter JoinEventKind = "trust-centre"
	EventChild       JoinEventKind = "child"
	EventAnnounce    JoinEventKind = "announce"

	// EventWindowOpened and EventWindowClosed are the stack's own report of
	// the join window changing state. Seeing them is what separates "the
	// coordinator was listening and nothing called" from "the window never
	// opened".
	EventWindowOpened JoinEventKind = "window-opened"
	EventWindowClosed JoinEventKind = "window-closed"
)

// JoinEvent is one signal from a pairing session: a device arriving on, or
// leaving, the network, or the join window opening and closing.
type JoinEvent struct {
	At   time.Time     `json:"at"`
	Kind JoinEventKind `json:"kind"`

	// NodeID and IEEE identify the device. Window events carry neither.
	NodeID uint16 `json:"node_id,omitempty"`
	IEEE   string `json:"ieee,omitempty"`
	// DeviceName is what the registry calls the device when that is more than
	// repeating its address — the difference between "something joined" and
	// "the living room thermo is back".
	DeviceName string `json:"device_name,omitempty"`

	// New reports that the registry had never seen this device before.
	New bool `json:"new,omitempty"`
	// Leaving is set when the device is departing rather than arriving.
	Leaving bool `json:"leaving,omitempty"`

	// Description is the human-readable detail of the event: what the device
	// did and what the trust centre decided about it, its node type, or its
	// own account of how it is powered and whether it listens.
	Description string `json:"description,omitempty"`
}

// Joins reports pairing activity. Each arrival is merged into the device
// registry and saved as it happens, so a device that joins is known — and can
// be named, interviewed and read — whether or not anything else is running.
//
// Joins is independent of the Readings loop and safe alongside it. Subscribe
// before opening the window with PermitJoin, so a fast device cannot join in
// the gap between the two. When the buffer is full an event is dropped rather
// than blocking the protocol; the registry still records what the event said.
// Errors — a callback that would not decode, a registry that would not save —
// arrive on errs without ending the watch, because one bad frame should not
// end a pairing session.
//
// Both channels close when cancel is called or the coordinator closes.
func (c *Coordinator) Joins(buffer int) (<-chan JoinEvent, <-chan error, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	out := make(chan JoinEvent, buffer)
	errs := make(chan error, buffer)
	if err := c.checkOpen(); err != nil {
		errs <- err
		close(out)
		close(errs)
		return out, errs, func() {}
	}

	joins, joinErrs, cancelJoins := c.conn.WatchJoins(buffer)
	stack, cancelStack := c.conn.Subscribe(func(m ezsp.Message) bool {
		return m.Callback && m.ID == ezsp.FrameStackStatusHandler
	}, 8)

	go func() {
		defer close(out)
		defer close(errs)
		for joins != nil || joinErrs != nil || stack != nil {
			select {
			case ev, ok := <-joins:
				if !ok {
					joins = nil
					continue
				}
				event, err := c.recordJoin(ev)
				if err != nil {
					sendError(errs, err)
				}
				sendJoin(out, event)
			case err, ok := <-joinErrs:
				if !ok {
					joinErrs = nil
					continue
				}
				sendError(errs, err)
			case m, ok := <-stack:
				if !ok {
					stack = nil
					continue
				}
				status, err := ezsp.StackStatus(m)
				if err != nil {
					continue
				}
				switch status {
				case ezsp.StatusNetworkOpened:
					sendJoin(out, JoinEvent{At: time.Now(), Kind: EventWindowOpened, Description: status.String()})
				case ezsp.StatusNetworkClosed:
					sendJoin(out, JoinEvent{At: time.Now(), Kind: EventWindowClosed, Description: status.String()})
				}
			}
		}
	}()

	cancel := func() {
		cancelJoins()
		cancelStack()
	}
	return out, errs, cancel
}

// recordJoin merges one event into the registry and shapes its public form.
//
// A departing device is looked up but not recorded: leaving is not a
// sighting, yet the record is what says whose departure this is. Arrivals are
// saved immediately rather than left to Close, because a pairing session is
// exactly the moment the registry must not depend on a clean exit.
func (c *Coordinator) recordJoin(ev ezsp.JoinEvent) (JoinEvent, error) {
	event := JoinEvent{
		At:          ev.At,
		Kind:        JoinEventKind(ev.Kind.String()),
		NodeID:      ev.NodeID,
		IEEE:        ev.IEEE.String(),
		Leaving:     ev.Leaving,
		Description: joinDescription(ev),
	}

	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	if ev.Leaving {
		if d, ok := c.db.Get(event.IEEE); ok {
			event.DeviceName = displayName(d)
		}
		return event, nil
	}

	d, isNew := c.db.Observe(event.IEEE, ev.NodeID, ev.At)
	event.New = isNew
	if ev.NodeType != nil {
		d.NodeType = ev.NodeType.String()
	}
	if ev.Parent != nil {
		parent := *ev.Parent
		d.Parent = &parent
	}
	if ev.Capability != nil {
		capability := uint8(*ev.Capability)
		d.Capability = &capability
	}
	event.DeviceName = displayName(d)

	if err := c.db.Save(); err != nil {
		return event, fmt.Errorf("zigbee: recording join of %s: %w", event.IEEE, err)
	}
	return event, nil
}

// joinDescription renders the detail each callback uniquely carries.
func joinDescription(ev ezsp.JoinEvent) string {
	switch {
	case ev.Update != nil && ev.Decision != nil:
		detail := fmt.Sprintf("%s, %s", *ev.Update, *ev.Decision)
		if ev.Parent != nil {
			detail += fmt.Sprintf(", via 0x%04X", *ev.Parent)
		}
		return detail
	case ev.NodeType != nil:
		detail := ev.NodeType.String()
		if ev.Leaving {
			detail += ", left"
		}
		return detail
	case ev.Capability != nil:
		return ev.Capability.String()
	}
	return ""
}

// displayName is Describe without the address fallback: what to call a device
// when that is more than repeating the address the event already carries.
func displayName(d *store.Device) string {
	if name := d.Describe(); name != d.IEEE {
		return name
	}
	return ""
}

func sendJoin(out chan<- JoinEvent, ev JoinEvent) {
	select {
	case out <- ev:
	default:
	}
}
