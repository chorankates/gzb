package zigbee

// Reading and writing attributes on demand is the other half of the protocol
// loop. Readings arrive when a device decides to send them; this is how the
// hub asks a question of its own — and, with a reporting configuration, how it
// arranges never to have to ask again.
//
// Configuring reporting is the one of the three that changes a device's
// behaviour rather than merely inspecting it. It survives a reboot of the hub
// because it lives in the device, which is exactly what makes it worth doing
// and also what makes it worth being careful with: a sensor told to report
// every second will do so until something tells it otherwise, and on a battery
// device that is measured in weeks of life.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/internal/zcl"
)

// Target addresses one cluster on one endpoint of one device.
//
// All three are needed. A device is a node address; what it can do lives on an
// endpoint; and what an attribute means depends on the cluster it belongs to —
// attribute 0x0000 is a temperature on one cluster and an on/off state on
// another.
type Target struct {
	Node     uint16
	Endpoint uint8
	Cluster  uint16
}

func (t Target) String() string {
	return fmt.Sprintf("0x%04X endpoint %d cluster %s", t.Node, t.Endpoint, zcl.ClusterName(t.Cluster))
}

// DataType is a ZCL attribute encoding. It has to be stated when writing an
// attribute or configuring a report, because both declare the type before the
// device has said anything about it.
type DataType uint8

func (t DataType) String() string { return zcl.DataType(t).String() }

// MarshalJSON writes the type by name, so that machine-readable output is also
// readable: "int16" rather than 41. A type gzb has no name for is written as
// hex, which ParseDataType accepts back.
func (t DataType) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// UnmarshalJSON accepts what MarshalJSON writes.
func (t *DataType) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	parsed, ok := ParseDataType(name)
	if !ok {
		return fmt.Errorf("zigbee: %q is not a data type", name)
	}
	*t = parsed
	return nil
}

// Data types that can be written. These mirror the ZCL encodings; the names
// are what ParseDataType accepts.
const (
	TypeBool     = DataType(zcl.TypeBool)
	TypeBitmap8  = DataType(zcl.TypeBitmap8)
	TypeBitmap16 = DataType(zcl.TypeBitmap16)
	TypeBitmap32 = DataType(zcl.TypeBitmap32)
	TypeUint8    = DataType(zcl.TypeUint8)
	TypeUint16   = DataType(zcl.TypeUint16)
	TypeUint24   = DataType(zcl.TypeUint24)
	TypeUint32   = DataType(zcl.TypeUint32)
	TypeUint48   = DataType(zcl.TypeUint48)
	TypeInt8     = DataType(zcl.TypeInt8)
	TypeInt16    = DataType(zcl.TypeInt16)
	TypeInt24    = DataType(zcl.TypeInt24)
	TypeInt32    = DataType(zcl.TypeInt32)
	TypeEnum8    = DataType(zcl.TypeEnum8)
	TypeEnum16   = DataType(zcl.TypeEnum16)
	TypeSingle   = DataType(zcl.TypeSingle)
	TypeOctetStr = DataType(zcl.TypeOctetStr)
	TypeCharStr  = DataType(zcl.TypeCharStr)
	TypeUTCTime  = DataType(zcl.TypeUTCTime)
)

// ParseDataType resolves a type written by name, as ParseDataType's own String
// renders it.
func ParseDataType(s string) (DataType, bool) {
	t, ok := zcl.ParseDataType(s)
	return DataType(t), ok
}

// AttributeType reports the encoding of an attribute gzb knows, so that a
// caller writing a well-known attribute does not have to supply one.
func AttributeType(cluster, attr uint16) (DataType, bool) {
	t, ok := zcl.AttributeType(cluster, attr)
	return DataType(t), ok
}

// AttributeValue is one attribute as a device reported it.
type AttributeValue struct {
	ID   uint16   `json:"id"`
	Name string   `json:"name"`
	Type DataType `json:"type"`
	// Value is the decoded value in its own terms: an integer, a string, a
	// boolean, or raw bytes for a type gzb does not interpret.
	Value any `json:"value"`
	// Scaled and Unit carry the physical meaning where there is one. A
	// temperature arrives as 2820 and means 28.20 °C; both are reported,
	// because the raw value is what the device actually said.
	Scaled *float64 `json:"scaled,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	// Current marks the values that are a reading — what the device is
	// measuring or doing now, rather than a limit it was built with or a
	// statistic it keeps. Only these are written to the registry, and a caller
	// storing them anywhere else wants the same distinction.
	Current bool `json:"current,omitempty"`
	// Status explains why an attribute has no value — most often that the
	// device does not implement it. It is empty when the read succeeded.
	Status string `json:"status,omitempty"`
}

// AttributeWrite is one attribute to set.
type AttributeWrite struct {
	ID   uint16
	Type DataType
	// Value must suit the type: an integer for the integer types, a bool for
	// TypeBool, a string for TypeCharStr.
	Value any
}

// AttributeResult is the outcome of a write or a reporting configuration for
// one attribute.
type AttributeResult struct {
	ID   uint16 `json:"id"`
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Status is the device's reason for refusing, empty when it did not. A
	// write to a read-only attribute and a write of the wrong type both come
	// back here rather than as a delivery failure.
	Status string `json:"status,omitempty"`
}

// ReportingOff, as a ReportConfig's Max, stops a device reporting an
// attribute. It is the longest interval the protocol can express, which the
// specification reserves to mean "never" — so it cannot also be asked for as
// an eighteen-hour heartbeat.
//
// As *both* Min and Max it means something else entirely: revert to the
// device's own default configuration. See ReportDefaults, which says that
// without relying on the reader to notice which fields are set.
const ReportingOff = time.Duration(zcl.MaxIntervalDisabled) * time.Second

// ReportDefaults returns a configuration that asks a device to revert an
// attribute to its own default reporting behaviour.
//
// This is the counterpart to turning reporting off, and not the same thing:
// off means "never report this", whereas this restores whatever the device
// shipped with. Undoing a configuration means this one — stopping the reports
// a device was making on its own is a change, not an undo.
func ReportDefaults(attr uint16, dataType DataType) ReportConfig {
	return ReportConfig{ID: attr, Type: dataType, Min: ReportingOff, Max: ReportingOff}
}

// ReportConfig asks a device to report one attribute on its own initiative.
type ReportConfig struct {
	ID uint16
	// Type must match the attribute's actual encoding; a device rejects a
	// wrong guess rather than coercing it. AttributeType supplies it for the
	// attributes gzb knows.
	Type DataType

	// Min is the shortest interval between two reports: it throttles a
	// fast-changing value so a noisy sensor cannot flood the network.
	Min time.Duration
	// Max is the longest interval: a heartbeat that proves the device is still
	// alive when nothing has changed. ReportingOff turns reporting off, and
	// ReportingOff in both Min and Max reverts the device to its own defaults
	// — see ReportDefaults.
	Max time.Duration
	// Change is how far the value must move before a report is worth sending,
	// in the attribute's own encoding — a temperature reported in hundredths
	// takes 50 to mean half a degree. It applies only to analog types, and nil
	// asks for a report on every change.
	Change any
}

// ReadAttributes asks a device for the current value of some attributes.
//
// The device answers with one record per attribute, in no guaranteed order and
// possibly with a status instead of a value for the ones it does not
// implement, so the results are matched to the request by attribute ID rather
// than by position.
//
// Values that gzb recognises as measurements are recorded in the device
// registry, exactly as a report would be: a reading is a reading whether the
// device volunteered it or was asked. Call Close to persist that.
func (c *Coordinator) ReadAttributes(ctx context.Context, t Target, attrs []uint16) ([]AttributeValue, error) {
	if len(attrs) == 0 {
		return nil, fmt.Errorf("zigbee: a read needs at least one attribute")
	}

	seq := c.nextSequence()
	frame, err := c.zclRequest(ctx, t, seq, zcl.CmdReadAttributesResponse, zcl.ReadAttributesRequest(seq, attrs))
	if err != nil {
		return nil, err
	}
	records, err := frame.Attributes()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	values := make([]AttributeValue, 0, len(records))
	for _, record := range records {
		value := AttributeValue{
			ID:   record.ID,
			Name: zcl.AttributeName(t.Cluster, record.ID),
			Type: DataType(record.Type),
		}
		if record.Status != zcl.StatusSuccess {
			value.Status = zcl.StatusName(record.Status)
			values = append(values, value)
			continue
		}
		value.Value = record.Value
		if text, ok := record.Value.(string); ok {
			// Devices pad strings to a fixed width with NULs. The padding is
			// not information, and it makes a model name unprintable.
			value.Value = strings.TrimRight(text, "\x00")
		}
		if quantity, ok := zcl.Interpret(t.Cluster, record); ok {
			scaled := quantity.Value
			value.Scaled, value.Unit, value.Current = &scaled, quantity.Unit, quantity.Current
		}
		values = append(values, value)
	}
	c.recordValues(t, values, now)
	return values, nil
}

// WriteAttributes sets attributes on a device.
//
// A device reports success for the whole write as a single status and names
// only what it refused, so a result is returned for every attribute asked for
// either way.
func (c *Coordinator) WriteAttributes(ctx context.Context, t Target, writes []AttributeWrite) ([]AttributeResult, error) {
	if len(writes) == 0 {
		return nil, fmt.Errorf("zigbee: a write needs at least one attribute")
	}

	records := make([]zcl.WriteRecord, 0, len(writes))
	requested := make([]uint16, 0, len(writes))
	for _, write := range writes {
		records = append(records, zcl.WriteRecord{ID: write.ID, Type: zcl.DataType(write.Type), Value: write.Value})
		requested = append(requested, write.ID)
	}

	seq := c.nextSequence()
	payload, err := zcl.WriteAttributesRequest(seq, records)
	if err != nil {
		return nil, err
	}
	frame, err := c.zclRequest(ctx, t, seq, zcl.CmdWriteAttributesResponse, payload)
	if err != nil {
		return nil, err
	}
	outcomes, err := frame.WriteResponse(requested)
	if err != nil {
		return nil, err
	}

	results := make([]AttributeResult, 0, len(outcomes))
	for _, outcome := range outcomes {
		results = append(results, attributeResult(t.Cluster, outcome.ID, outcome.Status))
	}
	return results, nil
}

// ConfigureReporting asks a device to report attributes on its own initiative,
// which is what turns a hub that polls into one that listens.
//
// The configuration lives in the device and survives the hub restarting, so it
// only needs setting once — and stays set until something changes it.
func (c *Coordinator) ConfigureReporting(ctx context.Context, t Target, configs []ReportConfig) ([]AttributeResult, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("zigbee: a reporting configuration needs at least one attribute")
	}

	records := make([]zcl.ReportingConfig, 0, len(configs))
	requested := make([]uint16, 0, len(configs))
	for _, config := range configs {
		min, err := reportInterval(config.Min, "minimum")
		if err != nil {
			return nil, err
		}
		max, err := reportInterval(config.Max, "maximum")
		if err != nil {
			return nil, err
		}
		record := zcl.ReportingConfig{
			ID:     config.ID,
			Type:   zcl.DataType(config.Type),
			Min:    min,
			Max:    max,
			Change: config.Change,
		}
		// An analog attribute must carry a reportable change. Left unsaid, it
		// means "report every change", which the wire spells as zero.
		if record.Change == nil && record.Type.Analog() {
			record.Change = uint64(0)
		}
		records = append(records, record)
		requested = append(requested, config.ID)
	}

	seq := c.nextSequence()
	payload, err := zcl.ConfigureReportingRequest(seq, records)
	if err != nil {
		return nil, err
	}
	frame, err := c.zclRequest(ctx, t, seq, zcl.CmdConfigureReportingRsp, payload)
	if err != nil {
		return nil, err
	}
	outcomes, err := frame.ConfigureReportingResponse(requested)
	if err != nil {
		return nil, err
	}

	results := make([]AttributeResult, 0, len(outcomes))
	for _, outcome := range outcomes {
		results = append(results, attributeResult(t.Cluster, outcome.ID, outcome.Status))
	}
	return results, nil
}

// ReportingStatus is one attribute's reporting configuration as the device
// actually holds it, which is not necessarily what it was asked for.
type ReportingStatus struct {
	ID   uint16 `json:"id"`
	Name string `json:"name"`
	// Reporting is false when the device is not sending this attribute at all.
	Reporting bool `json:"reporting"`
	// Min and Max are the configured interval bounds, zero when the device
	// refused the question or reports nothing.
	Min time.Duration `json:"min,omitempty"`
	Max time.Duration `json:"max,omitempty"`
	// Change is the configured reportable change in the attribute's own units,
	// nil for a discrete attribute that has none. It is reported because
	// restoring a configuration means matching it, and a threshold that is not
	// visible cannot be matched.
	Change *uint64 `json:"change,omitempty"`
	// Type is the encoding the device holds for this attribute, which is what
	// a configuration written back to it has to state.
	Type DataType `json:"type,omitempty"`
	// Status is the device's reason for refusing, empty when it answered.
	Status string `json:"status,omitempty"`
}

// ReportingConfiguration asks a device what reporting it currently holds for
// some attributes.
//
// This is the question that settles what a configuration actually did.
// ConfigureReporting comes back with a status and nothing else, so it confirms
// that a device accepted an instruction, not what it now holds — and the two
// come apart precisely when it matters, because an attribute that has been
// switched off looks exactly like one that simply has nothing new to say.
func (c *Coordinator) ReportingConfiguration(ctx context.Context, t Target, attrs []uint16) ([]ReportingStatus, error) {
	if len(attrs) == 0 {
		return nil, fmt.Errorf("zigbee: a reporting configuration read needs at least one attribute")
	}

	seq := c.nextSequence()
	payload, err := zcl.ReadReportingConfigRequest(seq, attrs)
	if err != nil {
		return nil, err
	}
	frame, err := c.zclRequest(ctx, t, seq, zcl.CmdReadReportingConfigRsp, payload)
	if err != nil {
		return nil, err
	}
	records, err := frame.ReadReportingConfigResponse()
	if err != nil {
		return nil, err
	}

	out := make([]ReportingStatus, 0, len(records))
	for _, record := range records {
		status := ReportingStatus{
			ID:        record.ID,
			Name:      zcl.AttributeName(t.Cluster, record.ID),
			Reporting: record.Reporting(),
		}
		if record.Status != zcl.StatusSuccess {
			status.Status = zcl.StatusName(record.Status)
		} else if status.Reporting {
			status.Type = DataType(record.Type)
			status.Min = time.Duration(record.Min) * time.Second
			status.Max = time.Duration(record.Max) * time.Second
			if change, ok := decodeChange(record.Change); ok {
				status.Change = &change
			}
		}
		out = append(out, status)
	}
	return out, nil
}

// decodeChange reads a reportable change back out of its wire bytes. It is
// little-endian in the attribute's own type, and only its magnitude matters.
func decodeChange(raw []byte) (uint64, bool) {
	if len(raw) == 0 || len(raw) > 8 {
		return 0, false
	}
	var v uint64
	for i := len(raw) - 1; i >= 0; i-- {
		v = v<<8 | uint64(raw[i])
	}
	return v, true
}

// zclRequest sends one ZCL command and waits for the response that answers it.
//
// A device may reply either with the specific response the command calls for
// or with a Default Response, which is how it says it does not implement the
// command at all. Accepting both is what makes a refusal arrive as a refusal
// instead of as a timeout thirty seconds later.
func (c *Coordinator) zclRequest(ctx context.Context, t Target, seq, want uint8, payload []byte) (zcl.Frame, error) {
	if err := c.checkOpen(); err != nil {
		return zcl.Frame{}, err
	}
	// Checking the network first turns the commonest failure — an adapter
	// that has no network at all — into an immediate explanation rather than
	// a wait for a device that was never going to be reachable. It costs a
	// round trip to the NCP on the local serial line, not to the device.
	state, err := c.conn.NetworkState(ctx)
	if err != nil {
		return zcl.Frame{}, fmt.Errorf("zigbee: reading network state: %w", err)
	}
	if !state.Joined() {
		return zcl.Frame{}, fmt.Errorf("zigbee: no network on this adapter (%s)", state)
	}

	aps := ezsp.APSFrame{
		Profile:  ezsp.ProfileHomeAutomation,
		Cluster:  t.Cluster,
		SourceEP: ezsp.DefaultEndpoint.ID,
		DestEP:   t.Endpoint,
		Options:  ezsp.APSOptionRetry,
	}
	msg, err := c.conn.Request(ctx, t.Node, aps, payload, func(m ezsp.IncomingMessage) bool {
		// The sequence identifies the question and the sender identifies the
		// answerer. Both are needed: sequence numbers are only unique per
		// device, so another node's reply can carry the same one.
		if m.APS.Cluster != t.Cluster || m.Sender != t.Node {
			return false
		}
		f, err := zcl.Decode(m.Payload)
		if err != nil || f.Sequence != seq || f.Type != zcl.FrameProfileWide {
			return false
		}
		return f.Command == want || f.Command == zcl.CmdDefaultResponse
	})
	if err != nil {
		return zcl.Frame{}, err
	}

	frame, err := zcl.Decode(msg.Payload)
	if err != nil {
		return zcl.Frame{}, err
	}
	if frame.Command != want {
		return zcl.Frame{}, c.refusal(t, frame)
	}
	return frame, nil
}

// refusal turns a Default Response into the error it stands for.
func (c *Coordinator) refusal(t Target, frame zcl.Frame) error {
	response, err := frame.DefaultResponse()
	if err != nil {
		return err
	}
	command := zcl.CommandName(response.Command)
	if response.Status == zcl.StatusSuccess {
		// A bare acknowledgement where the specification requires a real
		// response leaves the outcome genuinely unknown: the device may well
		// have applied the command, but it did not say what it did with each
		// attribute, and inventing per-attribute successes would be a guess.
		return fmt.Errorf("zigbee: %s acknowledged %s without saying what it did with each attribute; it may have been applied", t, command)
	}
	return fmt.Errorf("zigbee: %s refused %s: %s", t, command, zcl.StatusName(response.Status))
}

// attributeResult renders one attribute's outcome.
func attributeResult(cluster, attr uint16, status uint8) AttributeResult {
	result := AttributeResult{
		ID:   attr,
		Name: zcl.AttributeName(cluster, attr),
		OK:   status == zcl.StatusSuccess,
	}
	if !result.OK {
		result.Status = zcl.StatusName(status)
	}
	return result
}

// reportInterval converts an interval to the whole seconds the wire carries.
func reportInterval(d time.Duration, which string) (uint16, error) {
	if d < 0 || d%time.Second != 0 {
		return 0, fmt.Errorf("zigbee: %s reporting interval must be a whole number of seconds, not %s", which, d)
	}
	seconds := d / time.Second
	if seconds > math.MaxUint16 {
		return 0, fmt.Errorf("zigbee: %s reporting interval %s is longer than the protocol's limit of %s", which, d, ReportingOff)
	}
	return uint16(seconds), nil
}

// recordValues stores what a read learned in the device registry.
//
// Only devices the registry already knows are recorded, for the same reason an
// interview does not invent one: a read can be aimed at any address, and a
// record keyed by an address nobody has identified is noise.
func (c *Coordinator) recordValues(t Target, values []AttributeValue, now time.Time) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	device, ok := c.db.ByNodeID(t.Node)
	if !ok {
		return
	}
	// An answer of any kind is proof the device is alive, even one that says
	// it does not implement what was asked.
	device.LastSeen = now
	for _, value := range values {
		// A registry reading is answered by "what is it now", and every entry
		// carries a timestamp saying so. The range a sensor can measure and
		// the coldest it has been are both temperatures, and neither answers
		// that question, so neither is stored as though it did.
		if value.Scaled == nil || !value.Current {
			continue
		}
		device.Record(value.Name, *value.Scaled, value.Unit, now)
	}
	recordBasicIdentity(device, t.Cluster, values)
}

// recordBasicIdentity keeps a manufacturer or model read on demand, so that a
// device named by a plain `gzb read` is named everywhere afterwards.
func recordBasicIdentity(device *store.Device, cluster uint16, values []AttributeValue) {
	if cluster != zcl.ClusterBasic {
		return
	}
	for _, value := range values {
		s, ok := value.Value.(string)
		if !ok || s == "" {
			continue
		}
		switch value.ID {
		case attrManufacturer:
			device.Manufacturer = s
		case attrModel:
			device.Model = s
		}
	}
}

// The rest of this file is the vocabulary an application needs to turn what a
// person typed into something addressable, without reaching past this package
// for it.

// ClusterName renders a cluster ID, falling back to hex.
func ClusterName(id uint16) string { return zcl.ClusterName(id) }

// AttributeName renders a well-known attribute ID for a cluster, falling back
// to hex.
func AttributeName(cluster, attr uint16) string { return zcl.AttributeName(cluster, attr) }

// ParseCluster resolves a cluster given either as a name gzb knows or as a hex
// identifier in the 0xNNNN form Zigbee tools print.
func ParseCluster(s string) (uint16, bool) { return zcl.ParseCluster(s) }

// ParseAttribute resolves an attribute given either as a name gzb knows on
// this cluster or as a hex identifier.
func ParseAttribute(cluster uint16, s string) (uint16, bool) { return zcl.ParseAttribute(cluster, s) }

// ScaleValue renders a raw attribute value as the measurement it stands for,
// reporting false when gzb knows of no interpretation. The unit is empty for
// quantities that have none, such as an on/off state.
func ScaleValue(cluster, attr uint16, raw float64) (value float64, unit string, ok bool) {
	reading, ok := zcl.Scale(cluster, attr, raw)
	return reading.Value, reading.Unit, ok
}

// KnownAttributes lists the attributes gzb knows on a cluster, in ID order.
// It is a reasonable default for a read that names a cluster and no attribute,
// but it is only what gzb happens to know: a device may implement more, and
// may implement none of these.
func KnownAttributes(cluster uint16) []uint16 { return zcl.KnownAttributes(cluster) }
