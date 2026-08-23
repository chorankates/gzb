package zcl

import "fmt"

// This file is the thin friendly layer over the raw decoder: it turns
// (cluster, attribute, value) into something with a name and a unit.
//
// Everything here is a convenience. An attribute with no entry still decodes
// and still reports its raw value; it just prints as a number rather than as
// "21.5 °C".

// Cluster IDs seen from sensors, lights, plugs and switches.
const (
	ClusterBasic              uint16 = 0x0000
	ClusterPowerConfiguration uint16 = 0x0001
	ClusterIdentify           uint16 = 0x0003
	ClusterGroups             uint16 = 0x0004
	ClusterScenes             uint16 = 0x0005
	ClusterOnOff              uint16 = 0x0006
	ClusterLevelControl       uint16 = 0x0008
	ClusterTime               uint16 = 0x000A
	ClusterOTAUpgrade         uint16 = 0x0019
	ClusterColorControl       uint16 = 0x0300
	ClusterIlluminance        uint16 = 0x0400
	ClusterTemperature        uint16 = 0x0402
	ClusterPressure           uint16 = 0x0403
	ClusterRelativeHumidity   uint16 = 0x0405
	ClusterOccupancySensing   uint16 = 0x0406
	ClusterIASZone            uint16 = 0x0500
	ClusterMetering           uint16 = 0x0702
	ClusterElectricalMeasure  uint16 = 0x0B04
	ClusterSonoff             uint16 = 0xFC11
)

// ClusterName renders a cluster ID, falling back to hex.
func ClusterName(id uint16) string {
	switch id {
	case ClusterBasic:
		return "basic"
	case ClusterPowerConfiguration:
		return "power"
	case ClusterIdentify:
		return "identify"
	case ClusterGroups:
		return "groups"
	case ClusterScenes:
		return "scenes"
	case ClusterOnOff:
		return "on/off"
	case ClusterLevelControl:
		return "level"
	case ClusterTime:
		return "time"
	case ClusterOTAUpgrade:
		return "ota"
	case ClusterColorControl:
		return "color"
	case ClusterIlluminance:
		return "illuminance"
	case ClusterTemperature:
		return "temperature"
	case ClusterPressure:
		return "pressure"
	case ClusterRelativeHumidity:
		return "humidity"
	case ClusterOccupancySensing:
		return "occupancy"
	case ClusterIASZone:
		return "ias zone"
	case ClusterMetering:
		return "metering"
	case ClusterElectricalMeasure:
		return "electrical"
	}
	if id >= 0xFC00 {
		return fmt.Sprintf("manufacturer 0x%04X", id)
	}
	return fmt.Sprintf("cluster 0x%04X", id)
}

// Reading is an attribute interpreted as a physical quantity.
type Reading struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

func (r Reading) String() string {
	if r.Unit == "" {
		return fmt.Sprintf("%s %g", r.Name, r.Value)
	}
	return fmt.Sprintf("%s %.2f %s", r.Name, r.Value, r.Unit)
}

// interpretation describes how to turn a raw attribute into a reading.
type interpretation struct {
	name  string
	unit  string
	scale float64
}

// interpretations maps (cluster, attribute) to a physical quantity.
//
// The scale factors are from the ZCL specification: temperature and humidity
// are reported in hundredths, battery percentage in half-percent steps, and
// pressure in tenths of a kilopascal.
var interpretations = map[[2]uint16]interpretation{
	{ClusterTemperature, 0x0000}:        {"temperature", "°C", 0.01},
	{ClusterRelativeHumidity, 0x0000}:   {"humidity", "%", 0.01},
	{ClusterPressure, 0x0000}:           {"pressure", "kPa", 0.1},
	{ClusterIlluminance, 0x0000}:        {"illuminance", "lx", 1},
	{ClusterPowerConfiguration, 0x0020}: {"battery", "V", 0.1},
	{ClusterPowerConfiguration, 0x0021}: {"battery", "%", 0.5},
	{ClusterOccupancySensing, 0x0000}:   {"occupancy", "", 1},
	{ClusterOnOff, 0x0000}:              {"on/off", "", 1},
	{ClusterLevelControl, 0x0000}:       {"level", "", 1},
	// SONOFF display sensors report these device-maintained statistics in
	// hundredths on their manufacturer cluster. The exact time window is
	// firmware-dependent. The third value in each group is documented only as
	// a reference value, so do not misrepresent it as current or average.
	{ClusterSonoff, 0x2008}: {"temperature maximum", "°C", 0.01},
	{ClusterSonoff, 0x2009}: {"temperature minimum", "°C", 0.01},
	{ClusterSonoff, 0x200A}: {"temperature reference", "°C", 0.01},
	{ClusterSonoff, 0x200B}: {"humidity maximum", "%", 0.01},
	{ClusterSonoff, 0x200C}: {"humidity minimum", "%", 0.01},
	{ClusterSonoff, 0x200D}: {"humidity reference", "%", 0.01},
}

// Interpret turns an attribute into a physical reading. It reports false when
// the attribute has no known interpretation, or holds a value that is not
// numeric.
func Interpret(cluster uint16, a Attribute) (Reading, bool) {
	spec, ok := interpretations[[2]uint16{cluster, a.ID}]
	if !ok {
		return Reading{}, false
	}
	n, ok := numeric(a.Value)
	if !ok {
		return Reading{}, false
	}
	return Reading{Name: spec.name, Value: n * spec.scale, Unit: spec.unit}, true
}

// numeric converts a decoded attribute value to a float, if it is a number.
func numeric(v any) (float64, bool) {
	switch t := v.(type) {
	case uint64:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// AttributeName renders a well-known attribute ID for a cluster.
func AttributeName(cluster, attr uint16) string {
	if spec, ok := interpretations[[2]uint16{cluster, attr}]; ok {
		return spec.name
	}
	if cluster == ClusterBasic {
		switch attr {
		case 0x0004:
			return "manufacturer"
		case 0x0005:
			return "model"
		case 0x0007:
			return "power source"
		}
	}
	return fmt.Sprintf("attr 0x%04X", attr)
}
