package ezsp

import (
	"context"
	"fmt"
)

// mfglib is the NCP's manufacturing-test library. Its receive mode reports
// every 802.15.4 frame the radio decodes on one channel, whoever it is
// addressed to and whatever network it belongs to. That makes it a sniffer:
// the one view that shows a device which has not joined anything — a device
// sending beacon requests, or associating with somebody else — and which no
// stack callback will ever mention.
//
// It is only usable while the radio is not on a network, so an adapter
// sniffing is an adapter that is not coordinating. Open with
// SkipNetworkRestore to get one.
const (
	FrameMfglibStart      FrameID = 0x0083
	FrameMfglibEnd        FrameID = 0x0084
	FrameMfglibSetChannel FrameID = 0x008A
	FrameMfglibRxHandler  FrameID = 0x008E
)

// MfglibStart switches the radio into test mode. With rx set, every frame
// received is reported through mfglibRxHandler.
func (c *Conn) MfglibStart(ctx context.Context, rx bool) error {
	var b uint8
	if rx {
		b = 1
	}
	return c.statusCommand(ctx, FrameMfglibStart, []byte{b})
}

// MfglibEnd leaves test mode. The stack is not usable again until the NCP is
// reset, which the next Open does anyway.
func (c *Conn) MfglibEnd(ctx context.Context) error {
	return c.statusCommand(ctx, FrameMfglibEnd, nil)
}

// MfglibSetChannel retunes the test-mode radio.
func (c *Conn) MfglibSetChannel(ctx context.Context, channel uint8) error {
	return c.statusCommand(ctx, FrameMfglibSetChannel, []byte{channel})
}

func (c *Conn) statusCommand(ctx context.Context, id FrameID, params []byte) error {
	resp, err := c.command(ctx, id, params)
	if err != nil {
		return err
	}
	r := newBuf(resp)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: %s: %s", id, status)
	}
	return nil
}

// RawFrame is one 802.15.4 frame as the radio received it, FCS included.
type RawFrame struct {
	LQI    uint8
	RSSI   int8
	Packet []byte
}

// DecodeMfglibRx parses mfglibRxHandler.
//
//	linkQuality(1) rssi(1) packetLength(1) packetContents(packetLength)
func DecodeMfglibRx(m Message) (RawFrame, error) {
	r := newBuf(m.Params)
	f := RawFrame{LQI: r.u8(), RSSI: int8(r.u8())}
	n := int(r.u8())
	if r.err != nil {
		return RawFrame{}, fmt.Errorf("ezsp: mfglibRxHandler: %w", r.err)
	}
	if n > r.remaining() {
		return RawFrame{}, fmt.Errorf("ezsp: mfglibRxHandler claims %d bytes, %d follow", n, r.remaining())
	}
	f.Packet = append([]byte(nil), m.Params[r.pos:r.pos+n]...)
	return f, nil
}

// FrameMfglibSendPacket transmits one raw 802.15.4 frame in test mode.
const FrameMfglibSendPacket FrameID = 0x0089

// MfglibSendPacket transmits psdu, which must end with two placeholder bytes
// for the FCS: the radio computes and fills them in.
func (c *Conn) MfglibSendPacket(ctx context.Context, psdu []byte) error {
	if len(psdu) < 3 || len(psdu) > 127 {
		return fmt.Errorf("ezsp: mfglibSendPacket: %d-byte frame", len(psdu))
	}
	return c.statusCommand(ctx, FrameMfglibSendPacket, append([]byte{uint8(len(psdu))}, psdu...))
}
