package main

import (
	"strings"
	"testing"

	"github.com/chorankates/gzb/internal/ezsp"
)

func heard(channel uint8, pan uint16, open bool) ezsp.FoundNetwork {
	return ezsp.FoundNetwork{
		Channel:       channel,
		PanID:         pan,
		ExtendedPanID: ezsp.EUI64{0x4B, 0xF9, 0x3D, 0xC3, 0x75, 0x45, 0x69, uint8(pan)},
		AllowingJoin:  open,
		StackProfile:  2,
	}
}

// "Nothing to join" has three causes — nothing in range, a network that is
// closed, and too many that are open — and each wants a different action, so
// each must be told apart in the error.
func TestChooseNetwork(t *testing.T) {
	tests := []struct {
		name    string
		found   []ezsp.FoundNetwork
		panID   uint16
		wantPan uint16
		wantErr string
	}{
		{name: "nothing heard", wantErr: "no Zigbee networks heard"},
		{name: "the one open network", found: []ezsp.FoundNetwork{heard(15, 0x6F88, true)}, wantPan: 0x6F88},
		{name: "one open among closed", found: []ezsp.FoundNetwork{heard(11, 0x1111, false), heard(15, 0x6F88, true)}, wantPan: 0x6F88},
		{name: "only closed networks", found: []ezsp.FoundNetwork{heard(15, 0x6F88, false)}, wantErr: "none is accepting joins"},
		{name: "several open", found: []ezsp.FoundNetwork{heard(15, 0x6F88, true), heard(20, 0x2222, true)}, wantErr: "--pan-id"},
		{name: "pan id picks", found: []ezsp.FoundNetwork{heard(15, 0x6F88, true), heard(20, 0x2222, true)}, panID: 0x2222, wantPan: 0x2222},
		{name: "pan id not heard", found: []ezsp.FoundNetwork{heard(15, 0x6F88, true)}, panID: 0x2222, wantErr: "no network with PAN ID 0x2222"},
		{name: "pan id heard but closed", found: []ezsp.FoundNetwork{heard(15, 0x6F88, false), heard(20, 0x2222, true)}, panID: 0x6F88, wantErr: "none is accepting joins"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseNetwork(tt.found, tt.panID)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseNetwork: %v", err)
			}
			if got.PanID != tt.wantPan {
				t.Errorf("chose PAN ID 0x%04X, want 0x%04X", got.PanID, tt.wantPan)
			}
		})
	}
}

// The error for a closed network lists what was heard, so the person can see
// their network is there and merely shut rather than absent.
func TestChooseNetworkNamesWhatItHeard(t *testing.T) {
	_, err := chooseNetwork([]ezsp.FoundNetwork{heard(15, 0x6F88, false)}, 0)
	if err == nil || !strings.Contains(err.Error(), "0x6F88") || !strings.Contains(err.Error(), "channel 15") {
		t.Fatalf("error = %v, want the closed network described", err)
	}
}

func TestScanFlagsMask(t *testing.T) {
	all, one := 0, 15
	def := int(ezsp.DefaultScanDuration)
	if m, err := (scanFlags{&all, &def}).mask(); err != nil || m != ezsp.DefaultChannelMask {
		t.Errorf("default = 0x%08X, %v; want the whole band", m, err)
	}
	if m, err := (scanFlags{&one, &def}).mask(); err != nil || m != 1<<15 {
		t.Errorf("channel 15 = 0x%08X, %v; want bit 15", m, err)
	}
	bad, tooLong := 27, 15
	if _, err := (scanFlags{&bad, &def}).mask(); err == nil {
		t.Error("channel 27 must be refused")
	}
	if _, err := (scanFlags{&all, &tooLong}).mask(); err == nil {
		t.Error("duration 15 must be refused")
	}
}
