package ezsp

import (
	"context"
	"fmt"
	"time"
)

// An active scan is how a node finds a network to join: it sends a beacon
// request on each channel and every router in earshot answers with a beacon
// carrying the network's identity and whether it is accepting joins. The NCP
// reports each beacon through networkFoundHandler and the end of the sweep
// through scanCompleteHandler.

// scanActive selects an active scan, as opposed to an energy scan (0x00).
const scanActive uint8 = 0x01

// DefaultScanDuration is the per-channel scan duration exponent. The MAC
// listens for (2^duration + 1) superframes of 15.36 ms on each channel, so 3
// is about 140 ms per channel and a little over two seconds for the band.
const DefaultScanDuration uint8 = 3

// MaxScanDuration is the largest exponent the MAC accepts.
const MaxScanDuration uint8 = 14

// FoundNetwork is one network heard during an active scan.
type FoundNetwork struct {
	Channel       uint8  `json:"channel"`
	PanID         uint16 `json:"pan_id"`
	ExtendedPanID EUI64  `json:"extended_pan_id"`
	// AllowingJoin reports whether some router on the network is accepting
	// joins right now: its permit-join window is open.
	AllowingJoin bool  `json:"allowing_join"`
	StackProfile uint8 `json:"stack_profile"`
	NwkUpdateID  uint8 `json:"nwk_update_id"`
	// LQI and RSSI are of the strongest beacon heard from this network.
	LQI  uint8 `json:"lqi"`
	RSSI int8  `json:"rssi"`
}

// String is the one-line form: where it is and whether it is open.
func (n FoundNetwork) String() string {
	open := "closed"
	if n.AllowingJoin {
		open = "open"
	}
	return fmt.Sprintf("channel %d, PAN ID 0x%04X, ext PAN ID %s, %s", n.Channel, n.PanID, n.ExtendedPanID, open)
}

// SameNetwork reports whether two beacons came from the same network.
func (n FoundNetwork) SameNetwork(o FoundNetwork) bool {
	return n.Channel == o.Channel && n.PanID == o.PanID && n.ExtendedPanID == o.ExtendedPanID
}

// decodeNetworkFound parses networkFoundHandler.
//
//	EmberZigbeeNetwork: channel(1) panId(2) extendedPanId(8) allowingJoin(1)
//	                    stackProfile(1) nwkUpdateId(1)
//	then lastHopLqi(1) lastHopRssi(1)
func decodeNetworkFound(params []byte) (FoundNetwork, error) {
	r := newBuf(params)
	n := FoundNetwork{
		Channel:       r.u8(),
		PanID:         r.u16(),
		ExtendedPanID: r.ieee(),
		AllowingJoin:  r.u8() != 0,
		StackProfile:  r.u8(),
		NwkUpdateID:   r.u8(),
	}
	n.LQI = r.u8()
	n.RSSI = int8(r.u8())
	if r.err != nil {
		return FoundNetwork{}, fmt.Errorf("ezsp: networkFoundHandler: %w", r.err)
	}
	return n, nil
}

// decodeScanComplete parses scanCompleteHandler.
//
//	channel(1) status(1)
//
// The channel is meaningful only when the status reports a failure on it;
// a successful sweep reports success with the channel unspecified.
func decodeScanComplete(params []byte) (uint8, EmberStatus, error) {
	r := newBuf(params)
	channel := r.u8()
	status := EmberStatus(r.u8())
	if r.err != nil {
		return 0, 0, fmt.Errorf("ezsp: scanCompleteHandler: %w", r.err)
	}
	return channel, status, nil
}

// startScanError interprets the status startScan answers with.
//
// This is the one command in this package whose status width is uncertain:
// the EZSP specification has it returning a four-byte sl_status_t from
// version 8 onward, while every other status this firmware returns is a
// single EmberStatus byte. Zero means success in both encodings, so the
// check accepts either width and only the wording of a failure depends on
// which arrived.
func startScanError(params []byte) error {
	switch len(params) {
	case 0:
		return fmt.Errorf("ezsp: startScan: empty response")
	case 1, 2, 3:
		if s := EmberStatus(params[0]); !s.OK() {
			return fmt.Errorf("ezsp: startScan: %s", s)
		}
		return nil
	default:
		if v := newBuf(params).u32(); v != 0 {
			return fmt.Errorf("ezsp: startScan: sl_status 0x%08X", v)
		}
		return nil
	}
}

// mergeNetwork folds one beacon into the list of networks heard so far. Every
// router on a network beacons, so one network arrives several times; the
// result keeps one entry per network with the strongest signal heard and
// open if any router said it was.
func mergeNetwork(found []FoundNetwork, n FoundNetwork) []FoundNetwork {
	for i := range found {
		if !found[i].SameNetwork(n) {
			continue
		}
		if n.AllowingJoin {
			found[i].AllowingJoin = true
		}
		if n.LQI > found[i].LQI {
			found[i].LQI, found[i].RSSI = n.LQI, n.RSSI
		}
		return found
	}
	return append(found, n)
}

// scanTimeout bounds a scan by what the MAC will take to run it, plus a
// margin for the NCP to report the end of it.
func scanTimeout(channelMask uint32, duration uint8) time.Duration {
	perChannel := time.Duration((1<<duration)+1) * 15360 * time.Microsecond
	channels := len(ChannelList(channelMask))
	if channels == 0 {
		channels = 16
	}
	return perChannel*time.Duration(channels) + 10*time.Second
}

// ScanNetworks runs an active scan over the channels in channelMask and
// returns the networks heard, one entry per network. The scan takes a few
// seconds and the radio is busy for the whole of it.
func (c *Conn) ScanNetworks(ctx context.Context, channelMask uint32, duration uint8) ([]FoundNetwork, error) {
	if duration > MaxScanDuration {
		return nil, fmt.Errorf("ezsp: scan duration %d is outside the MAC range 0-%d", duration, MaxScanDuration)
	}
	if channelMask&DefaultChannelMask == 0 {
		return nil, fmt.Errorf("ezsp: channel mask 0x%08X selects no Zigbee channel", channelMask)
	}

	// Subscribe before starting: the first beacon can arrive before the
	// command's own response is processed.
	msgs, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && (m.ID == FrameNetworkFoundHandler || m.ID == FrameScanCompleteHandler)
	}, 64)
	defer cancel()

	var w wbuf
	w.u8(scanActive)
	w.u32(channelMask)
	w.u8(duration)
	params, err := c.command(ctx, FrameStartScan, w.b)
	if err != nil {
		return nil, err
	}
	if err := startScanError(params); err != nil {
		return nil, err
	}

	ctx, cancelWait := context.WithTimeout(ctx, scanTimeout(channelMask, duration))
	defer cancelWait()

	var found []FoundNetwork
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return found, ErrClosed
			}
			switch m.ID {
			case FrameNetworkFoundHandler:
				n, err := decodeNetworkFound(m.Params)
				if err != nil {
					return found, err
				}
				found = mergeNetwork(found, n)
			case FrameScanCompleteHandler:
				channel, status, err := decodeScanComplete(m.Params)
				if err != nil {
					return found, err
				}
				if !status.OK() {
					return found, fmt.Errorf("ezsp: scan failed on channel %d: %s", channel, status)
				}
				return found, nil
			}
		case <-ctx.Done():
			return found, fmt.Errorf("ezsp: scan did not complete: %w", ctx.Err())
		}
	}
}
