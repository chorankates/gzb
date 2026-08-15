package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/conor/gzb/internal/ezsp"
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
	case "leave":
		return cmdNetworkLeave(ctx, g, args[1:])
	default:
		return fmt.Errorf("unknown network subcommand %q (want show, form or leave)", args[0])
	}
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

	if err := conn.PermitJoining(ctx, uint8(seconds)); err != nil {
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
