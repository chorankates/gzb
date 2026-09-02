package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	fs.Usage = func() { nameUsage(fs) }
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
		fmt.Print(describeCleared(d.IEEE, was))
		return nil

	case fs.NArg() == 1:
		if g.json {
			return emitJSON(d)
		}
		fmt.Print(describeName(deviceHeading(d), d.Name))
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
		fmt.Print(describeRenamed(d.IEEE, was, d.Name))
		return nil
	}
}

// The three things `name` can say, worded once so that a session saying them
// says the same thing.

func describeCleared(ieee, was string) string {
	if was == "" {
		return fmt.Sprintf("%s had no name.\n", ieee)
	}
	return fmt.Sprintf("%s is no longer called %q.\n", ieee, was)
}

func describeName(heading, name string) string {
	if name == "" {
		return fmt.Sprintf("%s has no name.\n", heading)
	}
	return heading + "\n"
}

func describeRenamed(ieee, was, now string) string {
	if was != "" && was != now {
		return fmt.Sprintf("%s renamed from %q to %q.\n", ieee, was, now)
	}
	return fmt.Sprintf("%s is now %q.\n", ieee, now)
}

func nameUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `usage: gzb name [flags] [device] [name...]

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

// nameEntry is one device's naming, for JSON output. It deliberately omits
// everything else `gzb devices` reports.
type nameEntry struct {
	Name   string `json:"name,omitempty"`
	IEEE   string `json:"ieee"`
	NodeID string `json:"node_id"`
}

func listNames(db *store.Store, g *globals) error {
	devices := db.List()
	entries := make([]nameEntry, 0, len(devices))
	for _, d := range devices {
		entries = append(entries, nameEntry{Name: d.Name, IEEE: d.IEEE, NodeID: d.NodeIDHex()})
	}

	if g.json {
		return emitJSON(entries)
	}
	if len(entries) == 0 {
		fmt.Printf("No devices recorded in %s.\n\nRun `gzb join 60` and pair a device.\n", db.Path())
		return nil
	}
	printNames(entries)
	return nil
}

// printNames lists what devices are called, one per line.
func printNames(entries []nameEntry) {
	for _, entry := range entries {
		name := entry.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Printf("%-24s %s  %s\n", name, entry.NodeID, entry.IEEE)
	}
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
