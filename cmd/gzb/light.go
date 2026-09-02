package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// defaultLightTimeout is how long to wait for a light to answer.
//
// It is far shorter than the timeout a sensor gets, and the reason is the
// difference between the two: a light is mains-powered and listening all the
// time, so a command that has not been answered in a few seconds is not early,
// it is lost. Waiting five minutes for one would only hide that.
const defaultLightTimeout = 20 * time.Second

// lightFlags are the flags `light` takes wherever it is typed: on the command
// line, or at the prompt of a session already holding the port.
type lightFlags struct {
	endpoint   *int
	timeout    *time.Duration
	transition *time.Duration
	persist    *bool
}

func addLightFlags(fs *flag.FlagSet) lightFlags {
	return lightFlags{
		endpoint:   fs.Int("endpoint", 0, "endpoint to address (default: whichever one has the light clusters)"),
		timeout:    fs.Duration("timeout", defaultLightTimeout, "how long to wait for the light to answer"),
		transition: fs.Duration("transition", 0, "how long the light should take to get there"),
		persist:    fs.Bool("persist", false, "also set the brightness the light returns to when something else turns it on"),
	}
}

func lightUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `usage: gzb light [flags] <device> <what>...

Tells a light what to be. Several things may be said at once, and they are
carried out in the order given.

  gzb light light1 red dim
  gzb light light1 brighter
  gzb light "hallway lamp" on warm 40%%
  gzb light light1 off

The vocabulary is a pattern rather than a list: a plain word is a place to go
to, and its comparative is a distance to move. "dim" puts a light at a quarter
brightness; "dimmer" takes a quarter off wherever it happens to be now.

  on, off, toggle
  full, bright, half, dim, low, min, faint   an absolute brightness
  brighter, up, dimmer, darker, down         a step from where it is
  N%%                                         an absolute brightness
  %s
                                             a colour
  %s
                                             a white point
  #rrggbb                                    a colour as sRGB
  NNNNk                                      a white point, e.g. 2700k
  hue:H/S                                    an exact hue and saturation, 0-254
  level:N                                    an exact level, 0-254

How a colour is sent depends on what the lamp says it can be told, not on what
model it is: hue and saturation where it supports them, CIE xy where it does
not, and a colour temperature where that is all it has.

--persist matters for a light something else switches on — a motion sensor, a
wall switch. Setting a brightness changes what the lamp is doing now; --persist
also writes the level it will come back at, which is the one that survives
being turned off and on again by something that is not gzb.

flags:
`, strings.Join(zigbee.ColorNames(), ", "), strings.Join(zigbee.WhitePointNames(), ", "))
	fs.PrintDefaults()
}

func cmdLight(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("light", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	f := addLightFlags(fs)
	fs.Usage = func() { lightUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return flag.ErrHelp
	}

	actions, err := zigbee.ParseActions(fs.Args()[1:])
	if err != nil {
		return err
	}
	devices, err := zigbee.LoadDevices(*dbPath)
	if err != nil {
		return err
	}
	light, name, err := resolveLight(devices, fs.Arg(0), *f.endpoint)
	if err != nil {
		return err
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	return runLight(ctx, g, coordinator, name, light, actions, f)
}

// runLight tells a light what to be, through a coordinator that is already
// open. It is the half of the command a session shares with the command line.
func runLight(ctx context.Context, g *globals, coordinator *zigbee.Coordinator, name string, light zigbee.Light, actions []zigbee.Action, f lightFlags) error {
	ctx, cancel := context.WithTimeout(ctx, *f.timeout)
	defer cancel()

	if !g.json {
		printLightPlan(name, light, actions)
	}

	if err := coordinator.Apply(ctx, light, actions, *f.transition); err != nil {
		return err
	}
	if *f.persist {
		level, ok := lastAbsoluteLevel(actions)
		if !ok {
			return fmt.Errorf("--persist needs an absolute brightness to persist, not just a step (say `dim` or `25%%`, not `dimmer`)")
		}
		if err := coordinator.SetOnLevel(ctx, light, level); err != nil {
			return err
		}
		if !g.json {
			printOnLevel(level)
		}
	}

	if g.json {
		return emitJSON(lightReport{Device: name, Node: light.Node, Endpoint: light.Endpoint, Applied: describeActions(actions)})
	}
	printLightDone()
	return nil
}

// printLightPlan says what a light is being told, before it is told it.
//
// Echoing the phrase back is most of the interface: "dim" and "dimmer" differ
// by one letter and by which command they become, so showing which one was
// heard is cheaper than explaining the grammar afterwards.
func printLightPlan(name string, light zigbee.Light, actions []zigbee.Action) {
	fmt.Printf("%s (0x%04X) endpoint %d\n", name, light.Node, light.Endpoint)
	for _, action := range actions {
		fmt.Printf("  %s\n", action)
	}
}

// printOnLevel reports the level written for the next time something else
// turns the light on, which is a different fact from the level it is at now
// and worth saying in full rather than as a number.
func printOnLevel(level uint8) {
	fmt.Printf("  on level %d — what it returns to when something else switches it on\n", level)
}

func printLightDone() { fmt.Println("  ok") }

// lightReport is what --json says about a light that was told something.
type lightReport struct {
	Device   string   `json:"device"`
	Node     uint16   `json:"node"`
	Endpoint uint8    `json:"endpoint"`
	Applied  []string `json:"applied"`
}

func describeActions(actions []zigbee.Action) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, action.String())
	}
	return out
}

// lastAbsoluteLevel finds the brightness a phrase settled on, if it named one.
// The last wins, for the same reason the actions are applied in order: a
// person who said two is telling you they changed their mind.
func lastAbsoluteLevel(actions []zigbee.Action) (uint8, bool) {
	for i := len(actions) - 1; i >= 0; i-- {
		if actions[i].Verb == zigbee.VerbLevel {
			return actions[i].Level, true
		}
	}
	return 0, false
}
