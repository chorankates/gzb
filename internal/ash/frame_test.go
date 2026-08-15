package ash

import (
	"bytes"
	"errors"
	"testing"
)

func TestCRCMatchesHardware(t *testing.T) {
	// Both vectors were captured from a live EFR32 dongle, so they pin the
	// CRC variant down to the exact one the silicon uses.
	if got := crc16([]byte{0xC0}); got != 0x38BC {
		t.Errorf("crc16(RST) = 0x%04X, want 0x38BC", got)
	}
	if got := crc16([]byte{0xC1, 0x02, 0x02}); got != 0x9B7B {
		t.Errorf("crc16(RSTACK) = 0x%04X, want 0x9B7B", got)
	}
}

func TestEncodeRST(t *testing.T) {
	got, err := Encode(Frame{Type: FrameRST})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// The cancel byte that conventionally precedes RST is added by Conn.
	want := []byte{0xC0, 0x38, 0xBC, 0x7E}
	if !bytes.Equal(got, want) {
		t.Errorf("Encode(RST) = % X, want % X", got, want)
	}
}

func TestDecodeRSTACKFromHardware(t *testing.T) {
	// Exactly what the dongle sent, minus the leading cancel byte and the
	// trailing flag, both of which the framing reader strips.
	f, err := Decode([]byte{0xC1, 0x02, 0x02, 0x9B, 0x7B})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if f.Type != FrameRSTACK {
		t.Fatalf("Type = %s, want RSTACK", f.Type)
	}
	if len(f.Data) != 2 || f.Data[0] != 0x02 || ResetCode(f.Data[1]) != ResetPowerOn {
		t.Errorf("Data = % X, want ASH version 2 and a power-on reset", f.Data)
	}
}

// TestRandomizeMatchesSpec checks the LFSR against the sequence Silicon Labs
// documents for the EZSP version command, which is the canonical worked
// example in UG101. An all-zero payload exposes the raw generator output.
func TestRandomizeMatchesSpec(t *testing.T) {
	got := randomize([]byte{0x00, 0x00, 0x00, 0x04})
	want := []byte{0x42, 0x21, 0xA8, 0x50}
	if !bytes.Equal(got, want) {
		t.Errorf("randomize = % X, want % X", got, want)
	}
}

func TestRandomizeIsItsOwnInverse(t *testing.T) {
	orig := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0xFF, 0x7E, 0x11}
	if got := randomize(randomize(orig)); !bytes.Equal(got, orig) {
		t.Errorf("double randomize = % X, want % X", got, orig)
	}
}

func TestStuffEscapesEveryReservedByte(t *testing.T) {
	reserved := []byte{FlagByte, EscapeByte, XOnByte, XOffByte, SubstByte, CancelByte}
	got := stuff(reserved)

	// Only the terminating flag may survive unescaped.
	for i, b := range got[:len(got)-1] {
		if b == FlagByte {
			t.Fatalf("unescaped flag byte at offset %d in % X", i, got)
		}
	}
	if got[len(got)-1] != FlagByte {
		t.Fatalf("frame does not end with a flag: % X", got)
	}

	back, err := unstuff(got[:len(got)-1])
	if err != nil {
		t.Fatalf("unstuff: %v", err)
	}
	if !bytes.Equal(back, reserved) {
		t.Errorf("round trip = % X, want % X", back, reserved)
	}
}

func TestDataFrameRoundTrip(t *testing.T) {
	for frm := uint8(0); frm < 8; frm++ {
		for _, retx := range []bool{false, true} {
			in := Frame{
				Type:   FrameData,
				FrmNum: frm,
				AckNum: (frm + 3) % 8,
				ReTx:   retx,
				// Include reserved bytes so stuffing is exercised too.
				Data: []byte{0x00, 0x7E, 0x7D, 0x11, 0x13, 0x18, 0x1A, 0xFF},
			}
			wire, err := Encode(in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if wire[len(wire)-1] != FlagByte {
				t.Fatalf("frame does not end with flag: % X", wire)
			}
			out, err := Decode(wire[:len(wire)-1])
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if out.Type != FrameData || out.FrmNum != in.FrmNum || out.AckNum != in.AckNum || out.ReTx != in.ReTx {
				t.Errorf("header round trip: got %v, want %v", out, in)
			}
			if !bytes.Equal(out.Data, in.Data) {
				t.Errorf("payload round trip = % X, want % X", out.Data, in.Data)
			}
		}
	}
}

func TestAckNakRoundTrip(t *testing.T) {
	for _, typ := range []FrameType{FrameACK, FrameNAK} {
		for ack := uint8(0); ack < 8; ack++ {
			for _, nrdy := range []bool{false, true} {
				in := Frame{Type: typ, AckNum: ack, NotReady: nrdy}
				wire, err := Encode(in)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				out, err := Decode(wire[:len(wire)-1])
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if out.Type != typ || out.AckNum != ack || out.NotReady != nrdy {
					t.Errorf("got %v, want %v", out, in)
				}
			}
		}
	}
}

func TestDecodeRejectsBadCRC(t *testing.T) {
	_, err := Decode([]byte{0xC1, 0x02, 0x02, 0x9B, 0x00})
	if !errors.Is(err, ErrBadCRC) {
		t.Fatalf("err = %v, want ErrBadCRC", err)
	}
}

func TestDecodeRejectsShortFrame(t *testing.T) {
	if _, err := Decode([]byte{0xC1, 0x02}); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("err = %v, want ErrShortFrame", err)
	}
}

// TestVersionCommandFrame builds the frame we will actually send first, so a
// regression in any layer shows up here rather than as a silent timeout.
func TestVersionCommandFrame(t *testing.T) {
	// Legacy EZSP version command: seq 0, control 0, frame ID 0, version 4.
	got, err := Encode(Frame{Type: FrameData, FrmNum: 0, AckNum: 0, Data: []byte{0x00, 0x00, 0x00, 0x04}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{0x00, 0x42, 0x21, 0xA8, 0x50, 0xED, 0x2C, 0x7E}
	if !bytes.Equal(got, want) {
		t.Errorf("version command = % X, want % X", got, want)
	}
}
