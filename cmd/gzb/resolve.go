package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// Turning what a person typed into a device to talk to is the same job for
// every command, and it is done here once. What differs is where the device
// list comes from: a one-shot command reads the registry file, because it has
// not opened the port yet and should not for a typo; a session holding the
// port asks the coordinator, whose copy of the registry is the one being
// written to.
//
// Resolution is scoped before it is loose. A command that names a cluster —
// `light` means on/off, `read <dev> temperature` means temperature — looks
// first among the devices whose interview says they carry that cluster, and
// only then across the whole registry. That is what lets "light 1" mean light1
// on a network that also has an "outside #1 thermo": "1" is ambiguous among
// every device and unambiguous among the lights. Nothing that resolved before
// stops resolving, because the whole registry is still tried when the scope
// has no match at all.

// resolution is a device argument, resolved.
type resolution struct {
	node uint16
	// name is what to call the device in output.
	name string
	// device is the registry record, or nil for a bare network address the
	// registry does not know — which is how a device that joined while
	// nothing was listening gets talked to.
	device *zigbee.Device
}

// notFound says which device was missing and how it was asked for, while
// still answering errors.Is(err, zigbee.ErrNoDevice). Wrapping the sentinel
// with %w instead would append "no such device" to a sentence that already
// says so.
type notFound struct{ msg string }

func (e *notFound) Error() string { return e.msg }
func (e *notFound) Unwrap() error { return zigbee.ErrNoDevice }

func noSuchDevice(format string, args ...any) error {
	return &notFound{msg: fmt.Sprintf(format, args...)}
}

// resolveDevice finds the device an argument means. A scope of zero means any
// device; otherwise the devices carrying that input cluster are tried first.
func resolveDevice(devices []zigbee.Device, arg string, scope uint16) (resolution, error) {
	var found zigbee.Device
	err := zigbee.ErrNoDevice
	if scope != 0 {
		found, err = findDevice(withCluster(devices, scope), arg)
	}
	if errors.Is(err, zigbee.ErrNoDevice) {
		found, err = findDevice(devices, arg)
	}
	switch {
	case err == nil:
		return resolution{node: found.NodeID, name: found.Describe(), device: &found}, nil
	case errors.Is(err, zigbee.ErrNoDevice):
		if node, ok := store.ParseNodeID(arg); ok {
			return resolution{node: node, name: arg}, nil
		}
		return resolution{}, resolveError(err)
	default:
		// An ambiguous name is a question for the user, not something to
		// guess past.
		return resolution{}, err
	}
}

// withCluster keeps the devices whose interview says they carry the cluster.
// A device that has not been interviewed is left out: nothing is known about
// it, and a scope is a claim of knowledge.
func withCluster(devices []zigbee.Device, cluster uint16) []zigbee.Device {
	var out []zigbee.Device
	for _, d := range devices {
		if _, ok := d.Endpoint(cluster); ok {
			out = append(out, d)
		}
	}
	return out
}

// findDevice finds the device a query means among the candidates, from an
// IEEE address, a network address in hex, or a name.
//
// Names match loosely, in tiers — exactly, then by prefix, then by suffix,
// then by any substring — and a tier matching several devices is an error
// naming them, never an arbitrary pick. The suffix tier is what makes "1"
// find "light1": the number at the end of a name is what people say when they
// have several of a thing.
//
// Addresses are tried before names and matched exactly, so nothing a user can
// call a device changes what an address means.
func findDevice(devices []zigbee.Device, query string) (zigbee.Device, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return zigbee.Device{}, noSuchDevice("no device given")
	}
	for _, d := range devices {
		if d.IEEE != "" && strings.EqualFold(d.IEEE, q) {
			return d, nil
		}
	}
	if id, ok := store.ParseNodeID(q); ok {
		for _, d := range devices {
			if d.NodeID == id {
				return d, nil
			}
		}
		return zigbee.Device{}, noSuchDevice("no device at network address 0x%04X in the registry", id)
	}

	fold := strings.ToLower(store.NormalizeName(q))
	var tiers [4][]zigbee.Device
	for _, d := range devices {
		if d.Name == "" {
			continue
		}
		switch name := strings.ToLower(d.Name); {
		case name == fold:
			tiers[0] = append(tiers[0], d)
		case strings.HasPrefix(name, fold):
			tiers[1] = append(tiers[1], d)
		case strings.HasSuffix(name, fold):
			tiers[2] = append(tiers[2], d)
		case strings.Contains(name, fold):
			tiers[3] = append(tiers[3], d)
		}
	}
	// An earlier tier beats a later one outright: "bedroom" should find
	// "bedroom lamp" even though "second bedroom lamp" also contains it.
	for _, candidates := range tiers {
		switch len(candidates) {
		case 0:
			continue
		case 1:
			return candidates[0], nil
		default:
			return zigbee.Device{}, ambiguous(q, candidates)
		}
	}
	return zigbee.Device{}, noSuchDevice("no device matching %q in the registry", q)
}

func ambiguous(query string, candidates []zigbee.Device) error {
	names := make([]string, 0, len(candidates))
	for _, d := range candidates {
		names = append(names, strconv.Quote(d.Name))
	}
	return fmt.Errorf("%q matches %d devices (%s); use the full name or an address",
		query, len(candidates), strings.Join(names, ", "))
}

// checkEndpoint rejects an endpoint outside the application range before it
// goes out. Endpoint 0 is ZDO and 241 upwards is Green Power, and addressing
// either with a cluster command is a mistake worth catching here.
func checkEndpoint(endpoint int) error {
	if endpoint != 0 && (endpoint < firstAppEndpoint || endpoint > lastAppEndpoint) {
		return fmt.Errorf("endpoint %d is outside the application range %d-%d (0 is ZDO)", endpoint, firstAppEndpoint, lastAppEndpoint)
	}
	return nil
}

// resolveTarget turns a device argument and a cluster into an address.
func resolveTarget(devices []zigbee.Device, arg string, cluster uint16, endpoint int) (zigbee.Target, string, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return zigbee.Target{}, "", err
	}
	r, err := resolveDevice(devices, arg, cluster)
	if err != nil {
		return zigbee.Target{}, "", err
	}
	// Endpoint 1 is where a device with only one puts everything, and is the
	// right guess for a device the registry has never interviewed.
	target := zigbee.Target{Node: r.node, Cluster: cluster, Endpoint: firstAppEndpoint}
	if r.device != nil {
		if ep, ok := r.device.Endpoint(cluster); ok {
			target.Endpoint = ep
		}
	}
	if endpoint != 0 {
		target.Endpoint = uint8(endpoint)
	}
	return target, r.name, nil
}

// lightClusters are the clusters a light is operated through, in the order
// that decides which endpoint is the light on a device with several.
var lightClusters = []uint16{zigbee.ClusterOnOff, zigbee.ClusterLevelControl, zigbee.ClusterColorControl}

// resolveLight turns a device argument into a light: a node, and the endpoint
// its control clusters are on.
//
// The endpoint is found by asking which one carries the light clusters, in
// order of how definitive they are. A device with several endpoints — a
// two-gang switch, a lamp with a sensor on a second endpoint — is why this is
// worth doing rather than assuming endpoint 1.
func resolveLight(devices []zigbee.Device, arg string, endpoint int) (zigbee.Light, string, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return zigbee.Light{}, "", err
	}
	r, err := resolveDevice(devices, arg, zigbee.ClusterOnOff)
	if err != nil {
		return zigbee.Light{}, "", err
	}
	light := zigbee.Light{Node: r.node, Endpoint: firstAppEndpoint}
	if endpoint != 0 {
		light.Endpoint = uint8(endpoint)
		return light, r.name, nil
	}
	if r.device != nil {
		for _, cluster := range lightClusters {
			if ep, ok := r.device.Endpoint(cluster); ok {
				light.Endpoint = ep
				break
			}
		}
	}
	return light, r.name, nil
}
