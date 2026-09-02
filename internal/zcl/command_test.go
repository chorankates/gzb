package zcl

import (
	"bytes"
	"testing"
)

// A cluster-specific command differs from a profile-wide one by a single bit
// in the frame control field, and everything after that bit changes meaning.
// Getting it wrong does not fail loudly: command 0x06 is a valid profile-wide
// command too, so the light would be sent a well-formed Configure Reporting.
func TestClusterCommandRequestSetsTheClusterSpecificBit(t *testing.T) {
	got := ClusterCommandRequest(0x07, CmdMoveToHueAndSaturation, []byte{0x00, 0xFE, 0x0A, 0x00})
	want := []byte{
		0x01, // cluster-specific, client to server, default response enabled
		0x07,
		CmdMoveToHueAndSaturation,
		0x00, 0xFE, 0x0A, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = % X, want % X", got, want)
	}
	if FrameType(got[0]&fcTypeMask) != FrameClusterSpecific {
		t.Errorf("frame type = %d, want cluster-specific", got[0]&fcTypeMask)
	}
}

// A command has no response of its own, so the Default Response is the only
// evidence it was accepted. Suppressing it would make a refused command look
// exactly like one that worked.
func TestClusterCommandRequestKeepsTheDefaultResponse(t *testing.T) {
	got := ClusterCommandRequest(0x01, CmdOn, nil)
	if got[0]&fcDisableDefaultResponse != 0 {
		t.Errorf("frame control = 0x%02X, want the default response left enabled", got[0])
	}
	if got[0]&fcDirection != 0 {
		t.Errorf("frame control = 0x%02X, want client to server", got[0])
	}
}

// The payload layouts are from the ZCL specification's field tables. An
// encoder checked only against its own decoder is free to be wrong twice.
func TestCommandPayloadEncodings(t *testing.T) {
	for _, c := range []struct {
		name string
		got  []byte
		want []byte
	}{
		{
			// Move to Level: level, then transition in tenths of a second.
			"move to level",
			MoveToLevelPayload(63, 10),
			[]byte{0x3F, 0x0A, 0x00},
		},
		{
			// Step: mode, size, transition. Down is 0x01.
			"step down",
			StepLevelPayload(StepDown, 64, 4),
			[]byte{0x01, 0x40, 0x04, 0x00},
		},
		{
			// Move to Hue and Saturation: hue, saturation, transition. Red is
			// hue 0 at full saturation, and full saturation is 254.
			"move to hue and saturation",
			MoveToHueAndSaturationPayload(0, 254, 10),
			[]byte{0x00, 0xFE, 0x0A, 0x00},
		},
		{
			// Move to Color: x and y as fractions of 65536, then transition.
			"move to color",
			MoveToColorPayload(41943, 21627, 0),
			[]byte{0xD7, 0xA3, 0x7B, 0x54, 0x00, 0x00},
		},
		{
			"move to color temperature",
			MoveToColorTemperaturePayload(370, 10),
			[]byte{0x72, 0x01, 0x0A, 0x00},
		},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s = % X, want % X", c.name, c.got, c.want)
		}
	}
}

// A command ID means nothing without its cluster. Naming one with the
// profile-wide table is how a refused Move to Hue and Saturation comes back
// reported as "configure reporting", pointing at the wrong cluster entirely.
func TestClusterCommandNameNeedsItsCluster(t *testing.T) {
	if got := ClusterCommandName(ClusterColorControl, 0x06); got != "move to hue and saturation" {
		t.Errorf("colour command 6 = %q", got)
	}
	if got := ClusterCommandName(ClusterLevelControl, 0x06); got != "step level (with on/off)" {
		t.Errorf("level command 6 = %q", got)
	}
	if got := CommandName(0x06); got != "configure reporting" {
		t.Errorf("profile-wide command 6 = %q, and this is the one that is not a light command", got)
	}
	// An unmodelled command still has to render as something a person can
	// look up, rather than as a bare number.
	if got := ClusterCommandName(ClusterColorControl, 0x4B); got == "" {
		t.Error("an unknown cluster command rendered as nothing at all")
	}
}
