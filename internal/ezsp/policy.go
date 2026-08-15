package ezsp

import (
	"context"
	"fmt"
)

// Policy commands.
const (
	FrameSetPolicy           FrameID = 0x0055
	FrameGetPolicy           FrameID = 0x0056
	FrameAddTransientLinkKey FrameID = 0x00AF
)

// BroadcastEUI64 is the wildcard address meaning "any joining device".
var BroadcastEUI64 = EUI64{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// AddTransientLinkKey installs a short-lived link key for a device that has
// not joined yet.
//
// A Zigbee 3.0 device joins by encrypting its first exchange with the
// well-known trust-centre link key, and the trust centre needs a matching
// transient entry to answer. Installing it against the wildcard address makes
// the trust centre willing to talk to any device that arrives during the join
// window. The entry expires on its own, so it does not leave the network
// permanently open.
func (c *Conn) AddTransientLinkKey(ctx context.Context, partner EUI64, key [16]byte) error {
	var w wbuf
	w.ieee(partner)
	w.bytes(key[:])

	params, err := c.command(ctx, FrameAddTransientLinkKey, w.b)
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: addTransientLinkKey: %s", status)
	}
	return nil
}

// PolicyID selects one of the NCP's configurable behaviours.
type PolicyID uint8

const (
	PolicyTrustCenter    PolicyID = 0x00
	PolicyBindingMod     PolicyID = 0x01
	PolicyUnicastReplies PolicyID = 0x02
	PolicyPollHandler    PolicyID = 0x03
	PolicyMessageContent PolicyID = 0x04
	PolicyTCKeyRequest   PolicyID = 0x05
	PolicyAppKeyRequest  PolicyID = 0x06
)

func (p PolicyID) String() string {
	switch p {
	case PolicyTrustCenter:
		return "trustCenter"
	case PolicyBindingMod:
		return "bindingModification"
	case PolicyUnicastReplies:
		return "unicastReplies"
	case PolicyPollHandler:
		return "pollHandler"
	case PolicyMessageContent:
		return "messageContentsInCallback"
	case PolicyTCKeyRequest:
		return "trustCenterKeyRequest"
	case PolicyAppKeyRequest:
		return "applicationKeyRequest"
	default:
		return fmt.Sprintf("policy 0x%02X", uint8(p))
	}
}

// Trust-centre policy decision bits. These control what the coordinator does
// when a device tries to join.
const (
	// DecisionAllowJoins permits devices to join using the global
	// trust-centre link key. Without it the NCP denies every join, however
	// wide open permitJoining leaves the window.
	DecisionAllowJoins uint8 = 0x01
	// DecisionAllowUnsecuredRejoins lets a device that already holds the
	// network key rejoin after losing its parent — a battery sensor waking
	// up to find its router gone, typically.
	DecisionAllowUnsecuredRejoins uint8 = 0x02
	// DecisionSendKeyInClear transmits the network key unencrypted. This is
	// pre-3.0 behaviour and is deliberately not used here.
	DecisionSendKeyInClear uint8 = 0x04
	// DecisionJoinsUseInstallCodeKey requires per-device install codes.
	DecisionJoinsUseInstallCodeKey uint8 = 0x10
)

// Trust-centre key request decisions.
const (
	DecisionDenyTCKeyRequests uint8 = 0x00
	// DecisionAllowTCKeyRequests answers a key request with the key the trust
	// centre already holds — the well-known global one. A Zigbee 3.0 device
	// that asked for a key of its own can reject that and leave the network.
	DecisionAllowTCKeyRequests uint8 = 0x50
	// DecisionGenerateTCKeyOnRequest mints a unique link key for the asking
	// device, which is what Zigbee 3.0 expects of a centralised trust centre.
	DecisionGenerateTCKeyOnRequest uint8 = 0x51
	DecisionDenyAppKeyRequests     uint8 = 0x60
	DecisionAllowAppKeyRequests    uint8 = 0x61
)

// EzspStatus is the result of a host-to-NCP configuration command. It is a
// separate enum from EmberStatus, which reports stack operations.
type EzspStatus uint8

const (
	EzspSuccess EzspStatus = 0x00
	// EzspErrorInvalidCall means the command is not valid in the NCP's current
	// state — most often a configuration write attempted while the network is
	// up, since most values may only be changed while it is down.
	EzspErrorVersionNotSet  EzspStatus = 0x30
	EzspErrorInvalidFrameID EzspStatus = 0x31
	EzspErrorInvalidValue   EzspStatus = 0x36
	EzspErrorInvalidID      EzspStatus = 0x37
	EzspErrorInvalidCall    EzspStatus = 0x38
	EzspErrorNoResponse     EzspStatus = 0x39
)

func (s EzspStatus) String() string {
	switch s {
	case EzspSuccess:
		return "success"
	case EzspErrorVersionNotSet:
		return "version not set"
	case EzspErrorInvalidFrameID:
		return "invalid frame id"
	case EzspErrorInvalidValue:
		return "invalid value"
	case EzspErrorInvalidID:
		return "invalid id"
	case EzspErrorInvalidCall:
		return "invalid call in this state (the network is probably up)"
	case EzspErrorNoResponse:
		return "no response"
	default:
		return fmt.Sprintf("ezsp status 0x%02X", uint8(s))
	}
}

// OK reports whether the command succeeded.
func (s EzspStatus) OK() bool { return s == EzspSuccess }

// SetPolicy configures one NCP policy.
func (c *Conn) SetPolicy(ctx context.Context, id PolicyID, decision uint8) error {
	params, err := c.command(ctx, FrameSetPolicy, []byte{uint8(id), decision})
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EzspStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: setPolicy %s = 0x%02X: %s", id, decision, status)
	}
	return nil
}

// GetPolicy reads back one NCP policy.
func (c *Conn) GetPolicy(ctx context.Context, id PolicyID) (uint8, error) {
	params, err := c.command(ctx, FrameGetPolicy, []byte{uint8(id)})
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	status := EzspStatus(r.u8())
	decision := r.u8()
	if r.err != nil {
		return 0, r.err
	}
	if !status.OK() {
		return 0, fmt.Errorf("ezsp: getPolicy %s: %s", id, status)
	}
	return decision, nil
}

// AllowJoins configures the trust centre to accept joining devices on a
// Zigbee 3.0 centralised network.
//
// This is separate from permitJoining, and both are required: permitJoining
// opens the time window, while these policies decide what happens to a device
// that arrives during it. The NCP resets its policies whenever it reboots, and
// resetting the ASH link reboots it, so this must be re-applied every session
// rather than set once at formation.
func (c *Conn) AllowJoins(ctx context.Context) error {
	want := DecisionAllowJoins | DecisionAllowUnsecuredRejoins
	if err := c.SetPolicy(ctx, PolicyTrustCenter, want); err != nil {
		return err
	}
	// Zigbee 3.0 devices ask the trust centre for a link key of their own once
	// they are on the network. Generate one rather than handing back the
	// well-known global key: refusing the request, or answering it with a key
	// the device considers insecure, makes it join and then drop off again.
	if err := c.SetPolicy(ctx, PolicyTCKeyRequest, DecisionGenerateTCKeyOnRequest); err != nil {
		return err
	}

	// Read the policy back rather than trusting the success status. A silently
	// ineffective policy and a device that was never in pairing mode look
	// identical from here — nothing joins — and that ambiguity is expensive to
	// debug against hardware.
	got, err := c.GetPolicy(ctx, PolicyTrustCenter)
	if err != nil {
		return fmt.Errorf("verifying trust centre policy: %w", err)
	}
	if got&DecisionAllowJoins == 0 {
		return fmt.Errorf("ezsp: trust centre still refuses joins after setPolicy (policy reads 0x%02X, wanted 0x%02X)", got, want)
	}

	// Offer the trust centre a transient key for whoever turns up. This is
	// deliberately best-effort: EmberZNet 7.4.4 rejects it with 0xB5, and it is
	// not the mechanism this network relies on. setInitialSecurityState already
	// installed ZigBeeAlliance09 as the global trust-centre link key — confirmed
	// by getCurrentSecurityState reporting bitmask 0x0074 — and that key covers
	// every joining device. Firmware that wants a transient entry instead gets
	// one; firmware that refuses is no worse off.
	_ = c.AddTransientLinkKey(ctx, BroadcastEUI64, ZigbeeAlliance09Key)
	return nil
}
