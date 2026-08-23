package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/conor/gzb/internal/ezsp"
	"github.com/conor/gzb/internal/store"
	"github.com/conor/gzb/internal/zcl"
	"github.com/conor/gzb/internal/zdo"
)

// An interview asks a device what it is: its node descriptor, which endpoints
// it has, and what each endpoint implements. The answers are what turn "a
// device at 0x90CB" into "a SONOFF temperature and humidity sensor with a
// temperature cluster on endpoint 1".
//
// Interviewing a sleepy device is the hard case. It only receives while
// polling its parent, so requests sit in the NCP's indirect queue until it
// wakes. That is why the timeouts here are generous and why the best moment to
// interview is immediately after a join, while the device is still awake.

// defaultInterviewTimeout bounds each individual request.
const defaultInterviewTimeout = 30 * time.Second

// interviewer issues ZDO and ZCL queries to one device.
type interviewer struct {
	conn    *ezsp.Conn
	node    uint16
	timeout time.Duration
	seq     uint8
	// verbose reports progress, since an interview of a sleepy device can
	// spend a long time waiting with nothing to show.
	verbose bool
}

// next returns the next transaction sequence number.
func (iv *interviewer) next() uint8 {
	iv.seq++
	return iv.seq
}

func (iv *interviewer) log(format string, args ...any) {
	if iv.verbose {
		fmt.Printf(format+"\n", args...)
	}
}

// zdoRequest sends one ZDO request and returns the matching response payload.
func (iv *interviewer) zdoRequest(ctx context.Context, cluster uint16, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, iv.timeout)
	defer cancel()

	want := zdo.ResponseCluster(cluster)
	seq := payload[0]

	aps := ezsp.APSFrame{
		Profile:  ezsp.ProfileZDO,
		Cluster:  cluster,
		SourceEP: 0, // ZDO always travels on endpoint 0
		DestEP:   0,
		Options:  ezsp.APSOptionRetry,
	}

	msg, err := iv.conn.Request(ctx, iv.node, aps, payload, func(m ezsp.IncomingMessage) bool {
		if m.APS.Profile != ezsp.ProfileZDO || m.APS.Cluster != want {
			return false
		}
		// Match on the transaction sequence, so a reply to somebody else's
		// question is not mistaken for ours.
		got, ok := zdo.Sequence(m.Payload)
		return ok && got == seq
	})
	if err != nil {
		return nil, err
	}
	return msg.Payload, nil
}

// NodeDescriptor asks what kind of node this is.
func (iv *interviewer) NodeDescriptor(ctx context.Context) (zdo.NodeDescriptor, error) {
	iv.log("  node descriptor...")
	payload, err := iv.zdoRequest(ctx, zdo.ClusterNodeDescriptorReq, zdo.AddressRequest(iv.next(), iv.node))
	if err != nil {
		return zdo.NodeDescriptor{}, err
	}
	_, nd, status, err := zdo.ParseNodeDescriptor(payload)
	if err != nil {
		return zdo.NodeDescriptor{}, err
	}
	if !status.OK() {
		return zdo.NodeDescriptor{}, fmt.Errorf("node descriptor: %s", status)
	}
	return nd, nil
}

// PowerDescriptor asks how the device is powered.
func (iv *interviewer) PowerDescriptor(ctx context.Context) (zdo.PowerDescriptor, error) {
	iv.log("  power descriptor...")
	payload, err := iv.zdoRequest(ctx, zdo.ClusterPowerDescriptorReq, zdo.AddressRequest(iv.next(), iv.node))
	if err != nil {
		return zdo.PowerDescriptor{}, err
	}
	_, pd, status, err := zdo.ParsePowerDescriptor(payload)
	if err != nil {
		return zdo.PowerDescriptor{}, err
	}
	if !status.OK() {
		return zdo.PowerDescriptor{}, fmt.Errorf("power descriptor: %s", status)
	}
	return pd, nil
}

// ActiveEndpoints lists the endpoints the device has.
func (iv *interviewer) ActiveEndpoints(ctx context.Context) ([]uint8, error) {
	iv.log("  active endpoints...")
	payload, err := iv.zdoRequest(ctx, zdo.ClusterActiveEndpointsReq, zdo.AddressRequest(iv.next(), iv.node))
	if err != nil {
		return nil, err
	}
	_, eps, status, err := zdo.ParseActiveEndpoints(payload)
	if err != nil {
		return nil, err
	}
	if !status.OK() {
		return nil, fmt.Errorf("active endpoints: %s", status)
	}
	return eps, nil
}

// SimpleDescriptor asks what one endpoint implements.
func (iv *interviewer) SimpleDescriptor(ctx context.Context, ep uint8) (zdo.SimpleDescriptor, error) {
	iv.log("  endpoint %d descriptor...", ep)
	payload, err := iv.zdoRequest(ctx, zdo.ClusterSimpleDescriptorReq,
		zdo.SimpleDescriptorRequest(iv.next(), iv.node, ep))
	if err != nil {
		return zdo.SimpleDescriptor{}, err
	}
	_, sd, status, err := zdo.ParseSimpleDescriptor(payload)
	if err != nil {
		return zdo.SimpleDescriptor{}, err
	}
	if !status.OK() {
		return zdo.SimpleDescriptor{}, fmt.Errorf("endpoint %d: %s", ep, status)
	}
	return sd, nil
}

// Basic reads the manufacturer and model strings from the Basic cluster.
//
// This is ZCL rather than ZDO, and it is the only part of an interview that
// produces a name a person would recognise.
func (iv *interviewer) Basic(ctx context.Context, ep uint8) (manufacturer, model string, err error) {
	iv.log("  manufacturer and model...")
	ctx, cancel := context.WithTimeout(ctx, iv.timeout)
	defer cancel()

	seq := iv.next()
	payload := zcl.ReadAttributesRequest(seq, []uint16{0x0004, 0x0005})
	aps := ezsp.APSFrame{
		Profile:  ezsp.ProfileHomeAutomation,
		Cluster:  zcl.ClusterBasic,
		SourceEP: ezsp.DefaultEndpoint.ID,
		DestEP:   ep,
		Options:  ezsp.APSOptionRetry,
	}

	msg, err := iv.conn.Request(ctx, iv.node, aps, payload, func(m ezsp.IncomingMessage) bool {
		if m.APS.Cluster != zcl.ClusterBasic {
			return false
		}
		f, err := zcl.Decode(m.Payload)
		return err == nil && f.Sequence == seq && f.Command == zcl.CmdReadAttributesResponse
	})
	if err != nil {
		return "", "", err
	}

	frame, err := zcl.Decode(msg.Payload)
	if err != nil {
		return "", "", err
	}
	attrs, err := frame.Attributes()
	if err != nil {
		return "", "", err
	}
	for _, a := range attrs {
		s, ok := a.Value.(string)
		if !ok {
			continue
		}
		// Some devices pad these with trailing NULs.
		s = strings.TrimRight(s, "\x00")
		switch a.ID {
		case 0x0004:
			manufacturer = s
		case 0x0005:
			model = s
		}
	}
	return manufacturer, model, nil
}

// interviewResult is everything an interview discovered.
type interviewResult struct {
	IEEE         string                 `json:"ieee,omitempty"`
	NodeID       uint16                 `json:"node_id"`
	Manufacturer string                 `json:"manufacturer,omitempty"`
	Model        string                 `json:"model,omitempty"`
	Node         *zdo.NodeDescriptor    `json:"node_descriptor,omitempty"`
	Power        *zdo.PowerDescriptor   `json:"power_descriptor,omitempty"`
	Endpoints    []zdo.SimpleDescriptor `json:"endpoints,omitempty"`
	// Problems records what could not be asked, so a partial interview is
	// still a useful one rather than an error.
	Problems []string `json:"problems,omitempty"`
}

// runInterview asks a device everything, tolerating individual failures.
//
// A sleepy device may answer some questions and sleep through others, and a
// partial answer is far more useful than none — so a failed step is recorded
// and the interview continues.
func runInterview(ctx context.Context, iv *interviewer) interviewResult {
	res := interviewResult{NodeID: iv.node}

	if nd, err := iv.NodeDescriptor(ctx); err != nil {
		res.Problems = append(res.Problems, err.Error())
	} else {
		res.Node = &nd
	}

	if pd, err := iv.PowerDescriptor(ctx); err != nil {
		res.Problems = append(res.Problems, err.Error())
	} else {
		res.Power = &pd
	}

	eps, err := iv.ActiveEndpoints(ctx)
	if err != nil {
		res.Problems = append(res.Problems, err.Error())
		return res
	}
	for _, ep := range eps {
		sd, err := iv.SimpleDescriptor(ctx, ep)
		if err != nil {
			res.Problems = append(res.Problems, err.Error())
			continue
		}
		res.Endpoints = append(res.Endpoints, sd)
	}

	// The Basic cluster lives on whichever endpoint implements it.
	if ep, ok := endpointWithCluster(res.Endpoints, zcl.ClusterBasic); ok {
		manufacturer, model, err := iv.Basic(ctx, ep)
		if err != nil {
			res.Problems = append(res.Problems, err.Error())
		} else {
			res.Manufacturer, res.Model = manufacturer, model
		}
	}
	return res
}

// endpointWithCluster finds an endpoint implementing the given input cluster.
func endpointWithCluster(eps []zdo.SimpleDescriptor, cluster uint16) (uint8, bool) {
	for _, sd := range eps {
		for _, in := range sd.Input {
			if in == cluster {
				return sd.Endpoint, true
			}
		}
	}
	return 0, false
}

func cmdInterview(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("interview", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	timeout := fs.Duration("timeout", defaultInterviewTimeout, "how long to wait for each reply")
	all := fs.Bool("all", false, "interview every device in the registry")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb interview [flags] [device]

Asks a device what it is: node descriptor, power descriptor, endpoints, the
clusters each endpoint implements, and its manufacturer and model. Results are
written to the device registry.

The device may be given as an IEEE address (as `+"`gzb devices`"+` prints it), as a
network address in hex, or as the name `+"`gzb name`"+` gave it. With --all, every
device in the registry is interviewed in turn.

Battery devices sleep between transmissions and only receive while polling
their parent, so an interview can take a while or time out entirely. The best
moment to interview one is right after pairing, while it is still awake.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 || (fs.NArg() == 0 && !*all) {
		fs.Usage()
		return flag.ErrHelp
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}

	targets, err := interviewTargets(db, fs.Args(), *all)
	if err != nil {
		return err
	}

	conn, err := dial(ctx, g)
	if err != nil {
		return err
	}
	defer conn.Close()

	state, err := conn.NetworkState(ctx)
	if err != nil {
		return fmt.Errorf("reading network state: %w", err)
	}
	if !state.Joined() {
		return fmt.Errorf("no network on this adapter (%s); form one first", state)
	}

	results := make([]interviewResult, 0, len(targets))
	for _, target := range targets {
		if !g.json {
			fmt.Printf("Interviewing %s (0x%04X)\n", target.name, target.node)
		}
		iv := &interviewer{conn: conn, node: target.node, timeout: *timeout, verbose: !g.json}
		res := runInterview(ctx, iv)
		res.IEEE = target.ieee
		results = append(results, res)

		if target.ieee != "" {
			applyInterview(db, target.ieee, res)
		}
		if !g.json {
			fmt.Println()
			printInterview(res)
			fmt.Println()
		}
	}

	if err := db.Save(); err != nil {
		return err
	}
	if g.json {
		return emitJSON(results)
	}
	return nil
}

// interviewTarget is one device to interview.
type interviewTarget struct {
	name string
	ieee string
	node uint16
}

// interviewTargets resolves command-line arguments to devices.
func interviewTargets(db *store.Store, args []string, all bool) ([]interviewTarget, error) {
	if all {
		devices := db.List()
		if len(devices) == 0 {
			return nil, fmt.Errorf("no devices in the registry; run `gzb join 90` and pair one")
		}
		targets := make([]interviewTarget, 0, len(devices))
		for _, d := range devices {
			targets = append(targets, interviewTarget{name: d.Describe(), ieee: d.IEEE, node: d.NodeID})
		}
		return targets, nil
	}

	arg := args[0]
	d, err := db.Resolve(arg)
	if err == nil {
		return []interviewTarget{{name: d.Describe(), ieee: d.IEEE, node: d.NodeID}}, nil
	}
	// An ambiguous name is a question for the user, not something to fall back
	// from — only "I have never heard of this" is worth trying another way.
	if !errors.Is(err, store.ErrNoDevice) {
		return nil, err
	}
	// A bare hex address addresses a device the registry may not know, which is
	// how a device that joined while nothing was listening gets interviewed.
	if node, ok := store.ParseNodeID(arg); ok {
		return []interviewTarget{{name: arg, node: node}}, nil
	}
	return nil, resolveError(err)
}

// applyInterview records what the interview learned.
func applyInterview(db *store.Store, ieee string, res interviewResult) {
	d, _ := db.Observe(ieee, res.NodeID, time.Now())
	if res.Manufacturer != "" {
		d.Manufacturer = res.Manufacturer
	}
	if res.Model != "" {
		d.Model = res.Model
	}
	if res.Node != nil {
		d.NodeType = res.Node.LogicalType.String()
		cap := uint8(res.Node.MACCapability)
		d.Capability = &cap
	}
	if res.Power != nil {
		d.PowerSource = res.Power.Description()
	}
	if len(res.Endpoints) > 0 {
		d.Endpoints = nil
		for _, sd := range res.Endpoints {
			d.Endpoints = append(d.Endpoints, store.Endpoint{
				ID:      sd.Endpoint,
				Profile: sd.Profile,
				Device:  sd.Device,
				Input:   sd.Input,
				Output:  sd.Output,
			})
		}
		d.Interviewed = time.Now()
	}
}

func printInterview(res interviewResult) {
	if res.Manufacturer != "" || res.Model != "" {
		fmt.Printf("  %s %s\n", res.Manufacturer, res.Model)
	}
	if nd := res.Node; nd != nil {
		power := "battery"
		if nd.MACCapability.Mains() {
			power = "mains"
		}
		listening := "sleepy"
		if !nd.MACCapability.Sleepy() {
			listening = "always listening"
		}
		fmt.Printf("  %s, %s, %s\n", nd.LogicalType, power, listening)
		fmt.Printf("  Manufacturer code 0x%04X\n", nd.ManufacturerCode)
	}
	if pd := res.Power; pd != nil {
		fmt.Printf("  Powered by %s\n", pd.Description())
	}

	for _, sd := range res.Endpoints {
		fmt.Printf("\n  Endpoint %d  profile 0x%04X  device 0x%04X\n", sd.Endpoint, sd.Profile, sd.Device)
		if len(sd.Input) > 0 {
			fmt.Printf("    in   %s\n", clusterNames(sd.Input))
		}
		if len(sd.Output) > 0 {
			fmt.Printf("    out  %s\n", clusterNames(sd.Output))
		}
	}

	for _, p := range res.Problems {
		fmt.Printf("  ! %s\n", p)
	}
}

// clusterNames renders a cluster list readably.
func clusterNames(ids []uint16) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, zcl.ClusterName(id))
	}
	return strings.Join(names, ", ")
}
