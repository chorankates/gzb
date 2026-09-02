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

// clusterNames is the one place a cluster's short name is written down, so
// that printing an ID and accepting one on a command line cannot disagree.
var clusterNames = map[uint16]string{
	ClusterBasic:              "basic",
	ClusterPowerConfiguration: "power",
	ClusterIdentify:           "identify",
	ClusterGroups:             "groups",
	ClusterScenes:             "scenes",
	ClusterOnOff:              "on/off",
	ClusterLevelControl:       "level",
	ClusterTime:               "time",
	ClusterOTAUpgrade:         "ota",
	ClusterColorControl:       "color",
	ClusterIlluminance:        "illuminance",
	ClusterTemperature:        "temperature",
	ClusterPressure:           "pressure",
	ClusterRelativeHumidity:   "humidity",
	ClusterOccupancySensing:   "occupancy",
	ClusterIASZone:            "ias zone",
	ClusterMetering:           "metering",
	ClusterElectricalMeasure:  "electrical",
}

// ClusterName renders a cluster ID, falling back to hex.
func ClusterName(id uint16) string {
	if name, ok := clusterNames[id]; ok {
		return name
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
	// Current says whether this is the device's value of that quantity now,
	// rather than a constant or a statistic it keeps about itself. Both render
	// the same way; only a current one is a reading.
	Current bool `json:"current"`
}

func (r Reading) String() string {
	if r.Unit == "" {
		return fmt.Sprintf("%s %g", r.Name, r.Value)
	}
	return fmt.Sprintf("%s %.2f %s", r.Name, r.Value, r.Unit)
}

// Interpret turns an attribute into a physical reading. It reports false when
// the attribute has no known interpretation, or holds a value that is not
// numeric.
func Interpret(cluster uint16, a Attribute) (Reading, bool) {
	n, ok := numeric(a.Value)
	if !ok {
		return Reading{}, false
	}
	return Scale(cluster, a.ID, n)
}

// Scale renders a raw attribute value as the quantity it stands for, without
// needing a decoded attribute to do it.
//
// This is what makes a reportable change legible. The threshold travels on the
// wire in the attribute's own units, so a temperature takes 50 to mean half a
// degree; saying so before the device is asked is what turns a mistake by a
// factor of a hundred into something a person notices.
func Scale(cluster, attr uint16, raw float64) (Reading, bool) {
	spec, ok := attributes[[2]uint16{cluster, attr}]
	if !ok || !spec.measurement() {
		return Reading{}, false
	}
	value := raw * spec.scale
	if spec.convert != nil {
		converted, ok := spec.convert(raw)
		if !ok {
			return Reading{}, false
		}
		value = converted
	}
	return Reading{Name: spec.name, Value: value, Unit: spec.unit, Current: spec.current}, true
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
