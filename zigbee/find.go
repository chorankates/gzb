package zigbee

import (
	"context"
	"fmt"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/zdo"
)

// Find asks the mesh where a device is and records the answer.
//
// The registry normally learns of a device from its join, but a device that
// joined while nothing was listening — through a router, during a restart —
// is on the network and unknown to it. Its address is printed on the device,
// so the question "what network address does this IEEE have?" is broadcast to
// every listening node, and the device answers for itself. Nothing needs to
// know which router it sits behind. The context bounds the wait; a few
// seconds is plenty for a mains-powered device, and a sleeping one will not
// hear a broadcast at all.
func (c *Coordinator) Find(ctx context.Context, ieee string) (Device, error) {
	if err := c.checkOpen(); err != nil {
		return Device{}, err
	}
	eui, err := ezsp.ParseEUI64(ieee)
	if err != nil {
		return Device{}, fmt.Errorf("zigbee: %w", err)
	}

	payload := zdo.NetworkAddressRequest(c.nextSequence(), eui)
	aps := ezsp.APSFrame{
		Profile:  ezsp.ProfileZDO,
		Cluster:  zdo.ClusterNetworkAddress,
		SourceEP: zdoEndpoint,
		DestEP:   zdoEndpoint,
	}
	var found uint16
	msg, err := c.conn.BroadcastRequest(ctx, ezsp.BroadcastRxOnWhenIdle, aps, 0, payload, func(m ezsp.IncomingMessage) bool {
		if m.APS.Profile != ezsp.ProfileZDO || m.APS.Cluster != zdo.ResponseCluster(zdo.ClusterNetworkAddress) {
			return false
		}
		got, addr, status, err := zdo.ParseNetworkAddress(m.Payload)
		if err != nil || !status.OK() || got != [8]byte(eui) {
			return false
		}
		found = addr
		return true
	})
	if err != nil {
		return Device{}, fmt.Errorf("zigbee: finding %s: %w", eui, err)
	}
	_ = msg

	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	d, _ := c.db.Observe(eui.String(), found, time.Now())
	if err := c.db.Save(); err != nil {
		return snapshotDevice(d), fmt.Errorf("zigbee: recording %s: %w", eui, err)
	}
	return snapshotDevice(d), nil
}
