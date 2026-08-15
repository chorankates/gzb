package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// cmdConfig dumps the NCP's configuration values.
//
// This is a diagnostic: when nothing joins and the coordinator insists it is
// open, the answer is usually in here. A stack profile other than 2 makes
// joining devices reject our beacons, and a child table of zero makes the
// coordinator refuse end devices outright — both look identical from outside.
func cmdConfig(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: gzb config\n\nReads the NCP's configuration values. Changes nothing.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := dial(ctx, g)
	if err != nil {
		return err
	}
	defer conn.Close()

	values, err := conn.Configuration(ctx)
	if err != nil {
		return err
	}

	if g.json {
		return emitJSON(values)
	}
	for _, v := range values {
		if v.Error != "" {
			fmt.Printf("  %-30s %s\n", v.Name, v.Error)
			continue
		}
		fmt.Printf("  %-30s %d\n", v.Name, v.Value)
	}
	return nil
}
