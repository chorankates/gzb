package ash

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingPort struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (p *recordingPort) Read([]byte) (int, error) { return 0, io.EOF }
func (p *recordingPort) Close() error             { return nil }
func (p *recordingPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.Write(b)
}

func (p *recordingPort) frame(t *testing.T) Frame {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	wire := p.b.Bytes()
	if len(wire) == 0 || wire[len(wire)-1] != FlagByte {
		t.Fatalf("written frame = % X", wire)
	}
	f, err := Decode(wire[:len(wire)-1])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return f
}

func testConn(port io.ReadWriteCloser, rxSeq uint8, dataBuffer int) *Conn {
	return &Conn{
		rw:       port,
		rxSeq:    rxSeq,
		ackCh:    make(chan ackEvent, 8),
		dataCh:   make(chan []byte, dataBuffer),
		resetCh:  make(chan Frame, 4),
		readDone: make(chan struct{}),
	}
}

func TestHandleReACKsPreviousDataFrame(t *testing.T) {
	port := &recordingPort{}
	c := testConn(port, 3, 1)

	c.handle(Frame{Type: FrameData, FrmNum: 2, Data: []byte{0xAA}})

	got := port.frame(t)
	if got.Type != FrameACK || got.AckNum != 3 {
		t.Fatalf("response = %v, want ACK for sequence 3", got)
	}
	select {
	case payload := <-c.dataCh:
		t.Fatalf("duplicate payload was delivered again: % X", payload)
	default:
	}
}

func TestHandleNAKsUnexpectedDataFrame(t *testing.T) {
	port := &recordingPort{}
	c := testConn(port, 3, 1)

	c.handle(Frame{Type: FrameData, FrmNum: 1})

	got := port.frame(t)
	if got.Type != FrameNAK || got.AckNum != 3 {
		t.Fatalf("response = %v, want NAK for sequence 3", got)
	}
}

func TestHandleCountsDroppedData(t *testing.T) {
	port := &recordingPort{}
	c := testConn(port, 0, 0)

	c.handle(Frame{Type: FrameData, FrmNum: 0, Data: []byte{0xAA}})

	if got := c.Stats().DroppedData; got != 1 {
		t.Fatalf("DroppedData = %d, want 1", got)
	}
}

type overlapPort struct {
	active  atomic.Int32
	overlap atomic.Bool
}

func (p *overlapPort) Read([]byte) (int, error) { return 0, io.EOF }
func (p *overlapPort) Close() error             { return nil }
func (p *overlapPort) Write(b []byte) (int, error) {
	if p.active.Add(1) != 1 {
		p.overlap.Store(true)
	}
	time.Sleep(10 * time.Millisecond)
	p.active.Add(-1)
	return len(b), nil
}

func TestWriteSerializesFrames(t *testing.T) {
	port := &overlapPort{}
	c := testConn(port, 0, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := c.write(Frame{Type: FrameACK}); err != nil {
			t.Errorf("write ACK: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := c.write(Frame{Type: FrameNAK}); err != nil {
			t.Errorf("write NAK: %v", err)
		}
	}()
	wg.Wait()

	if port.overlap.Load() {
		t.Fatal("serial writes overlapped")
	}
}
