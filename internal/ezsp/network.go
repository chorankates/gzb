package ezsp

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

// Security and formation command IDs.
const (
	FrameSetInitialSecurityState FrameID = 0x0068
	FrameGetCurrentSecurityState FrameID = 0x0069
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
	secRequireEncryptedKey      uint16 = 0x0800
)

// Join methods for EmberNetworkParameters.
const (
	joinMACAssociation uint8 = 0x00
)

// EmberStatus values seen during formation.
const (
	StatusNetworkAlreadyUp EmberStatus = 0x91
)

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
	var w wbuf
	// A centralised Zigbee 3.0 network: we hold both keys, devices must use
	// the global trust-centre link key, and the network key may only be
	// delivered encrypted.
	w.u16(secHavePreconfiguredKey | secHaveNetworkKey | secRequireEncryptedKey | secTrustCenterGlobalLinkKey)
	w.bytes(ZigbeeAlliance09Key[:])
	w.bytes(networkKey[:])
	w.u8(0) // network key sequence number
	w.ieee(EUI64{})

	params, err := c.command(ctx, FrameSetInitialSecurityState, w.b)
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

	var w wbuf
	w.ieee(cfg.ExtendedPanID)
	w.u16(cfg.PanID)
	w.u8(uint8(cfg.TxPower))
	w.u8(cfg.Channel)
	w.u8(joinMACAssociation)
	w.u16(0) // network manager ID
	w.u8(0)  // network update ID
	w.u32(DefaultChannelMask)

	params, err := c.command(ctx, FrameFormNetwork, w.b)
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

	if err := awaitNetworkUp(ctx, up); err != nil {
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

// awaitNetworkUp waits for the stack to report that the network is live.
func awaitNetworkUp(ctx context.Context, up <-chan Message) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
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
			// Any other stack status during formation is a failure to report.
			if status != StatusSuccess {
				return fmt.Errorf("ezsp: stack reported %s while forming", status)
			}
		case <-ctx.Done():
			return fmt.Errorf("ezsp: network did not come up: %w", ctx.Err())
		}
	}
}

// randomPanID picks a PAN ID, avoiding the two reserved values.
func randomPanID() (uint16, error) {
	var b [2]byte
	for i := 0; i < 8; i++ {
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
