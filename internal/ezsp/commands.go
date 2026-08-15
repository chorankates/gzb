package ezsp

import (
	"context"
	"fmt"
)

// NetworkState reports whether the NCP currently holds network credentials.
// This is the check that decides whether forming a network is safe: forming
// on top of a live network would orphan every device already joined to it.
func (c *Conn) NetworkState(ctx context.Context) (NetworkStatus, error) {
	params, err := c.command(ctx, FrameNetworkState, nil)
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	status := NetworkStatus(r.u8())
	return status, r.err
}

// EUI64 returns the NCP's own IEEE address.
func (c *Conn) EUI64(ctx context.Context) (EUI64, error) {
	params, err := c.command(ctx, FrameGetEUI64, nil)
	if err != nil {
		return EUI64{}, err
	}
	r := newBuf(params)
	addr := r.ieee()
	return addr, r.err
}

// NodeID returns the NCP's 16-bit network address. It is meaningful only
// while the NCP is on a network.
func (c *Conn) NodeID(ctx context.Context) (uint16, error) {
	params, err := c.command(ctx, FrameGetNodeID, nil)
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	id := r.u16()
	return id, r.err
}

// NetworkParameters describes the network the NCP is operating on.
type NetworkParameters struct {
	ExtendedPanID EUI64  `json:"extended_pan_id"`
	PanID         uint16 `json:"pan_id"`
	RadioTxPower  int8   `json:"radio_tx_power_dbm"`
	RadioChannel  uint8  `json:"radio_channel"`
	JoinMethod    uint8  `json:"join_method"`
	NwkManagerID  uint16 `json:"nwk_manager_id"`
	NwkUpdateID   uint8  `json:"nwk_update_id"`
	Channels      uint32 `json:"channel_mask"`
}

// NetworkParameters reads the live network configuration: PAN ID, extended
// PAN ID, channel and transmit power. It fails when no network is formed.
func (c *Conn) NetworkParameters(ctx context.Context) (NodeType, NetworkParameters, error) {
	params, err := c.command(ctx, FrameGetNetworkParameters, nil)
	if err != nil {
		return 0, NetworkParameters{}, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	nodeType := NodeType(r.u8())
	if r.err != nil {
		return 0, NetworkParameters{}, r.err
	}
	if !status.OK() {
		return nodeType, NetworkParameters{}, fmt.Errorf("ezsp: getNetworkParameters: %s", status)
	}

	np := NetworkParameters{
		ExtendedPanID: r.ieee(),
		PanID:         r.u16(),
		RadioTxPower:  int8(r.u8()),
		RadioChannel:  r.u8(),
		JoinMethod:    r.u8(),
		NwkManagerID:  r.u16(),
		NwkUpdateID:   r.u8(),
		Channels:      r.u32(),
	}
	if r.err != nil {
		return nodeType, NetworkParameters{}, r.err
	}
	return nodeType, np, nil
}

// ChannelList expands a channel bitmask into the 2.4 GHz channel numbers it
// selects. Zigbee uses channels 11 through 26.
func ChannelList(mask uint32) []int {
	var out []int
	for ch := 11; ch <= 26; ch++ {
		if mask&(1<<uint(ch)) != 0 {
			out = append(out, ch)
		}
	}
	return out
}
