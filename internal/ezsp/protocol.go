// Package ezsp implements the EmberZNet Serial Protocol, the command
// interface an EFR32 network coprocessor exposes over an ASH link.
//
// EZSP has two incompatible frame layouts and the client must discover which
// one applies before it can issue a second command:
//
//	legacy (protocol version < 8)
//	  [seq] [frameControl] [frameID] [parameters...]
//
//	extended (protocol version >= 8)
//	  [seq] [frameControlLo] [frameControlHi] [frameIDLo] [frameIDHi] [params...]
//
// The version command is the bootstrap: it is always sent in the legacy
// layout, and its reply states which protocol version the NCP actually
// speaks. Every later command uses the layout implied by that answer.
package ezsp

import (
	"encoding/binary"
	"fmt"
)

// FrameID identifies an EZSP command or callback.
type FrameID uint16

// Commands and callbacks used by this client.
const (
	FrameVersion               FrameID = 0x0000
	FrameStackStatusHandler    FrameID = 0x0019
	FrameNetworkInit           FrameID = 0x0017
	FrameNetworkState          FrameID = 0x0018
	FrameFormNetwork           FrameID = 0x001E
	FrameLeaveNetwork          FrameID = 0x0020
	FramePermitJoining         FrameID = 0x0022
	FrameGetEUI64              FrameID = 0x0026
	FrameGetNodeID             FrameID = 0x0027
	FrameGetNetworkParameters  FrameID = 0x0028
	FrameGetConfigurationValue FrameID = 0x0052
	FrameSetConfigurationValue FrameID = 0x0053
	FrameGetValue              FrameID = 0x00AA
	FrameSendUnicast           FrameID = 0x0034
	FrameIncomingMessage       FrameID = 0x0045
	FrameTrustCenterJoin       FrameID = 0x0024
	FrameChildJoinHandler      FrameID = 0x0023
)

func (f FrameID) String() string {
	switch f {
	case FrameVersion:
		return "version"
	case FrameStackStatusHandler:
		return "stackStatusHandler"
	case FrameNetworkInit:
		return "networkInit"
	case FrameNetworkState:
		return "networkState"
	case FrameFormNetwork:
		return "formNetwork"
	case FrameLeaveNetwork:
		return "leaveNetwork"
	case FramePermitJoining:
		return "permitJoining"
	case FrameGetEUI64:
		return "getEui64"
	case FrameGetNodeID:
		return "getNodeId"
	case FrameGetNetworkParameters:
		return "getNetworkParameters"
	case FrameGetConfigurationValue:
		return "getConfigurationValue"
	case FrameSetConfigurationValue:
		return "setConfigurationValue"
	case FrameGetValue:
		return "getValue"
	case FrameSendUnicast:
		return "sendUnicast"
	case FrameIncomingMessage:
		return "incomingMessageHandler"
	case FrameTrustCenterJoin:
		return "trustCenterJoinHandler"
	case FrameChildJoinHandler:
		return "childJoinHandler"
	default:
		return fmt.Sprintf("frame 0x%04X", uint16(f))
	}
}

// LegacyVersion is the protocol version assumed before negotiation. Every NCP
// understands a version command sent in this layout, whatever it goes on to
// negotiate.
const LegacyVersion = 4

// extendedFormatFrom is the first protocol version using the extended layout.
const extendedFormatFrom = 8

// frameControlExtended marks a host-to-NCP command as frame format version 1.
// It is sent little-endian, so the wire order is 0x00 then 0x01.
const frameControlExtended uint16 = 0x0100

// Message is a decoded EZSP frame from the NCP.
type Message struct {
	Sequence uint8
	ID       FrameID
	// Callback reports whether the NCP sent this of its own accord — a device
	// announcing itself, an incoming message — rather than as a reply.
	Callback bool
	Params   []byte
}

func (m Message) String() string {
	// The direction arrow supplied by the tracer already distinguishes a
	// command from a reply, so only the callback case needs calling out.
	s := fmt.Sprintf("%s seq=%d (%d bytes)", m.ID, m.Sequence, len(m.Params))
	if m.Callback {
		return "callback " + s
	}
	return s
}

// encodeCommand builds a host-to-NCP frame for the negotiated version.
func encodeCommand(version int, seq uint8, id FrameID, params []byte) []byte {
	if version < extendedFormatFrom {
		out := make([]byte, 0, 3+len(params))
		out = append(out, seq, 0x00, byte(id))
		return append(out, params...)
	}
	out := make([]byte, 0, 5+len(params))
	out = append(out, seq)
	out = binary.LittleEndian.AppendUint16(out, frameControlExtended)
	out = binary.LittleEndian.AppendUint16(out, uint16(id))
	return append(out, params...)
}

// decodeMessage parses an NCP-to-host frame for the negotiated version.
func decodeMessage(version int, data []byte) (Message, error) {
	if version < extendedFormatFrom {
		if len(data) < 3 {
			return Message{}, fmt.Errorf("ezsp: legacy frame too short: %d bytes", len(data))
		}
		return Message{
			Sequence: data[0],
			ID:       FrameID(data[2]),
			Callback: isCallback(uint16(data[1])),
			Params:   data[3:],
		}, nil
	}
	if len(data) < 5 {
		return Message{}, fmt.Errorf("ezsp: extended frame too short: %d bytes", len(data))
	}
	return Message{
		Sequence: data[0],
		ID:       FrameID(binary.LittleEndian.Uint16(data[3:5])),
		Callback: isCallback(binary.LittleEndian.Uint16(data[1:3])),
		Params:   data[5:],
	}, nil
}

// isCallback reads the callback-type field of a frame control word. Bits 4-5
// of the low byte are zero for an ordinary response and non-zero when the NCP
// is reporting an asynchronous event.
func isCallback(frameControl uint16) bool {
	return frameControl&0x0030 != 0
}

// EmberStatus is the result code returned by most stack operations.
type EmberStatus uint8

const (
	StatusSuccess          EmberStatus = 0x00
	StatusInvalidCall      EmberStatus = 0x70
	StatusNotJoined        EmberStatus = 0x93
	StatusNetworkUp        EmberStatus = 0x90
	StatusNetworkDown      EmberStatus = 0x91
	StatusInvalidParameter EmberStatus = 0xB4
)

func (s EmberStatus) String() string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusInvalidCall:
		return "invalid call"
	case StatusNotJoined:
		return "not joined to a network"
	case StatusNetworkUp:
		return "network up"
	case StatusNetworkDown:
		return "network down"
	case StatusInvalidParameter:
		return "invalid parameter"
	default:
		return fmt.Sprintf("status 0x%02X", uint8(s))
	}
}

// OK reports whether the status indicates success.
func (s EmberStatus) OK() bool { return s == StatusSuccess }

// NetworkStatus reports whether the NCP is currently on a network. It answers
// the question that decides whether forming is safe.
type NetworkStatus uint8

// These follow EmberNetworkStatus. Note that "joined" is 2, not 1: value 1 is
// the transient joining state, and reading the two the other way round makes a
// live network look like it is still coming up.
const (
	NetworkNone     NetworkStatus = 0x00
	NetworkJoining  NetworkStatus = 0x01
	NetworkJoined   NetworkStatus = 0x02
	NetworkNoParent NetworkStatus = 0x03
	NetworkLeaving  NetworkStatus = 0x04
)

func (s NetworkStatus) String() string {
	switch s {
	case NetworkNone:
		return "down (no network)"
	case NetworkJoining:
		return "joining"
	case NetworkJoined:
		return "up"
	case NetworkNoParent:
		return "joined, no parent"
	case NetworkLeaving:
		return "leaving"
	default:
		return fmt.Sprintf("network status 0x%02X", uint8(s))
	}
}

// Joined reports whether the NCP holds live network credentials.
func (s NetworkStatus) Joined() bool {
	return s == NetworkJoined || s == NetworkNoParent
}

// NodeType is a device's logical role on the network.
type NodeType uint8

const (
	NodeUnknown     NodeType = 0x00
	NodeCoordinator NodeType = 0x01
	NodeRouter      NodeType = 0x02
	NodeEndDevice   NodeType = 0x03
	NodeSleepyEnd   NodeType = 0x04
)

func (n NodeType) String() string {
	switch n {
	case NodeUnknown:
		return "unknown"
	case NodeCoordinator:
		return "coordinator"
	case NodeRouter:
		return "router"
	case NodeEndDevice:
		return "end device"
	case NodeSleepyEnd:
		return "sleepy end device"
	default:
		return fmt.Sprintf("node type 0x%02X", uint8(n))
	}
}
