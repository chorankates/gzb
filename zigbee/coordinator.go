// Package zigbee provides a high-level interface to a Zigbee coordinator.
//
// It owns the protocol loop that turns device attribute reports into readings,
// answers services expected from a coordinator, and enriches readings with the
// local device registry. Applications should use this package instead of
// decoding EZSP or ZCL frames themselves.
package zigbee

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chorankates/gzb/internal/ash"
	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
)

// DefaultBaud is the serial rate used by EmberZNet NCP firmware.
const DefaultBaud = ezsp.DefaultBaud

var (
	// ErrClosed is returned after the coordinator has been closed.
	ErrClosed = errors.New("zigbee: coordinator closed")
	// ErrReadingsActive is reported when a second readings loop is requested.
	// A Coordinator has one protocol loop so that frames are never processed
	// twice by competing consumers.
	ErrReadingsActive = errors.New("zigbee: readings already active")
)

// Options configures a Coordinator.
type Options struct {
	// Path is the coordinator serial device, for example /dev/ttyUSB0.
	Path string
	// Baud defaults to DefaultBaud when zero.
	Baud int
	// RegistryPath is the device registry file. An empty value uses gzb's
	// standard registry path (or GZB_DB when that environment variable is set).
	RegistryPath string

	// TraceEZSP receives human-readable decoded EZSP frames.
	TraceEZSP func(direction, message string)
	// TraceASH receives human-readable raw ASH frames.
	TraceASH func(direction, frame string)

	// OnUnhandled is called for incoming application frames that do not
	// produce a Reading. It is primarily useful for diagnostic tools.
	OnUnhandled func(Event)
}

// Event describes an incoming frame that did not produce a physical reading.
type Event struct {
	At          time.Time `json:"at"`
	IEEE        string    `json:"ieee,omitempty"`
	DeviceName  string    `json:"device_name,omitempty"`
	NodeID      uint16    `json:"node_id"`
	Cluster     string    `json:"cluster"`
	Description string    `json:"description"`
}

// Reading is one interpreted attribute report from a device.
type Reading struct {
	IEEE       string    `json:"ieee,omitempty"`
	DeviceName string    `json:"device_name,omitempty"`
	Capability string    `json:"capability"`
	Unit       string    `json:"unit,omitempty"`
	Value      float64   `json:"value"`
	At         time.Time `json:"at"`
	LQI        uint8     `json:"lqi"`
	RSSI       int8      `json:"rssi"`

	// NodeID and Cluster retain useful routing and diagnostic context. IEEE is
	// the stable identity; NodeID may change whenever a device rejoins.
	NodeID  uint16 `json:"node_id"`
	Cluster string `json:"cluster"`
}

// connection is the internal surface the high-level coordinator needs. The
// interface also keeps the report loop testable without serial hardware.
type connection interface {
	NetworkState(context.Context) (ezsp.NetworkStatus, error)
	Subscribe(func(ezsp.Message) bool, int) (<-chan ezsp.Message, func())
	SendUnicast(context.Context, uint16, ezsp.APSFrame, uint8, []byte) (uint8, error)
	Request(context.Context, uint16, ezsp.APSFrame, []byte, func(ezsp.IncomingMessage) bool) (ezsp.IncomingMessage, error)
	AllowJoins(context.Context) error
	PermitJoining(context.Context, uint8) error
	Close() error
}

// Coordinator is an open session with a Zigbee network coordinator.
// It is safe to call PermitJoin concurrently with an active Readings loop.
type Coordinator struct {
	conn connection
	db   *store.Store
	opts Options

	mu             sync.Mutex
	closed         bool
	readingsActive bool
	readingsDone   chan struct{}

	// seq is the transaction sequence counter for request/response traffic.
	// It is atomic rather than guarded by mu because discovery queries may run
	// concurrently with each other and with an active readings loop.
	seq atomic.Uint32
}

// Open connects to the adapter and restores its saved network, if any.
func Open(ctx context.Context, opts Options) (*Coordinator, error) {
	db, err := store.Open(opts.RegistryPath)
	if err != nil {
		return nil, err
	}

	connOpts := ezsp.Options{Path: opts.Path, Baud: opts.Baud}
	if opts.TraceEZSP != nil {
		connOpts.TraceEZSP = func(direction string, message ezsp.Message) {
			opts.TraceEZSP(direction, message.String())
		}
	}
	if opts.TraceASH != nil {
		connOpts.TraceASH = func(direction string, frame ash.Frame) {
			opts.TraceASH(direction, frame.String())
		}
	}

	conn, err := ezsp.Open(ctx, connOpts)
	if err != nil {
		return nil, err
	}
	return &Coordinator{conn: conn, db: db, opts: opts}, nil
}

// PermitJoin opens the network to new devices for duration. A zero duration
// closes it. EZSP represents the duration as whole seconds in one byte, so
// values must be between zero and 255 seconds. The protocol reserves 255
// seconds to mean open indefinitely.
func (c *Coordinator) PermitJoin(ctx context.Context, duration time.Duration) error {
	if duration < 0 || duration > 255*time.Second || duration%time.Second != 0 {
		return fmt.Errorf("zigbee: permit-join duration must be a whole number of seconds from 0s through 255s")
	}
	if err := c.checkOpen(); err != nil {
		return err
	}

	state, err := c.conn.NetworkState(ctx)
	if err != nil {
		return fmt.Errorf("zigbee: reading network state: %w", err)
	}
	if !state.Joined() {
		return fmt.Errorf("zigbee: no network on this adapter (%s)", state)
	}

	seconds := uint8(duration / time.Second)
	if seconds != 0 {
		// Trust-centre policy is volatile NCP state and must be restored after
		// every connection before opening a join window.
		if err := c.conn.AllowJoins(ctx); err != nil {
			return fmt.Errorf("zigbee: configuring trust centre to accept joins: %w", err)
		}
	}
	if err := c.conn.PermitJoining(ctx, seconds); err != nil {
		return fmt.Errorf("zigbee: permitting joins: %w", err)
	}
	return nil
}

// Close stops the session, saves the device registry, and releases the serial
// port. It is safe to call more than once.
func (c *Coordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	readingsDone := c.readingsDone
	c.mu.Unlock()

	connErr := c.conn.Close()
	if readingsDone != nil {
		<-readingsDone
	}
	saveErr := c.db.Save()
	return errors.Join(connErr, saveErr)
}

func (c *Coordinator) checkOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	return nil
}
