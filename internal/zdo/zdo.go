// Package zdo implements the Zigbee Device Objects layer: the addressing and
// discovery protocol that answers "what is this device?".
//
// ZDO travels on profile 0x0000, and every request has a matching response
// whose cluster ID is the request's with bit 15 set. Both carry a transaction
// sequence number in their first byte, which is how a reply is matched to the
// question that produced it.
package zdo

import (
	"encoding/binary"
	"fmt"
)

// Request cluster IDs.
const (
	ClusterNodeDescriptorReq   uint16 = 0x0002
	ClusterPowerDescriptorReq  uint16 = 0x0003
	ClusterSimpleDescriptorReq uint16 = 0x0004
	ClusterActiveEndpointsReq  uint16 = 0x0005
	ClusterDeviceAnnounce      uint16 = 0x0013
)

// ResponseCluster is the cluster a reply to the given request arrives on.
func ResponseCluster(request uint16) uint16 { return request | 0x8000 }

// Status is a ZDO result code.
type Status uint8

const (
	StatusSuccess        Status = 0x00
	StatusInvalidRequest Status = 0x80
	StatusDeviceNotFound Status = 0x81
	StatusInvalidEP      Status = 0x82
	StatusNotActive      Status = 0x83
	StatusNotSupported   Status = 0x84
	StatusTimeout        Status = 0x85
	StatusNoDescriptor   Status = 0x89
)

func (s Status) String() string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusInvalidRequest:
		return "invalid request type"
	case StatusDeviceNotFound:
		return "device not found"
	case StatusInvalidEP:
		return "invalid endpoint"
	case StatusNotActive:
		return "endpoint not active"
	case StatusNotSupported:
		return "not supported"
	case StatusTimeout:
		return "timeout"
	case StatusNoDescriptor:
		return "no descriptor"
	default:
		return fmt.Sprintf("zdo status 0x%02X", uint8(s))
	}
}

// OK reports whether the request succeeded.
func (s Status) OK() bool { return s == StatusSuccess }

// AddressRequest builds any of the ZDO requests whose only parameter is the
// address of interest: node descriptor, power descriptor, active endpoints.
func AddressRequest(seq uint8, addr uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{seq}, addr)
}

// SimpleDescriptorRequest asks for the descriptor of one endpoint.
func SimpleDescriptorRequest(seq uint8, addr uint16, endpoint uint8) []byte {
	return append(AddressRequest(seq, addr), endpoint)
}

// LogicalType is a device's role, as reported in its node descriptor.
type LogicalType uint8

const (
	TypeCoordinator LogicalType = 0
	TypeRouter      LogicalType = 1
	TypeEndDevice   LogicalType = 2
)

func (t LogicalType) String() string {
	switch t {
	case TypeCoordinator:
		return "coordinator"
	case TypeRouter:
		return "router"
	case TypeEndDevice:
		return "end device"
	default:
		return fmt.Sprintf("logical type %d", uint8(t))
	}
}

// MACCapability is the capability bitmask carried in a node descriptor. It is
// the same field a device broadcasts when it announces itself.
type MACCapability uint8

const (
	CapAlternatePANCoord MACCapability = 0x01
	CapFullFunction      MACCapability = 0x02
	CapMainsPowered      MACCapability = 0x04
	CapRxOnWhenIdle      MACCapability = 0x08
	CapSecurityCapable   MACCapability = 0x40
	CapAllocateAddress   MACCapability = 0x80
)

// Mains reports whether the device runs from mains rather than a battery.
func (c MACCapability) Mains() bool { return c&CapMainsPowered != 0 }

// Sleepy reports whether the device sleeps between transmissions. A sleepy
// device cannot be reached on demand; it must be caught while awake.
func (c MACCapability) Sleepy() bool { return c&CapRxOnWhenIdle == 0 }

// NodeDescriptor describes a device's capabilities as a network node.
type NodeDescriptor struct {
	LogicalType      LogicalType   `json:"logical_type"`
	MACCapability    MACCapability `json:"mac_capability"`
	ManufacturerCode uint16        `json:"manufacturer_code"`
	MaxBufferSize    uint8         `json:"max_buffer_size"`
	MaxIncomingSize  uint16        `json:"max_incoming_transfer_size"`
	ServerMask       uint16        `json:"server_mask"`
	MaxOutgoingSize  uint16        `json:"max_outgoing_transfer_size"`
}

// ParseNodeDescriptor decodes a Node Descriptor Response payload.
func ParseNodeDescriptor(payload []byte) (uint16, NodeDescriptor, Status, error) {
	r := &cursor{b: payload}
	_ = r.u8() // transaction sequence
	status := Status(r.u8())
	addr := r.u16()
	if r.err != nil {
		return 0, NodeDescriptor{}, status, fmt.Errorf("zdo: node descriptor response: %w", r.err)
	}
	if !status.OK() {
		return addr, NodeDescriptor{}, status, nil
	}

	first := r.u8()
	_ = r.u8() // APS flags and frequency band
	nd := NodeDescriptor{
		LogicalType:      LogicalType(first & 0x07),
		MACCapability:    MACCapability(r.u8()),
		ManufacturerCode: r.u16(),
		MaxBufferSize:    r.u8(),
		MaxIncomingSize:  r.u16(),
		ServerMask:       r.u16(),
		MaxOutgoingSize:  r.u16(),
	}
	if r.err != nil {
		return addr, NodeDescriptor{}, status, fmt.Errorf("zdo: node descriptor: %w", r.err)
	}
	return addr, nd, status, nil
}

// ParseActiveEndpoints decodes an Active Endpoint Response payload.
func ParseActiveEndpoints(payload []byte) (uint16, []uint8, Status, error) {
	r := &cursor{b: payload}
	_ = r.u8()
	status := Status(r.u8())
	addr := r.u16()
	if r.err != nil {
		return 0, nil, status, fmt.Errorf("zdo: active endpoint response: %w", r.err)
	}
	if !status.OK() {
		return addr, nil, status, nil
	}

	count := int(r.u8())
	eps := make([]uint8, 0, count)
	for i := 0; i < count; i++ {
		eps = append(eps, r.u8())
	}
	if r.err != nil {
		return addr, nil, status, fmt.Errorf("zdo: active endpoint list: %w", r.err)
	}
	return addr, eps, status, nil
}

// SimpleDescriptor describes what one endpoint implements. It is the answer to
// "what can this device do", in the form of the clusters it speaks.
type SimpleDescriptor struct {
	Endpoint uint8    `json:"endpoint"`
	Profile  uint16   `json:"profile"`
	Device   uint16   `json:"device_id"`
	Version  uint8    `json:"version"`
	Input    []uint16 `json:"input_clusters"`
	Output   []uint16 `json:"output_clusters"`
}

// ParseSimpleDescriptor decodes a Simple Descriptor Response payload.
func ParseSimpleDescriptor(payload []byte) (uint16, SimpleDescriptor, Status, error) {
	r := &cursor{b: payload}
	_ = r.u8()
	status := Status(r.u8())
	addr := r.u16()
	if r.err != nil {
		return 0, SimpleDescriptor{}, status, fmt.Errorf("zdo: simple descriptor response: %w", r.err)
	}
	if !status.OK() {
		return addr, SimpleDescriptor{}, status, nil
	}
	_ = r.u8() // descriptor length, implied by the fields that follow

	sd := SimpleDescriptor{
		Endpoint: r.u8(),
		Profile:  r.u16(),
		Device:   r.u16(),
		Version:  r.u8() & 0x0F,
	}
	inCount := int(r.u8())
	for i := 0; i < inCount; i++ {
		sd.Input = append(sd.Input, r.u16())
	}
	outCount := int(r.u8())
	for i := 0; i < outCount; i++ {
		sd.Output = append(sd.Output, r.u16())
	}
	if r.err != nil {
		return addr, SimpleDescriptor{}, status, fmt.Errorf("zdo: simple descriptor: %w", r.err)
	}
	return addr, sd, status, nil
}

// PowerDescriptor reports how a device is powered.
type PowerDescriptor struct {
	CurrentMode  uint8 `json:"current_mode"`
	AvailableSrc uint8 `json:"available_sources"`
	CurrentSrc   uint8 `json:"current_source"`
	CurrentLevel uint8 `json:"current_level"`
}

// Power source bits, used by both the available and current source fields.
const (
	PowerMains      uint8 = 0x01
	PowerRechargble uint8 = 0x02
	PowerDisposable uint8 = 0x04
)

// Description renders the power source in words.
func (p PowerDescriptor) Description() string {
	switch {
	case p.CurrentSrc&PowerMains != 0:
		return "mains"
	case p.CurrentSrc&PowerRechargble != 0:
		return "rechargeable battery"
	case p.CurrentSrc&PowerDisposable != 0:
		return "disposable battery"
	default:
		return fmt.Sprintf("power source 0x%02X", p.CurrentSrc)
	}
}

// ParsePowerDescriptor decodes a Power Descriptor Response payload.
func ParsePowerDescriptor(payload []byte) (uint16, PowerDescriptor, Status, error) {
	r := &cursor{b: payload}
	_ = r.u8()
	status := Status(r.u8())
	addr := r.u16()
	if r.err != nil {
		return 0, PowerDescriptor{}, status, fmt.Errorf("zdo: power descriptor response: %w", r.err)
	}
	if !status.OK() {
		return addr, PowerDescriptor{}, status, nil
	}

	// Four nibbles packed into two bytes.
	w := r.u16()
	if r.err != nil {
		return addr, PowerDescriptor{}, status, fmt.Errorf("zdo: power descriptor: %w", r.err)
	}
	return addr, PowerDescriptor{
		CurrentMode:  uint8(w & 0x0F),
		AvailableSrc: uint8(w >> 4 & 0x0F),
		CurrentSrc:   uint8(w >> 8 & 0x0F),
		CurrentLevel: uint8(w >> 12 & 0x0F),
	}, status, nil
}

// Sequence reads the transaction sequence number a ZDO payload opens with,
// which is what matches a response to its request.
func Sequence(payload []byte) (uint8, bool) {
	if len(payload) == 0 {
		return 0, false
	}
	return payload[0], true
}

// cursor is a bounds-checked little-endian reader with a latched error.
type cursor struct {
	b   []byte
	pos int
	err error
}

func (r *cursor) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.pos+n > len(r.b) {
		r.err = fmt.Errorf("truncated: need %d bytes at offset %d, have %d", n, r.pos, len(r.b))
		return false
	}
	return true
}

func (r *cursor) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *cursor) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}
