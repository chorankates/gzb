package ezsp

import (
	"context"
	"fmt"
)

// OutgoingType selects how the stack should route an outgoing message.
type OutgoingType uint8

const (
	// OutgoingDirect addresses a device by its current network address.
	OutgoingDirect OutgoingType = 0x00
	// OutgoingViaAddressTable and OutgoingViaBinding address a device through
	// the NCP's own tables instead.
	OutgoingViaAddressTable OutgoingType = 0x01
	OutgoingViaBinding      OutgoingType = 0x02
	OutgoingMulticast       OutgoingType = 0x03
	OutgoingBroadcast       OutgoingType = 0x04
)

// APS options.
const (
	APSOptionNone       uint16 = 0x0000
	APSOptionEncryption uint16 = 0x0020
	// APSOptionRetry asks the stack to retry until the destination
	// acknowledges. Worth having for anything that matters, but note that a
	// sleepy device only acknowledges once it next polls its parent.
	APSOptionRetry uint16 = 0x0040
)

// Request sends an application message and waits for a reply that satisfies
// match.
//
// The subscription is established before the message goes out, because a
// mains-powered device can answer faster than the send call returns.
//
// The caller's context bounds the wait, and choosing that bound is a real
// decision: a sleepy device only receives while polling its parent, so a
// request to one is not late until it has had time to wake up.
func (c *Conn) Request(ctx context.Context, dest uint16, aps APSFrame, payload []byte, match func(IncomingMessage) bool) (IncomingMessage, error) {
	replies, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && m.ID == FrameIncomingMessage
	}, 32)
	defer cancel()

	if _, err := c.SendUnicast(ctx, dest, aps, 0, payload); err != nil {
		return IncomingMessage{}, err
	}

	for {
		select {
		case m, ok := <-replies:
			if !ok {
				return IncomingMessage{}, ErrClosed
			}
			msg, err := decodeIncomingMessage(m.Params)
			if err != nil || !match(msg) {
				continue
			}
			return msg, nil
		case <-ctx.Done():
			return IncomingMessage{}, fmt.Errorf("%w: no reply from 0x%04X on cluster 0x%04X", ErrTimeout, dest, aps.Cluster)
		}
	}
}

// SendUnicast sends an application message to one device.
//
// The returned sequence is the APS counter the stack assigned. Success here
// means the NCP accepted the message for transmission, not that the device
// received it: delivery is reported later through messageSentHandler.
func (c *Conn) SendUnicast(ctx context.Context, dest uint16, aps APSFrame, tag uint8, payload []byte) (uint8, error) {
	if len(payload) > 255 {
		return 0, fmt.Errorf("ezsp: payload of %d bytes exceeds the APS limit", len(payload))
	}

	var w wbuf
	w.u8(uint8(OutgoingDirect))
	w.u16(dest)
	w.u16(aps.Profile)
	w.u16(aps.Cluster)
	w.u8(aps.SourceEP)
	w.u8(aps.DestEP)
	w.u16(aps.Options)
	w.u16(aps.GroupID)
	w.u8(aps.APSSequence)
	w.u8(tag)
	w.u8(uint8(len(payload)))
	w.bytes(payload)

	params, err := c.command(ctx, FrameSendUnicast, w.b)
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	seq := r.u8()
	if r.err != nil {
		return 0, r.err
	}
	if !status.OK() {
		return 0, fmt.Errorf("ezsp: sendUnicast to 0x%04X: %s", dest, status)
	}
	return seq, nil
}
