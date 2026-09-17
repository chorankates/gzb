package ezsp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A networkFoundHandler as the layout in UG100 has it: the 14-byte
// EmberZigbeeNetwork, then the LQI and RSSI of the beacon. The extended PAN
// ID is on the wire least-significant byte first, like every EUI-64.
func beacon(channel uint8, pan uint16, open bool, lqi uint8, rssi int8) []byte {
	var w wbuf
	w.u8(channel)
	w.u16(pan)
	w.ieee(EUI64{0x4B, 0xF9, 0x3D, 0xC3, 0x75, 0x45, 0x69, 0x2B})
	if open {
		w.u8(1)
	} else {
		w.u8(0)
	}
	w.u8(2) // stack profile: ZigBee PRO
	w.u8(7) // nwkUpdateId
	w.u8(lqi)
	w.u8(uint8(rssi))
	return w.b
}

func TestDecodeNetworkFound(t *testing.T) {
	params := beacon(15, 0x6F88, true, 255, -40)
	if len(params) != 16 {
		t.Fatalf("test vector is %d bytes, want 16", len(params))
	}
	n, err := decodeNetworkFound(params)
	if err != nil {
		t.Fatalf("decodeNetworkFound: %v", err)
	}
	if n.Channel != 15 || n.PanID != 0x6F88 || !n.AllowingJoin || n.StackProfile != 2 || n.NwkUpdateID != 7 {
		t.Errorf("decoded %+v", n)
	}
	if got, want := n.ExtendedPanID.String(), "4B:F9:3D:C3:75:45:69:2B"; got != want {
		t.Errorf("extended PAN ID = %s, want %s (byte order)", got, want)
	}
	if n.LQI != 255 || n.RSSI != -40 {
		t.Errorf("lqi/rssi = %d/%d, want 255/-40", n.LQI, n.RSSI)
	}
}

func TestDecodeNetworkFoundRejectsATruncatedBeacon(t *testing.T) {
	if _, err := decodeNetworkFound(beacon(15, 0x6F88, true, 255, -40)[:13]); err == nil {
		t.Error("expected an error for a truncated networkFoundHandler")
	}
}

func TestDecodeScanComplete(t *testing.T) {
	channel, status, err := decodeScanComplete([]byte{0x00, 0x00})
	if err != nil || channel != 0 || !status.OK() {
		t.Errorf("success sweep: channel=%d status=%s err=%v", channel, status, err)
	}
	channel, status, err = decodeScanComplete([]byte{0x14, 0x70})
	if err != nil || channel != 20 || status != StatusInvalidCall {
		t.Errorf("failed sweep: channel=%d status=%s err=%v", channel, status, err)
	}
	if _, _, err := decodeScanComplete([]byte{0x14}); err == nil {
		t.Error("expected an error for a truncated scanCompleteHandler")
	}
}

// Every router on a network answers a beacon request, so the same network
// arrives once per router. The list must have it once, open if any router
// said so, with the strongest signal heard.
func TestMergeNetworkKeepsOneEntryPerNetwork(t *testing.T) {
	first, _ := decodeNetworkFound(beacon(15, 0x6F88, false, 100, -70))
	second, _ := decodeNetworkFound(beacon(15, 0x6F88, true, 255, -40))
	weaker, _ := decodeNetworkFound(beacon(15, 0x6F88, false, 50, -85))
	other, _ := decodeNetworkFound(beacon(20, 0x6F88, false, 200, -50))

	var found []FoundNetwork
	for _, n := range []FoundNetwork{first, second, weaker, other} {
		found = mergeNetwork(found, n)
	}
	if len(found) != 2 {
		t.Fatalf("got %d networks, want 2 (same PAN ID on another channel is another network)", len(found))
	}
	if !found[0].AllowingJoin {
		t.Error("network should read as open when any of its routers is")
	}
	if found[0].LQI != 255 || found[0].RSSI != -40 {
		t.Errorf("lqi/rssi = %d/%d, want the strongest beacon's 255/-40", found[0].LQI, found[0].RSSI)
	}
	if found[1].Channel != 20 || found[1].AllowingJoin {
		t.Errorf("second network = %+v", found[1])
	}
}

// startScan's status is the one whose width is not pinned by a capture. Zero
// is success in both the one-byte and four-byte encodings, and a failure has
// to be reported in whichever arrived.
func TestStartScanErrorAcceptsEitherStatusWidth(t *testing.T) {
	if err := startScanError([]byte{0x00}); err != nil {
		t.Errorf("one-byte success: %v", err)
	}
	if err := startScanError([]byte{0x00, 0x00, 0x00, 0x00}); err != nil {
		t.Errorf("four-byte success: %v", err)
	}
	if err := startScanError([]byte{0x70}); err == nil || !strings.Contains(err.Error(), "invalid call") {
		t.Errorf("one-byte failure = %v, want the EmberStatus named", err)
	}
	if err := startScanError([]byte{0x01, 0x00, 0x00, 0x00}); err == nil || !strings.Contains(err.Error(), "0x00000001") {
		t.Errorf("four-byte failure = %v, want the sl_status shown", err)
	}
	if err := startScanError(nil); err == nil {
		t.Error("empty response must be an error, not a success")
	}
}

func TestScanTimeoutGrowsWithDurationAndChannels(t *testing.T) {
	one := scanTimeout(1<<15, DefaultScanDuration)
	all := scanTimeout(DefaultChannelMask, DefaultScanDuration)
	long := scanTimeout(DefaultChannelMask, DefaultScanDuration+1)
	if !(one < all && all < long) {
		t.Errorf("timeouts %v (one channel) %v (band) %v (band, longer) are not increasing", one, all, long)
	}
	if all < 12*time.Second {
		t.Errorf("band scan timeout %v leaves no margin over the ~2.2s sweep", all)
	}
}

// A join that fails names why through stackStatusHandler, and the wait must
// end on that rather than run out the clock waiting for an up that is not
// coming.
func TestAwaitNetworkUpStopsOnAJoinFailure(t *testing.T) {
	up := make(chan Message, 1)
	up <- Message{Callback: true, ID: FrameStackStatusHandler, Params: []byte{byte(StatusNoBeacons)}}
	err := awaitNetworkUp(t.Context(), up, "joining")
	if err == nil || !strings.Contains(err.Error(), "no beacons") || !strings.Contains(err.Error(), "joining") {
		t.Errorf("error = %v, want the failure named and what was being done", err)
	}
}

func TestAwaitNetworkUpReturnsOnUp(t *testing.T) {
	up := make(chan Message, 2)
	up <- Message{Callback: true, ID: FrameStackStatusHandler, Params: []byte{byte(StatusSuccess)}}
	up <- Message{Callback: true, ID: FrameStackStatusHandler, Params: []byte{byte(StatusNetworkUp)}}
	if err := awaitNetworkUp(t.Context(), up, "joining"); err != nil {
		t.Errorf("awaitNetworkUp: %v", err)
	}
}

func TestAwaitNetworkUpReportsAClosedSession(t *testing.T) {
	up := make(chan Message)
	close(up)
	if err := awaitNetworkUp(t.Context(), up, "forming"); !errors.Is(err, ErrClosed) {
		t.Errorf("error = %v, want %v", err, ErrClosed)
	}
}
