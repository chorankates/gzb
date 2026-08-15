package ezsp

import (
	"fmt"
	"time"
)

// This file decodes the three ways the NCP tells us a device has appeared.
//
// They are complementary, not redundant. trustCenterJoinHandler fires on the
// coordinator for any device joining anywhere in the mesh and carries both
// addresses, which makes it the authoritative source. childJoinHandler fires
// only for devices that pick the coordinator as their direct parent, and adds
// the node type. The ZDO Device Announce is broadcast by the device itself and
// is the one signal that also carries its capability flags.

// DeviceUpdate reports what a device did, as seen by the trust centre.
type DeviceUpdate uint8

const (
	UpdateSecuredRejoin    DeviceUpdate = 0x00
	UpdateUnsecuredJoin    DeviceUpdate = 0x01
	UpdateDeviceLeft       DeviceUpdate = 0x02
	UpdateUnsecuredRejoin  DeviceUpdate = 0x03
	UpdateHighSecureRejoin DeviceUpdate = 0x04
	UpdateHighSecureJoin   DeviceUpdate = 0x05
	UpdateHighUnsecRejoin  DeviceUpdate = 0x07
)

func (u DeviceUpdate) String() string {
	switch u {
	case UpdateSecuredRejoin:
		return "secured rejoin"
	case UpdateUnsecuredJoin:
		return "joined"
	case UpdateDeviceLeft:
		return "left"
	case UpdateUnsecuredRejoin:
		return "unsecured rejoin"
	case UpdateHighSecureRejoin:
		return "secured rejoin (high security)"
	case UpdateHighSecureJoin:
		return "joined (high security)"
	case UpdateHighUnsecRejoin:
		return "unsecured rejoin (high security)"
	default:
		return fmt.Sprintf("device update 0x%02X", uint8(u))
	}
}

// Left reports whether the device is leaving rather than arriving.
func (u DeviceUpdate) Left() bool { return u == UpdateDeviceLeft }

// JoinDecision is what the trust centre decided to do about a join.
type JoinDecision uint8

const (
	JoinUsePreconfiguredKey JoinDecision = 0x00
	JoinSendKeyInClear      JoinDecision = 0x01
	JoinDeny                JoinDecision = 0x02
	JoinNoAction            JoinDecision = 0x03
)

func (d JoinDecision) String() string {
	switch d {
	case JoinUsePreconfiguredKey:
		return "accepted (preconfigured key)"
	case JoinSendKeyInClear:
		return "accepted (key sent in the clear)"
	case JoinDeny:
		return "denied"
	case JoinNoAction:
		return "no action"
	default:
		return fmt.Sprintf("join decision 0x%02X", uint8(d))
	}
}

// EventKind distinguishes which callback produced a JoinEvent.
type EventKind int

const (
	EventTrustCenter EventKind = iota
	EventChild
	EventAnnounce
)

func (k EventKind) String() string {
	switch k {
	case EventTrustCenter:
		return "trust-centre"
	case EventChild:
		return "child"
	case EventAnnounce:
		return "announce"
	default:
		return "unknown"
	}
}

// JoinEvent is a device arriving on, or leaving, the network.
type JoinEvent struct {
	Kind   EventKind `json:"kind"`
	At     time.Time `json:"at"`
	NodeID uint16    `json:"node_id"`
	IEEE   EUI64     `json:"ieee"`

	// Parent is the node the device attached through. Only the trust-centre
	// event carries it.
	Parent *uint16 `json:"parent,omitempty"`
	// NodeType comes from the child event.
	NodeType *NodeType `json:"node_type,omitempty"`
	// Update and Decision come from the trust-centre event.
	Update   *DeviceUpdate `json:"update,omitempty"`
	Decision *JoinDecision `json:"decision,omitempty"`
	// Capability comes from the device announce.
	Capability *Capability `json:"capability,omitempty"`
	// Leaving is set when the device is departing rather than arriving.
	Leaving bool `json:"leaving"`
}

// Capability is the MAC capability bitmask a device broadcasts when it
// announces itself. It is the quickest read on what kind of device this is.
type Capability uint8

const (
	CapAlternatePANCoord Capability = 0x01
	CapFullFunction      Capability = 0x02
	CapMainsPowered      Capability = 0x04
	CapRxOnWhenIdle      Capability = 0x08
	CapSecurityCapable   Capability = 0x40
	CapAllocateAddress   Capability = 0x80
)

// Mains reports whether the device is mains powered rather than battery.
func (c Capability) Mains() bool { return c&CapMainsPowered != 0 }

// Sleepy reports whether the device sleeps between transmissions, which means
// it cannot be polled on demand and must be caught while awake.
func (c Capability) Sleepy() bool { return c&CapRxOnWhenIdle == 0 }

// Router reports whether the device can route for others.
func (c Capability) Router() bool { return c&CapFullFunction != 0 }

func (c Capability) String() string {
	power, listen := "battery", "sleepy"
	if c.Mains() {
		power = "mains"
	}
	if !c.Sleepy() {
		listen = "always listening"
	}
	kind := "end device"
	if c.Router() {
		kind = "router-capable"
	}
	return fmt.Sprintf("%s, %s, %s", kind, power, listen)
}

// decodeTrustCenterJoin parses trustCenterJoinHandler.
//
//	newNodeId(2) newNodeEui64(8) status(1) policyDecision(1) parentOfNewNode(2)
func decodeTrustCenterJoin(params []byte) (JoinEvent, error) {
	r := newBuf(params)
	node := r.u16()
	ieee := r.ieee()
	update := DeviceUpdate(r.u8())
	decision := JoinDecision(r.u8())
	parent := r.u16()
	if r.err != nil {
		return JoinEvent{}, fmt.Errorf("ezsp: trustCenterJoinHandler: %w", r.err)
	}
	return JoinEvent{
		Kind:     EventTrustCenter,
		At:       time.Now(),
		NodeID:   node,
		IEEE:     ieee,
		Parent:   &parent,
		Update:   &update,
		Decision: &decision,
		Leaving:  update.Left(),
	}, nil
}

// decodeChildJoin parses childJoinHandler.
//
//	index(1) joining(1) childId(2) childEui64(8) childType(1)
func decodeChildJoin(params []byte) (JoinEvent, error) {
	r := newBuf(params)
	_ = r.u8() // child table index
	joining := r.u8() != 0
	node := r.u16()
	ieee := r.ieee()
	nodeType := NodeType(r.u8())
	if r.err != nil {
		return JoinEvent{}, fmt.Errorf("ezsp: childJoinHandler: %w", r.err)
	}
	return JoinEvent{
		Kind:     EventChild,
		At:       time.Now(),
		NodeID:   node,
		IEEE:     ieee,
		NodeType: &nodeType,
		Leaving:  !joining,
	}, nil
}

// ZDO cluster IDs carried inside an incoming message on profile 0x0000.
const (
	ZDODeviceAnnounce uint16 = 0x0013
	ProfileZDO        uint16 = 0x0000
)

// decodeDeviceAnnounce parses the ZDO Device Announce payload.
//
//	transactionSeq(1) nwkAddr(2) ieeeAddr(8) capability(1)
func decodeDeviceAnnounce(payload []byte) (JoinEvent, error) {
	r := newBuf(payload)
	_ = r.u8() // ZDO transaction sequence
	node := r.u16()
	ieee := r.ieee()
	capability := Capability(r.u8())
	if r.err != nil {
		return JoinEvent{}, fmt.Errorf("ezsp: device announce: %w", r.err)
	}
	return JoinEvent{
		Kind:       EventAnnounce,
		At:         time.Now(),
		NodeID:     node,
		IEEE:       ieee,
		Capability: &capability,
	}, nil
}

// APSFrame is the application-layer addressing of a message.
type APSFrame struct {
	Profile     uint16
	Cluster     uint16
	SourceEP    uint8
	DestEP      uint8
	Options     uint16
	GroupID     uint16
	APSSequence uint8
}

// IncomingMessage is a decoded incomingMessageHandler callback.
type IncomingMessage struct {
	Type    uint8
	APS     APSFrame
	LQI     uint8
	RSSI    int8
	Sender  uint16
	Payload []byte
}

// incomingPrefixLen is the fixed part of incomingMessageHandler before the
// length-prefixed payload:
//
//	type(1) apsFrame(11) lastHopLqi(1) lastHopRssi(1) sender(2)
//	bindingIndex(1) addressIndex(1) = 18 bytes
//
// EZSP v14 replaces the middle of that with an EmberRxPacketInfo struct. This
// adapter negotiates v13, so the layout above applies.
//
// EmberZNet 7.4.4 appends one byte after the payload that the layout does not
// account for — a device announce arrives as 32 bytes carrying a 12-byte
// payload and a trailing 0x02. Its meaning is unknown, so the decoder reads
// exactly messageLength bytes from the cursor and ignores whatever follows,
// rather than slicing from the end of the frame and shifting every field.
const incomingPrefixLen = 18

// DecodeIncomingMessage parses an incomingMessageHandler callback into its APS
// addressing and payload.
func DecodeIncomingMessage(m Message) (IncomingMessage, error) {
	return decodeIncomingMessage(m.Params)
}

func decodeIncomingMessage(params []byte) (IncomingMessage, error) {
	if len(params) < incomingPrefixLen+1 {
		return IncomingMessage{}, fmt.Errorf("ezsp: incomingMessageHandler: %d bytes, want at least %d", len(params), incomingPrefixLen+1)
	}
	r := newBuf(params)
	msg := IncomingMessage{Type: r.u8()}
	msg.APS = APSFrame{
		Profile:     r.u16(),
		Cluster:     r.u16(),
		SourceEP:    r.u8(),
		DestEP:      r.u8(),
		Options:     r.u16(),
		GroupID:     r.u16(),
		APSSequence: r.u8(),
	}
	msg.LQI = r.u8()
	msg.RSSI = int8(r.u8())
	msg.Sender = r.u16()
	_ = r.u8() // binding index
	_ = r.u8() // address table index
	length := int(r.u8())
	if r.err != nil {
		return IncomingMessage{}, fmt.Errorf("ezsp: incomingMessageHandler: %w", r.err)
	}
	if length > r.remaining() {
		return IncomingMessage{}, fmt.Errorf(
			"ezsp: incomingMessageHandler claims a %d-byte payload but only %d bytes follow; frame layout differs from EZSP v13 (raw: % X)",
			length, r.remaining(), params)
	}
	msg.Payload = params[r.pos : r.pos+length]
	return msg, nil
}

// WatchJoins reports devices joining or leaving the network. The returned
// cancel function stops the watch and closes the channel.
//
// Errors decoding a callback are delivered on the errs channel rather than
// stopping the watch: one malformed frame should not end a pairing session.
func (c *Conn) WatchJoins(buffer int) (events <-chan JoinEvent, errs <-chan error, cancel func()) {
	if buffer <= 0 {
		buffer = 32
	}
	msgs, unsubscribe := c.Subscribe(func(m Message) bool {
		if !m.Callback {
			return false
		}
		switch m.ID {
		case FrameTrustCenterJoin, FrameChildJoinHandler, FrameIncomingMessage:
			return true
		}
		return false
	}, buffer)

	out := make(chan JoinEvent, buffer)
	errCh := make(chan error, buffer)

	go func() {
		defer close(out)
		defer close(errCh)
		for m := range msgs {
			ev, err := decodeJoinMessage(m)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				continue
			}
			if ev == nil {
				continue // an incoming message that was not a device announce
			}
			select {
			case out <- *ev:
			default:
			}
		}
	}()

	return out, errCh, unsubscribe
}

// StackStatus extracts the status carried by a stackStatusHandler callback.
func StackStatus(m Message) (EmberStatus, error) {
	r := newBuf(m.Params)
	s := EmberStatus(r.u8())
	if r.err != nil {
		return 0, fmt.Errorf("ezsp: stackStatusHandler: %w", r.err)
	}
	return s, nil
}

// decodeJoinMessage turns a callback into a JoinEvent, or nil when the
// callback is not about a device arriving.
func decodeJoinMessage(m Message) (*JoinEvent, error) {
	switch m.ID {
	case FrameTrustCenterJoin:
		ev, err := decodeTrustCenterJoin(m.Params)
		if err != nil {
			return nil, err
		}
		return &ev, nil
	case FrameChildJoinHandler:
		ev, err := decodeChildJoin(m.Params)
		if err != nil {
			return nil, err
		}
		return &ev, nil
	case FrameIncomingMessage:
		msg, err := decodeIncomingMessage(m.Params)
		if err != nil {
			return nil, err
		}
		if msg.APS.Profile != ProfileZDO || msg.APS.Cluster != ZDODeviceAnnounce {
			return nil, nil
		}
		ev, err := decodeDeviceAnnounce(msg.Payload)
		if err != nil {
			return nil, err
		}
		return &ev, nil
	}
	return nil, nil
}
