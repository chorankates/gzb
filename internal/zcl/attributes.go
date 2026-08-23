package zcl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is what gzb knows about individual attributes: what to call them,
// how they are encoded on the wire, and — where an attribute stands for a
// physical quantity — the unit and scale that turn a raw integer into a
// measurement.
//
// The encoding only starts to matter once the hub stops merely listening. A
// report states the type of every value it carries, so decoding needs no table
// at all. A write and a reporting configuration must both declare the type
// before the device has said anything, and a device rejects a wrong guess with
// INVALID_DATA_TYPE. Everything gzb can write or configure without being told
// the type is therefore here.

// attributeSpec is what is known about one attribute.
type attributeSpec struct {
	name string
	typ  DataType
	// unit and scale describe the physical quantity the attribute stands for.
	// A zero scale means it is not a measurement — a model string, a bitmask,
	// an interval — and Interpret leaves those alone rather than reporting a
	// meaningless number.
	unit  string
	scale float64
}

// attributes maps (cluster, attribute) to what gzb knows about it.
//
// The scale factors are from the ZCL specification: temperature and humidity
// are reported in hundredths, battery percentage in half-percent steps, and
// pressure in tenths of a kilopascal.
var attributes = map[[2]uint16]attributeSpec{
	{ClusterBasic, 0x0000}: {name: "zcl version", typ: TypeUint8},
	{ClusterBasic, 0x0001}: {name: "application version", typ: TypeUint8},
	{ClusterBasic, 0x0002}: {name: "stack version", typ: TypeUint8},
	{ClusterBasic, 0x0003}: {name: "hardware version", typ: TypeUint8},
	{ClusterBasic, 0x0004}: {name: "manufacturer", typ: TypeCharStr},
	{ClusterBasic, 0x0005}: {name: "model", typ: TypeCharStr},
	{ClusterBasic, 0x0006}: {name: "date code", typ: TypeCharStr},
	{ClusterBasic, 0x0007}: {name: "power source", typ: TypeEnum8},
	{ClusterBasic, 0x0010}: {name: "location", typ: TypeCharStr},
	{ClusterBasic, 0x0011}: {name: "physical environment", typ: TypeEnum8},
	{ClusterBasic, 0x0012}: {name: "device enabled", typ: TypeBool},

	{ClusterPowerConfiguration, 0x0020}: {name: "battery voltage", typ: TypeUint8, unit: "V", scale: 0.1},
	{ClusterPowerConfiguration, 0x0021}: {name: "battery percentage", typ: TypeUint8, unit: "%", scale: 0.5},

	{ClusterIdentify, 0x0000}: {name: "identify time", typ: TypeUint16},

	{ClusterOnOff, 0x0000}: {name: "on/off", typ: TypeBool, scale: 1},
	{ClusterOnOff, 0x4001}: {name: "on time", typ: TypeUint16},
	{ClusterOnOff, 0x4002}: {name: "off wait time", typ: TypeUint16},
	{ClusterOnOff, 0x4003}: {name: "startup on/off", typ: TypeEnum8},

	{ClusterLevelControl, 0x0000}: {name: "level", typ: TypeUint8, scale: 1},
	{ClusterLevelControl, 0x0010}: {name: "transition time", typ: TypeUint16},
	{ClusterLevelControl, 0x0011}: {name: "on level", typ: TypeUint8},

	{ClusterTime, 0x0000}: {name: "time", typ: TypeUTCTime},
	{ClusterTime, 0x0001}: {name: "time status", typ: TypeBitmap8},
	{ClusterTime, 0x0002}: {name: "time zone", typ: TypeInt32},
	{ClusterTime, 0x0007}: {name: "local time", typ: TypeUint32},

	{ClusterColorControl, 0x0000}: {name: "hue", typ: TypeUint8},
	{ClusterColorControl, 0x0001}: {name: "saturation", typ: TypeUint8},
	{ClusterColorControl, 0x0003}: {name: "color x", typ: TypeUint16},
	{ClusterColorControl, 0x0004}: {name: "color y", typ: TypeUint16},
	{ClusterColorControl, 0x0007}: {name: "color temperature", typ: TypeUint16},
	{ClusterColorControl, 0x0008}: {name: "color mode", typ: TypeEnum8},

	{ClusterIlluminance, 0x0000}: {name: "illuminance", typ: TypeUint16, unit: "lx", scale: 1},

	{ClusterTemperature, 0x0000}: {name: "temperature", typ: TypeInt16, unit: "°C", scale: 0.01},
	{ClusterTemperature, 0x0001}: {name: "minimum measurable", typ: TypeInt16},
	{ClusterTemperature, 0x0002}: {name: "maximum measurable", typ: TypeInt16},
	{ClusterTemperature, 0x0003}: {name: "tolerance", typ: TypeUint16},

	{ClusterPressure, 0x0000}: {name: "pressure", typ: TypeInt16, unit: "kPa", scale: 0.1},

	{ClusterRelativeHumidity, 0x0000}: {name: "humidity", typ: TypeUint16, unit: "%", scale: 0.01},
	{ClusterRelativeHumidity, 0x0001}: {name: "minimum measurable", typ: TypeUint16},
	{ClusterRelativeHumidity, 0x0002}: {name: "maximum measurable", typ: TypeUint16},

	{ClusterOccupancySensing, 0x0000}: {name: "occupancy", typ: TypeBitmap8, scale: 1},
	{ClusterOccupancySensing, 0x0001}: {name: "occupancy sensor type", typ: TypeEnum8},

	{ClusterIASZone, 0x0000}: {name: "zone state", typ: TypeEnum8},
	{ClusterIASZone, 0x0001}: {name: "zone type", typ: TypeEnum16},
	{ClusterIASZone, 0x0002}: {name: "zone status", typ: TypeBitmap16},

	{ClusterMetering, 0x0000}: {name: "summation delivered", typ: TypeUint48},

	{ClusterElectricalMeasure, 0x0505}: {name: "rms voltage", typ: TypeUint16},
	{ClusterElectricalMeasure, 0x0508}: {name: "rms current", typ: TypeUint16},
	{ClusterElectricalMeasure, 0x050B}: {name: "active power", typ: TypeInt16},

	// SONOFF display sensors report these device-maintained statistics in
	// hundredths on their manufacturer cluster. The exact time window is
	// firmware-dependent. The third value in each group is documented only as
	// a reference value, so do not misrepresent it as current or average.
	{ClusterSonoff, 0x2008}: {name: "temperature maximum", typ: TypeInt16, unit: "°C", scale: 0.01},
	{ClusterSonoff, 0x2009}: {name: "temperature minimum", typ: TypeInt16, unit: "°C", scale: 0.01},
	{ClusterSonoff, 0x200A}: {name: "temperature reference", typ: TypeInt16, unit: "°C", scale: 0.01},
	{ClusterSonoff, 0x200B}: {name: "humidity maximum", typ: TypeUint16, unit: "%", scale: 0.01},
	{ClusterSonoff, 0x200C}: {name: "humidity minimum", typ: TypeUint16, unit: "%", scale: 0.01},
	{ClusterSonoff, 0x200D}: {name: "humidity reference", typ: TypeUint16, unit: "%", scale: 0.01},
}

// AttributeName renders a well-known attribute ID for a cluster.
func AttributeName(cluster, attr uint16) string {
	if spec, ok := attributes[[2]uint16{cluster, attr}]; ok {
		return spec.name
	}
	return fmt.Sprintf("attr 0x%04X", attr)
}

// AttributeType reports the wire encoding of a well-known attribute.
//
// Reading never needs this: the device states the type in its answer. Writing
// does, and so does configuring a report, because both have to declare the
// type before the device has said anything at all.
func AttributeType(cluster, attr uint16) (DataType, bool) {
	spec, ok := attributes[[2]uint16{cluster, attr}]
	if !ok {
		return 0, false
	}
	return spec.typ, true
}

// KnownAttributes lists the attributes gzb knows on a cluster, in ID order.
// It is what a read falls back to when given a cluster and no attribute.
func KnownAttributes(cluster uint16) []uint16 {
	var ids []uint16
	for key := range attributes {
		if key[0] == cluster {
			ids = append(ids, key[1])
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ParseAttribute resolves an attribute given either as a name gzb knows on
// this cluster or as a hex identifier.
func ParseAttribute(cluster uint16, s string) (uint16, bool) {
	for key, spec := range attributes {
		if key[0] == cluster && strings.EqualFold(spec.name, s) {
			return key[1], true
		}
	}
	return ParseHex16(s)
}

// ParseCluster resolves a cluster given either as a name gzb knows or as a hex
// identifier.
func ParseCluster(s string) (uint16, bool) {
	for id, name := range clusterNames {
		if strings.EqualFold(name, s) {
			return id, true
		}
	}
	return ParseHex16(s)
}

// ParseHex16 reads a 16-bit identifier in the 0xNNNN form Zigbee tools print.
//
// The prefix is required for the same reason store.ParseNodeID requires it:
// without it there is no telling whether "1000" was meant as hex or decimal,
// and the two are different attributes.
func ParseHex16(s string) (uint16, bool) {
	if len(s) < 3 || !strings.EqualFold(s[:2], "0x") {
		return 0, false
	}
	v, err := strconv.ParseUint(s[2:], 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}
