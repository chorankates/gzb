package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/chorankates/gzb/internal/store"
	"github.com/chorankates/gzb/zigbee"
)

// A session holds the port open and takes commands at a prompt.
//
// Every command run from the shell opens the adapter, resets it, waits for the
// network to come back and closes it again: a second or two each time, so a
// light told three things is told them three seconds apart. A session pays
// that once. The reason it exists, though, is the Tab key — see
// repl_grammar.go — and the reason it is here rather than in a shell
// completion script is that the completions depend on what each device's
// interview found, which is context a shell script has no good way to reach.

func replUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `usage: gzb repl [flags]

Opens the adapter once and takes commands at a prompt until Ctrl-D.

  gzb> light <Tab>              the devices interviewed as lights
  gzb> light 1 <Tab>            what that light can be told
  gzb> light 1 red dim
  gzb> read liv<Tab>            completes the name, quotes included
  gzb> read "living room thermo" <Tab>
                                the clusters its interview found
  gzb> interview --all          fills in the answers Tab draws on

Completion comes from the device registry: a device's name, the clusters its
interview found, the attributes gzb knows on them, and the words of the light
grammar that device understands. A device that has not been interviewed is
not offered as a light, because nothing is known about it; interview it.

A device is matched loosely — "1" is light1 when light1 is the only light
ending in 1 — and an ambiguous name is an error naming the candidates rather
than a guess.

Reports arriving from devices are recorded in the registry for as long as
the session is open; "monitor" prints them until Enter. Ctrl-C interrupts a
command that is waiting on a device, and quits at an empty prompt.

Without a terminal, commands are read one per line from standard input.

flags:
`)
	fs.PrintDefaults()
}

func cmdRepl(ctx context.Context, g *globals, args []string) error {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	dbPath := fs.String("db", "", "device registry file (default: "+store.DefaultPath()+")")
	fs.Usage = func() { replUsage(fs) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return flag.ErrHelp
	}

	coordinator, err := zigbee.Open(ctx, coordinatorOptions(g, *dbPath))
	if err != nil {
		return err
	}
	defer coordinator.Close()

	s := &session{
		g:           g,
		coordinator: coordinator,
		registry:    registryPath(*dbPath),
		commands:    replCommands(),
		prompt:      "gzb> ",
	}
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if term.IsTerminal(in) && term.IsTerminal(out) {
		return s.runTerminal(ctx, in)
	}
	return s.runPlain(ctx)
}

// session is one open prompt.
type session struct {
	g           *globals
	coordinator *zigbee.Coordinator
	registry    string
	commands    []replCommand
	prompt      string

	// The terminal, when there is one. Without one, lines come from standard
	// input and nothing completes.
	term  *term.Terminal
	keys  *keyPump
	width int

	// sink is where reports go while `monitor` is printing them, and nil the
	// rest of the time. The loop that records them to the registry runs
	// either way.
	sinkMu sync.Mutex
	sink   chan<- zigbee.Reading
}

// errQuit is how a command ends the session.
var errQuit = errors.New("quit")

// runTerminal takes commands from a person at a terminal.
func (s *session) runTerminal(ctx context.Context, fd int) error {
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("putting the terminal into raw mode: %w", err)
	}
	defer term.Restore(fd, state)
	// Raw mode also stops the terminal turning "\n" into "\r\n" on the way
	// out, which every printer in gzb relies on. Put that one thing back.
	if err := keepOutputProcessing(fd); err != nil {
		return fmt.Errorf("configuring the terminal: %w", err)
	}

	s.keys = newKeyPump(os.Stdin)
	s.term = term.NewTerminal(terminalIO{s.keys, os.Stdout}, s.prompt)
	s.term.History = loadHistory(historyPath(s.registry))
	s.term.AutoCompleteCallback = s.onKey
	s.listen(ctx)
	s.banner()

	for {
		s.resize(fd)
		line, err := s.term.ReadLine()
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			if errors.Is(err, io.EOF) {
				// Ctrl-D, or Ctrl-C at the prompt: end the line the cursor
				// is on so the shell's prompt starts on a fresh one.
				fmt.Println()
				return nil
			}
			return err
		}
		if err := s.run(ctx, line); err != nil {
			if errors.Is(err, errQuit) {
				return nil
			}
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// runPlain takes commands one per line from standard input, for a pipe or a
// script. Nothing completes and nothing is prompted for; errors go to
// standard error as they would from the command line.
func (s *session) runPlain(ctx context.Context) error {
	s.listen(ctx)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if err := s.run(ctx, scanner.Text()); err != nil {
			if errors.Is(err, errQuit) {
				return nil
			}
			fmt.Fprintf(os.Stderr, "gzb: %v\n", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return scanner.Err()
}

// resize tells the line editor how wide the terminal is, which it needs to
// wrap a long line and move the cursor back through it. It is asked before
// every line rather than once, because a window is resized between lines,
// not during one.
func (s *session) resize(fd int) {
	width, height, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return
	}
	s.width = width
	s.term.SetSize(width, height)
}

// listen starts the readings loop for the life of the session. Reports are
// recorded to the registry as they arrive, whether or not anything is
// printing them, so a sensor that spoke while a light was being told
// something is not lost.
func (s *session) listen(ctx context.Context) {
	readings, errs := s.coordinator.Readings(ctx)
	go func() {
		for readings != nil || errs != nil {
			select {
			case r, ok := <-readings:
				if !ok {
					readings = nil
					continue
				}
				s.sinkMu.Lock()
				if s.sink != nil {
					select {
					case s.sink <- r:
					default:
					}
				}
				s.sinkMu.Unlock()
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				s.async(fmt.Sprintf("gzb: %v\n", err))
			}
		}
	}()
}

func (s *session) setSink(sink chan<- zigbee.Reading) {
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	s.sink = sink
}

// async prints something that arrived on its own schedule: above the prompt
// when one is showing, so the line being typed survives it.
func (s *session) async(text string) {
	if s.term != nil {
		s.term.Write([]byte(text))
		return
	}
	fmt.Fprint(os.Stderr, text)
}

func (s *session) banner() {
	devices := s.coordinator.Devices()
	var interviewed int
	for _, d := range devices {
		if !d.Interviewed.IsZero() {
			interviewed++
		}
	}
	fmt.Printf("gzb on %s: %d device(s) in the registry, %d interviewed.\n", s.g.port, len(devices), interviewed)
	fmt.Println("Tab completes; `help` lists commands; Ctrl-D quits.")
	if len(devices) > 0 && interviewed == 0 {
		fmt.Println("Nothing has been interviewed yet, so Tab has little to offer; `interview --all` fixes that.")
	}
	fmt.Println()
}

// run carries out one line.
func (s *session) run(ctx context.Context, line string) error {
	words := tokenize(line)
	if len(words) == 0 {
		return nil
	}
	args := make([]string, len(words))
	for i, w := range words {
		args[i] = w.text
	}
	cmd, ok := lookupCommand(s.commands, args[0])
	if !ok {
		return fmt.Errorf("unknown command %q (try `help`)", args[0])
	}

	fs, run := cmd.bound(os.Stdout)
	if err := fs.Parse(args[1:]); err != nil {
		// The flag package has already said what was wrong, and shown the
		// usage; -h is not an error at all.
		return nil
	}

	// A command that waits on a sleepy device can wait minutes. Ctrl-C ends
	// the wait without ending the session, which in raw mode means reading
	// it here: no signal is coming.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.keys != nil {
		stop := s.keys.watch(func(key byte) {
			if key == keyInterrupt || (cmd.stopsOnEnter && (key == '\r' || key == '\n')) {
				cancel()
			}
		})
		defer stop()
	}

	err := run(ctx, s, fs.Args())
	if err != nil && ctx.Err() != nil {
		fmt.Println("  interrupted")
		return nil
	}
	return err
}

// keyInterrupt is what Ctrl-C arrives as in raw mode.
const keyInterrupt = 0x03

// replCommands is everything the prompt understands. The ones that talk to a
// device share their parsing and running with the command-line commands of
// the same name; only where the device list comes from differs.
func replCommands() []replCommand {
	return []replCommand{
		{
			name: "light", synopsis: "<device> <what>...", summary: `tell a light what to be, e.g. "light 1 red dim"`,
			scope: zigbee.ClusterOnOff, shape: []argKind{argDevice, argAction}, repeatFrom: 1,
			usage: lightUsage,
			bind: func(fs *flag.FlagSet) runner {
				f := addLightFlags(fs)
				return func(ctx context.Context, s *session, args []string) error {
					if len(args) < 2 {
						fs.Usage()
						return nil
					}
					actions, err := zigbee.ParseActions(args[1:])
					if err != nil {
						return err
					}
					light, name, err := resolveLight(s.coordinator.Devices(), args[0], *f.endpoint)
					if err != nil {
						return err
					}
					return runLight(ctx, s.g, s.coordinator, name, light, actions, f)
				}
			},
		},
		{
			name: "read", synopsis: "<device> <cluster> [attribute...]", summary: "read attributes now, instead of waiting to be told",
			shape: []argKind{argDevice, argCluster, argAttribute}, repeatFrom: 2,
			usage: readUsage,
			bind: func(fs *flag.FlagSet) runner {
				f := addTargetFlags(fs, defaultRequestTimeout)
				return func(ctx context.Context, s *session, args []string) error {
					if len(args) < 2 {
						fs.Usage()
						return nil
					}
					cluster, attrs, err := parseReadArgs(args[1:])
					if err != nil {
						return err
					}
					target, name, err := resolveTarget(s.coordinator.Devices(), args[0], cluster, *f.endpoint)
					if err != nil {
						return err
					}
					return runRead(ctx, s.g, s.coordinator, name, target, attrs, *f.timeout)
				}
			},
		},
		{
			name: "write", synopsis: "<device> <cluster> <attribute> <value>...", summary: "set attributes on a device",
			shape: []argKind{argDevice, argCluster, argAttribute, argValue}, repeatFrom: 2,
			usage: writeUsage,
			bind: func(fs *flag.FlagSet) runner {
				f := addWriteFlags(fs)
				return func(ctx context.Context, s *session, args []string) error {
					if len(args) < 4 || len(args)%2 != 0 {
						fs.Usage()
						return nil
					}
					cluster, writes, err := parseWriteArgs(args[1:], *f.typeName)
					if err != nil {
						return err
					}
					target, name, err := resolveTarget(s.coordinator.Devices(), args[0], cluster, *f.endpoint)
					if err != nil {
						return err
					}
					return runWrite(ctx, s.g, s.coordinator, name, target, writes, *f.timeout)
				}
			},
		},
		{
			name: "reporting", synopsis: "<device> <cluster> <attribute...>", summary: "ask a device to report an attribute on its own",
			shape: []argKind{argDevice, argCluster, argAttribute}, repeatFrom: 2,
			usage: reportingUsage,
			bind: func(fs *flag.FlagSet) runner {
				f := addReportingFlags(fs)
				return func(ctx context.Context, s *session, args []string) error {
					if len(args) < 3 {
						fs.Usage()
						return nil
					}
					cluster, attrs, configs, err := f.parseArgs(args[1:])
					if err != nil {
						return err
					}
					target, name, err := resolveTarget(s.coordinator.Devices(), args[0], cluster, *f.endpoint)
					if err != nil {
						return err
					}
					return runReporting(ctx, s.g, s.coordinator, name, target, attrs, configs, *f.show, *f.timeout)
				}
			},
		},
		{
			name: "interview", synopsis: "<device> | --all", summary: "ask a device what it is, which is what Tab draws on",
			shape: []argKind{argDevice}, repeatFrom: -1,
			usage: interviewUsage,
			bind: func(fs *flag.FlagSet) runner {
				f := addInterviewFlags(fs)
				return func(ctx context.Context, s *session, args []string) error {
					if len(args) > 1 || (len(args) == 0 && !*f.all) {
						fs.Usage()
						return nil
					}
					targets, err := interviewPlan(s.g, s.coordinator.Devices(), args, *f.all, *f.full)
					if err != nil || len(targets) == 0 {
						return err
					}
					return runInterview(ctx, s.g, s.coordinator, targets, zigbee.InterviewOptions{Timeout: *f.timeout, Full: *f.full})
				}
			},
		},
		{
			name: "name", synopsis: "[device] [name...]", summary: "call a device something human, or list what they are called",
			shape: []argKind{argDevice, argText}, repeatFrom: 1,
			usage: nameUsage,
			bind: func(fs *flag.FlagSet) runner {
				clear := fs.Bool("clear", false, "remove the device's name instead of setting one")
				return func(ctx context.Context, s *session, args []string) error {
					return s.name(args, *clear)
				}
			},
		},
		{
			name: "devices", summary: "list the registry: identities, last seen, last readings",
			repeatFrom: -1,
			bind: func(fs *flag.FlagSet) runner {
				return func(ctx context.Context, s *session, args []string) error {
					printDevices(s.coordinator.Devices(), s.registry)
					return nil
				}
			},
		},
		{
			name: "monitor", summary: "print reports as they arrive, until Enter",
			repeatFrom: -1, stopsOnEnter: true,
			bind: func(fs *flag.FlagSet) runner {
				duration := fs.Duration("for", 0, "stop after this long (default: until Enter or Ctrl-C)")
				return func(ctx context.Context, s *session, args []string) error {
					return s.monitor(ctx, *duration)
				}
			},
		},
		{
			name: "help", synopsis: "[command]", summary: "this list, or one command's flags and grammar",
			shape: []argKind{argCommand}, repeatFrom: -1,
			bind: func(fs *flag.FlagSet) runner {
				return func(ctx context.Context, s *session, args []string) error {
					return s.help(args)
				}
			},
		},
		{
			name: "quit", summary: "close the port and leave",
			repeatFrom: -1,
			bind:       func(fs *flag.FlagSet) runner { return quit },
		},
		{
			name: "exit", hidden: true, repeatFrom: -1,
			bind: func(fs *flag.FlagSet) runner { return quit },
		},
	}
}

func quit(context.Context, *session, []string) error { return errQuit }

// name is `gzb name` at the prompt, through the coordinator's copy of the
// registry rather than the file: the session is the process writing it.
func (s *session) name(args []string, clear bool) error {
	devices := s.coordinator.Devices()
	if len(args) == 0 {
		if clear {
			return errors.New("--clear needs a device to clear the name of")
		}
		if len(devices) == 0 {
			fmt.Printf("No devices recorded in %s.\n\nRun `gzb join 60` and pair a device.\n", s.registry)
			return nil
		}
		entries := make([]nameEntry, 0, len(devices))
		for _, d := range devices {
			entries = append(entries, nameEntry{Name: d.Name, IEEE: d.IEEE, NodeID: fmt.Sprintf("0x%04X", d.NodeID)})
		}
		if s.g.json {
			return emitJSON(entries)
		}
		printNames(entries)
		return nil
	}

	r, err := resolveDevice(devices, args[0], 0)
	if err != nil {
		return err
	}
	if r.device == nil {
		return fmt.Errorf("no device at network address 0x%04X in the registry, so there is nothing to name yet", r.node)
	}
	// A record that has not been identified is addressed by the network
	// address it is filed under; everything else by the identity it has.
	key := r.device.IEEE
	if key == "" {
		key = fmt.Sprintf("0x%04X", r.node)
	}

	switch {
	case clear:
		if len(args) > 1 {
			return errors.New("--clear takes a device and nothing after it")
		}
		d, was, err := s.coordinator.ClearName(key)
		if err != nil {
			return err
		}
		if s.g.json {
			return emitJSON(d)
		}
		fmt.Print(describeCleared(deviceWord(d), was))
		return nil

	case len(args) == 1:
		if s.g.json {
			return emitJSON(*r.device)
		}
		fmt.Print(describeName(heading(r.device.Describe(), r.device.IEEE, r.node), r.device.Name))
		return nil

	default:
		was := r.device.Name
		d, err := s.coordinator.SetName(key, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		if s.g.json {
			return emitJSON(d)
		}
		fmt.Print(describeRenamed(identityWord(d), was, d.Name))
		return nil
	}
}

// identityWord is how a device is referred to in a message about its name:
// by its address, since the name is the thing under discussion.
func identityWord(d zigbee.Device) string {
	if d.IEEE != "" {
		return d.IEEE
	}
	return fmt.Sprintf("0x%04X", d.NodeID)
}

// monitor prints reports as they arrive, until the context ends — which Enter
// and Ctrl-C both do — or the duration runs out.
func (s *session) monitor(ctx context.Context, duration time.Duration) error {
	sink := make(chan zigbee.Reading, 64)
	s.setSink(sink)
	defer s.setSink(nil)

	if !s.g.json {
		fmt.Println("Printing reports as they arrive. Enter or Ctrl-C stops.")
	}
	var stop <-chan time.Time
	if duration > 0 {
		t := time.NewTimer(duration)
		defer t.Stop()
		stop = t.C
	}
	enc := json.NewEncoder(os.Stdout)
	for {
		select {
		case reading := <-sink:
			if s.g.json {
				if err := enc.Encode(reading); err != nil {
					return err
				}
				continue
			}
			fmt.Println(formatReading(reading))
		case <-stop:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *session) help(args []string) error {
	if len(args) == 0 {
		fmt.Println("commands:")
		width := 0
		for _, cmd := range s.commands {
			width = max(width, len(strings.TrimSpace(cmd.name+" "+cmd.synopsis)))
		}
		for _, cmd := range s.commands {
			if cmd.hidden {
				continue
			}
			fmt.Printf("  %-*s  %s\n", width, strings.TrimSpace(cmd.name+" "+cmd.synopsis), cmd.summary)
		}
		fmt.Print(`
Tab completes devices, clusters, attributes and the light words, from what
the interviews found. Flags go before the device, as on the command line;
"help <command>" shows them. Ctrl-C interrupts a command; Ctrl-D quits.
`)
		return nil
	}
	cmd, ok := lookupCommand(s.commands, args[0])
	if !ok {
		return fmt.Errorf("unknown command %q (try `help`)", args[0])
	}
	fs := cmd.flagSet(os.Stdout)
	if cmd.usage == nil {
		fmt.Printf("usage: %s %s\n\n%s\n", cmd.name, cmd.synopsis, cmd.summary)
		fs.PrintDefaults()
		return nil
	}
	fs.Usage()
	return nil
}
