package zigbee

// ZDO discovery is how a coordinator finds out what a device is. A device that
// has joined is only an address until it has been asked three questions: what
// kind of node it is (the node descriptor), which endpoints it has (the active
// endpoint list), and what each of those endpoints implements (a simple
// descriptor per endpoint). Together they turn "a device at 0x90CB" into "a
// temperature and humidity sensor with a temperature cluster on endpoint 1".
//
// Every query here is a round trip to the device, which makes a sleepy battery
// node the hard case: it only receives while polling its parent, so a request
// sits in the NCP's indirect queue until the device next wakes. That is why
// the timeouts are generous, why a partial answer is preferred to an error,
// and why the best moment to interview a device is immediately after it joins.

import (
	"context"
	"fmt"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/internal/zcl"
	"github.com/chorankates/gzb/internal/zdo"
)

// DefaultInterviewTimeout bounds each individual request an interview makes.
const DefaultInterviewTimeout = 30 * time.Second

// zdoEndpoint is the endpoint ZDO always travels on, at both ends.
const zdoEndpoint uint8 = 0

// The two Basic cluster attributes that say what a device is.
const (
	attrManufacturer uint16 = 0x0004
	attrModel        uint16 = 0x0005
)

// NodeDescriptor describes a device's role and capabilities as a network node.
type NodeDescriptor struct {
	// LogicalType is "coordinator", "router" or "end device".
	LogicalType string `json:"logical_type"`
	// Mains reports whether the device runs from mains rather than a battery.
	Mains bool `json:"mains_powered"`
	// Sleepy reports whether the device sleeps between transmissions. A sleepy
	// device cannot be reached on demand; it must be caught while awake.
	Sleepy bool `json:"sleepy"`
	// Capability is the raw MAC capability bitmask. Mains and Sleepy are the
	// two bits worth reading by name; the whole byte is kept because it is
	// also what a device broadcasts when it announces itself, and the registry
	// stores it for comparison.
	Capability uint8 `json:"mac_capability"`

	ManufacturerCode uint16 `json:"manufacturer_code"`
	MaxBufferSize    uint8  `json:"max_buffer_size"`
	MaxIncomingSize  uint16 `json:"max_incoming_transfer_size"`
	MaxOutgoingSize  uint16 `json:"max_outgoing_transfer_size"`
	ServerMask       uint16 `json:"server_mask"`
}

// PowerDescriptor reports how a device is powered.
type PowerDescriptor struct {
	// Source describes the supply currently in use, for example "mains" or
	// "disposable battery".
	Source string `json:"source"`
	// Level is the charge remaining, as the four-step scale the descriptor
	// carries: 0 is critical, 4 is a third, 8 is two thirds and 12 is full.
	Level uint8 `json:"level"`
}

// Cluster names one cluster an endpoint speaks. The identifier is what travels
// on the wire; the name is there so output does not have to be decoded by eye.
type Cluster struct {
	ID   uint16 `json:"id"`
	Name string `json:"name"`
}

// Endpoint is one endpoint's simple descriptor: what that endpoint implements.
type Endpoint struct {
	ID       uint8     `json:"endpoint"`
	Profile  uint16    `json:"profile"`
	DeviceID uint16    `json:"device_id"`
	Version  uint8     `json:"version"`
	Input    []Cluster `json:"input_clusters,omitempty"`
	Output   []Cluster `json:"output_clusters,omitempty"`
}

// Description is everything an interview discovered about one device.
type Description struct {
	IEEE         string           `json:"ieee,omitempty"`
	DeviceName   string           `json:"device_name,omitempty"`
	NodeID       uint16           `json:"node_id"`
	Manufacturer string           `json:"manufacturer,omitempty"`
	Model        string           `json:"model,omitempty"`
	Node         *NodeDescriptor  `json:"node_descriptor,omitempty"`
	Power        *PowerDescriptor `json:"power_descriptor,omitempty"`
	Endpoints    []Endpoint       `json:"endpoints,omitempty"`
	At           time.Time        `json:"at"`

	// Problems records what could not be asked. A device that answers some
	// questions and sleeps through the rest has still told you something, so
	// a failed step is recorded here rather than abandoning the interview.
	Problems []string `json:"problems,omitempty"`
}

// InterviewOptions tunes an interview.
type InterviewOptions struct {
	// Timeout bounds each individual request. Zero means
	// DefaultInterviewTimeout.
	Timeout time.Duration
	// Progress, when set, is called with the name of each step as it begins.
	// An interview of a sleepy device can spend a long time waiting with
	// nothing to show, which is worth reporting.
	Progress func(step string)
}

// nextSequence returns the next transaction sequence number.
//
// Every request carries one and its response echoes it, which is how a reply
// is matched to the question that produced it. It must therefore be unique
// across concurrent queries on the same coordinator. ZDO and ZCL count in
// separate spaces on the wire, but sharing one counter costs nothing and
// leaves no chance of the two colliding in a caller's own bookkeeping.
func (c *Coordinator) nextSequence() uint8 {
	return uint8(c.seq.Add(1))
}

// zdoQuery sends one ZDO request and returns the matching response payload.
// The caller's context bounds the wait.
func (c *Coordinator) zdoQuery(ctx context.Context, node, cluster uint16, payload []byte) ([]byte, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}

	want := zdo.ResponseCluster(cluster)
	seq := payload[0]
	aps := ezsp.APSFrame{
		Profile:  ezsp.ProfileZDO,
		Cluster:  cluster,
		SourceEP: zdoEndpoint,
		DestEP:   zdoEndpoint,
		Options:  ezsp.APSOptionRetry,
	}

	msg, err := c.conn.Request(ctx, node, aps, payload, func(m ezsp.IncomingMessage) bool {
		if m.APS.Profile != ezsp.ProfileZDO || m.APS.Cluster != want {
			return false
		}
		// The sequence identifies the question and the sender identifies the
		// answerer. Both are needed: sequence numbers are only unique per
		// device, so another node's reply can carry the same one.
		if m.Sender != node {
			return false
		}
		got, ok := zdo.Sequence(m.Payload)
		return ok && got == seq
	})
	if err != nil {
		return nil, err
	}
	return msg.Payload, nil
}

// NodeDescriptor asks a device what kind of node it is: its role in the mesh,
// whether it is mains powered, and whether it listens continuously.
//
// The context bounds the wait, and choosing that bound is a real decision: a
// request to a sleepy device is not late until the device has had time to wake.
func (c *Coordinator) NodeDescriptor(ctx context.Context, node uint16) (NodeDescriptor, error) {
	payload, err := c.zdoQuery(ctx, node, zdo.ClusterNodeDescriptorReq, zdo.AddressRequest(c.nextSequence(), node))
	if err != nil {
		return NodeDescriptor{}, err
	}
	_, nd, status, err := zdo.ParseNodeDescriptor(payload)
	if err != nil {
		return NodeDescriptor{}, err
	}
	if !status.OK() {
		return NodeDescriptor{}, fmt.Errorf("zigbee: node descriptor for 0x%04X: %s", node, status)
	}
	return NodeDescriptor{
		LogicalType:      nd.LogicalType.String(),
		Mains:            nd.MACCapability.Mains(),
		Sleepy:           nd.MACCapability.Sleepy(),
		Capability:       uint8(nd.MACCapability),
		ManufacturerCode: nd.ManufacturerCode,
		MaxBufferSize:    nd.MaxBufferSize,
		MaxIncomingSize:  nd.MaxIncomingSize,
		MaxOutgoingSize:  nd.MaxOutgoingSize,
		ServerMask:       nd.ServerMask,
	}, nil
}

// PowerDescriptor asks a device how it is powered.
func (c *Coordinator) PowerDescriptor(ctx context.Context, node uint16) (PowerDescriptor, error) {
	payload, err := c.zdoQuery(ctx, node, zdo.ClusterPowerDescriptorReq, zdo.AddressRequest(c.nextSequence(), node))
	if err != nil {
		return PowerDescriptor{}, err
	}
	_, pd, status, err := zdo.ParsePowerDescriptor(payload)
	if err != nil {
		return PowerDescriptor{}, err
	}
	if !status.OK() {
		return PowerDescriptor{}, fmt.Errorf("zigbee: power descriptor for 0x%04X: %s", node, status)
	}
	return PowerDescriptor{Source: pd.Description(), Level: pd.CurrentLevel}, nil
}

// ActiveEndpoints lists the endpoints a device has. An endpoint is the address
// a cluster lives at, so this is the question that must be answered before
// anything can be asked about what the device actually does.
func (c *Coordinator) ActiveEndpoints(ctx context.Context, node uint16) ([]uint8, error) {
	payload, err := c.zdoQuery(ctx, node, zdo.ClusterActiveEndpointsReq, zdo.AddressRequest(c.nextSequence(), node))
	if err != nil {
		return nil, err
	}
	_, eps, status, err := zdo.ParseActiveEndpoints(payload)
	if err != nil {
		return nil, err
	}
	if !status.OK() {
		return nil, fmt.Errorf("zigbee: active endpoints for 0x%04X: %s", node, status)
	}
	return eps, nil
}

// SimpleDescriptor asks what one endpoint implements: its profile, its device
// type, and the clusters it speaks in each direction.
func (c *Coordinator) SimpleDescriptor(ctx context.Context, node uint16, endpoint uint8) (Endpoint, error) {
	request := zdo.SimpleDescriptorRequest(c.nextSequence(), node, endpoint)
	payload, err := c.zdoQuery(ctx, node, zdo.ClusterSimpleDescriptorReq, request)
	if err != nil {
		return Endpoint{}, err
	}
	_, sd, status, err := zdo.ParseSimpleDescriptor(payload)
	if err != nil {
		return Endpoint{}, err
	}
	if !status.OK() {
		return Endpoint{}, fmt.Errorf("zigbee: endpoint %d of 0x%04X: %s", endpoint, node, status)
	}
	return Endpoint{
		ID:       sd.Endpoint,
		Profile:  sd.Profile,
		DeviceID: sd.Device,
		Version:  sd.Version,
		Input:    namedClusters(sd.Input),
		Output:   namedClusters(sd.Output),
	}, nil
}

// Interview asks a device everything: its node and power descriptors, its
// endpoints, what each endpoint implements, and its manufacturer and model.
//
// Individual failures are tolerated. A sleepy device may answer some questions
// and sleep through others, and a partial answer is far more useful than none,
// so a failed step is recorded in Problems and the interview continues. An
// error is returned only when no question could be asked at all.
//
// What the interview learns is written to the device registry for devices the
// registry already knows, so later output can name a device by its model
// rather than its address. Call Close to persist that.
func (c *Coordinator) Interview(ctx context.Context, node uint16, opts InterviewOptions) (Description, error) {
	if err := c.checkOpen(); err != nil {
		return Description{}, err
	}

	// One state check up front turns "every query timed out" into an
	// immediate, accurate explanation.
	state, err := c.conn.NetworkState(ctx)
	if err != nil {
		return Description{}, fmt.Errorf("zigbee: reading network state: %w", err)
	}
	if !state.Joined() {
		return Description{}, fmt.Errorf("zigbee: no network on this adapter (%s)", state)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultInterviewTimeout
	}

	desc := Description{NodeID: node, At: time.Now()}
	step := func(name string, ask func(context.Context) error) {
		if opts.Progress != nil {
			opts.Progress(name)
		}
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := ask(stepCtx); err != nil {
			desc.Problems = append(desc.Problems, err.Error())
		}
	}

	step("node descriptor", func(ctx context.Context) error {
		nd, err := c.NodeDescriptor(ctx, node)
		if err == nil {
			desc.Node = &nd
		}
		return err
	})
	step("power descriptor", func(ctx context.Context) error {
		pd, err := c.PowerDescriptor(ctx, node)
		if err == nil {
			desc.Power = &pd
		}
		return err
	})

	var endpoints []uint8
	step("active endpoints", func(ctx context.Context) error {
		eps, err := c.ActiveEndpoints(ctx, node)
		endpoints = eps
		return err
	})
	for _, ep := range endpoints {
		step(fmt.Sprintf("endpoint %d descriptor", ep), func(ctx context.Context) error {
			sd, err := c.SimpleDescriptor(ctx, node, ep)
			if err == nil {
				desc.Endpoints = append(desc.Endpoints, sd)
			}
			return err
		})
	}

	// The Basic cluster lives on whichever endpoint implements it, so it can
	// only be read once the endpoints are known. This part is ZCL rather than
	// ZDO, and it is the only part of an interview that produces a name a
	// person would recognise.
	if ep, ok := endpointWithCluster(desc.Endpoints, zcl.ClusterBasic); ok {
		step("manufacturer and model", func(ctx context.Context) error {
			manufacturer, model, err := c.readBasic(ctx, node, ep)
			if err == nil {
				desc.Manufacturer, desc.Model = manufacturer, model
			}
			return err
		})
	}

	c.recordInterview(node, &desc)
	return desc, nil
}

// readBasic reads the manufacturer and model strings from the Basic cluster.
//
// This is a plain attribute read like any other, so it goes through the same
// path a caller would use rather than a second copy of it.
func (c *Coordinator) readBasic(ctx context.Context, node uint16, endpoint uint8) (manufacturer, model string, err error) {
	target := Target{Node: node, Endpoint: endpoint, Cluster: zcl.ClusterBasic}
	values, err := c.ReadAttributes(ctx, target, []uint16{attrManufacturer, attrModel})
	if err != nil {
		return "", "", err
	}
	for _, value := range values {
		text, ok := value.Value.(string)
		if !ok {
			continue
		}
		switch value.ID {
		case attrManufacturer:
			manufacturer = text
		case attrModel:
			model = text
		}
	}
	return manufacturer, model, nil
}

// recordInterview writes what the interview learned to the registry, and fills
// in the device's identity on the description.
//
// Only devices the registry already knows are recorded. An interview can be
// aimed at any address — the coordinator itself, or a device that joined while
// nothing was listening — and inventing a registry entry for one of those
// would fill the registry with records that have no stable identity behind
// them.
func (c *Coordinator) recordInterview(node uint16, desc *Description) {
	device, ok := c.db.ByNodeID(node)
	if !ok {
		return
	}
	if learnedSomething(*desc) {
		applyInterview(device, *desc)
	}

	// Identity is settled last, so a model this interview just learned can
	// name a device that had nothing but an address before it ran.
	if device.Identified() {
		desc.IEEE = device.IEEE
		desc.DeviceName = device.Describe()
		return
	}
	// A record keyed by network address has no identity to report. It can
	// still carry a name, or the model just discovered; what it cannot supply
	// is the placeholder address Describe falls back to.
	if name := device.Describe(); name != device.IEEE {
		desc.DeviceName = name
	}
}

// learnedSomething reports whether an interview came back with anything. Any
// answer at all is also proof the device is alive.
func learnedSomething(desc Description) bool {
	return desc.Node != nil || desc.Power != nil || len(desc.Endpoints) > 0 ||
		desc.Manufacturer != "" || desc.Model != ""
}

// applyInterview merges one interview's findings into a registry record.
func applyInterview(device *store.Device, desc Description) {
	device.LastSeen = desc.At

	if desc.Manufacturer != "" {
		device.Manufacturer = desc.Manufacturer
	}
	if desc.Model != "" {
		device.Model = desc.Model
	}
	if nd := desc.Node; nd != nil {
		device.NodeType = nd.LogicalType
		capability := nd.Capability
		device.Capability = &capability
	}
	if pd := desc.Power; pd != nil {
		device.PowerSource = pd.Source
	}
	if len(desc.Endpoints) > 0 {
		device.Endpoints = nil
		for _, ep := range desc.Endpoints {
			device.Endpoints = append(device.Endpoints, store.Endpoint{
				ID:      ep.ID,
				Profile: ep.Profile,
				Device:  ep.DeviceID,
				Input:   clusterIDs(ep.Input),
				Output:  clusterIDs(ep.Output),
			})
		}
		device.Interviewed = desc.At
	}
}

// endpointWithCluster finds an endpoint implementing the given input cluster.
func endpointWithCluster(endpoints []Endpoint, cluster uint16) (uint8, bool) {
	for _, ep := range endpoints {
		for _, in := range ep.Input {
			if in.ID == cluster {
				return ep.ID, true
			}
		}
	}
	return 0, false
}

func namedClusters(ids []uint16) []Cluster {
	if len(ids) == 0 {
		return nil
	}
	clusters := make([]Cluster, 0, len(ids))
	for _, id := range ids {
		clusters = append(clusters, Cluster{ID: id, Name: zcl.ClusterName(id)})
	}
	return clusters
}

func clusterIDs(clusters []Cluster) []uint16 {
	if len(clusters) == 0 {
		return nil
	}
	ids := make([]uint16, 0, len(clusters))
	for _, cluster := range clusters {
		ids = append(ids, cluster.ID)
	}
	return ids
}
