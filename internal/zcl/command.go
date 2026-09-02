package zcl

// Cluster-specific commands are the half of the ZCL that does something, as
// opposed to the profile-wide half that reads and writes state. A light does
// not change colour because someone wrote to its CurrentHue attribute — that
// attribute is read-only, and a device says so — but because it was sent Move
// to Hue and Saturation.
//
// The distinction is one bit in the frame control field, and everything after
// it changes meaning: the command byte is no longer a Read or a Write, it is
// whatever the cluster says command 6 is. That is why a name for one of these
// is only meaningful alongside its cluster, and why CommandName cannot be
// asked to render it.

import (
	"encoding/binary"
	"fmt"
)

// Cluster-specific command IDs for the clusters that control a light.
//
// These are the standard ZCL commands, not a vendor's extensions, which is
// what makes them worth modelling: any device claiming the cluster implements
// them, so a light gzb has never seen answers the same commands as one it
// knows well.
const (
	// On/Off (0x0006).
	CmdOff    uint8 = 0x00
	CmdOn     uint8 = 0x01
	CmdToggle uint8 = 0x02

	// Level Control (0x0008). The "with on/off" variants change the on/off
	// state as a side effect; the plain ones set the level and leave a light
	// that is off exactly as off as it was.
	CmdMoveToLevel          uint8 = 0x00
	CmdMoveLevel            uint8 = 0x01
	CmdStepLevel            uint8 = 0x02
	CmdStopLevel            uint8 = 0x03
	CmdMoveToLevelWithOnOff uint8 = 0x04
	CmdMoveLevelWithOnOff   uint8 = 0x05
	CmdStepLevelWithOnOff   uint8 = 0x06
	CmdStopLevelWithOnOff   uint8 = 0x07

	// Colour Control (0x0300).
	CmdMoveToHue              uint8 = 0x00
	CmdMoveHue                uint8 = 0x01
	CmdStepHue                uint8 = 0x02
	CmdMoveToSaturation       uint8 = 0x03
	CmdMoveSaturation         uint8 = 0x04
	CmdStepSaturation         uint8 = 0x05
	CmdMoveToHueAndSaturation uint8 = 0x06
	CmdMoveToColor            uint8 = 0x07
	CmdMoveColor              uint8 = 0x08
	CmdStepColor              uint8 = 0x09
	CmdMoveToColorTemperature uint8 = 0x0A

	// Identify (0x0003), which is how a person finds out which lamp on the
	// shelf is the one being addressed.
	CmdIdentify uint8 = 0x00
)

// StepMode values for the Step commands, which move a value by an amount
// rather than to one.
const (
	StepUp   uint8 = 0x00
	StepDown uint8 = 0x01
)

// clusterCommands names the cluster-specific commands gzb can send.
//
// A device's refusal echoes only the command ID, so without this a rejected
// Move to Hue and Saturation would be reported as whatever profile-wide
// command happens to share the number 6 — "configure reporting", which is not
// merely unhelpful but points at the wrong cluster entirely.
var clusterCommands = map[[2]uint16]string{
	{ClusterOnOff, uint16(CmdOff)}:    "off",
	{ClusterOnOff, uint16(CmdOn)}:     "on",
	{ClusterOnOff, uint16(CmdToggle)}: "toggle",

	{ClusterLevelControl, uint16(CmdMoveToLevel)}:          "move to level",
	{ClusterLevelControl, uint16(CmdMoveLevel)}:            "move level",
	{ClusterLevelControl, uint16(CmdStepLevel)}:            "step level",
	{ClusterLevelControl, uint16(CmdStopLevel)}:            "stop level",
	{ClusterLevelControl, uint16(CmdMoveToLevelWithOnOff)}: "move to level (with on/off)",
	{ClusterLevelControl, uint16(CmdMoveLevelWithOnOff)}:   "move level (with on/off)",
	{ClusterLevelControl, uint16(CmdStepLevelWithOnOff)}:   "step level (with on/off)",
	{ClusterLevelControl, uint16(CmdStopLevelWithOnOff)}:   "stop level (with on/off)",

	{ClusterColorControl, uint16(CmdMoveToHue)}:              "move to hue",
	{ClusterColorControl, uint16(CmdMoveHue)}:                "move hue",
	{ClusterColorControl, uint16(CmdStepHue)}:                "step hue",
	{ClusterColorControl, uint16(CmdMoveToSaturation)}:       "move to saturation",
	{ClusterColorControl, uint16(CmdMoveSaturation)}:         "move saturation",
	{ClusterColorControl, uint16(CmdStepSaturation)}:         "step saturation",
	{ClusterColorControl, uint16(CmdMoveToHueAndSaturation)}: "move to hue and saturation",
	{ClusterColorControl, uint16(CmdMoveToColor)}:            "move to color",
	{ClusterColorControl, uint16(CmdMoveColor)}:              "move color",
	{ClusterColorControl, uint16(CmdStepColor)}:              "step color",
	{ClusterColorControl, uint16(CmdMoveToColorTemperature)}: "move to color temperature",

	{ClusterIdentify, uint16(CmdIdentify)}: "identify",
}

// ClusterCommandName renders a cluster-specific command ID.
//
// The cluster is not optional. Command 6 is Move to Hue and Saturation on the
// colour cluster and Step (with On/Off) on the level cluster, and neither is
// the profile-wide command 6.
func ClusterCommandName(cluster uint16, cmd uint8) string {
	if name, ok := clusterCommands[[2]uint16{cluster, uint16(cmd)}]; ok {
		return name
	}
	return fmt.Sprintf("command 0x%02X on %s", cmd, ClusterName(cluster))
}

// ClusterCommandRequest builds a cluster-specific command.
//
// The default response is left enabled, as it is for a write. A command has no
// response of its own — the light simply does the thing — so the Default
// Response is the only evidence that it arrived and was accepted, and
// suppressing it would make a refused command indistinguishable from one that
// worked.
func ClusterCommandRequest(seq, cmd uint8, payload []byte) []byte {
	out := []byte{byte(FrameClusterSpecific), seq, cmd}
	return append(out, payload...)
}

// MoveToLevelPayload sets a light's brightness.
//
// Level runs 1 to 254; 0 is not "off" but the lowest step a device may or may
// not render, and On/Off is the cluster that turns a light off. The transition
// travels in tenths of a second.
func MoveToLevelPayload(level uint8, transition uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{level}, transition)
}

// StepLevelPayload moves a light's brightness by an amount, which is what
// "brighter" means: the device does the arithmetic against whatever it is
// currently at, so no read is needed first and two of them in a row compose.
func StepLevelPayload(mode, size uint8, transition uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{mode, size}, transition)
}

// MoveToHueAndSaturationPayload sets a colour on a device that reports the
// hue/saturation capability. Both run 0 to 254.
func MoveToHueAndSaturationPayload(hue, saturation uint8, transition uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{hue, saturation}, transition)
}

// MoveToColorPayload sets a colour as CIE 1931 xy, for a device that reports
// the XY capability and not hue/saturation. Each coordinate is a fraction of
// 65536, so 0.7006 travels as 45914.
func MoveToColorPayload(x, y, transition uint16) []byte {
	out := binary.LittleEndian.AppendUint16(nil, x)
	out = binary.LittleEndian.AppendUint16(out, y)
	return binary.LittleEndian.AppendUint16(out, transition)
}

// MoveToColorTemperaturePayload sets a white point in mireds, for a device
// that can only do colour temperature. A saturated colour cannot be expressed
// this way at all, which is the caller's problem to notice.
func MoveToColorTemperaturePayload(mireds, transition uint16) []byte {
	out := binary.LittleEndian.AppendUint16(nil, mireds)
	return binary.LittleEndian.AppendUint16(out, transition)
}
