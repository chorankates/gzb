// Command gzb is a command-line client for Silicon Labs EFR32 Zigbee
// coordinators speaking EZSP over ASH, such as the Sonoff ZBDongle-E.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chorankates/gzb/internal/ash"
	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/zigbee"
)

const defaultPort = "/dev/ttyUSB0"

// globals holds flags shared by every subcommand.
type globals struct {
	port  string
	baud  int
	json  bool
	trace bool
	ashv  bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var g globals
	fs := flag.NewFlagSet("gzb", flag.ContinueOnError)
	fs.StringVar(&g.port, "port", envOr("GZB_PORT", defaultPort), "serial device of the coordinator")
	fs.IntVar(&g.baud, "baud", ezsp.DefaultBaud, "serial baud rate")
	fs.BoolVar(&g.json, "json", false, "emit machine-readable JSON instead of text")
	fs.BoolVar(&g.trace, "trace", false, "log decoded EZSP frames to stderr")
	fs.BoolVar(&g.ashv, "trace-ash", false, "log raw ASH frames to stderr")
	fs.Usage = func() { usage(fs) }

	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		usage(fs)
		return flag.ErrHelp
	}

	// Ctrl-C must close the port cleanly so the next run can reopen it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd := rest[0]; cmd {
	case "probe":
		return cmdProbe(ctx, &g, rest[1:])
	case "network":
		return cmdNetwork(ctx, &g, rest[1:])
	case "permit-join":
		return cmdPermitJoin(ctx, &g, rest[1:])
	case "join":
		return cmdJoin(ctx, &g, rest[1:])
	case "devices":
		return cmdDevices(ctx, &g, rest[1:])
	case "name":
		return cmdName(ctx, &g, rest[1:])
	case "config":
		return cmdConfig(ctx, &g, rest[1:])
	case "monitor":
		return cmdMonitor(ctx, &g, rest[1:])
	case "interview":
		return cmdInterview(ctx, &g, rest[1:])
	case "read":
		return cmdRead(ctx, &g, rest[1:])
	case "write":
		return cmdWrite(ctx, &g, rest[1:])
	case "light":
		return cmdLight(ctx, &g, rest[1:])
	case "reporting":
		return cmdReporting(ctx, &g, rest[1:])
	case "help", "-h", "--help":
		usage(fs)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try `gzb help`)", cmd)
	}
}

func usage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `gzb — a Zigbee coordinator client for EmberZNet (EZSP) adapters

usage: gzb [global flags] <command> [flags]

commands:
  probe             inspect the adapter: firmware, network state, radio parameters
  network show      same as probe
  network form      create a new network (destructive, needs --confirm)
  network leave     tear down the current network (destructive, needs --confirm)
  permit-join <s>   open the network to new devices for s seconds, then exit
  join [s]          open the network and watch devices arrive (default 60s)
  devices           list devices in the local registry
  name <dev> <name> call a device something human, e.g. "living room thermo"
  interview <dev>   ask a device what it is: endpoints, clusters, model
  read <dev> ...    read attributes now, instead of waiting to be told
  write <dev> ...   set attributes on a device
  light <dev> ...   tell a light what to be, e.g. "light1 red dim"
  reporting <dev>   ask a device to report an attribute on its own
  monitor           print device reports as they arrive
  config            dump the NCP's configuration values (diagnostic)
  help              show this message

global flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(os.Stderr, `
The port may also be set with GZB_PORT.
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// dial opens and negotiates a session with the adapter.
func dial(ctx context.Context, g *globals) (*ezsp.Conn, error) {
	opts := ezsp.Options{Path: g.port, Baud: g.baud}
	if g.trace {
		opts.TraceEZSP = func(dir string, m ezsp.Message) {
			fmt.Fprintf(os.Stderr, "%s ezsp %s\n", dir, m)
		}
	}
	if g.ashv {
		opts.TraceASH = func(dir string, f ash.Frame) {
			fmt.Fprintf(os.Stderr, "%s ash  %s\n", dir, f)
		}
	}
	conn, err := ezsp.Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nIs the dongle plugged in, are you in the `dialout` group, and is nothing else using the port?", err)
	}
	return conn, nil
}

// coordinatorOptions maps the CLI's global connection flags to the public
// high-level package used by long-running application commands.
func coordinatorOptions(g *globals, registryPath string) zigbee.Options {
	opts := zigbee.Options{
		Path:         g.port,
		Baud:         g.baud,
		RegistryPath: registryPath,
	}
	if g.trace {
		opts.TraceEZSP = func(direction, message string) {
			fmt.Fprintf(os.Stderr, "%s ezsp %s\n", direction, message)
		}
	}
	if g.ashv {
		opts.TraceASH = func(direction, frame string) {
			fmt.Fprintf(os.Stderr, "%s ash  %s\n", direction, frame)
		}
	}
	return opts
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// probeReport is the full picture of an adapter's current state.
type probeReport struct {
	Port            string                  `json:"port"`
	Protocol        string                  `json:"protocol"`
	EZSPVersion     int                     `json:"ezsp_version"`
	StackVersion    string                  `json:"stack_version"`
	StackType       uint8                   `json:"stack_type"`
	IEEE            ezsp.EUI64              `json:"ieee"`
	NetworkState    string                  `json:"network_state"`
	Joined          bool                    `json:"joined"`
	NodeType        string                  `json:"node_type,omitempty"`
	NodeID          *uint16                 `json:"node_id,omitempty"`
	Network         *ezsp.NetworkParameters `json:"network,omitempty"`
	NetworkChannels []int                   `json:"network_channels,omitempty"`
}

func cmdProbe(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: gzb probe\n\nReads adapter identity and network state. Changes nothing.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := dial(ctx, g)
	if err != nil {
		return err
	}
	defer conn.Close()

	stackVer, stackType := conn.StackVersion()
	rep := probeReport{
		Port:         g.port,
		Protocol:     "EZSP over ASH (EmberZNet)",
		EZSPVersion:  conn.ProtocolVersion(),
		StackVersion: formatStackVersion(stackVer),
		StackType:    stackType,
	}

	if rep.IEEE, err = conn.EUI64(ctx); err != nil {
		return fmt.Errorf("reading IEEE address: %w", err)
	}

	state, err := conn.NetworkState(ctx)
	if err != nil {
		return fmt.Errorf("reading network state: %w", err)
	}
	rep.NetworkState = state.String()
	rep.Joined = state.Joined()

	if state.Joined() {
		if id, err := conn.NodeID(ctx); err == nil {
			rep.NodeID = &id
		}
		nodeType, np, err := conn.NetworkParameters(ctx)
		if err != nil {
			return fmt.Errorf("reading network parameters: %w", err)
		}
		rep.NodeType = nodeType.String()
		rep.Network = &np
		rep.NetworkChannels = ezsp.ChannelList(np.Channels)
	}

	if g.json {
		return emitJSON(rep)
	}
	printProbe(rep)
	return nil
}

// formatStackVersion renders EmberZNet's packed version word, which encodes
// major, minor, patch and build in one nibble each.
func formatStackVersion(v uint16) string {
	return fmt.Sprintf("%d.%d.%d build %d", v>>12&0xF, v>>8&0xF, v>>4&0xF, v&0xF)
}

func printProbe(rep probeReport) {
	fmt.Printf("Adapter      %s\n", rep.Port)
	fmt.Printf("Protocol     %s\n", rep.Protocol)
	fmt.Printf("EZSP version %d\n", rep.EZSPVersion)
	fmt.Printf("Stack        EmberZNet %s (type %d)\n", rep.StackVersion, rep.StackType)
	fmt.Printf("IEEE         %s\n", rep.IEEE)
	fmt.Printf("Network      %s\n", rep.NetworkState)

	if rep.Network == nil {
		fmt.Print("\nNo network is formed on this adapter.\n")
		return
	}

	n := rep.Network
	fmt.Printf("\nNetwork\n")
	fmt.Printf("  Role         %s\n", rep.NodeType)
	if rep.NodeID != nil {
		fmt.Printf("  Node ID      0x%04X\n", *rep.NodeID)
	}
	fmt.Printf("  PAN ID       0x%04X\n", n.PanID)
	fmt.Printf("  Ext PAN ID   %s\n", n.ExtendedPanID)
	fmt.Printf("  Channel      %d\n", n.RadioChannel)
	fmt.Printf("  TX power     %d dBm\n", n.RadioTxPower)
	fmt.Printf("  Update ID    %d\n", n.NwkUpdateID)
	if len(rep.NetworkChannels) > 0 {
		fmt.Printf("  Chan mask    %v\n", rep.NetworkChannels)
	}
}
