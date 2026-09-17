package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/ezsp"
	"github.com/chorankates/gzb/zigbee"
)

// cmdNetwork dispatches the network subcommands.
func cmdNetwork(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return cmdNetworkShow(ctx, g, nil)
	}
	switch args[0] {
	case "show":
		return cmdNetworkShow(ctx, g, args[1:])
	case "form":
		return cmdNetworkForm(ctx, g, args[1:])
	case "scan":
		return cmdNetworkScan(ctx, g, args[1:])
	case "join":
		return cmdNetworkJoin(ctx, g, args[1:])
	case "leave":
		return cmdNetworkLeave(ctx, g, args[1:])
	default:
		return fmt.Errorf("unknown network subcommand %q (want show, form, scan, join or leave)", args[0])
	}
}

// scanFlags are the flags scan and join share: which channels to listen on,
// and for how long.
type scanFlags struct {
	channel  *int
	duration *int
}

func addScanFlags(fs *flag.FlagSet) scanFlags {
	return scanFlags{
		channel:  fs.Int("channel", 0, "scan only this 2.4 GHz channel (11-26; default: all of them)"),
		duration: fs.Int("duration", int(ezsp.DefaultScanDuration), "per-channel scan duration exponent (0-14); each step doubles the time"),
	}
}

// mask turns the flags into the channel mask the scan takes.
func (f scanFlags) mask() (uint32, error) {
	if *f.duration < 0 || *f.duration > int(ezsp.MaxScanDuration) {
		return 0, fmt.Errorf("scan duration %d is outside the range 0-%d", *f.duration, ezsp.MaxScanDuration)
	}
	if *f.channel == 0 {
		return ezsp.DefaultChannelMask, nil
	}
	if *f.channel < 11 || *f.channel > 26 {
		return 0, fmt.Errorf("channel %d is outside the Zigbee range 11-26", *f.channel)
	}
	return 1 << uint32(*f.channel), nil
}

func cmdNetworkScan(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("network scan", flag.ContinueOnError)
	f := addScanFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb network scan [flags]

Listens on each channel for the networks in range and lists them, with
whether each is currently accepting joins. Changes nothing; takes a few
seconds, during which the radio is busy.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	mask, err := f.mask()
	if err != nil {
		return err
	}

	conn, err := dial(ctx, g)
	if err != nil {
		return err
	}
	defer conn.Close()

	if !g.json {
		fmt.Printf("Scanning %s...\n", describeChannels(mask))
	}
	found, err := conn.ScanNetworks(ctx, mask, uint8(*f.duration))
	if err != nil {
		return fmt.Errorf("scanning: %w", err)
	}
	if g.json {
		if found == nil {
			found = []ezsp.FoundNetwork{}
		}
		return emitJSON(found)
	}
	fmt.Println()
	printNetworks(found)
	return nil
}

func cmdNetworkJoin(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("network join", flag.ContinueOnError)
	f := addScanFlags(fs)
	panID := fs.String("pan-id", "", "join the network with this PAN ID, as hex, e.g. 0x1A2B (default: the one open network found)")
	txPower := fs.Int("tx-power", 8, "radio transmit power in dBm")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb network join [flags]

Joins an existing Zigbee network as a router, which gives this adapter a
second seat on a network another adapter coordinates: every device command
works from here, addressed straight to the device, while the coordinator
carries on running the network.

The network must be open to new devices first — "gzb join 60" on the
coordinator, or whatever opens pairing on the application holding it. This
scans for networks, joins the one that is open, and refuses when none is or
when more than one is; --pan-id picks among several.

Nothing is destructive about it, but the credentials the trust centre hands
over are stored on the adapter, so it comes back onto the network every time
it is opened until "gzb network leave --confirm". An adapter already on a
network is left alone: leave it first.

Pairing new devices is still the coordinator's job, and reports devices send
on their own still go to the coordinator; this seat is for asking.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	mask, err := f.mask()
	if err != nil {
		return err
	}
	var wantPan uint16
	if *panID != "" {
		v, err := strconv.ParseUint(trimHexPrefix(*panID), 16, 16)
		if err != nil {
			return fmt.Errorf("invalid --pan-id %q: %w", *panID, err)
		}
		wantPan = uint16(v)
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
	if state.Joined() {
		nodeType, np, err := conn.NetworkParameters(ctx)
		if err == nil {
			return fmt.Errorf("adapter is already a %s on the network on channel %d, PAN ID 0x%04X; run `gzb network leave --confirm` first if that is not the one you want",
				nodeType, np.RadioChannel, np.PanID)
		}
		return fmt.Errorf("adapter is already on a network (%s); run `gzb network leave --confirm` first", state)
	}

	if !g.json {
		fmt.Printf("Scanning %s...\n", describeChannels(mask))
	}
	found, err := conn.ScanNetworks(ctx, mask, uint8(*f.duration))
	if err != nil {
		return fmt.Errorf("scanning: %w", err)
	}
	target, err := chooseNetwork(found, wantPan)
	if err != nil {
		return err
	}

	if !g.json {
		fmt.Printf("Joining the network on channel %d, PAN ID 0x%04X, as a router...\n", target.Channel, target.PanID)
	}
	res, err := conn.JoinNetwork(ctx, ezsp.JoinConfig{
		Channel:       target.Channel,
		PanID:         target.PanID,
		ExtendedPanID: target.ExtendedPanID,
		NwkUpdateID:   target.NwkUpdateID,
		TxPower:       int8(*txPower),
	})
	if err != nil {
		return fmt.Errorf("joining network: %w", err)
	}

	if g.json {
		return emitJSON(res)
	}
	fmt.Printf("\nJoined.\n\n")
	fmt.Printf("  Role         %s\n", res.NodeType)
	fmt.Printf("  Node ID      0x%04X\n", res.NodeID)
	fmt.Printf("  IEEE         %s\n", res.IEEE)
	fmt.Printf("  Channel      %d\n", res.Channel)
	fmt.Printf("  PAN ID       0x%04X\n", res.PanID)
	fmt.Printf("  Ext PAN ID   %s\n", res.ExtendedPanID)
	fmt.Printf("  TX power     %d dBm\n", res.TxPower)
	fmt.Print("\nThe adapter keeps these credentials and rejoins on every open. `gzb repl`\n" +
		"here now reaches the same devices the coordinator does; the coordinator\n" +
		"still pairs new ones and still receives what devices report on their own.\n")
	return nil
}

// chooseNetwork picks the network to join from what a scan heard: the one
// that is open, or the one --pan-id names if it is open. Anything else is an
// error that says what was heard, because "nothing to join" has three
// different causes and only one of them is fixed by walking closer.
func chooseNetwork(found []ezsp.FoundNetwork, panID uint16) (ezsp.FoundNetwork, error) {
	if len(found) == 0 {
		return ezsp.FoundNetwork{}, fmt.Errorf("no Zigbee networks heard; check the coordinator is running and within range, or scan a wider set of channels")
	}
	matching := found
	if panID != 0 {
		matching = nil
		for _, n := range found {
			if n.PanID == panID {
				matching = append(matching, n)
			}
		}
		if len(matching) == 0 {
			return ezsp.FoundNetwork{}, fmt.Errorf("no network with PAN ID 0x%04X heard; %s", panID, listNetworks(found))
		}
	}
	var open []ezsp.FoundNetwork
	for _, n := range matching {
		if n.AllowingJoin {
			open = append(open, n)
		}
	}
	switch len(open) {
	case 0:
		return ezsp.FoundNetwork{}, fmt.Errorf("%s, but none is accepting joins; open it from its coordinator (`gzb join 60` there) and re-run", listNetworks(matching))
	case 1:
		return open[0], nil
	default:
		return ezsp.FoundNetwork{}, fmt.Errorf("%s and more than one is open; say which with --pan-id", listNetworks(open))
	}
}

// listNetworks renders what a scan heard for an error message.
func listNetworks(found []ezsp.FoundNetwork) string {
	var b strings.Builder
	fmt.Fprintf(&b, "heard %d network(s):", len(found))
	for _, n := range found {
		b.WriteString("\n  ")
		b.WriteString(n.String())
	}
	return b.String()
}

// printNetworks tabulates a scan.
func printNetworks(found []ezsp.FoundNetwork) {
	if len(found) == 0 {
		fmt.Print("No Zigbee networks heard.\n\nA network only beacons in answer to a scan, so this means no router of one\nis in range on the channels scanned, not that the band is quiet.\n")
		return
	}
	fmt.Printf("%d network(s) heard:\n\n", len(found))
	fmt.Printf("  %-8s %-7s %-24s %-7s %-8s %-4s %s\n", "Channel", "PAN ID", "Ext PAN ID", "Joins", "Profile", "LQI", "RSSI")
	for _, n := range found {
		open := "closed"
		if n.AllowingJoin {
			open = "open"
		}
		fmt.Printf("  %-8d 0x%04X  %-24s %-7s %-8d %-4d %d dBm\n", n.Channel, n.PanID, n.ExtendedPanID, open, n.StackProfile, n.LQI, n.RSSI)
	}
	var open int
	for _, n := range found {
		if n.AllowingJoin {
			open++
		}
	}
	if open == 0 {
		fmt.Print("\nNone is accepting joins. A network opens from its coordinator: `gzb join 60`\nthere, and this adapter can `gzb network join` while the window is open.\n")
	}
}

// describeChannels says which channels a mask covers, for a progress line.
func describeChannels(mask uint32) string {
	channels := ezsp.ChannelList(mask)
	if len(channels) == 1 {
		return fmt.Sprintf("channel %d", channels[0])
	}
	if mask == ezsp.DefaultChannelMask {
		return "channels 11-26"
	}
	return fmt.Sprintf("channels %v", channels)
}

func cmdNetworkShow(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("network show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The probe report already covers everything network show would print.
	return cmdProbe(ctx, g, nil)
}

func cmdNetworkForm(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("network form", flag.ContinueOnError)
	channel := fs.Int("channel", 15, "2.4 GHz channel to form on (11-26)")
	panID := fs.String("pan-id", "", "16-bit PAN ID as hex, e.g. 0x1A2B (default: random)")
	txPower := fs.Int("tx-power", 8, "radio transmit power in dBm")
	confirm := fs.Bool("confirm", false, "actually form the network")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb network form [flags]

Creates a new Zigbee network with this adapter as coordinator.

This is destructive. It writes fresh credentials to the adapter, and any
devices joined to a previous network hold the old network key and will be
orphaned. Nothing happens without --confirm.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *channel < 11 || *channel > 26 {
		return fmt.Errorf("channel %d is outside the Zigbee range 11-26", *channel)
	}

	cfg := ezsp.FormationConfig{
		Channel: uint8(*channel),
		TxPower: int8(*txPower),
	}
	if *panID != "" {
		v, err := strconv.ParseUint(trimHexPrefix(*panID), 16, 16)
		if err != nil {
			return fmt.Errorf("invalid --pan-id %q: %w", *panID, err)
		}
		cfg.PanID = uint16(v)
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

	if !*confirm {
		fmt.Printf("Would form a new network on %s:\n", g.port)
		fmt.Printf("  Channel    %d\n", cfg.Channel)
		if cfg.PanID != 0 {
			fmt.Printf("  PAN ID     0x%04X\n", cfg.PanID)
		} else {
			fmt.Printf("  PAN ID     random\n")
		}
		fmt.Printf("  TX power   %d dBm\n", cfg.TxPower)
		fmt.Printf("  Network key  freshly generated\n")
		fmt.Printf("\nCurrent adapter state: %s\n", state)
		if state.Joined() {
			fmt.Print("\nWARNING: a network already exists on this adapter. Forming a new one\n" +
				"orphans every device joined to it. Run `gzb network leave --confirm` first.\n")
		}
		fmt.Print("\nRe-run with --confirm to proceed.\n")
		return nil
	}

	if state.Joined() {
		return fmt.Errorf("adapter is already on a network (%s); run `gzb network leave --confirm` first", state)
	}

	res, err := conn.FormNetwork(ctx, cfg)
	if err != nil {
		return fmt.Errorf("forming network: %w", err)
	}

	if g.json {
		return emitJSON(res)
	}
	fmt.Printf("Network formed.\n\n")
	fmt.Printf("  Channel      %d\n", res.Channel)
	fmt.Printf("  PAN ID       0x%04X\n", res.PanID)
	fmt.Printf("  Ext PAN ID   %s\n", res.ExtendedPanID)
	fmt.Printf("  TX power     %d dBm\n", res.TxPower)
	fmt.Printf("  Coordinator  0x%04X  %s\n", res.NodeID, res.IEEE)
	fmt.Printf("\nNetwork key  %s\n", hex.EncodeToString(res.NetworkKey[:]))
	fmt.Print("\nThe network key is stored on the adapter. Keep the copy above somewhere\n" +
		"safe: it is what a replacement coordinator would need to adopt this network\n" +
		"without re-pairing every device.\n")
	fmt.Print("\nNext: `gzb permit-join 60` to open the network, then put a device into\n" +
		"pairing mode.\n")
	return nil
}

func cmdNetworkLeave(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("network leave", flag.ContinueOnError)
	confirm := fs.Bool("confirm", false, "actually leave the network")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb network leave --confirm

Tears down the current network. Every joined device is orphaned and must be
re-paired. Nothing happens without --confirm.
`)
	}
	if err := fs.Parse(args); err != nil {
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
		fmt.Printf("Adapter is not on a network (%s). Nothing to leave.\n", state)
		return nil
	}

	if !*confirm {
		_, np, err := conn.NetworkParameters(ctx)
		if err == nil {
			fmt.Printf("Would leave the network on channel %d, PAN ID 0x%04X.\n", np.RadioChannel, np.PanID)
		}
		fmt.Print("Every joined device would be orphaned and need re-pairing.\n\nRe-run with --confirm to proceed.\n")
		return nil
	}

	if err := conn.LeaveNetwork(ctx); err != nil {
		return fmt.Errorf("leaving network: %w", err)
	}
	fmt.Println("Left the network.")
	return nil
}

// cmdPermitJoin opens the network to joining devices for a bounded window.
func cmdPermitJoin(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("permit-join", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb permit-join <seconds>

Opens the network so new devices can join, for the given number of seconds.
Use 0 to close it again. 255 means "open indefinitely", which leaves a
standing invitation on the network and is best avoided.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	seconds, err := strconv.ParseUint(fs.Arg(0), 10, 8)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", fs.Arg(0), err)
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, ""))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	if err := coordinator.PermitJoin(ctx, time.Duration(seconds)*time.Second); err != nil {
		return err
	}
	if seconds == 0 {
		fmt.Println("Network closed to new devices.")
		return nil
	}
	fmt.Printf("Network open to new devices for %d seconds.\n", seconds)
	return nil
}

// trimHexPrefix strips an optional 0x prefix.
func trimHexPrefix(s string) string {
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}
