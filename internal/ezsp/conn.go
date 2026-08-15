package ezsp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/conor/gzb/internal/ash"
	"go.bug.st/serial"
)

// DefaultBaud is the rate EmberZNet NCP firmware uses on the EFR32.
const DefaultBaud = 115200

// CommandTimeout bounds how long the NCP may take to answer. The ASH layer
// already retries at the frame level, so exceeding this means the NCP
// accepted the frame but never produced a response.
const CommandTimeout = 5 * time.Second

var (
	// ErrClosed is returned once the connection is shut down.
	ErrClosed = errors.New("ezsp: connection closed")
	// ErrTimeout is returned when the NCP does not answer a command.
	ErrTimeout = errors.New("ezsp: timed out waiting for NCP")
)

// Tracer observes decoded EZSP traffic.
type Tracer func(direction string, m Message)

// Options configures a connection.
type Options struct {
	// Path is the serial device, e.g. /dev/ttyUSB0.
	Path string
	// Baud defaults to DefaultBaud when zero.
	Baud int
	// TraceEZSP, when set, reports every decoded EZSP frame.
	TraceEZSP Tracer
	// TraceASH, when set, reports every ASH frame beneath it.
	TraceASH ash.Tracer
}

// Conn is a negotiated EZSP session with an NCP. It is safe for concurrent
// use; commands are serialised because the NCP handles one at a time.
type Conn struct {
	link  *ash.Conn
	trace Tracer

	// cmdMu serialises whole command/response exchanges.
	cmdMu sync.Mutex

	mu       sync.Mutex
	version  int
	seq      uint8
	pending  chan Message
	subs     map[int]*subscription
	nextSub  int
	closed   bool
	stackVer uint16
	stackTyp uint8

	readDone chan struct{}
}

type subscription struct {
	match func(Message) bool
	ch    chan Message
}

// Open connects to the NCP, resets the ASH link and negotiates the EZSP
// protocol version. The returned connection is ready for commands.
func Open(ctx context.Context, opts Options) (*Conn, error) {
	baud := opts.Baud
	if baud == 0 {
		baud = DefaultBaud
	}

	port, err := serial.Open(opts.Path, &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("ezsp: opening %s: %w", opts.Path, err)
	}
	// On this dongle the USB bridge's DTR and RTS lines are wired to the
	// EFR32's reset and bootloader pins. Deassert both so the chip runs the
	// application firmware rather than sitting in its bootloader.
	if err := port.SetDTR(false); err != nil {
		port.Close()
		return nil, fmt.Errorf("ezsp: clearing DTR: %w", err)
	}
	if err := port.SetRTS(false); err != nil {
		port.Close()
		return nil, fmt.Errorf("ezsp: clearing RTS: %w", err)
	}

	c := &Conn{
		link:     ash.NewConn(port, opts.TraceASH),
		trace:    opts.TraceEZSP,
		version:  LegacyVersion,
		subs:     make(map[int]*subscription),
		readDone: make(chan struct{}),
	}
	go c.readPump()

	if _, err := c.link.Reset(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("ezsp: resetting ASH link: %w", err)
	}
	if err := c.negotiate(ctx); err != nil {
		c.Close()
		return nil, err
	}
	// Resetting the ASH link resets the NCP, which comes back with its radio
	// idle even when credentials are saved in its tokens. Restoring them is
	// what makes an existing network survive a reconnect; it creates nothing
	// and changes nothing when no network is stored.
	if err := c.restoreNetwork(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// restoreNetwork brings a previously formed network back up. A fresh adapter
// with nothing stored reports StatusNotJoined, which is the expected answer
// and not an error.
func (c *Conn) restoreNetwork(ctx context.Context) error {
	up, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && m.ID == FrameStackStatusHandler
	}, 4)
	defer cancel()

	status, err := c.NetworkInit(ctx)
	if err != nil {
		return fmt.Errorf("ezsp: restoring saved network: %w", err)
	}
	if status == StatusNotJoined {
		return nil // nothing stored; the adapter has no network
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: networkInit: %s", status)
	}

	// The stack reports the network up asynchronously. Missing the callback
	// is not fatal — the caller can still query state — so this waits only
	// briefly rather than failing a connection that is otherwise usable.
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	for {
		select {
		case m, ok := <-up:
			if !ok {
				return nil
			}
			r := newBuf(m.Params)
			if s := EmberStatus(r.u8()); r.err == nil && s == stackStatusNetworkUp {
				return nil
			}
		case <-waitCtx.Done():
			return nil
		}
	}
}

// ProtocolVersion reports the negotiated EZSP version.
func (c *Conn) ProtocolVersion() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// StackVersion reports the NCP's EmberZNet stack version and type.
func (c *Conn) StackVersion() (version uint16, stackType uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackVer, c.stackTyp
}

// negotiate performs the version handshake that fixes the frame layout.
func (c *Conn) negotiate(ctx context.Context) error {
	// The first command is always sent in the legacy layout.
	got, err := c.versionExchange(ctx, LegacyVersion)
	if err != nil {
		return fmt.Errorf("ezsp: version handshake: %w", err)
	}

	if got == LegacyVersion {
		return nil
	}

	// The NCP speaks a newer protocol. Adopt its version, which may switch us
	// to the extended frame layout, and repeat the handshake so both sides
	// agree on the layout everything after this point will use.
	c.mu.Lock()
	c.version = int(got)
	c.mu.Unlock()

	confirmed, err := c.versionExchange(ctx, int(got))
	if err != nil {
		return fmt.Errorf("ezsp: confirming version %d: %w", got, err)
	}
	if confirmed != got {
		return fmt.Errorf("ezsp: NCP offered version %d then reported %d", got, confirmed)
	}
	return nil
}

// versionExchange issues one version command and records what came back.
func (c *Conn) versionExchange(ctx context.Context, desired int) (uint8, error) {
	params, err := c.command(ctx, FrameVersion, []byte{byte(desired)})
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	protocol := r.u8()
	stackType := r.u8()
	stackVer := r.u16()
	if r.err != nil {
		return 0, r.err
	}

	c.mu.Lock()
	c.stackTyp = stackType
	c.stackVer = stackVer
	c.mu.Unlock()
	return protocol, nil
}

// readPump decodes NCP traffic and routes it.
func (c *Conn) readPump() {
	defer close(c.readDone)
	for payload := range c.link.Data() {
		c.mu.Lock()
		version := c.version
		c.mu.Unlock()

		msg, err := decodeMessage(version, payload)
		if err != nil {
			continue
		}
		if c.trace != nil {
			c.trace("<-", msg)
		}
		c.route(msg)
	}
}

// route delivers a message to the waiting command or to subscribers.
func (c *Conn) route(msg Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A reply to the command in flight takes precedence; anything else is an
	// asynchronous event.
	if !msg.Callback && c.pending != nil {
		select {
		case c.pending <- msg:
			return
		default:
		}
	}
	for _, sub := range c.subs {
		if !sub.match(msg) {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
			// Drop rather than stall the read pump for every other consumer.
		}
	}
}

// Call issues an arbitrary EZSP command and returns the raw response
// parameters, undecoded. It is the escape hatch beneath every typed command:
// anything the NCP implements is reachable through it, including commands
// this package does not model.
func (c *Conn) Call(ctx context.Context, id FrameID, params []byte) ([]byte, error) {
	return c.command(ctx, id, params)
}

// command issues one EZSP command and returns the response parameters.
func (c *Conn) command(ctx context.Context, id FrameID, params []byte) ([]byte, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	reply := make(chan Message, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	seq := c.seq
	c.seq++
	version := c.version
	c.pending = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.pending = nil
		c.mu.Unlock()
	}()

	frame := encodeCommand(version, seq, id, params)
	if c.trace != nil {
		c.trace("->", Message{Sequence: seq, ID: id, Params: params})
	}
	if err := c.link.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("ezsp: sending %s: %w", id, err)
	}

	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()

	for {
		select {
		case msg := <-reply:
			if msg.ID != id {
				// A stale reply from an earlier command; keep waiting.
				continue
			}
			return msg.Params, nil
		case <-c.readDone:
			return nil, ErrClosed
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: no response to %s", ErrTimeout, id)
			}
			return nil, ctx.Err()
		}
	}
}

// Subscribe returns a channel of messages satisfying match, plus a function
// to cancel the subscription.
func (c *Conn) Subscribe(match func(Message) bool, buffer int) (<-chan Message, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	sub := &subscription{match: match, ch: make(chan Message, buffer)}

	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = sub
	c.mu.Unlock()

	return sub.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(sub.ch)
		}
	}
}

// Close shuts down the session and releases the port.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	for id, sub := range c.subs {
		delete(c.subs, id)
		close(sub.ch)
	}
	c.mu.Unlock()

	err := c.link.Close()
	<-c.readDone
	return err
}
