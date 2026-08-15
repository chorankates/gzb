package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/conor/gzb/internal/ezsp"
	"github.com/conor/gzb/internal/store"
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
	verbose := fs.Bool("verbose", false, "log every APS frame received during the window")
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

	db, err := store.Open(*dbPath)
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
		return fmt.Errorf("no network on this adapter (%s); run `gzb network form --confirm` first", state)
	}

	// permitJoining opens the time window; the trust-centre policy decides
	// what happens to a device that arrives during it. Both are needed, and
	// the policy does not survive the NCP reset that connecting performs.
	if err := conn.AllowJoins(ctx); err != nil {
		return fmt.Errorf("configuring trust centre to accept joins: %w", err)
	}

	// Subscribe before opening the window so a fast device cannot join in the
	// gap between the two.
	events, errs, cancelWatch := conn.WatchJoins(64)
	defer cancelWatch()

	// The stack announces the window opening and closing in its own right.
	// Watching that turns "nothing joined" into two distinguishable cases:
	// the window never opened, or it opened and no device called.
	stackMsgs, cancelStack := conn.Subscribe(func(m ezsp.Message) bool {
		return m.Callback && m.ID == ezsp.FrameStackStatusHandler
	}, 8)
	defer cancelStack()

	// Everything the device sends after joining, for working out why it might
	// not stay. Note that ZDO descriptor requests are answered by the NCP
	// itself and never reach the host, so silence here is expected until the
	// device starts speaking ZCL.
	apsMsgs := make(<-chan ezsp.Message)
	cancelAPS := func() {}
	if *verbose {
		apsMsgs, cancelAPS = conn.Subscribe(func(m ezsp.Message) bool {
			return m.Callback && m.ID == ezsp.FrameIncomingMessage
		}, 64)
	}
	defer cancelAPS()

	if err := conn.PermitJoining(ctx, uint8(seconds)); err != nil {
		return err
	}
	// However this function exits, the window must not be left open.
	defer closeJoinWindow(conn)

	if !g.json {
		fmt.Printf("Network open for %d seconds. Put the device into pairing mode now.\n", seconds)
		fmt.Printf("Registry: %s\n\n", db.Path())
	}

	start := time.Now()
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()

	var seen, fresh int
	var opened bool
	enc := json.NewEncoder(os.Stdout)

watch:
	for {
		select {
		case m := <-stackMsgs:
			status, err := ezsp.StackStatus(m)
			if err != nil {
				continue
			}
			if status == ezsp.StatusNetworkOpened {
				opened = true
			}
			if !g.json {
				fmt.Printf("[%6.1fs] stack         %s\n", time.Since(start).Seconds(), status)
			}
		case m := <-apsMsgs:
			msg, err := ezsp.DecodeIncomingMessage(m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
				continue
			}
			fmt.Printf("[%6.1fs] aps           0x%04X  profile 0x%04X cluster 0x%04X ep %d->%d  lqi %d rssi %d  % X\n",
				time.Since(start).Seconds(), msg.Sender, msg.APS.Profile, msg.APS.Cluster,
				msg.APS.SourceEP, msg.APS.DestEP, msg.LQI, msg.RSSI, msg.Payload)
		case ev := <-events:
			seen++
			if recordEvent(db, ev) {
				fresh++
			}
			if g.json {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			} else {
				fmt.Println(formatEvent(start, ev))
			}
		case err := <-errs:
			// A callback we could not decode is worth reporting but not worth
			// ending the pairing window over.
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

	if err := db.Save(); err != nil {
		return err
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
func closeJoinWindow(conn *ezsp.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.PermitJoining(ctx, 0); err != nil {
		fmt.Fprintf(os.Stderr, "gzb: warning: could not close the join window: %v\n", err)
	}
}

// recordEvent merges an event into the registry, reporting whether the device
// was previously unknown.
func recordEvent(db *store.Store, ev ezsp.JoinEvent) bool {
	if ev.Leaving {
		return false
	}
	d, isNew := db.Observe(ev.IEEE.String(), ev.NodeID, ev.At)
	if ev.NodeType != nil {
		d.NodeType = ev.NodeType.String()
	}
	if ev.Parent != nil {
		p := *ev.Parent
		d.Parent = &p
	}
	if ev.Capability != nil {
		c := uint8(*ev.Capability)
		d.Capability = &c
	}
	return isNew
}

func formatEvent(start time.Time, ev ezsp.JoinEvent) string {
	line := fmt.Sprintf("[%6.1fs] %-13s 0x%04X  %s", time.Since(start).Seconds(), ev.Kind, ev.NodeID, ev.IEEE)

	var detail string
	switch {
	case ev.Update != nil && ev.Decision != nil:
		detail = fmt.Sprintf("%s, %s", *ev.Update, *ev.Decision)
		if ev.Parent != nil {
			detail += fmt.Sprintf(", via 0x%04X", *ev.Parent)
		}
	case ev.NodeType != nil:
		detail = ev.NodeType.String()
		if ev.Leaving {
			detail += ", left"
		}
	case ev.Capability != nil:
		detail = ev.Capability.String()
	}
	if detail != "" {
		line += "  " + detail
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

	for _, d := range devices {
		kind := d.NodeType
		if kind == "" {
			kind = "unknown type"
		}
		label := d.Label
		if label == "" {
			label = d.IEEE
		}
		fmt.Printf("%s  %s\n", label, d.NodeIDHex())
		fmt.Printf("  %s, last seen %s\n", kind, d.LastSeen.Format(time.RFC3339))
		for _, name := range d.ReadingNames() {
			r := d.Readings[name]
			fmt.Printf("  %-14s %-12s (%s)\n", name, r, r.At.Format(time.RFC3339))
		}
		fmt.Println()
	}
	fmt.Printf("%d device(s) in %s\n", len(devices), db.Path())
	return nil
}
