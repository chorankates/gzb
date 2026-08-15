package ezsp

import (
	"context"
	"fmt"
)

// ConfigID selects one of the NCP's configuration values.
//
// On EmberZNet 7.x most of these are fixed at compile time and report the
// firmware's static allocation rather than something the host chose. Reading
// them is still the only way to find out what the NCP will actually do: a
// stack profile or child table that does not match expectations explains
// behaviour that otherwise looks like a dead radio.
type ConfigID uint8

const (
	ConfigPacketBufferCount      ConfigID = 0x01
	ConfigNeighborTableSize      ConfigID = 0x02
	ConfigAPSUnicastMessageCount ConfigID = 0x03
	ConfigBindingTableSize       ConfigID = 0x04
	ConfigAddressTableSize       ConfigID = 0x05
	ConfigMulticastTableSize     ConfigID = 0x06
	ConfigRouteTableSize         ConfigID = 0x07
	ConfigDiscoveryTableSize     ConfigID = 0x08
	ConfigStackProfile           ConfigID = 0x0C
	ConfigSecurityLevel          ConfigID = 0x0D
	ConfigMaxHops                ConfigID = 0x10
	ConfigMaxEndDeviceChildren   ConfigID = 0x11
	ConfigIndirectTxTimeout      ConfigID = 0x12
	ConfigEndDevicePollTimeout   ConfigID = 0x13
	ConfigTxPowerMode            ConfigID = 0x17
	ConfigTrustCenterCacheSize   ConfigID = 0x19
	ConfigSourceRouteTableSize   ConfigID = 0x1A
	ConfigKeyTableSize           ConfigID = 0x1E
	ConfigAPSAckTimeout          ConfigID = 0x1F
	ConfigBeaconJitterDuration   ConfigID = 0x20
	ConfigPanIDConflictThresh    ConfigID = 0x22
	ConfigRequestKeyTimeout      ConfigID = 0x24
	ConfigApplicationZDOFlags    ConfigID = 0x2A
	ConfigBroadcastTableSize     ConfigID = 0x2B
	ConfigMACFilterTableSize     ConfigID = 0x2C
	ConfigSupportedNetworks      ConfigID = 0x2D
	ConfigTransientKeyTimeout    ConfigID = 0x36
)

func (id ConfigID) String() string {
	switch id {
	case ConfigPacketBufferCount:
		return "packetBufferCount"
	case ConfigNeighborTableSize:
		return "neighborTableSize"
	case ConfigAPSUnicastMessageCount:
		return "apsUnicastMessageCount"
	case ConfigBindingTableSize:
		return "bindingTableSize"
	case ConfigAddressTableSize:
		return "addressTableSize"
	case ConfigMulticastTableSize:
		return "multicastTableSize"
	case ConfigRouteTableSize:
		return "routeTableSize"
	case ConfigDiscoveryTableSize:
		return "discoveryTableSize"
	case ConfigStackProfile:
		return "stackProfile"
	case ConfigSecurityLevel:
		return "securityLevel"
	case ConfigMaxHops:
		return "maxHops"
	case ConfigMaxEndDeviceChildren:
		return "maxEndDeviceChildren"
	case ConfigIndirectTxTimeout:
		return "indirectTransmissionTimeout"
	case ConfigEndDevicePollTimeout:
		return "endDevicePollTimeout"
	case ConfigTxPowerMode:
		return "txPowerMode"
	case ConfigTrustCenterCacheSize:
		return "trustCenterAddressCacheSize"
	case ConfigSourceRouteTableSize:
		return "sourceRouteTableSize"
	case ConfigKeyTableSize:
		return "keyTableSize"
	case ConfigAPSAckTimeout:
		return "apsAckTimeout"
	case ConfigBeaconJitterDuration:
		return "beaconJitterDuration"
	case ConfigPanIDConflictThresh:
		return "panIdConflictReportThreshold"
	case ConfigRequestKeyTimeout:
		return "requestKeyTimeout"
	case ConfigApplicationZDOFlags:
		return "applicationZdoFlags"
	case ConfigBroadcastTableSize:
		return "broadcastTableSize"
	case ConfigMACFilterTableSize:
		return "macFilterTableSize"
	case ConfigSupportedNetworks:
		return "supportedNetworks"
	case ConfigTransientKeyTimeout:
		return "transientKeyTimeoutSeconds"
	default:
		return fmt.Sprintf("config 0x%02X", uint8(id))
	}
}

// ConfigIDs lists the configuration values worth reporting, in a stable order.
var ConfigIDs = []ConfigID{
	ConfigStackProfile,
	ConfigSecurityLevel,
	ConfigMaxEndDeviceChildren,
	ConfigKeyTableSize,
	ConfigTrustCenterCacheSize,
	ConfigAddressTableSize,
	ConfigNeighborTableSize,
	ConfigRouteTableSize,
	ConfigBindingTableSize,
	ConfigMulticastTableSize,
	ConfigBroadcastTableSize,
	ConfigSourceRouteTableSize,
	ConfigDiscoveryTableSize,
	ConfigPacketBufferCount,
	ConfigAPSUnicastMessageCount,
	ConfigMaxHops,
	ConfigIndirectTxTimeout,
	ConfigEndDevicePollTimeout,
	ConfigTxPowerMode,
	ConfigAPSAckTimeout,
	ConfigRequestKeyTimeout,
	ConfigTransientKeyTimeout,
	ConfigApplicationZDOFlags,
	ConfigSupportedNetworks,
	ConfigPanIDConflictThresh,
}

// GetConfigurationValue reads one configuration value. Values the firmware
// does not implement report an error rather than a misleading zero.
func (c *Conn) GetConfigurationValue(ctx context.Context, id ConfigID) (uint16, error) {
	params, err := c.command(ctx, FrameGetConfigurationValue, []byte{uint8(id)})
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	status := EzspStatus(r.u8())
	value := r.u16()
	if r.err != nil {
		return 0, r.err
	}
	if !status.OK() {
		return 0, fmt.Errorf("ezsp: getConfigurationValue %s: %s", id, status)
	}
	return value, nil
}

// SetConfigurationValue writes one configuration value. Most values may only
// be changed while the network is down, and on EmberZNet 7.x many are fixed at
// compile time and will refuse the write.
func (c *Conn) SetConfigurationValue(ctx context.Context, id ConfigID, value uint16) error {
	var w wbuf
	w.u8(uint8(id))
	w.u16(value)

	params, err := c.command(ctx, FrameSetConfigurationValue, w.b)
	if err != nil {
		return err
	}
	r := newBuf(params)
	status := EzspStatus(r.u8())
	if r.err != nil {
		return r.err
	}
	if !status.OK() {
		return fmt.Errorf("ezsp: setConfigurationValue %s = %d: %s", id, value, status)
	}
	return nil
}

// requiredConfig is written to the NCP on every connection.
//
// EZSP configuration is host state, not adapter state: the NCP resets these to
// its own defaults whenever it reboots, and opening the ASH link reboots it.
// Anything not written here is whatever the firmware felt like, which is not
// necessarily anything a Zigbee device will talk to.
var requiredConfig = []struct {
	ID    ConfigID
	Value uint16
	Why   string
}{
	// This one decides whether the network is joinable at all. The value ends
	// up in the beacon payload, and a Zigbee 3.0 device that reads a profile
	// other than 2 rejects the beacon and never attempts to associate — so the
	// coordinator sees complete silence rather than a failed join.
	{ConfigStackProfile, 2, "ZigBee PRO"},
	{ConfigSecurityLevel, 5, "standard Zigbee security"},
}

// applyConfiguration writes the values gzb depends on.
//
// It must run while the network is down: most configuration values reject
// writes once the stack is up, which is why this happens between the version
// handshake and networkInit.
func (c *Conn) applyConfiguration(ctx context.Context) error {
	for _, want := range requiredConfig {
		if err := c.SetConfigurationValue(ctx, want.ID, want.Value); err == nil {
			continue
		}
		// Firmware may fix a value at compile time and refuse the write. That
		// is only a problem if what it fixed differs from what we need.
		got, readErr := c.GetConfigurationValue(ctx, want.ID)
		if readErr != nil {
			return fmt.Errorf("ezsp: cannot set or read %s (%s): %w", want.ID, want.Why, readErr)
		}
		if got != want.Value {
			return fmt.Errorf("ezsp: %s is %d, need %d (%s), and the NCP refused the write",
				want.ID, got, want.Value, want.Why)
		}
	}
	return nil
}

// ConfigValue is one configuration entry as read from the NCP.
type ConfigValue struct {
	ID    ConfigID `json:"-"`
	Name  string   `json:"name"`
	Value uint16   `json:"value,omitempty"`
	Error string   `json:"error,omitempty"`
}

// Configuration reads every value in ConfigIDs. Individual failures are
// recorded against their entry rather than aborting the sweep, since firmware
// legitimately omits some of them.
func (c *Conn) Configuration(ctx context.Context) ([]ConfigValue, error) {
	out := make([]ConfigValue, 0, len(ConfigIDs))
	for _, id := range ConfigIDs {
		entry := ConfigValue{ID: id, Name: id.String()}
		v, err := c.GetConfigurationValue(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			entry.Error = "unsupported"
		} else {
			entry.Value = v
		}
		out = append(out, entry)
	}
	return out, nil
}
