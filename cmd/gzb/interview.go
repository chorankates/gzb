package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// An interview asks a device what it is: its node descriptor, which endpoints
// it has, and what each endpoint implements. The queries themselves live in
// the zigbee package; what is left here is resolving which device to ask and
// presenting the answer.

// interviewFlags are the flags `interview` takes.
type interviewFlags struct {
	timeout *time.Duration
	all     *bool
	full    *bool
}

func addInterviewFlags(fs *flag.FlagSet) interviewFlags {
	return interviewFlags{
		timeout: fs.Duration("timeout", zigbee.DefaultInterviewTimeout, "how long to wait for each reply"),
		all:     fs.Bool("all", false, "interview every device the registry has not interviewed yet"),
		full:    fs.Bool("full", false, "ask every question again, of a device already interviewed"),
	}
}

func interviewUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `usage: gzb interview [flags] [device]

Asks a device what it is: node descriptor, power descriptor, endpoints, the
clusters each endpoint implements, and its manufacturer and model. Results are
written to the device registry.

The device may be given as an IEEE address (as `+"`gzb devices`"+` prints it), as a
network address in hex, or as the name `+"`gzb name`"+` gave it. With --all, every
device the registry has no interview for is interviewed in turn; --full asks
again even where there is already an answer.

Each answer is written to the registry as it arrives, and one device failing
does not end an --all run, so an interrupted run keeps what it learned and the
next one picks up where it stopped.

Battery devices sleep between transmissions and only receive while polling
their parent, so an interview can take a while or time out entirely. The best
moment to interview one is right after pairing, while it is still awake.

Because units of one model are built alike, a device that reports a model
already interviewed elsewhere inherits that device's endpoints and clusters
instead of being asked for them again — one round trip instead of five. Such a
record says which device it was copied from; --full asks everything anyway and
replaces it.

flags:
`)
	fs.PrintDefaults()
}

func cmdInterview(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("interview", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	f := addInterviewFlags(fs)
	fs.Usage = func() { interviewUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 || (fs.NArg() == 0 && !*f.all) {
		fs.Usage()
		return flag.ErrHelp
	}

	devices, err := zigbee.LoadDevices(*dbPath)
	if err != nil {
		return err
	}
	targets, err := interviewPlan(g, devices, fs.Args(), *f.all, *f.full)
	if err != nil || len(targets) == 0 {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	return runInterview(ctx, g, coordinator, targets, zigbee.InterviewOptions{Timeout: *f.timeout, Full: *f.full})
}

// interviewPlan resolves what an interview run will ask and says what it is
// skipping. It reports no targets, and no error, when there is nothing left
// to ask: that is a finished job rather than a failure, and it is settled
// before the port is opened so the adapter is not touched for no reason.
func interviewPlan(g *globals, devices []zigbee.Device, args []string, all, full bool) ([]interviewTarget, error) {
	targets, done, err := interviewTargets(devices, args, all, full)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		if g.json {
			return nil, emitJSON([]zigbee.Description{})
		}
		fmt.Printf("All %d device(s) in the registry have been interviewed.\n"+
			"`gzb interview --all --full` asks them again.\n", done)
		return nil, nil
	}
	if done > 0 && !g.json {
		fmt.Printf("%d device(s) already interviewed, skipping; --full asks them again.\n\n", done)
	}
	return targets, nil
}

// runInterview asks each target what it is, through an open coordinator.
func runInterview(ctx context.Context, g *globals, coordinator *zigbee.Coordinator, targets []interviewTarget, opts zigbee.InterviewOptions) error {
	if !g.json {
		// An interview of a sleepy device can wait a long time with nothing
		// to show, so say what it is waiting for.
		opts.Progress = func(step string) { fmt.Printf("  %s...\n", step) }
	}

	results := make([]zigbee.Description, 0, len(targets))
	var failed int
	for _, target := range targets {
		// Ctrl-C ends the run rather than racing through what is left with a
		// cancelled context. Everything answered so far is already saved.
		if ctx.Err() != nil {
			break
		}
		if !g.json {
			fmt.Printf("Interviewing %s (0x%04X)\n", target.name, target.node)
		}
		description, err := coordinator.Interview(ctx, target.node, opts)
		if err != nil {
			// Asking about one device says nothing about the next, so a run
			// over several of them carries on. Asking about one device and
			// only that device has nowhere to carry on to.
			if len(targets) == 1 {
				return err
			}
			failed++
			fmt.Fprintf(os.Stderr, "gzb: %s: %v\n\n", target.name, err)
			continue
		}
		results = append(results, description)
		if !g.json {
			fmt.Println()
			printInterview(description)
			fmt.Println()
		}
	}

	if g.json {
		if err := emitJSON(results); err != nil {
			return err
		}
	} else {
		printInterviewSummary(results, failed)
	}
	if failed > 0 && len(results) == 0 {
		return fmt.Errorf("none of the %d device(s) could be interviewed", failed)
	}
	return nil
}

// printInterviewSummary closes out a run over several devices. A single device
// has just printed its own answer in full and needs no counting up.
func printInterviewSummary(results []zigbee.Description, failed int) {
	if len(results)+failed < 2 {
		return
	}
	// A device that sat through every question is counted with the ones whose
	// interview could not start: both mean there is still nothing recorded and
	// the next run will ask again.
	var answered, inherited int
	for _, description := range results {
		if description.InheritedFrom != "" {
			inherited++
		}
		if learnedSomething(description) {
			answered++
		}
	}
	silent := len(results) - answered + failed

	fmt.Printf("%d device(s) interviewed", answered)
	if inherited > 0 {
		fmt.Printf(", %d of them from an identical device", inherited)
	}
	if silent > 0 {
		fmt.Printf("; %d could not be reached", silent)
	}
	fmt.Println(".")
	if silent > 0 {
		fmt.Println("Re-run to try those again; what succeeded is already recorded.")
	}
}

// learnedSomething reports whether an interview came back with anything at all.
func learnedSomething(description zigbee.Description) bool {
	return description.Node != nil || description.Power != nil ||
		len(description.Endpoints) > 0 || description.Model != "" ||
		description.Manufacturer != ""
}

// interviewTarget is one device to interview.
type interviewTarget struct {
	name string
	node uint16
}

// interviewTargets resolves the arguments to devices, returning what to ask
// and how many devices were left out because they have already answered.
func interviewTargets(devices []zigbee.Device, args []string, all, full bool) ([]interviewTarget, int, error) {
	if all {
		if len(devices) == 0 {
			return nil, 0, fmt.Errorf("no devices in the registry; run `gzb join 90` and pair one")
		}
		targets := make([]interviewTarget, 0, len(devices))
		var done int
		for _, d := range devices {
			// What an interview asks about does not change, so --all means
			// every device still without an answer. Re-asking is what --full
			// is for, and on sleepy devices the difference is most of an hour.
			if !full && !d.Interviewed.IsZero() {
				done++
				continue
			}
			targets = append(targets, interviewTarget{name: d.Describe(), node: d.NodeID})
		}
		return targets, done, nil
	}

	// A device named outright is asked whatever the registry already holds:
	// naming it is the request. A bare hex address the registry does not know
	// is asked too, which is how a device that joined while nothing was
	// listening gets interviewed.
	r, err := resolveDevice(devices, args[0], 0)
	if err != nil {
		return nil, 0, err
	}
	return []interviewTarget{{name: r.name, node: r.node}}, 0, nil
}

func printInterview(description zigbee.Description) {
	if description.Manufacturer != "" || description.Model != "" {
		fmt.Printf("  %s %s\n", description.Manufacturer, description.Model)
	}
	inherited := description.InheritedFrom != ""
	if inherited {
		from := description.InheritedFromName
		if from == "" {
			from = description.InheritedFrom
		}
		fmt.Printf("  Same model as %s, so everything below is that device's answers\n", from)
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
		// The registry does not keep the manufacturer code, so an inherited
		// descriptor has no honest value to print for it.
		if !inherited {
			fmt.Printf("  Manufacturer code 0x%04X\n", nd.ManufacturerCode)
		}
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
