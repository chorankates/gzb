package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chorankates/gzb/internal/store"
)

// Naming is a registry operation, not a network one: it touches no hardware and
// works while every device is asleep, which matters because the moment you most
// want to name a device is right after it appears in a monitor log — and a
// battery sensor is unreachable by then.
//
// A name is also an address. Once a device has one, every command that takes a
// device accepts it, so naming is what turns
//
//	gzb interview A4:C1:38:18:56:07:FF:FF
//
// into
//
//	gzb interview "living room thermo"
func cmdName(_ context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("name", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	clear := fs.Bool("clear", false, "remove the device's name instead of setting one")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gzb name [flags] [device] [name...]

Gives a device a human-friendly name, which is then shown wherever that device
appears — `+"`gzb devices`"+`, `+"`gzb join`"+` and every line of `+"`gzb monitor`"+` output.

With no arguments, lists the names in the registry. With a device and no name,
reports what that device is currently called.

The device may be named by IEEE address, by network address in hex, or by its
existing name. The new name is everything after it, so quoting is optional:

  gzb name A4:C1:38:18:56:07:FF:FF living room thermo
  gzb name 0x90CB "back door sensor"
  gzb name "living room thermo" hallway thermo    # rename
  gzb name --clear 0x90CB

Names must be unique, since they are used to address devices, and cannot look
like an address. Matching is loose: any unambiguous part of a name will do.

Reads no hardware, so it works while a device is asleep.

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

	if fs.NArg() == 0 {
		if *clear {
			fs.Usage()
			return flag.ErrHelp
		}
		return listNames(db, g)
	}

	d, err := db.Resolve(fs.Arg(0))
	if err != nil {
		return resolveError(err)
	}

	switch {
	case *clear:
		if fs.NArg() > 1 {
			fs.Usage()
			return flag.ErrHelp
		}
		_, was, err := db.ClearName(d.IEEE)
		if err != nil {
			return err
		}
		if err := db.Save(); err != nil {
			return err
		}
		if g.json {
			return emitJSON(d)
		}
		if was == "" {
			fmt.Printf("%s had no name.\n", d.IEEE)
			return nil
		}
		fmt.Printf("%s is no longer called %q.\n", d.IEEE, was)
		return nil

	case fs.NArg() == 1:
		if g.json {
			return emitJSON(d)
		}
		if d.Name == "" {
			fmt.Printf("%s has no name.\n", deviceHeading(d))
			return nil
		}
		fmt.Println(deviceHeading(d))
		return nil

	default:
		was := d.Name
		if _, err := db.SetName(d.IEEE, strings.Join(fs.Args()[1:], " ")); err != nil {
			return err
		}
		if err := db.Save(); err != nil {
			return err
		}
		if g.json {
			return emitJSON(d)
		}
		if was != "" && was != d.Name {
			fmt.Printf("%s renamed from %q to %q.\n", d.IEEE, was, d.Name)
			return nil
		}
		fmt.Printf("%s is now %q.\n", d.IEEE, d.Name)
		return nil
	}
}

// nameEntry is one device's naming, for JSON output. It deliberately omits
// everything else `gzb devices` reports.
type nameEntry struct {
	Name   string `json:"name,omitempty"`
	IEEE   string `json:"ieee"`
	NodeID string `json:"node_id"`
}

func listNames(db *store.Store, g *globals) error {
	devices := db.List()

	if g.json {
		entries := make([]nameEntry, 0, len(devices))
		for _, d := range devices {
			entries = append(entries, nameEntry{Name: d.Name, IEEE: d.IEEE, NodeID: d.NodeIDHex()})
		}
		return emitJSON(entries)
	}
	if len(devices) == 0 {
		fmt.Printf("No devices recorded in %s.\n\nRun `gzb join 60` and pair a device.\n", db.Path())
		return nil
	}

	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Printf("%-24s %s  %s\n", name, d.NodeIDHex(), d.IEEE)
	}
	return nil
}

// resolveError adds the advice that belongs to the command line rather than to
// the registry: an unmatched device is usually a typo, and the list of devices
// is one command away.
func resolveError(err error) error {
	if errors.Is(err, store.ErrNoDevice) {
		return fmt.Errorf("%w (run `gzb devices` to list them)", err)
	}
	return err
}
