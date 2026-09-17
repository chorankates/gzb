package ezsp

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

// Security and formation command IDs.
const (
	FrameSetExtendedSecurityBitmask FrameID = 0x0066
	FrameSetInitialSecurityState    FrameID = 0x0068
	FrameGetCurrentSecurityState    FrameID = 0x0069
)

// DefaultChannelMask covers the whole 2.4 GHz Zigbee band, channels 11-26.
const DefaultChannelMask uint32 = 0x07FFF800

// ZigbeeAlliance09Key is the well-known trust-centre link key defined by the
// Zigbee Alliance. It is public by design: joining devices use it to encrypt
// the one exchange in which the real, secret network key is delivered.
var ZigbeeAlliance09Key = [16]byte{
	'Z', 'i', 'g', 'B', 'e', 'e', 'A', 'l', 'l', 'i', 'a', 'n', 'c', 'e', '0', '9',
}

// Initial security bitmask flags, as passed to setInitialSecurityState.
const (
	secTrustCenterGlobalLinkKey uint16 = 0x0004
	secHavePreconfiguredKey     uint16 = 0x0100
	secHaveNetworkKey           uint16 = 0x0200
	secGetLinkKeyWhenJoining    uint16 = 0x0400
	secRequireEncryptedKey      uint16 = 0x0800
	secNoFrameCounterReset      uint16 = 0x1000
)

// coordinatorSecurity is the bitmask for a centralised Zigbee 3.0 network:
// we hold both keys, devices must use the global trust-centre link key, and
// the network key may only be delivered encrypted.
const coordinatorSecurity = secHavePreconfiguredKey | secHaveNetworkKey | secRequireEncryptedKey | secTrustCenterGlobalLinkKey

// joinerSecurity is the bitmask for a node joining somebody else's network.
// It holds only the well-known link key; the network key must come from the
// trust centre, encrypted under that. Asking for a link key of its own once
// joined is what a Zigbee 3.0 trust centre expects of a new node, and what
// gzb's own trust centre answers by generating one. The frame counters are
// kept across the call so a rejoin is not rejected as a replay.
const joinerSecurity = secHavePreconfiguredKey | secRequireEncryptedKey | secTrustCenterGlobalLinkKey |
	secGetLinkKeyWhenJoining | secNoFrameCounterReset

// Extended security bitmask flags, as passed to setExtendedSecurityBitmask.
const (
	extJoinerGlobalLinkKey uint16 = 0x0010
	extNoFrameCounterReset uint16 = 0x0020
	extR21StackBehavior    uint16 = 0x0080
	joinerExtendedSecurity        = extJoinerGlobalLinkKey | extNoFrameCounterReset | extR21StackBehavior
)

// Join methods for EmberNetworkParameters.
const (
	joinMACAssociation uint8 = 0x00
)

// networkUpTimeout bounds how long forming or joining may take to be
// reported up. Forming is near-instant; a join is an association, a key
// transport and a link-key request, each of them a round trip to a trust
// centre that may be busy.
const networkUpTimeout = 30 * time.Second

// stackStatusNetworkUp is the EmberStatus reported by stackStatusHandler once
// the network is live.
const stackStatusNetworkUp EmberStatus = 0x90

// FormationConfig describes the network to create.
type FormationConfig struct {
	// Channel is the 2.4 GHz channel, 11-26.
	Channel uint8
	// PanID is the 16-bit network identifier. Zero means pick one at random.
	PanID uint16
	// ExtendedPanID is the 64-bit network identifier. Zero means random.
	ExtendedPanID EUI64
	// NetworkKey is the secret key protecting all network traffic. Zero means
	// generate a fresh random key, which is what you want.
	NetworkKey [16]byte
	// TxPower is the radio transmit power in dBm.
	TxPower int8
}

// FormationResult reports what was actually created.
type FormationResult struct {
	Channel       uint8    `json:"channel"`
	PanID         uint16   `json:"pan_id"`
	ExtendedPanID EUI64    `json:"extended_pan_id"`
	NetworkKey    [16]byte `json:"-"`
	TxPower       int8     `json:"tx_power_dbm"`
	NodeID        uint16   `json:"node_id"`
	IEEE          EUI64    `json:"ieee"`
}

// SetInitialSecurityState installs the keys the stack will use. It must be
// called before forming a network and takes effect only while the network is
// down.
func (c *Conn) SetInitialSecurityState(ctx context.Context, networkKey [16]byte) error {
	return c.setInitialSecurityState(ctx, coordinatorSecurity, networkKey)
}

// SetJoinerSecurityState installs the keys a node needs to join an existing
// network: the well-known link key and nothing else. The network key arrives
// from the trust centre during the join. Like the coordinator's version it
// takes effect only while the network is down.
func (c *Conn) SetJoinerSecurityState(ctx context.Context) error {
	if err := c.setInitialSecurityState(ctx, joinerSecurity, [16]byte{}); err != nil {
		return err
	}
	// The extended bitmask tells the stack the joiner's link key is the global
	// one, which is how it knows to ask for its own afterwards. It is
	// best-effort on the same reasoning as the transient key in AllowJoins:
	// the initial state above is what the join depends on, and firmware that
	// refuses this is no worse off.
	_ = c.setExtendedSecurityBitmask(ctx, joinerExtendedSecurity)
	return nil
}

// securityStateParams encodes EmberInitialSecurityState.
//
//	bitmask(2) preconfiguredKey(16) networkKey(16) networkKeySequenceNumber(1)
//	preconfiguredTrustCenterEui64(8)
func securityStateParams(bitmask uint16, networkKey [16]byte) []byte {
	var w wbuf
	w.u16(bitmask)
	w.bytes(ZigbeeAlliance09Key[:])
	w.bytes(networkKey[:])
	w.u8(0) // network key sequence number
	w.ieee(EUI64{})
	return w.b
}

func (c *Conn) setInitialSecurityState(ctx context.Context, bitmask uint16, networkKey [16]byte) error {
	params, err := c.command(ctx, FrameSetInitialSecurityState, securityStateParams(bitmask, networkKey))
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: setInitialSecurityState: %s", status)
	}
	return nil
}

func (c *Conn) setExtendedSecurityBitmask(ctx context.Context, bitmask uint16) error {
	var w wbuf
	w.u16(bitmask)
	params, err := c.command(ctx, FrameSetExtendedSecurityBitmask, w.b)
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: setExtendedSecurityBitmask: %s", status)
	}
	return nil
}

// FormNetwork creates a new Zigbee network with this adapter as coordinator.
//
// This is destructive: it writes new credentials to the adapter and any
// devices joined to a previous network will be orphaned, since they hold the
// old network key. Callers must confirm intent before calling.
func (c *Conn) FormNetwork(ctx context.Context, cfg FormationConfig) (FormationResult, error) {
	if cfg.Channel < 11 || cfg.Channel > 26 {
		return FormationResult{}, fmt.Errorf("ezsp: channel %d is outside the Zigbee range 11-26", cfg.Channel)
	}

	// Refuse to form on top of a live network rather than silently orphaning
	// whatever is already joined.
	state, err := c.NetworkState(ctx)
	if err != nil {
		return FormationResult{}, fmt.Errorf("checking network state: %w", err)
	}
	if state.Joined() {
		return FormationResult{}, fmt.Errorf("ezsp: adapter is already on a network (%s); leave it first", state)
	}

	if cfg.NetworkKey == ([16]byte{}) {
		if _, err := rand.Read(cfg.NetworkKey[:]); err != nil {
			return FormationResult{}, fmt.Errorf("generating network key: %w", err)
		}
	}
	if cfg.ExtendedPanID.IsZero() {
		if _, err := rand.Read(cfg.ExtendedPanID[:]); err != nil {
			return FormationResult{}, fmt.Errorf("generating extended PAN ID: %w", err)
		}
	}
	if cfg.PanID == 0 {
		if cfg.PanID, err = randomPanID(); err != nil {
			return FormationResult{}, err
		}
	}
	if cfg.TxPower == 0 {
		cfg.TxPower = 8
	}

	if err := c.SetInitialSecurityState(ctx, cfg.NetworkKey); err != nil {
		return FormationResult{}, err
	}

	// Subscribe before issuing the command: the stack can report the network
	// up before formNetwork's own response is processed.
	up, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && m.ID == FrameStackStatusHandler
	}, 4)
	defer cancel()

	params, err := c.command(ctx, FrameFormNetwork, networkParams(NetworkParameters{
		ExtendedPanID: cfg.ExtendedPanID,
		PanID:         cfg.PanID,
		RadioTxPower:  cfg.TxPower,
		RadioChannel:  cfg.Channel,
		JoinMethod:    joinMACAssociation,
		Channels:      DefaultChannelMask,
	}))
	if err != nil {
		return FormationResult{}, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return FormationResult{}, r.err
	}
	if !status.OK() {
		return FormationResult{}, fmt.Errorf("ezsp: formNetwork: %s", status)
	}

	if err := awaitNetworkUp(ctx, up, "forming"); err != nil {
		return FormationResult{}, err
	}

	// Read back what the stack actually settled on rather than reporting what
	// we asked for.
	_, np, err := c.NetworkParameters(ctx)
	if err != nil {
		return FormationResult{}, fmt.Errorf("reading back network parameters: %w", err)
	}
	nodeID, _ := c.NodeID(ctx)
	ieee, _ := c.EUI64(ctx)

	return FormationResult{
		Channel:       np.RadioChannel,
		PanID:         np.PanID,
		ExtendedPanID: np.ExtendedPanID,
		NetworkKey:    cfg.NetworkKey,
		TxPower:       np.RadioTxPower,
		NodeID:        nodeID,
		IEEE:          ieee,
	}, nil
}

// networkParams encodes EmberNetworkParameters, the 20-byte description of a
// network that formNetwork and joinNetwork both take.
//
//	extendedPanId(8) panId(2) radioTxPower(1) radioChannel(1) joinMethod(1)
//	nwkManagerId(2) nwkUpdateId(1) channels(4)
func networkParams(np NetworkParameters) []byte {
	var w wbuf
	w.ieee(np.ExtendedPanID)
	w.u16(np.PanID)
	w.u8(uint8(np.RadioTxPower))
	w.u8(np.RadioChannel)
	w.u8(np.JoinMethod)
	w.u16(np.NwkManagerID)
	w.u8(np.NwkUpdateID)
	w.u32(np.Channels)
	return w.b
}

// JoinConfig names the network to join. Everything but the transmit power
// and role is what an active scan reports for it.
type JoinConfig struct {
	Channel       uint8
	PanID         uint16
	ExtendedPanID EUI64
	// NwkUpdateID is the network's current update counter, from its beacon.
	NwkUpdateID uint8
	// TxPower is the radio transmit power in dBm. Zero means 8.
	TxPower int8
	// NodeType is the role to join as. Zero means router, which is the role
	// for a mains-powered node that is always listening.
	NodeType NodeType
}

// JoinResult reports the seat the adapter took on the network.
type JoinResult struct {
	NodeType      NodeType `json:"node_type"`
	Channel       uint8    `json:"channel"`
	PanID         uint16   `json:"pan_id"`
	ExtendedPanID EUI64    `json:"extended_pan_id"`
	TxPower       int8     `json:"tx_power_dbm"`
	NodeID        uint16   `json:"node_id"`
	IEEE          EUI64    `json:"ieee"`
}

// JoinNetwork joins an existing network as a router or end device. The
// network's coordinator must be accepting joins, which is what ScanNetworks
// reports as AllowingJoin; a closed network fails with StatusJoinFailed
// rather than silence, but only after the association times out.
//
// The credentials the trust centre hands over are stored in the adapter's
// tokens, so NetworkInit brings the node back onto the network on every
// later connection. It refuses to run on an adapter that already holds a
// network: leaving is a separate, deliberate step.
func (c *Conn) JoinNetwork(ctx context.Context, cfg JoinConfig) (JoinResult, error) {
	if cfg.Channel < 11 || cfg.Channel > 26 {
		return JoinResult{}, fmt.Errorf("ezsp: channel %d is outside the Zigbee range 11-26", cfg.Channel)
	}
	if cfg.ExtendedPanID.IsZero() {
		return JoinResult{}, fmt.Errorf("ezsp: joining needs the network's extended PAN ID")
	}
	if cfg.TxPower == 0 {
		cfg.TxPower = 8
	}
	if cfg.NodeType == NodeUnknown {
		cfg.NodeType = NodeRouter
	}
	if cfg.NodeType != NodeRouter && cfg.NodeType != NodeEndDevice && cfg.NodeType != NodeSleepyEnd {
		return JoinResult{}, fmt.Errorf("ezsp: cannot join as %s", cfg.NodeType)
	}

	state, err := c.NetworkState(ctx)
	if err != nil {
		return JoinResult{}, fmt.Errorf("checking network state: %w", err)
	}
	if state.Joined() {
		return JoinResult{}, fmt.Errorf("ezsp: adapter is already on a network (%s); leave it first", state)
	}

	if err := c.SetJoinerSecurityState(ctx); err != nil {
		return JoinResult{}, err
	}

	// Subscribe before issuing the command, as formation does: the stack can
	// report a failure before joinNetwork's own response is processed.
	up, cancel := c.Subscribe(func(m Message) bool {
		return m.Callback && m.ID == FrameStackStatusHandler
	}, 4)
	defer cancel()

	params, err := c.command(ctx, FrameJoinNetwork, joinParams(cfg))
	if err != nil {
		return JoinResult{}, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return JoinResult{}, r.err
	}
	if !status.OK() {
		return JoinResult{}, fmt.Errorf("ezsp: joinNetwork: %s", status)
	}

	if err := awaitNetworkUp(ctx, up, "joining"); err != nil {
		return JoinResult{}, err
	}

	// Report the seat the stack actually took, not the one asked for.
	nodeType, np, err := c.NetworkParameters(ctx)
	if err != nil {
		return JoinResult{}, fmt.Errorf("reading back network parameters: %w", err)
	}
	nodeID, _ := c.NodeID(ctx)
	ieee, _ := c.EUI64(ctx)

	return JoinResult{
		NodeType:      nodeType,
		Channel:       np.RadioChannel,
		PanID:         np.PanID,
		ExtendedPanID: np.ExtendedPanID,
		TxPower:       np.RadioTxPower,
		NodeID:        nodeID,
		IEEE:          ieee,
	}, nil
}

// joinParams encodes the joinNetwork command: the role, then the network.
//
//	nodeType(1) EmberNetworkParameters(20)
func joinParams(cfg JoinConfig) []byte {
	var w wbuf
	w.u8(uint8(cfg.NodeType))
	w.bytes(networkParams(NetworkParameters{
		ExtendedPanID: cfg.ExtendedPanID,
		PanID:         cfg.PanID,
		RadioTxPower:  cfg.TxPower,
		RadioChannel:  cfg.Channel,
		JoinMethod:    joinMACAssociation,
		NwkUpdateID:   cfg.NwkUpdateID,
		Channels:      1 << uint32(cfg.Channel),
	}))
	return w.b
}

// awaitNetworkUp waits for the stack to report that the network is live.
// Any other failure status the stack reports first ends the wait: during a
// join those name what went wrong — no beacons, association refused, no key
// from the trust centre — and are the answer, not something to wait through.
func awaitNetworkUp(ctx context.Context, up <-chan Message, doing string) error {
	ctx, cancel := context.WithTimeout(ctx, networkUpTimeout)
	defer cancel()

	for {
		select {
		case m, ok := <-up:
			if !ok {
				return ErrClosed
			}
			r := newBuf(m.Params)
			status := EmberStatus(r.u8())
			if r.err != nil {
				continue
			}
			if status == stackStatusNetworkUp {
				return nil
			}
			if status != StatusSuccess {
				return fmt.Errorf("ezsp: stack reported %s while %s", status, doing)
			}
		case <-ctx.Done():
			return fmt.Errorf("ezsp: network did not come up: %w", ctx.Err())
		}
	}
}

// randomPanID picks a PAN ID, avoiding the two reserved values.
func randomPanID() (uint16, error) {
	var b [2]byte
	for range 8 {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("generating PAN ID: %w", err)
		}
		id := uint16(b[0]) | uint16(b[1])<<8
		if id != 0x0000 && id != 0xFFFF {
			return id, nil
		}
	}
	return 0, fmt.Errorf("ezsp: could not generate a usable PAN ID")
}

// LeaveNetwork tears down the current network. Devices joined to it are
// orphaned and must be re-paired.
func (c *Conn) LeaveNetwork(ctx context.Context) error {
	params, err := c.command(ctx, FrameLeaveNetwork, nil)
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: leaveNetwork: %s", status)
	}
	return nil
}

// NetworkInit restores a network previously saved in the adapter's tokens.
// It returns StatusNotJoined when there is nothing to restore, which is the
// normal answer on a fresh adapter and not an error.
func (c *Conn) NetworkInit(ctx context.Context) (EmberStatus, error) {
	// EmberNetworkInitStruct: a bitmask controlling how the stack rejoins.
	// Zero means the plain restore that a coordinator wants.
	params, err := c.command(ctx, FrameNetworkInit, []byte{0x00, 0x00})
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	return status, r.err
}

// PermitJoining opens the network to new devices for the given duration.
// A duration of 0 closes it immediately; 255 leaves it open indefinitely,
// which is a standing invitation and should be avoided.
func (c *Conn) PermitJoining(ctx context.Context, seconds uint8) error {
	params, err := c.command(ctx, FramePermitJoining, []byte{seconds})
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: permitJoining: %s", status)
	}
	return nil
}
