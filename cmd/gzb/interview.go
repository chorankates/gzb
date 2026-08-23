package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// An interview asks a device what it is: its node descriptor, which endpoints
// it has, and what each endpoint implements. The queries themselves live in
// the zigbee package; what is left here is resolving which device to ask and
// presenting the answer.

func cmdInterview(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("interview", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	timeout := fs.Duration("timeout", zigbee.DefaultInterviewTimeout, "how long to wait for each reply")
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

	targets, err := interviewTargets(*dbPath, fs.Args(), *all)
	if err != nil {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	opts := zigbee.InterviewOptions{Timeout: *timeout}
	if !g.json {
		// An interview of a sleepy device can wait a long time with nothing
		// to show, so say what it is waiting for.
		opts.Progress = func(step string) { fmt.Printf("  %s...\n", step) }
	}

	results := make([]zigbee.Description, 0, len(targets))
	for _, target := range targets {
		if !g.json {
			fmt.Printf("Interviewing %s (0x%04X)\n", target.name, target.node)
		}
		description, err := coordinator.Interview(ctx, target.node, opts)
		if err != nil {
			return err
		}
		results = append(results, description)
		if !g.json {
			fmt.Println()
			printInterview(description)
			fmt.Println()
		}
	}

	if g.json {
		return emitJSON(results)
	}
	return nil
}

// interviewTarget is one device to interview.
type interviewTarget struct {
	name string
	node uint16
}

// interviewTargets resolves command-line arguments to devices.
//
// This reads the registry but never writes it: the coordinator keeps its own
// copy and saves what the interview learns, so a second writer here would
// overwrite those results.
func interviewTargets(dbPath string, args []string, all bool) ([]interviewTarget, error) {
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}

	if all {
		devices := db.List()
		if len(devices) == 0 {
			return nil, fmt.Errorf("no devices in the registry; run `gzb join 90` and pair one")
		}
		targets := make([]interviewTarget, 0, len(devices))
		for _, d := range devices {
			targets = append(targets, interviewTarget{name: d.Describe(), node: d.NodeID})
		}
		return targets, nil
	}

	arg := args[0]
	d, err := db.Resolve(arg)
	if err == nil {
		return []interviewTarget{{name: d.Describe(), node: d.NodeID}}, nil
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

func printInterview(description zigbee.Description) {
	if description.Manufacturer != "" || description.Model != "" {
		fmt.Printf("  %s %s\n", description.Manufacturer, description.Model)
	}
	if nd := description.Node; nd != nil {
		power := "battery"
		if nd.Mains {
			power = "mains"
		}
		listening := "always listening"
		if nd.Sleepy {
			listening = "sleepy"
		}
		fmt.Printf("  %s, %s, %s\n", nd.LogicalType, power, listening)
		fmt.Printf("  Manufacturer code 0x%04X\n", nd.ManufacturerCode)
	}
	if pd := description.Power; pd != nil {
		fmt.Printf("  Powered by %s\n", pd.Source)
	}

	for _, ep := range description.Endpoints {
		fmt.Printf("\n  Endpoint %d  profile 0x%04X  device 0x%04X\n", ep.ID, ep.Profile, ep.DeviceID)
		if len(ep.Input) > 0 {
			fmt.Printf("    in   %s\n", clusterNames(ep.Input))
		}
		if len(ep.Output) > 0 {
			fmt.Printf("    out  %s\n", clusterNames(ep.Output))
		}
	}

	for _, problem := range description.Problems {
		fmt.Printf("  ! %s\n", problem)
	}
}

// clusterNames renders a cluster list readably.
func clusterNames(clusters []zigbee.Cluster) string {
	names := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		names = append(names, cluster.Name)
	}
	return strings.Join(names, ", ")
}
