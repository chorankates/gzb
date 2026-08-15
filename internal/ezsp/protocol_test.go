package ezsp

import (
	"bytes"
	"testing"
)

// The byte counts asserted here were observed on the wire against an EFR32
// running EmberZNet 7.4.4, which negotiated EZSP version 13. They pin down
// the header sizes of both frame layouts.

func TestEncodeLegacyVersionCommand(t *testing.T) {
	// The bootstrap command: always legacy, whatever the NCP goes on to speak.
	got := encodeCommand(LegacyVersion, 0, FrameVersion, []byte{LegacyVersion})
	want := []byte{0x00, 0x00, 0x00, 0x04}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
}

func TestEncodeExtendedCommand(t *testing.T) {
	// Once version 13 is negotiated, headers grow to five bytes and the frame
	// ID becomes little-endian 16-bit.
	got := encodeCommand(13, 2, FrameGetEUI64, nil)
	want := []byte{0x02, 0x00, 0x01, 0x26, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % X, want % X", got, want)
	}
	if len(got) != 5 {
		t.Errorf("extended header is %d bytes, hardware shows 5", len(got))
	}
}

func TestEncodeExtendedCarriesParams(t *testing.T) {
	got := encodeCommand(13, 1, FrameVersion, []byte{13})
	if len(got) != 6 {
		t.Fatalf("got %d bytes, hardware shows 6 for the v13 version command", len(got))
	}
	if got[5] != 13 {
		t.Errorf("parameter byte = %d, want 13", got[5])
	}
}

func TestDecodeLegacyVersionResponse(t *testing.T) {
	// Seven bytes on the wire: three header, four parameters.
	frame := []byte{0x00, 0x80, 0x00, 0x0D, 0x02, 0x40, 0x74}
	msg, err := decodeMessage(LegacyVersion, frame)
	if err != nil {
		t.Fatalf("decodeMessage: %v", err)
	}
	if msg.ID != FrameVersion || msg.Sequence != 0 {
		t.Fatalf("got %v, want the version response at seq 0", msg)
	}
	if len(msg.Params) != 4 {
		t.Fatalf("params = % X, want 4 bytes", msg.Params)
	}

	r := newBuf(msg.Params)
	protocol, stackType, stackVer := r.u8(), r.u8(), r.u16()
	if r.err != nil {
		t.Fatalf("parsing params: %v", r.err)
	}
	if protocol != 13 || stackType != 2 || stackVer != 0x7440 {
		t.Errorf("protocol=%d stackType=%d stackVer=0x%04X, want 13/2/0x7440", protocol, stackType, stackVer)
	}
}

func TestDecodeExtendedResponse(t *testing.T) {
	// Thirteen bytes: five header, eight for the IEEE address.
	frame := []byte{0x02, 0x80, 0x01, 0x26, 0x00, 0x33, 0x83, 0x05, 0xFE, 0xFF, 0xED, 0x2C, 0xC0}
	msg, err := decodeMessage(13, frame)
	if err != nil {
		t.Fatalf("decodeMessage: %v", err)
	}
	if msg.ID != FrameGetEUI64 {
		t.Fatalf("ID = %s, want getEui64", msg.ID)
	}
	if msg.Callback {
		t.Error("an ordinary response was flagged as a callback")
	}

	// EZSP sends an EUI-64 least-significant byte first, so the decoder must
	// reverse it. A correctly ordered address derived from a 48-bit MAC has
	// FF:FE in the middle; a reversed one does not.
	r := newBuf(msg.Params)
	addr := r.ieee()
	if r.err != nil {
		t.Fatalf("parsing address: %v", r.err)
	}
	if got, want := addr.String(), "C0:2C:ED:FF:FE:05:83:33"; got != want {
		t.Errorf("EUI64 = %s, want %s", got, want)
	}
	if addr[3] != 0xFF || addr[4] != 0xFE {
		t.Errorf("address %s lacks the FF:FE marker, so byte order is wrong", addr)
	}
}

func TestDecodeIdentifiesCallbacks(t *testing.T) {
	// Bits 4-5 of the frame control low byte carry the callback type.
	cb := []byte{0x00, 0x90, 0x01, 0x19, 0x00, 0x90}
	msg, err := decodeMessage(13, cb)
	if err != nil {
		t.Fatalf("decodeMessage: %v", err)
	}
	if !msg.Callback {
		t.Errorf("frame control 0x0190 should decode as a callback")
	}
	if msg.ID != FrameStackStatusHandler {
		t.Errorf("ID = %s, want stackStatusHandler", msg.ID)
	}
}

func TestDecodeRejectsTruncatedFrames(t *testing.T) {
	if _, err := decodeMessage(LegacyVersion, []byte{0x00, 0x80}); err == nil {
		t.Error("expected an error for a truncated legacy frame")
	}
	if _, err := decodeMessage(13, []byte{0x00, 0x80, 0x01, 0x26}); err == nil {
		t.Error("expected an error for a truncated extended frame")
	}
}

func TestChannelList(t *testing.T) {
	// The default Zigbee channel mask covers 11 through 26.
	all := ChannelList(0x07FFF800)
	if len(all) != 16 || all[0] != 11 || all[15] != 26 {
		t.Errorf("ChannelList(default mask) = %v, want channels 11-26", all)
	}
	if got := ChannelList(1 << 15); len(got) != 1 || got[0] != 15 {
		t.Errorf("ChannelList(channel 15) = %v", got)
	}
}

// TestNetworkStatusValues pins the EmberNetworkStatus enum down. Reading
// "joined" as 1 rather than 2 makes a live network report itself as still
// joining, which is how this was originally wrong.
func TestNetworkStatusValues(t *testing.T) {
	if NetworkJoined != 0x02 {
		t.Errorf("NetworkJoined = 0x%02X, want 0x02", uint8(NetworkJoined))
	}
	if NetworkJoining != 0x01 {
		t.Errorf("NetworkJoining = 0x%02X, want 0x01", uint8(NetworkJoining))
	}
	if !NetworkJoined.Joined() {
		t.Error("NetworkJoined must report Joined")
	}
	if !NetworkNoParent.Joined() {
		t.Error("NetworkNoParent must report Joined: credentials are held")
	}
	if NetworkJoining.Joined() {
		t.Error("NetworkJoining must not report Joined: the network is not up yet")
	}
	if NetworkNone.Joined() {
		t.Error("NetworkNone must not report Joined")
	}
}
