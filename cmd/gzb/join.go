package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
func cmdJoin(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	verbose := fs.Bool("verbose", false, "decode and log every frame received during the window")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb join [seconds]

Opens the network to new devices and watches them arrive, recording each one
in the local device registry. Defaults to 60 seconds.

Put the device into pairing mode once this reports the network is open.
The window closes automatically when the time expires or on Ctrl-C.

With --json, each event is emitted as one JSON object per line.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	seconds := uint64(60)
	if fs.NArg() > 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	if fs.NArg() == 1 {
		var err error
		seconds, err = strconv.ParseUint(fs.Arg(0), 10, 8)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", fs.Arg(0), err)
		}
	}
	if seconds == 0 {
		return fmt.Errorf("a join window of 0 seconds would close the network immediately; use `gzb permit-join 0` for that")
	}

	// The watching, recording and window bookkeeping all live in the public
	// zigbee package, so an application pairing through it and this command
	// see exactly the same events.
	opts := coordinatorOptions(g, *dbPath)
	start := time.Now()
	if *verbose {
		// Everything the device sends after joining, for working out why it
		// might not stay. The readings loop decodes it, answers Time-cluster
		// reads, and hands the rest over as unhandled events. Note that ZDO
		// descriptor requests are answered by the NCP itself and never reach
		// the host, so silence here is expected until the device speaks ZCL.
		opts.OnUnhandled = func(ev zigbee.Event) {
			fmt.Printf("[%6.1fs] frame         0x%04X  %s  %s\n",
				time.Since(start).Seconds(), ev.NodeID, ev.Cluster, ev.Description)
		}
	}

	c, err := zigbee.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer c.Close()

	// Subscribe before opening the window so a fast device cannot join in the
	// gap between the two.
	events, errs, cancelWatch := c.Joins(64)
	defer cancelWatch()

	var readings <-chan zigbee.Reading
	var readErrs <-chan error
	if *verbose {
		readings, readErrs = c.Readings(ctx)
	}

	if err := c.PermitJoin(ctx, time.Duration(seconds)*time.Second); err != nil {
		return err
	}
	// However this function exits, the window must not be left open.
	defer closeJoinWindow(c)

	if !g.json {
		fmt.Printf("Network open for %d seconds. Put the device into pairing mode now.\n", seconds)
		fmt.Printf("Registry: %s\n\n", registryPath(*dbPath))
	}

	start = time.Now()
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()

	var seen, fresh int
	var opened bool
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
		case reading, ok := <-readings:
			if !ok {
				readings = nil
				continue
			}
			fmt.Printf("[%6.1fs] reading       0x%04X  %s %.2f %s\n",
				time.Since(start).Seconds(), reading.NodeID, reading.Capability, reading.Value, reading.Unit)
		case err, ok := <-readErrs:
			if !ok {
				readErrs = nil
				continue
			}
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		case <-deadline.C:
			break watch
		case <-ctx.Done():
			if !g.json {
				fmt.Print("\nInterrupted.\n")
			}
			break watch
		}
	}

	if g.json {
		return nil
	}
	fmt.Printf("\nWindow closed. %d event(s), %d new device(s).\n", seen, fresh)
	if seen > 0 {
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

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	devices := db.List()

	if g.json {
		return emitJSON(devices)
	}
	if len(devices) == 0 {
		fmt.Printf("No devices recorded in %s.\n\nRun `gzb join 60` and pair a device.\n", db.Path())
		return nil
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
		fmt.Println(deviceHeading(d))
		fmt.Printf("  %s, last seen %s\n", kind, d.LastSeen.Format(time.RFC3339))
		if d.InheritedFrom != "" {
			// A record deduced from an identical device must not read like one
			// the device gave itself.
			from := d.InheritedFrom
			if peer, ok := db.Get(d.InheritedFrom); ok {
				from = peer.Describe()
			}
			fmt.Printf("  endpoints inherited from %s, which has the same model\n", from)
		}
		for _, name := range d.ReadingNames() {
			r := d.Readings[name]
			fmt.Printf("  %-14s %-12s (%s)\n", name, r, r.At.Format(time.RFC3339))
		}
		fmt.Println()
	}
	fmt.Printf("%d device(s) in %s\n", len(devices), db.Path())
	if unnamed > 0 {
		fmt.Printf("%d unnamed. `gzb name <device> living room thermo` makes the rest of the output readable.\n", unnamed)
	}
	return nil
}

// deviceHeading is the first line of a device listing: what to call the device,
// then the addresses that identify it.
//
// The addresses stay even when there is a name, because a name is a convenience
// and the IEEE address is the identity — anything that has to be matched
// against another tool's output is matched on the address.
func deviceHeading(d *store.Device) string {
	if name := d.Describe(); name != d.IEEE {
		return fmt.Sprintf("%s  %s  %s", name, d.NodeIDHex(), d.IEEE)
	}
	return fmt.Sprintf("%s  %s", d.IEEE, d.NodeIDHex())
}
