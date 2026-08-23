package ash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Timing and retry limits. The ASH specification uses an adaptive
// acknowledgement timeout; a fixed value at the top of its range is simpler
// and behaves identically on a local USB link, where round trips are
// sub-millisecond and a timeout means something is genuinely wrong.
const (
	// AckTimeout bounds how long we wait for a DATA frame to be acknowledged.
	AckTimeout = 1200 * time.Millisecond
	// MaxRetries is how many times a DATA frame is retransmitted before the
	// link is declared dead.
	MaxRetries = 3
	// ResetTimeout bounds how long the NCP may take to send RSTACK.
	ResetTimeout = 5 * time.Second
)

var (
	// ErrClosed is returned once the connection is shut down.
	ErrClosed = errors.New("ash: connection closed")
	// ErrLinkFailed means a frame went unacknowledged after every retry.
	ErrLinkFailed = errors.New("ash: no acknowledgement after retries")
	// ErrNCPReset means the NCP restarted underneath us and the session is
	// no longer valid.
	ErrNCPReset = errors.New("ash: NCP reset")
)

// Tracer observes every frame crossing the link. Direction is "->" for
// host-to-NCP and "<-" for the reverse.
type Tracer func(direction string, f Frame)

// Conn is an ASH session over a serial port. It owns sequence numbering,
// acknowledgement and retransmission, and hands decoded EZSP payloads to
// its consumer. It is safe for concurrent use.
type Conn struct {
	rw    io.ReadWriteCloser
	trace Tracer

	// sendMu serialises whole send-and-await-ACK exchanges. ASH permits a
	// sliding window, but a window of one is sufficient for a host client and
	// removes an entire class of ordering bug.
	sendMu sync.Mutex

	// writeMu prevents acknowledgements emitted by the read pump from being
	// interleaved with DATA or reset frames written by callers.
	writeMu sync.Mutex

	mu     sync.Mutex
	txSeq  uint8 // frame number for our next outgoing DATA frame
	rxSeq  uint8 // frame number we next expect from the NCP
	closed bool

	ackCh    chan ackEvent
	dataCh   chan []byte
	resetCh  chan Frame
	readDone chan struct{}
	readErr  error

	droppedData  atomic.Uint64
	droppedACK   atomic.Uint64
	droppedReset atomic.Uint64
}

// Stats reports loss observed inside the ASH session.
type Stats struct {
	DroppedData  uint64
	DroppedACK   uint64
	DroppedReset uint64
}

// ackEvent reports an acknowledgement observed by the read pump. ASH
// piggybacks ackNum on DATA frames, so acknowledgements arrive on ACK, NAK
// and DATA frames alike.
type ackEvent struct {
	ackNum uint8
	nak    bool
}

// NewConn starts an ASH session over rw. Call Reset before sending data: the
// NCP ignores DATA frames until the link has been reset and sequence numbers
// on both sides agree.
func NewConn(rw io.ReadWriteCloser, trace Tracer) *Conn {
	c := &Conn{
		rw:       rw,
		trace:    trace,
		ackCh:    make(chan ackEvent, 8),
		dataCh:   make(chan []byte, 64),
		resetCh:  make(chan Frame, 4),
		readDone: make(chan struct{}),
	}
	go c.readPump()
	return c
}

// Data returns the stream of EZSP payloads received from the NCP. The channel
// closes when the connection does.
func (c *Conn) Data() <-chan []byte { return c.dataCh }

// Stats returns a point-in-time snapshot of ASH delivery counters.
func (c *Conn) Stats() Stats {
	return Stats{
		DroppedData:  c.droppedData.Load(),
		DroppedACK:   c.droppedACK.Load(),
		DroppedReset: c.droppedReset.Load(),
	}
}

// readPump owns all reads for the life of the connection.
func (c *Conn) readPump() {
	defer close(c.readDone)
	defer close(c.dataCh)

	sc := newScanner(c.rw)
	for {
		raw, err := sc.next()
		if err != nil {
			c.fail(err)
			return
		}
		if raw == nil {
			// A frame the scanner discarded as corrupt. The NCP will resend.
			continue
		}
		f, err := Decode(raw)
		if err != nil {
			// A bad CRC is expected occasionally on a serial link. NAK so the
			// NCP retransmits rather than waiting for a timeout.
			if errors.Is(err, ErrBadCRC) {
				c.mu.Lock()
				expect := c.rxSeq
				c.mu.Unlock()
				c.write(Frame{Type: FrameNAK, AckNum: expect})
			}
			continue
		}
		if c.trace != nil {
			c.trace("<-", f)
		}
		c.handle(f)
	}
}

// handle processes one decoded inbound frame.
func (c *Conn) handle(f Frame) {
	switch f.Type {
	case FrameData:
		// Every DATA frame carries an acknowledgement for our traffic.
		c.signalAck(ackEvent{ackNum: f.AckNum})

		c.mu.Lock()
		expect := c.rxSeq
		c.mu.Unlock()

		if f.FrmNum != expect {
			// Out of order. A repeat of the previous frame is a retransmission
			// whose ACK we lost, so re-ACK it; anything else needs a NAK.
			if f.FrmNum == (expect+7)%8 {
				c.write(Frame{Type: FrameACK, AckNum: expect})
			} else {
				c.write(Frame{Type: FrameNAK, AckNum: expect})
			}
			return
		}

		c.mu.Lock()
		c.rxSeq = (c.rxSeq + 1) % 8
		next := c.rxSeq
		c.mu.Unlock()

		c.write(Frame{Type: FrameACK, AckNum: next})

		select {
		case c.dataCh <- f.Data:
		default:
			// Never stall the read pump on a slow consumer; stalling here
			// would deadlock acknowledgement for every subsequent frame.
			c.droppedData.Add(1)
		}

	case FrameACK:
		c.signalAck(ackEvent{ackNum: f.AckNum})

	case FrameNAK:
		c.signalAck(ackEvent{ackNum: f.AckNum, nak: true})

	case FrameRSTACK, FrameError:
		select {
		case c.resetCh <- f:
		default:
			c.droppedReset.Add(1)
		}
	}
}

func (c *Conn) signalAck(ev ackEvent) {
	select {
	case c.ackCh <- ev:
	default:
		c.droppedACK.Add(1)
	}
}

// write encodes and transmits a frame.
func (c *Conn) write(f Frame) error {
	buf, err := Encode(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.trace != nil {
		c.trace("->", f)
	}
	_, err = c.rw.Write(buf)
	return err
}

// Reset restarts the ASH link: it cancels any partial frame, sends RST, and
// waits for the NCP's RSTACK. Sequence numbers on both sides return to zero.
func (c *Conn) Reset(ctx context.Context) (ResetCode, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	// Drain any stale reset notification from a previous attempt.
	for len(c.resetCh) > 0 {
		<-c.resetCh
	}

	buf, err := Encode(Frame{Type: FrameRST})
	if err != nil {
		return 0, err
	}
	// The cancel byte tells the NCP to discard any partial frame it is
	// holding, so RST is parsed cleanly even if we interrupted a transfer.
	c.writeMu.Lock()
	if _, err := c.rw.Write(append([]byte{CancelByte}, buf...)); err != nil {
		c.writeMu.Unlock()
		return 0, fmt.Errorf("ash: sending RST: %w", err)
	}
	if c.trace != nil {
		c.trace("->", Frame{Type: FrameRST})
	}
	c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, ResetTimeout)
	defer cancel()

	for {
		select {
		case f := <-c.resetCh:
			if f.Type == FrameError {
				code := ResetCode(0)
				if len(f.Data) == 2 {
					code = ResetCode(f.Data[1])
				}
				return code, fmt.Errorf("ash: NCP reported an error: %s", code)
			}
			c.mu.Lock()
			c.txSeq, c.rxSeq = 0, 0
			c.mu.Unlock()

			code := ResetCode(0)
			if len(f.Data) == 2 {
				code = ResetCode(f.Data[1])
			}
			return code, nil
		case <-c.readDone:
			return 0, c.err()
		case <-ctx.Done():
			return 0, fmt.Errorf("ash: no RSTACK from NCP: %w", ctx.Err())
		}
	}
}

// Send transmits an EZSP payload as a DATA frame and waits for it to be
// acknowledged, retransmitting as needed.
func (c *Conn) Send(ctx context.Context, payload []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	frm, ack := c.txSeq, c.rxSeq
	c.mu.Unlock()

	// The NCP acknowledges frame N by asking for N+1.
	wantAck := (frm + 1) % 8

	// Clear stale acknowledgements so we cannot mistake one for ours.
	for len(c.ackCh) > 0 {
		<-c.ackCh
	}

	f := Frame{Type: FrameData, FrmNum: frm, AckNum: ack, Data: payload}
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			f.ReTx = true
			// Refresh the piggybacked ACK; we may have received frames since.
			c.mu.Lock()
			f.AckNum = c.rxSeq
			c.mu.Unlock()
		}
		if err := c.write(f); err != nil {
			return fmt.Errorf("ash: writing DATA frame: %w", err)
		}

		timer := time.NewTimer(AckTimeout)
		acked, err := c.awaitAck(ctx, timer.C, wantAck)
		timer.Stop()
		if err != nil {
			return err
		}
		if acked {
			c.mu.Lock()
			c.txSeq = wantAck
			c.mu.Unlock()
			return nil
		}
	}
	return ErrLinkFailed
}

// awaitAck waits for an acknowledgement of wantAck. It reports false when the
// frame should be retransmitted, either after a NAK or a timeout.
func (c *Conn) awaitAck(ctx context.Context, timeout <-chan time.Time, wantAck uint8) (bool, error) {
	for {
		select {
		case ev := <-c.ackCh:
			if ev.nak {
				return false, nil
			}
			if ev.ackNum == wantAck {
				return true, nil
			}
			// An acknowledgement for something else; keep waiting.
		case f := <-c.resetCh:
			return false, fmt.Errorf("%w: %s", ErrNCPReset, f)
		case <-timeout:
			return false, nil
		case <-c.readDone:
			return false, c.err()
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.readErr == nil && !c.closed {
		c.readErr = err
	}
	c.mu.Unlock()
}

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil && !errors.Is(c.readErr, io.EOF) {
		return fmt.Errorf("ash: link lost: %w", c.readErr)
	}
	return ErrClosed
}

// Close shuts down the link and releases the underlying port.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	err := c.rw.Close()
	<-c.readDone
	return err
}

// scanner splits a byte stream into ASH frame bodies.
//
// The wire is not self-describing: frames end at a flag byte, and three
// control bytes can appear at any point. Cancel discards whatever has
// accumulated, substitute marks the frame corrupt so it is dropped at the
// next flag, and the software flow-control bytes are simply not data.
type scanner struct {
	r   io.Reader
	buf []byte
	// pend holds bytes read but not yet consumed. A single read can contain
	// the end of one frame and the start of the next, so next must resume
	// mid-buffer rather than reading again and losing the remainder.
	pend    []byte
	err     error
	frame   []byte
	corrupt bool
}

func newScanner(r io.Reader) *scanner {
	return &scanner{r: r, buf: make([]byte, 512)}
}

// next returns the next frame body, without its flag byte. A nil slice with a
// nil error means a frame was discarded as corrupt, so callers should test
// for nil rather than assuming a frame is present.
func (s *scanner) next() ([]byte, error) {
	for {
		for len(s.pend) > 0 {
			b := s.pend[0]
			s.pend = s.pend[1:]

			switch b {
			case CancelByte:
				// Discard the partial frame in progress and resynchronise.
				s.frame = s.frame[:0]
				s.corrupt = false
			case SubstByte:
				// A low-level error corrupted this frame; drop it at the flag.
				s.corrupt = true
			case XOnByte, XOffByte:
				// Software flow control, not frame content.
			case FlagByte:
				corrupt, empty := s.corrupt, len(s.frame) == 0
				out := append([]byte(nil), s.frame...)
				s.frame = s.frame[:0]
				s.corrupt = false
				if corrupt || empty {
					return nil, nil
				}
				return out, nil
			default:
				if len(s.frame) >= MaxFrameLen {
					// Runaway frame: drop it and resynchronise at the next flag.
					s.frame = s.frame[:0]
					s.corrupt = true
					continue
				}
				s.frame = append(s.frame, b)
			}
		}

		// Surface a read error only once its bytes have been consumed.
		if s.err != nil {
			return nil, s.err
		}
		n, err := s.r.Read(s.buf)
		// pend aliases buf, but the loop above drains it fully before the
		// next Read, so buf is never overwritten while pend is live.
		s.pend = s.buf[:n]
		if err != nil {
			s.err = err
		} else if n == 0 {
			s.err = io.EOF
		}
	}
}
