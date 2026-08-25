package ezsp

import (
	"context"
	"errors"
	"fmt"
	"time"
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

// ResendInterval is how often Request repeats a message nothing has answered.
//
// A message for a sleepy device waits in the NCP's indirect queue until the
// device polls its parent, and the NCP throws it away after
// EZSP_CONFIG_INDIRECT_TRANSMISSION_TIMEOUT — thirty seconds, as gzb
// configures it. Waiting longer than that is waiting on a message that no
// longer exists, which is why one send is not enough however patient the
// caller is.
//
// The interval is deliberately shorter than the queue's own patience, so that
// several copies overlap and one is always live when the device finally wakes.
// That costs a little radio traffic and a few packet buffers, and buys the
// difference between reaching a battery sensor and not.
const ResendInterval = 10 * time.Second

// Request sends an application message and waits for a reply that satisfies
// match, repeating the message until one arrives or the context expires.
//
// The subscription is established before the message goes out, because a
// mains-powered device can answer faster than the send call returns, and it
// spans every attempt so that a reply cannot land in the gap between two.
//
// Because the message is repeated verbatim, a request must be safe to deliver
// more than once. Reading an attribute, writing one, and configuring reporting
// all are: the second copy asks for exactly what the first did.
//
// The caller's context bounds the wait, and choosing that bound is a real
// decision: a sleepy device only receives while polling its parent, so a
// request to one is not late until it has had time to wake up.
func (c *Conn) Request(ctx context.Context, dest uint16, aps APSFrame, payload []byte, match func(IncomingMessage) bool) (IncomingMessage, error) {
	replies, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && m.ID == FrameIncomingMessage
	}, 32)
	defer cancel()

	started := time.Now()
	send := func() error {
		_, err := c.SendUnicast(ctx, dest, aps, 0, payload)
		return err
	}

	msg, attempts, err := awaitReply(ctx, replies, ResendInterval, send, match)
	if err == nil {
		return msg, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return IncomingMessage{}, fmt.Errorf("%w: no reply from 0x%04X on cluster 0x%04X after %d attempts over %s",
			ErrTimeout, dest, aps.Cluster, attempts, time.Since(started).Round(time.Second))
	}
	return IncomingMessage{}, err
}

// awaitReply waits for a reply that satisfies match, repeating send every
// resendEvery until the context expires.
//
// It reports how many times the message actually went out, which is the
// informative half of a timeout: "no reply after one attempt" and "no reply
// after thirty" describe different problems.
func awaitReply(ctx context.Context, replies <-chan Message, resendEvery time.Duration, send func() error, match func(IncomingMessage) bool) (IncomingMessage, int, error) {
	// The first send has to work. If nothing was ever queued there is nothing
	// to wait for, and the reason it failed explains more than a timeout would.
	if err := send(); err != nil {
		return IncomingMessage{}, 0, err
	}
	attempts := 1

	resend := time.NewTicker(resendEvery)
	defer resend.Stop()

	for {
		select {
		case m, ok := <-replies:
			if !ok {
				return IncomingMessage{}, attempts, ErrClosed
			}
			msg, err := decodeIncomingMessage(m.Params)
			if err != nil || !match(msg) {
				continue
			}
			return msg, attempts, nil
		case <-resend.C:
			// A later send failing is not fatal. The NCP may simply be out of
			// packet buffers this instant, and the next tick is another go;
			// abandoning the wait would throw away copies that are still
			// queued and still perfectly capable of being answered.
			if err := send(); err == nil {
				attempts++
			}
		case <-ctx.Done():
			return IncomingMessage{}, attempts, ctx.Err()
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
