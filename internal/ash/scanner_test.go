package ash

import (
	"bytes"
	"io"
	"testing"
)

// chunkReader hands out a fixed script of reads, so a frame can be split
// across reads exactly where it would hurt most.
type chunkReader struct {
	chunks [][]byte
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	if n < len(c.chunks[0]) {
		c.chunks[0] = c.chunks[0][n:]
	} else {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}

func collect(t *testing.T, r io.Reader) [][]byte {
	t.Helper()
	sc := newScanner(r)
	var out [][]byte
	for {
		f, err := sc.next()
		if err != nil {
			return out
		}
		if f != nil {
			out = append(out, f)
		}
	}
}

func TestScannerReadsBackToBackFramesInOneRead(t *testing.T) {
	// Two complete frames arriving in a single read: the regression that
	// silently loses the second one.
	stream := []byte{
		0xC1, 0x02, 0x02, 0x9B, 0x7B, FlagByte,
		0xC0, 0x38, 0xBC, FlagByte,
	}
	got := collect(t, bytes.NewReader(stream))
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2: %v", len(got), got)
	}
	if !bytes.Equal(got[0], []byte{0xC1, 0x02, 0x02, 0x9B, 0x7B}) {
		t.Errorf("frame 0 = % X", got[0])
	}
	if !bytes.Equal(got[1], []byte{0xC0, 0x38, 0xBC}) {
		t.Errorf("frame 1 = % X", got[1])
	}
}

func TestScannerReassemblesSplitFrame(t *testing.T) {
	r := &chunkReader{chunks: [][]byte{
		{0xC1, 0x02},
		{0x02, 0x9B},
		{0x7B, FlagByte},
	}}
	got := collect(t, r)
	if len(got) != 1 || !bytes.Equal(got[0], []byte{0xC1, 0x02, 0x02, 0x9B, 0x7B}) {
		t.Fatalf("got %v, want one reassembled frame", got)
	}
}

func TestScannerCancelDiscardsPartialFrame(t *testing.T) {
	// Garbage, then a cancel, then a real frame — exactly what the dongle
	// sends when it resets mid-transfer.
	stream := []byte{0xAA, 0xBB, CancelByte, 0xC1, 0x02, 0x02, 0x9B, 0x7B, FlagByte}
	got := collect(t, bytes.NewReader(stream))
	if len(got) != 1 || !bytes.Equal(got[0], []byte{0xC1, 0x02, 0x02, 0x9B, 0x7B}) {
		t.Fatalf("got %v, want the frame after the cancel byte", got)
	}
}

func TestScannerDropsSubstitutedFrame(t *testing.T) {
	stream := []byte{
		0xC1, SubstByte, 0x02, 0x9B, 0x7B, FlagByte, // corrupt, must be dropped
		0xC0, 0x38, 0xBC, FlagByte, // and the next one must still arrive
	}
	got := collect(t, bytes.NewReader(stream))
	if len(got) != 1 || !bytes.Equal(got[0], []byte{0xC0, 0x38, 0xBC}) {
		t.Fatalf("got %v, want only the second frame", got)
	}
}

func TestScannerIgnoresFlowControlBytes(t *testing.T) {
	stream := []byte{0xC0, XOnByte, 0x38, XOffByte, 0xBC, FlagByte}
	got := collect(t, bytes.NewReader(stream))
	if len(got) != 1 || !bytes.Equal(got[0], []byte{0xC0, 0x38, 0xBC}) {
		t.Fatalf("got %v, want flow-control bytes stripped", got)
	}
}

func TestScannerSkipsEmptyFrames(t *testing.T) {
	stream := []byte{FlagByte, FlagByte, 0xC0, 0x38, 0xBC, FlagByte}
	got := collect(t, bytes.NewReader(stream))
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
}
