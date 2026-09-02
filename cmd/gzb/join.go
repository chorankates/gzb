package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// cmdJoin opens the network and watches devices arrive.
//
// This differs from permit-join, which opens the window and exits: here the
// session stays up for the whole window so the join callbacks can be decoded
// and recorded. A device that joins while nothing is listening is still joined
// to the network, but nothing local knows it exists.
func joinUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `usage: gzb join [seconds]

Opens the network to new devices and watches them arrive, recording each one
in the local device registry. Defaults to 60 seconds.

Put the device into pairing mode once this reports the network is open.
The window closes automatically when the time expires or on Ctrl-C.

With --json, each event is emitted as one JSON object per line.

flags:
`)
	fs.PrintDefaults()
}

func cmdJoin(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	verbose := fs.Bool("verbose", false, "decode and log every frame received during the window")
	fs.Usage = func() { joinUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	seconds, err := parseJoinWindow(fs.Args())
	if err != nil {
		return err
	}

	// The watching, recording and window bookkeeping all live in the public
	// zigbee package, so an application pairing through it and this command
	// see exactly the same events.
	opts := coordinatorOptions(g, *dbPath)
	var feed joinFeed
	if *verbose {
		frames := make(chan zigbee.Event, 64)
		opts.OnUnhandled = func(ev zigbee.Event) {
			select {
			case frames <- ev:
			default:
			}
		}
		feed.frames = frames
	}

	c, err := zigbee.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer c.Close()

	if *verbose {
		feed.readings, feed.readErrs = c.Readings(ctx)
	}
	return runJoin(ctx, g, c, seconds, feed, registryPath(*dbPath))
}

// parseJoinWindow reads how long the network is to be open for, in whole
// seconds, as the protocol carries it.
func parseJoinWindow(args []string) (uint64, error) {
	seconds := uint64(60)
	if len(args) == 1 {
		var err error
		seconds, err = strconv.ParseUint(args[0], 10, 8)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", args[0], err)
		}
	}
	if seconds == 0 {
		return 0, fmt.Errorf("a join window of 0 seconds would close the network immediately; use `gzb permit-join 0` for that")
	}
	return seconds, nil
}

// joinFeed is what a join run prints besides its own events: everything the
// device sends after joining, when --verbose asks for it, for working out why
// it might not stay. The readings loop decodes that traffic, answers
// Time-cluster reads, and hands the rest over as unhandled events. ZDO
// descriptor requests are answered by the NCP itself and never reach the host,
// so silence is expected until the device speaks ZCL.
//
// Any of the channels may be nil, and a nil channel is simply never read.
type joinFeed struct {
	readings <-chan zigbee.Reading
	readErrs <-chan error
	frames   <-chan zigbee.Event
}

// runJoin opens the network for a window and watches devices arrive, through
// an open coordinator. It is the half of the command a session shares with
// the command line, and at a prompt it is the more useful half: a device that
// has just joined is awake, and the interview it needs is one line away.
func runJoin(ctx context.Context, g *globals, c *zigbee.Coordinator, seconds uint64, feed joinFeed, registry string) error {
	// Subscribe before opening the window so a fast device cannot join in the
	// gap between the two.
	events, errs, cancelWatch := c.Joins(64)
	defer cancelWatch()

	if err := c.PermitJoin(ctx, time.Duration(seconds)*time.Second); err != nil {
		return err
	}
	// However this function exits, the window must not be left open.
	defer closeJoinWindow(c)

	if !g.json {
		fmt.Printf("Network open for %d seconds. Put the device into pairing mode now.\n", seconds)
		fmt.Printf("Registry: %s\n\n", registry)
	}

	start := time.Now()
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()

	var seen, fresh int
	var opened, interrupted bool
	enc := json.NewEncoder(os.Stdout)

watch:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break watch
			}
			// The stack announces the window opening and closing in its own
			// right. Watching that turns "nothing joined" into two
			// distinguishable cases: the window never opened, or it opened
			// and no device called.
			if ev.Kind == zigbee.EventWindowOpened || ev.Kind == zigbee.EventWindowClosed {
				if ev.Kind == zigbee.EventWindowOpened {
					opened = true
				}
				if g.json {
					if err := enc.Encode(ev); err != nil {
						return err
					}
				} else {
					fmt.Printf("[%6.1fs] stack         %s\n", time.Since(start).Seconds(), ev.Description)
				}
				continue
			}
			seen++
			if ev.New {
				fresh++
			}
			if g.json {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			} else {
				fmt.Println(formatEvent(start, ev))
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// A callback we could not decode is worth reporting but not worth
			// ending the pairing window over.
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		case reading, ok := <-feed.readings:
			if !ok {
				feed.readings = nil
				continue
			}
			fmt.Printf("[%6.1fs] reading       0x%04X  %s %.2f %s\n",
				time.Since(start).Seconds(), reading.NodeID, reading.Capability, reading.Value, reading.Unit)
		case err, ok := <-feed.readErrs:
			if !ok {
				feed.readErrs = nil
				continue
			}
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		case ev, ok := <-feed.frames:
			if !ok {
				feed.frames = nil
				continue
			}
			fmt.Printf("[%6.1fs] frame         0x%04X  %s  %s\n",
				time.Since(start).Seconds(), ev.NodeID, ev.Cluster, ev.Description)
		case <-deadline.C:
			break watch
		case <-ctx.Done():
			if !g.json {
				fmt.Print("\nInterrupted.\n")
			}
			interrupted = true
			break watch
		}
	}

	if g.json {
		return nil
	}
	fmt.Printf("\nWindow closed. %d event(s), %d new device(s).\n", seen, fresh)
	// A window cut short says nothing about the device: the advice below is
	// about a window that stayed open and heard nothing.
	if seen > 0 || interrupted {
		return nil
	}

	if !opened {
		fmt.Print("\nThe stack never reported the network opening, so the fault is on this\n" +
			"side rather than the device's. Re-run with --trace to see the exchange.\n")
		return nil
	}
	fmt.Print("\nThe coordinator was open and accepting joins for the whole window, and\n" +
		"no device transmitted. That points at the device rather than the adapter:\n" +
		"  - confirm it is Zigbee; a Wi-Fi device will never appear here\n" +
		"  - hold the pairing button until the LED blinks, then re-run this\n" +
		"  - factory reset it if it was previously paired to another hub\n" +
		"  - keep it within a couple of metres for the first join\n")
	return nil
}

// closeJoinWindow shuts the network again. It uses its own context because the
// caller's is often already cancelled — Ctrl-C is the usual way out of a watch,
// and that must not leave the network standing open.
func closeJoinWindow(c *zigbee.Coordinator) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.PermitJoin(ctx, 0); err != nil {
		fmt.Fprintf(os.Stderr, "gzb: warning: could not close the join window: %v\n", err)
	}
}

// registryPath names the registry the coordinator was opened with, for output.
func registryPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return store.DefaultPath()
}

// formatEvent renders one device join or leave event.
func formatEvent(start time.Time, ev zigbee.JoinEvent) string {
	line := fmt.Sprintf("[%6.1fs] %-13s 0x%04X  %s", time.Since(start).Seconds(), ev.Kind, ev.NodeID, ev.IEEE)
	// A returning device is recognised by name, which is the difference
	// between "something joined" and "the living room thermo is back".
	if ev.DeviceName != "" {
		line += "  " + ev.DeviceName
	}
	if ev.Description != "" {
		line += "  " + ev.Description
	}
	return line
}

// cmdDevices lists what the registry knows. It takes a context only to match
// the shape of every other subcommand; it touches no hardware.
func cmdDevices(_ context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb devices

Lists the devices recorded in the local registry. Reads no hardware.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if g.json {
		// The machine-readable form is the registry record itself, with
		// everything the file holds.
		db, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		return emitJSON(db.List())
	}

	devices, err := zigbee.LoadDevices(*dbPath)
	if err != nil {
		return err
	}
	printDevices(devices, registryPath(*dbPath))
	return nil
}

// printDevices lists what is known about each device: its identity, when it
// was last heard from, and the last thing it said.
func printDevices(devices []zigbee.Device, path string) {
	if len(devices) == 0 {
		fmt.Printf("No devices recorded in %s.\n\nRun `gzb join 60` and pair a device.\n", path)
		return
	}

	var unnamed int
	for _, d := range devices {
		kind := d.NodeType
		if kind == "" {
			kind = "unknown type"
		}
		if d.Name == "" {
			unnamed++
		}
		fmt.Println(heading(d.Describe(), d.IEEE, d.NodeID))
		fmt.Printf("  %s, last seen %s\n", kind, d.LastSeen.Format(time.RFC3339))
		if d.InheritedFrom != "" {
			// A record deduced from an identical device must not read like one
			// the device gave itself.
			from := d.InheritedFrom
			for _, peer := range devices {
				if peer.IEEE == d.InheritedFrom {
					from = peer.Describe()
					break
				}
			}
			fmt.Printf("  endpoints inherited from %s, which has the same model\n", from)
		}
		for _, name := range slices.Sorted(maps.Keys(d.Latest)) {
			r := d.Latest[name]
			fmt.Printf("  %-14s %-12s (%s)\n", name, formatLatest(r), r.At.Format(time.RFC3339))
		}
		fmt.Println()
	}
	fmt.Printf("%d device(s) in %s\n", len(devices), path)
	if unnamed > 0 {
		fmt.Printf("%d unnamed. `gzb name <device> living room thermo` makes the rest of the output readable.\n", unnamed)
	}
}

// formatLatest renders the last known value of one quantity.
func formatLatest(r zigbee.LatestReading) string {
	if r.Unit == "" {
		return fmt.Sprintf("%g", r.Value)
	}
	return fmt.Sprintf("%.2f %s", r.Value, r.Unit)
}

// deviceHeading is the first line of a device listing: what to call the device,
// then the addresses that identify it.
func deviceHeading(d *store.Device) string {
	return heading(d.Describe(), d.IEEE, d.NodeID)
}

// heading is the first line of a device listing: what to call the device,
// then the addresses that identify it.
//
// The addresses stay even when there is a name, because a name is a convenience
// and the IEEE address is the identity — anything that has to be matched
// against another tool's output is matched on the address. A device known
// only by network address has nothing more to add than the address itself.
func heading(name, ieee string, node uint16) string {
	hex := fmt.Sprintf("0x%04X", node)
	switch {
	case ieee == "":
		return fmt.Sprintf("%s  %s", name, hex)
	case name != ieee:
		return fmt.Sprintf("%s  %s  %s", name, hex, ieee)
	default:
		return fmt.Sprintf("%s  %s", ieee, hex)
	}
}
