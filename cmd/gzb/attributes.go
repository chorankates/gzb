package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// Reading, writing and configuring reports all address the same thing — one
// cluster on one endpoint of one device — and all three have to turn what a
// person typed into it. That resolution is the bulk of this file; the queries
// themselves live in the zigbee package.

// defaultRequestTimeout is how long to wait for an answer.
//
// It is long on purpose. A sleepy device receives nothing until it next polls
// its parent, so until then a request is not late, merely early — and the
// measurements behind this number are unambiguous: reaching one of these
// sensors took anywhere from seconds to nine minutes depending on where in its
// sleep cycle the request arrived. A default that gives up first turns a
// working command into one that appears broken, so this errs entirely towards
// eventually succeeding. Pass -timeout to be less patient.
const defaultRequestTimeout = 5 * time.Minute

// zclEndpointRange is the range of endpoints an application may use. Endpoint
// 0 is reserved for ZDO and 241 upwards for Green Power, so addressing either
// with a cluster read is a mistake worth catching before it goes out.
const (
	firstAppEndpoint = 1
	lastAppEndpoint  = 240
)

func cmdRead(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	endpoint := fs.Int("endpoint", 0, "endpoint to address (default: whichever one has the cluster)")
	timeout := fs.Duration("timeout", defaultRequestTimeout, "how long to wait for the reply")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb read [flags] <device> <cluster> [attribute...]

Asks a device for the current value of some attributes, rather than waiting for
it to report them.

The cluster may be given by name ("temperature") or as a hex ID ("0x0402"), and
so may each attribute. With no attribute named, every attribute gzb knows on
that cluster is asked for — which is a fair way to find out what a device
actually implements, since it answers for the ones it has and says
"unsupported attribute" for the rest.

  gzb read "bedroom thermo" temperature
  gzb read "bedroom thermo" basic manufacturer model
  gzb read 0x90CB 0x0402 0x0000

Values that gzb recognises as measurements are written to the device registry,
exactly as a report would be.

Battery devices sleep between transmissions and only receive while polling
their parent, so a read can take a while or time out entirely.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return flag.ErrHelp
	}

	cluster, ok := zigbee.ParseCluster(fs.Arg(1))
	if !ok {
		return unknownCluster(fs.Arg(1))
	}
	attrs, err := parseAttributes(cluster, fs.Args()[2:])
	if err != nil {
		return err
	}
	if len(attrs) == 0 {
		return fmt.Errorf("gzb knows no attributes on cluster %s; name one, e.g. 0x0000", zigbee.ClusterName(cluster))
	}

	target, name, err := resolveTarget(*dbPath, fs.Arg(0), cluster, *endpoint)
	if err != nil {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if !g.json {
		fmt.Printf("%s %s\n", describeTarget(name, target), zigbee.ClusterName(cluster))
		noteWait(*timeout)
	}

	values, err := coordinator.ReadAttributes(ctx, target, attrs)
	if err != nil {
		return err
	}
	if g.json {
		return emitJSON(values)
	}
	printAttributeValues(values)
	return nil
}

func cmdWrite(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	endpoint := fs.Int("endpoint", 0, "endpoint to address (default: whichever one has the cluster)")
	timeout := fs.Duration("timeout", defaultRequestTimeout, "how long to wait for the reply")
	typeName := fs.String("type", "", "wire encoding of the value, when gzb does not know the attribute")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb write [flags] <device> <cluster> <attribute> <value> [attribute value...]

Sets attributes on a device. Several may be written at once, as
attribute/value pairs, which for a battery device matters: they go out in one
message and so need the device to be awake only once.

  gzb write "hallway lamp" on/off on/off true
  gzb write "hallway lamp" identify "identify time" 10
  gzb write -type string 0x90CB basic 0x0010 upstairs

Flags come before the device, as they do everywhere in gzb: the first argument
that is not a flag ends the flags.

gzb knows the encoding of the attributes it can name. For any other, --type
must say how to encode the value: bool, uint8, uint16, uint24, uint32, uint48,
int8, int16, int24, int32, enum8, enum16, bitmap8, bitmap16, bitmap32, single,
string, octets or utc. A device rejects a wrong guess rather than coercing it.

Not every attribute can be written. A device answers a write to a read-only
attribute with "read only", which is reported here as such.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 4 || fs.NArg()%2 != 0 {
		fs.Usage()
		return flag.ErrHelp
	}

	cluster, ok := zigbee.ParseCluster(fs.Arg(1))
	if !ok {
		return unknownCluster(fs.Arg(1))
	}
	writes, err := parseWrites(cluster, fs.Args()[2:], *typeName)
	if err != nil {
		return err
	}

	target, name, err := resolveTarget(*dbPath, fs.Arg(0), cluster, *endpoint)
	if err != nil {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if !g.json {
		fmt.Printf("%s %s\n", describeTarget(name, target), zigbee.ClusterName(cluster))
		for _, write := range writes {
			fmt.Printf("  %-24s = %v (%s)\n", zigbee.AttributeName(cluster, write.ID), write.Value, write.Type)
		}
		noteWait(*timeout)
	}

	results, err := coordinator.WriteAttributes(ctx, target, writes)
	if err != nil {
		return err
	}
	if g.json {
		return emitJSON(results)
	}
	printAttributeResults(results)
	return nil
}

func cmdReporting(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("reporting", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	endpoint := fs.Int("endpoint", 0, "endpoint to address (default: whichever one has the cluster)")
	timeout := fs.Duration("timeout", defaultRequestTimeout, "how long to wait for the reply")
	typeName := fs.String("type", "", "wire encoding of the attribute, when gzb does not know it")
	min := fs.Duration("min", time.Minute, "shortest interval between reports")
	max := fs.Duration("max", time.Hour, "longest interval between reports, the heartbeat")
	change := fs.Int64("change", 0, "how far the value must move to be worth reporting, in the attribute's own units")
	off := fs.Bool("off", false, "stop the device reporting these attributes")
	defaults := fs.Bool("default", false, "revert these attributes to the device's own default reporting")
	show := fs.Bool("show", false, "report what the device currently has configured, and change nothing")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb reporting [flags] <device> <cluster> <attribute...>

Asks a device to report attributes on its own initiative, so that gzb learns of
a change without having to ask. The configuration lives in the device, not
here: it survives gzb restarting, and stays set until something changes it.

  gzb reporting -show "bedroom thermo" temperature temperature
  gzb reporting -min 60s -max 1h -change 50 "bedroom thermo" temperature temperature
  gzb reporting -default "bedroom thermo" temperature temperature
  gzb reporting -off "bedroom thermo" humidity humidity

--show asks the device what it currently holds and changes nothing. It is the
only way to tell a configuration that took from one that did not: an attribute
switched off looks exactly like one that has nothing new to say.

Flags come before the device, as they do everywhere in gzb: the first argument
that is not a flag ends the flags.

--off and --default are not the same undo. --off tells the device never to
report the attribute; --default restores whatever reporting it shipped with.
Undoing a configuration you set is --default: silencing a sensor that was
reporting on its own is a change in its own right.

--min throttles a fast-changing value; --max is a heartbeat that proves the
device is alive when nothing has changed. --change is in the attribute's own
units, because that is what travels on the wire: a temperature reported in
hundredths of a degree takes 50 to mean half a degree. What that works out to
is printed back, so a wrong guess is visible immediately.

Reporting costs a battery device transmissions. Asking a sensor for a report
every second will get you one, until the battery is flat.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 3 {
		fs.Usage()
		return flag.ErrHelp
	}

	cluster, ok := zigbee.ParseCluster(fs.Arg(1))
	if !ok {
		return unknownCluster(fs.Arg(1))
	}
	attrs, err := parseAttributes(cluster, fs.Args()[2:])
	if err != nil {
		return err
	}

	if *change < 0 {
		return fmt.Errorf("a reportable change is a distance, so it cannot be negative")
	}
	if *show && (*off || *defaults) {
		return fmt.Errorf("-show only asks what the device holds; it cannot be combined with a change")
	}
	if *off && *defaults {
		return fmt.Errorf("-off and -default ask for different things: never report, versus report as the device sees fit")
	}
	configs := make([]zigbee.ReportConfig, 0, len(attrs))
	for _, attr := range attrs {
		dataType, err := attributeType(cluster, attr, *typeName)
		if err != nil {
			return err
		}
		config := zigbee.ReportConfig{ID: attr, Type: dataType, Min: *min, Max: *max, Change: uint64(*change)}
		switch {
		case *defaults:
			config = zigbee.ReportDefaults(attr, dataType)
		case *off:
			config.Max = zigbee.ReportingOff
		}
		configs = append(configs, config)
	}

	target, name, err := resolveTarget(*dbPath, fs.Arg(0), cluster, *endpoint)
	if err != nil {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if *show {
		if !g.json {
			fmt.Printf("%s %s\n", describeTarget(name, target), zigbee.ClusterName(cluster))
			noteWait(*timeout)
		}
		holding, err := coordinator.ReportingConfiguration(ctx, target, attrs)
		if err != nil {
			return err
		}
		if g.json {
			return emitJSON(holding)
		}
		printReportingStatus(holding)
		return nil
	}

	if !g.json {
		fmt.Printf("%s %s\n", describeTarget(name, target), zigbee.ClusterName(cluster))
		for _, config := range configs {
			fmt.Printf("  %-24s %s\n", zigbee.AttributeName(cluster, config.ID), describeReporting(cluster, config))
		}
		noteWait(*timeout)
	}

	results, err := coordinator.ConfigureReporting(ctx, target, configs)
	if err != nil {
		return err
	}
	if g.json {
		return emitJSON(results)
	}
	printAttributeResults(results)
	return nil
}

func printReportingStatus(holding []zigbee.ReportingStatus) {
	for _, status := range holding {
		switch {
		case status.Status != "":
			fmt.Printf("  %-24s !  %s\n", status.Name, status.Status)
		case status.Reporting:
			line := fmt.Sprintf("every %s to %s", status.Min, status.Max)
			if status.Change != nil {
				line = fmt.Sprintf("%s, on a change of %d", line, *status.Change)
			}
			fmt.Printf("  %-24s %s (%s)\n", status.Name, line, status.Type)
		default:
			fmt.Printf("  %-24s not reported\n", status.Name)
		}
	}
}

// resolveTarget turns a device argument and a cluster into an address.
//
// This reads the registry but never writes it: the coordinator keeps its own
// copy and saves what it learns, so a second writer here would overwrite it.
func resolveTarget(dbPath, deviceArg string, cluster uint16, endpoint int) (zigbee.Target, string, error) {
	if endpoint != 0 && (endpoint < firstAppEndpoint || endpoint > lastAppEndpoint) {
		return zigbee.Target{}, "", fmt.Errorf("endpoint %d is outside the application range %d-%d (0 is ZDO)", endpoint, firstAppEndpoint, lastAppEndpoint)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return zigbee.Target{}, "", err
	}

	// Endpoint 1 is where a device with only one puts everything, and is the
	// right guess for a device the registry has never interviewed.
	target := zigbee.Target{Cluster: cluster, Endpoint: firstAppEndpoint}
	name := deviceArg

	device, err := db.Resolve(deviceArg)
	switch {
	case err == nil:
		target.Node = device.NodeID
		name = device.Describe()
		if ep, ok := device.HasCluster(cluster); ok {
			target.Endpoint = ep
		}
	case errors.Is(err, store.ErrNoDevice):
		// A bare hex address addresses a device the registry may not know,
		// which is how something that joined unobserved gets talked to.
		node, ok := store.ParseNodeID(deviceArg)
		if !ok {
			return zigbee.Target{}, "", resolveError(err)
		}
		target.Node = node
	default:
		// An ambiguous name is a question for the user, not something to
		// guess past.
		return zigbee.Target{}, "", err
	}

	if endpoint != 0 {
		target.Endpoint = uint8(endpoint)
	}
	return target, name, nil
}

// parseAttributes resolves the attribute arguments, falling back to everything
// gzb knows on the cluster when none were named.
func parseAttributes(cluster uint16, args []string) ([]uint16, error) {
	if len(args) == 0 {
		return zigbee.KnownAttributes(cluster), nil
	}
	attrs := make([]uint16, 0, len(args))
	for _, arg := range args {
		attr, ok := zigbee.ParseAttribute(cluster, arg)
		if !ok {
			return nil, fmt.Errorf("unknown attribute %q on cluster %s (name one gzb knows, or give a hex ID like 0x0000)", arg, zigbee.ClusterName(cluster))
		}
		attrs = append(attrs, attr)
	}
	return attrs, nil
}

// parseWrites reads the attribute/value pairs a write command was given.
func parseWrites(cluster uint16, args []string, typeName string) ([]zigbee.AttributeWrite, error) {
	writes := make([]zigbee.AttributeWrite, 0, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		attr, ok := zigbee.ParseAttribute(cluster, args[i])
		if !ok {
			return nil, fmt.Errorf("unknown attribute %q on cluster %s (name one gzb knows, or give a hex ID like 0x0010)", args[i], zigbee.ClusterName(cluster))
		}
		dataType, err := attributeType(cluster, attr, typeName)
		if err != nil {
			return nil, err
		}
		value, err := parseValue(dataType, args[i+1])
		if err != nil {
			return nil, fmt.Errorf("attribute %s: %w", zigbee.AttributeName(cluster, attr), err)
		}
		writes = append(writes, zigbee.AttributeWrite{ID: attr, Type: dataType, Value: value})
	}
	return writes, nil
}

// attributeType settles how a value is to be encoded: what the flag said if it
// said anything, otherwise what gzb knows about the attribute.
func attributeType(cluster, attr uint16, typeName string) (zigbee.DataType, error) {
	if typeName != "" {
		dataType, ok := zigbee.ParseDataType(typeName)
		if !ok {
			return 0, fmt.Errorf("unknown data type %q", typeName)
		}
		return dataType, nil
	}
	dataType, ok := zigbee.AttributeType(cluster, attr)
	if !ok {
		return 0, fmt.Errorf("gzb does not know how attribute 0x%04X on cluster %s is encoded; say so with -type", attr, zigbee.ClusterName(cluster))
	}
	return dataType, nil
}

// parseValue reads a value written on the command line in the form its wire
// encoding requires.
func parseValue(t zigbee.DataType, s string) (any, error) {
	switch t {
	case zigbee.TypeBool:
		switch strings.ToLower(s) {
		case "true", "on", "yes", "1":
			return true, nil
		case "false", "off", "no", "0":
			return false, nil
		default:
			return nil, fmt.Errorf("%q is not a boolean (try true or false)", s)
		}

	case zigbee.TypeCharStr, zigbee.TypeOctetStr:
		return s, nil

	case zigbee.TypeSingle:
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		return v, nil

	case zigbee.TypeInt8, zigbee.TypeInt16, zigbee.TypeInt24, zigbee.TypeInt32:
		v, err := strconv.ParseInt(s, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number", s)
		}
		return v, nil

	default:
		// Everything else on the wire is an unsigned integer: the unsigned
		// widths, the enumerations, the bitmaps and UTC time.
		v, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number, and %s values are unsigned", s, t)
		}
		return v, nil
	}
}

func describeTarget(name string, target zigbee.Target) string {
	return fmt.Sprintf("%s (0x%04X) endpoint %d,", name, target.Node, target.Endpoint)
}

// noteWait says how long the command is prepared to wait, because the honest
// answer to "why is nothing happening" is that the device is asleep and there
// is nothing to do but keep asking.
func noteWait(timeout time.Duration) {
	fmt.Printf("  waiting up to %s; a battery device only listens while polling its parent\n", timeout)
}

// describeReporting says back what a reporting configuration asks for, so that
// a change given in the wrong units is obvious before the device applies it.
func describeReporting(cluster uint16, config zigbee.ReportConfig) string {
	switch {
	case config.Min == zigbee.ReportingOff && config.Max == zigbee.ReportingOff:
		return "reverting to the device's own default"
	case config.Max == zigbee.ReportingOff:
		return "reporting off"
	}
	line := fmt.Sprintf("every %s to %s", config.Min, config.Max)
	change, ok := config.Change.(uint64)
	if !ok || change == 0 {
		return line + ", on any change"
	}
	line = fmt.Sprintf("%s, on a change of %d", line, change)
	// The threshold travels in the attribute's own units, so saying what that
	// works out to is how a mistake by a factor of a hundred gets noticed.
	if scaled, unit, ok := zigbee.ScaleValue(cluster, config.ID, float64(change)); ok && unit != "" {
		line = fmt.Sprintf("%s (%.2f %s)", line, scaled, unit)
	}
	return line
}

func printAttributeValues(values []zigbee.AttributeValue) {
	for _, value := range values {
		switch {
		case value.Status != "":
			fmt.Printf("  %-24s !  %s\n", value.Name, value.Status)
		case value.Scaled != nil && value.Unit != "":
			// Both halves are worth printing: the scaled value is what the
			// measurement means, the raw one is what the device actually said.
			fmt.Printf("  %-24s %.2f %-4s (raw %v, %s)\n", value.Name, *value.Scaled, value.Unit, value.Value, value.Type)
		default:
			fmt.Printf("  %-24s %v (%s)\n", value.Name, value.Value, value.Type)
		}
	}
}

func printAttributeResults(results []zigbee.AttributeResult) {
	for _, result := range results {
		if result.OK {
			fmt.Printf("  ok  %s\n", result.Name)
			continue
		}
		fmt.Printf("  !   %s: %s\n", result.Name, result.Status)
	}
}

func unknownCluster(arg string) error {
	return fmt.Errorf("unknown cluster %q (name one gzb knows, or give a hex ID like 0x0402)", arg)
}
