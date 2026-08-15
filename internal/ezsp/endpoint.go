package ezsp

import (
	"context"
	"fmt"
)

// FrameAddEndpoint registers an application endpoint on the NCP.
const FrameAddEndpoint FrameID = 0x0002

// Zigbee application profiles.
const (
	ProfileHomeAutomation uint16 = 0x0104
	ProfileGreenPower     uint16 = 0xA1E0
)

// Clusters this coordinator advertises. The names follow the ZCL specification.
const (
	ClusterBasic                uint16 = 0x0000
	ClusterPowerConfiguration   uint16 = 0x0001
	ClusterIdentify             uint16 = 0x0003
	ClusterGroups               uint16 = 0x0004
	ClusterScenes               uint16 = 0x0005
	ClusterOnOff                uint16 = 0x0006
	ClusterLevelControl         uint16 = 0x0008
	ClusterTime                 uint16 = 0x000A
	ClusterOTAUpgrade           uint16 = 0x0019
	ClusterPollControl          uint16 = 0x0020
	ClusterColorControl         uint16 = 0x0300
	ClusterIlluminance          uint16 = 0x0400
	ClusterTemperature          uint16 = 0x0402
	ClusterPressure             uint16 = 0x0403
	ClusterRelativeHumidity     uint16 = 0x0405
	ClusterOccupancySensing     uint16 = 0x0406
	ClusterIASZone              uint16 = 0x0500
	ClusterMetering             uint16 = 0x0702
	ClusterElectricalMeasuremnt uint16 = 0x0B04
)

// Endpoint describes an application endpoint to register on the coordinator.
type Endpoint struct {
	ID      uint8
	Profile uint16
	Device  uint16
	Version uint8
	// Input clusters are those this endpoint implements as a server: other
	// devices send commands to them.
	Input []uint16
	// Output clusters are those this endpoint drives as a client. Attribute
	// reports from a sensor land here, which is why a hub lists far more
	// outputs than inputs.
	Output []uint16
}

// deviceConfigurationTool is the HA device type a hub presents itself as.
const deviceConfigurationTool uint16 = 0x0005

// DefaultEndpoint is the endpoint gzb registers on every connection.
//
// Registering at least one endpoint is not optional. Until an endpoint exists
// the coordinator has no APS address a device can talk to: ZDO discovery finds
// nothing, bindings have nothing to point at, and attribute reports are
// rejected. A joining device reads that as a network it cannot use, and leaves
// again a few seconds after joining.
var DefaultEndpoint = Endpoint{
	ID:      1,
	Profile: ProfileHomeAutomation,
	Device:  deviceConfigurationTool,
	Input: []uint16{
		ClusterBasic,
		ClusterIdentify,
		ClusterTime,
		ClusterOTAUpgrade,
	},
	Output: []uint16{
		ClusterBasic,
		ClusterPowerConfiguration,
		ClusterIdentify,
		ClusterGroups,
		ClusterScenes,
		ClusterOnOff,
		ClusterLevelControl,
		ClusterPollControl,
		ClusterColorControl,
		ClusterIlluminance,
		ClusterTemperature,
		ClusterPressure,
		ClusterRelativeHumidity,
		ClusterOccupancySensing,
		ClusterIASZone,
		ClusterMetering,
		ClusterElectricalMeasuremnt,
	},
}

// AddEndpoint registers an application endpoint.
//
// This may only be done while the network is down, which is why it happens
// during connection setup rather than on demand. Like configuration values,
// endpoints live in NCP RAM and vanish on reset, so they must be registered
// again every session.
func (c *Conn) AddEndpoint(ctx context.Context, ep Endpoint) error {
	if len(ep.Input) > 255 || len(ep.Output) > 255 {
		return fmt.Errorf("ezsp: endpoint %d has too many clusters", ep.ID)
	}

	var w wbuf
	w.u8(ep.ID)
	w.u16(ep.Profile)
	w.u16(ep.Device)
	w.u8(ep.Version)
	w.u8(uint8(len(ep.Input)))
	w.u8(uint8(len(ep.Output)))
	for _, cl := range ep.Input {
		w.u16(cl)
	}
	for _, cl := range ep.Output {
		w.u16(cl)
	}

	params, err := c.command(ctx, FrameAddEndpoint, w.b)
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EzspStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: addEndpoint %d: %s", ep.ID, status)
	}
	return nil
}

// registerEndpoints installs the endpoints the coordinator serves.
//
// A duplicate registration is treated as success: the endpoint already being
// present is the state we wanted, and failing the whole connection over it
// would make reconnecting to a live NCP impossible.
func (c *Conn) registerEndpoints(ctx context.Context) error {
	err := c.AddEndpoint(ctx, DefaultEndpoint)
	if err == nil {
		return nil
	}
	if state, stateErr := c.NetworkState(ctx); stateErr == nil && state.Joined() {
		return fmt.Errorf("ezsp: registering endpoint %d: %w (the network was already up, so it was too late to register)", DefaultEndpoint.ID, err)
	}
	return fmt.Errorf("ezsp: registering endpoint %d: %w", DefaultEndpoint.ID, err)
}
