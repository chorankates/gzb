package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// cmdMonitor prints network traffic as it arrives.
//
// Most of what a Zigbee network carries is unsolicited: sensors report when a
// value changes, and sleepy devices are unreachable the rest of the time. So
// listening is not a debugging convenience, it is the primary way to get data
// out of a battery device.
func cmdMonitor(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	duration := fs.Duration("for", 0, "stop after this long (default: run until Ctrl-C)")
	raw := fs.Bool("raw", false, "also print frames that carry no readable attributes")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb monitor [flags]

Listens for reports from devices on the network and prints them as readings.
Runs until Ctrl-C unless --for is given.

Readings are recorded in the device registry as they arrive, so `+"`gzb devices`"+`
can show the last known value of a sensor that is currently asleep.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := coordinatorOptions(g, *dbPath)
	if *raw && !g.json {
		opts.OnUnhandled = func(event zigbee.Event) {
			name := event.DeviceName
			if name == "" {
				name = event.IEEE
			}
			if name == "" {
				name = fmt.Sprintf("0x%04X", event.NodeID)
			}
			fmt.Printf("%s  %-24s %-12s %s\n",
				event.At.Format("15:04:05"), name, event.Cluster, event.Description)
		}
	}

	coordinator, err := zigbee.Open(ctx, opts)
	if err != nil {
		return err
	}
	defer coordinator.Close()

	if !g.json {
		fmt.Printf("Listening on %s. Ctrl-C to stop.\n\n", g.port)
	}

	var stop <-chan time.Time
	if *duration > 0 {
		t := time.NewTimer(*duration)
		defer t.Stop()
		stop = t.C
	}

	enc := json.NewEncoder(os.Stdout)
	readings, errs := coordinator.Readings(ctx)
	var terminalErr error
	for readings != nil || errs != nil {
		select {
		case reading, ok := <-readings:
			if !ok {
				readings = nil
				continue
			}
			if g.json {
				if err := enc.Encode(reading); err != nil {
					return err
				}
				continue
			}
			fmt.Println(formatReading(reading))
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			terminalErr = err
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		case <-stop:
			return nil
		case <-ctx.Done():
			if !g.json {
				fmt.Print("\nStopped.\n")
			}
			return nil
		}
	}
	return terminalErr
}

// formatReading renders one report as a monitor line: when, who, what.
func formatReading(reading zigbee.Reading) string {
	name := reading.DeviceName
	if name == "" {
		name = reading.IEEE
	}
	if name == "" {
		name = fmt.Sprintf("0x%04X", reading.NodeID)
	}
	return fmt.Sprintf("%s  %-24s %-12s %8.2f %-3s  lqi %3d  rssi %d",
		reading.At.Format("15:04:05"), name, reading.Capability,
		reading.Value, reading.Unit, reading.LQI, reading.RSSI)
}
