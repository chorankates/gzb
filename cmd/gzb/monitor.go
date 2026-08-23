package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/conor/gzb/internal/ezsp"
	"github.com/conor/gzb/internal/store"
	"github.com/conor/gzb/internal/zcl"
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
		return fmt.Errorf("no network on this adapter (%s); form one first", state)
	}

	msgs, cancel := conn.Subscribe(func(m ezsp.Message) bool {
		return m.Callback && m.ID == ezsp.FrameIncomingMessage
	}, 128)
	defer cancel()

	if !g.json {
		fmt.Printf("Listening on %s. Ctrl-C to stop.\n\n", g.port)
	}

	// The registry is saved on the way out rather than per message, so a busy
	// network does not turn into a write per report.
	defer func() {
		if err := db.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "gzb: saving registry: %v\n", err)
		}
	}()

	var stop <-chan time.Time
	if *duration > 0 {
		t := time.NewTimer(*duration)
		defer t.Stop()
		stop = t.C
	}

	enc := json.NewEncoder(os.Stdout)
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return nil
			}
			handleReport(ctx, conn, db, g, enc, m, *raw)
		case <-stop:
			return nil
		case <-ctx.Done():
			if !g.json {
				fmt.Print("\nStopped.\n")
			}
			return nil
		}
	}
}

// reportLine is one decoded reading, for JSON output.
type reportLine struct {
	At   time.Time `json:"at"`
	IEEE string    `json:"ieee,omitempty"`
	// Device is the name the device was given, if it has one. The identity is
	// still the IEEE address; this is here so a stream of readings can be read
	// without a second lookup.
	Device  string `json:"device,omitempty"`
	NodeID  uint16 `json:"node_id"`
	Cluster string    `json:"cluster"`
	Name    string    `json:"name"`
	Value   float64   `json:"value"`
	Unit    string    `json:"unit,omitempty"`
	LQI     uint8     `json:"lqi"`
	RSSI    int8      `json:"rssi"`
}

func handleReport(ctx context.Context, conn *ezsp.Conn, db *store.Store, g *globals, enc *json.Encoder, m ezsp.Message, raw bool) {
	msg, err := ezsp.DecodeIncomingMessage(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		return
	}
	// ZDO traffic is addressing, not data, and the NCP answers it for us.
	if msg.APS.Profile == ezsp.ProfileZDO {
		return
	}

	now := time.Now()
	// An unknown sender can only be identified by the address it transmitted
	// from; a known one is called whatever the registry can call it, which for
	// a named device is the name a person chose.
	name := fmt.Sprintf("0x%04X", msg.Sender)
	var device *store.Device
	if d, ok := db.ByNodeID(msg.Sender); ok {
		device = d
		device.LastSeen = now
		name = d.Describe()
	}

	frame, err := zcl.Decode(msg.Payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		return
	}

	// Devices read the time from us; answer before anything else.
	if serveTimeRead(ctx, conn, msg, frame) {
		if raw && !g.json {
			fmt.Printf("%s  %-24s %-12s answered time read\n", now.Format("15:04:05"), name, "time")
		}
		return
	}

	attrs, err := frame.Attributes()
	if err != nil {
		// Not an attribute report: a command, a read request, a response.
		// Worth showing only when asked, since devices chatter.
		if raw && !g.json {
			fmt.Printf("%s  %-24s %-12s %s\n", now.Format("15:04:05"), name,
				zcl.ClusterName(msg.APS.Cluster), zcl.CommandName(frame.Command))
		}
		return
	}

	for _, a := range attrs {
		reading, ok := zcl.Interpret(msg.APS.Cluster, a)
		if !ok {
			if raw && !g.json {
				fmt.Printf("%s  %-24s %-12s %s = %v\n", now.Format("15:04:05"), name,
					zcl.ClusterName(msg.APS.Cluster), zcl.AttributeName(msg.APS.Cluster, a.ID), a.Value)
			}
			continue
		}
		if device != nil {
			device.Record(reading.Name, reading.Value, reading.Unit, now)
		}
		if g.json {
			line := reportLine{
				At: now, NodeID: msg.Sender, Cluster: zcl.ClusterName(msg.APS.Cluster),
				Name: reading.Name, Value: reading.Value, Unit: reading.Unit,
				LQI: msg.LQI, RSSI: msg.RSSI,
			}
			if device != nil {
				line.IEEE, line.Device = device.IEEE, device.Name
			}
			if err := enc.Encode(line); err != nil {
				fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
			}
			continue
		}
		// The reading's own name already says what this is, so the cluster it
		// arrived on would only repeat it.
		fmt.Printf("%s  %-24s %-12s %8.2f %-3s  lqi %3d  rssi %d\n",
			now.Format("15:04:05"), name, reading.Name,
			reading.Value, reading.Unit, msg.LQI, msg.RSSI)
	}
}
