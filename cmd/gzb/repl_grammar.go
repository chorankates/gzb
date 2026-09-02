package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/chorankates/gzb/zigbee"
)

// The prompt's grammar, and completing it.
//
// A session exists mostly for the Tab key. Every command takes a device, most
// take a cluster and an attribute after it, and none of those are things a
// person should have to remember the spelling of: the registry knows the
// devices, each device's interview knows its clusters, and gzb's own table
// knows the attributes on them. Completion is what puts that knowledge under
// the fingers. The grammar is written down once, as data, so that completing
// a command and running it cannot disagree about what it takes.

// argKind is what a positional argument is, which decides what Tab offers.
type argKind int

const (
	// argDevice is a device, offered from those whose interview says they
	// carry the command's cluster where the command has one.
	argDevice argKind = iota
	// argCluster is a cluster the device before it carries.
	argCluster
	// argAttribute is an attribute gzb knows on the cluster before it.
	argAttribute
	// argValue is a value for the attribute before it.
	argValue
	// argAction is a word of the light grammar the device understands.
	argAction
	// argText is free text, such as a name; nothing is offered.
	argText
	// argCommand is another command, for help.
	argCommand
)

// runner is a command bound to its flags, ready to run.
type runner func(ctx context.Context, s *session, args []string) error

// replCommand is one thing a session can be told.
type replCommand struct {
	name     string
	synopsis string // the arguments, for the help listing
	summary  string // one line, for the help listing
	hidden   bool   // an alias, left out of the listing
	// scope is the cluster a device argument is looked up among first, zero
	// for any device.
	scope uint16
	// shape lists the positional arguments in order. Positions past the end
	// repeat from repeatFrom — so `write` takes attribute/value pairs and
	// `light` any number of words — or take nothing when repeatFrom is -1.
	shape      []argKind
	repeatFrom int
	// stopsOnEnter marks a command that runs until Enter as well as Ctrl-C.
	stopsOnEnter bool
	// bind registers the command's flags on fs and returns what runs it once
	// fs has parsed a line. Declaring the flags and reading them are one
	// closure, so a value cannot be read anywhere it was not declared.
	bind func(fs *flag.FlagSet) runner
	// usage prints the command's help onto fs.Output().
	usage func(fs *flag.FlagSet)
}

// kindAt reports what the positional argument at index is.
func (c replCommand) kindAt(index int) (argKind, bool) {
	if index < len(c.shape) {
		return c.shape[index], true
	}
	if c.repeatFrom < 0 || c.repeatFrom >= len(c.shape) {
		return 0, false
	}
	span := len(c.shape) - c.repeatFrom
	return c.shape[c.repeatFrom+(index-c.repeatFrom)%span], true
}

// bound declares the command's flags on a fresh set and returns it along with
// what runs the command once the set has parsed a line. Output goes to w.
func (c replCommand) bound(w io.Writer) (*flag.FlagSet, runner) {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(w)
	var run runner
	if c.bind != nil {
		run = c.bind(fs)
	}
	fs.Usage = func() {
		if c.usage != nil {
			c.usage(fs)
		}
	}
	return fs, run
}

// flagSet declares the command's flags without running anything, for
// completing them and for help.
func (c replCommand) flagSet(w io.Writer) *flag.FlagSet {
	fs, _ := c.bound(w)
	return fs
}

func lookupCommand(commands []replCommand, name string) (replCommand, bool) {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd, true
		}
	}
	return replCommand{}, false
}

func commandNames(commands []replCommand) []string {
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		if !cmd.hidden {
			names = append(names, cmd.name)
		}
	}
	return names
}

// word is one token of a line and where it came from, which completion needs
// in order to replace it.
type word struct {
	text  string // the word, quotes removed
	start int    // byte offset of its first character, an opening quote included
	end   int    // byte offset just past its last character, a closing quote included
	open  bool   // it began with a quote that never closed
}

// tokenize splits a line into words. Double quotes group the spaces a device
// name often has, and are the only quoting there is: a prompt is not a shell,
// and a person typing a name should not have to escape it.
func tokenize(line string) []word {
	var words []word
	i := 0
	for i < len(line) {
		if isSpace(line[i]) {
			i++
			continue
		}
		w := word{start: i}
		var text []byte
		quoted := false
		for i < len(line) && (quoted || !isSpace(line[i])) {
			if line[i] == '"' {
				quoted = !quoted
			} else {
				text = append(text, line[i])
			}
			i++
		}
		w.text, w.end, w.open = string(text), i, quoted
		words = append(words, w)
	}
	return words
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// completion is what Tab can put at the cursor: where the current word
// starts, and what it could become.
type completion struct {
	start      int
	candidates []string
}

// grammar is what completion draws on: the commands, and the devices as the
// registry currently has them.
type grammar struct {
	commands []replCommand
	devices  []zigbee.Device
}

// complete works out what could come next at the end of line, which is the
// text before the cursor.
func (g grammar) complete(line string) completion {
	words := tokenize(line)
	current := word{start: len(line), end: len(line)}
	if n := len(words); n > 0 && words[n-1].end == len(line) {
		current = words[n-1]
		words = words[:n-1]
	}
	c := completion{start: current.start}
	if len(words) == 0 {
		c.candidates = commandNames(g.commands)
	} else {
		cmd, ok := lookupCommand(g.commands, words[0].text)
		if !ok {
			return c
		}
		c.candidates = g.arguments(cmd, words[1:], current.text)
	}
	c.candidates = filterPrefix(c.candidates, current.text)
	return c
}

// arguments offers what could follow the words already typed after a command.
func (g grammar) arguments(cmd replCommand, words []word, current string) []string {
	fs := cmd.flagSet(io.Discard)

	// Flags come before the device, and each is either a switch or takes the
	// next word as its value. Walking them is what tells the positional index
	// from the words alone.
	var positionals []string
	flagsDone, expectValue := false, false
	for _, w := range words {
		switch {
		case expectValue:
			expectValue = false
		case !flagsDone && w.text == "--":
			flagsDone = true
		case !flagsDone && isFlag(w.text):
			name, hasValue := flagName(w.text)
			expectValue = !hasValue && takesValue(fs, name)
		default:
			positionals = append(positionals, w.text)
		}
	}
	switch {
	case expectValue:
		// The current word is a flag's value: a duration, a type name.
		return nil
	case !flagsDone && len(positionals) == 0 && strings.HasPrefix(current, "-"):
		return flagCandidates(fs, current)
	}

	kind, ok := cmd.kindAt(len(positionals))
	if !ok {
		return nil
	}
	switch kind {
	case argDevice:
		return g.deviceCandidates(cmd.scope)
	case argCluster:
		return clusterCandidates(g.deviceAt(cmd, positionals))
	case argAttribute:
		cluster, ok := clusterAt(cmd, positionals)
		if !ok {
			return nil
		}
		return attributeCandidates(cluster, attributesAt(cmd, positionals))
	case argValue:
		cluster, ok := clusterAt(cmd, positionals)
		if !ok || len(positionals) == 0 {
			return nil
		}
		return valueCandidates(cluster, positionals[len(positionals)-1])
	case argAction:
		return actionCandidates(g.deviceAt(cmd, positionals))
	case argCommand:
		return commandNames(g.commands)
	}
	return nil
}

func isFlag(s string) bool { return len(s) > 1 && s[0] == '-' }

// flagName reads the name out of a flag word, and whether the word carried
// its own value: "-min=60s" did, "-min" takes the next word.
func flagName(s string) (name string, hasValue bool) {
	name = strings.TrimLeft(s, "-")
	name, _, hasValue = strings.Cut(name, "=")
	return name, hasValue
}

// takesValue reports whether a flag consumes the word after it. A boolean
// flag is a switch; anything else wants a value.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}

func flagCandidates(fs *flag.FlagSet, typed string) []string {
	dashes := "-"
	if strings.HasPrefix(typed, "--") {
		dashes = "--"
	}
	var out []string
	fs.VisitAll(func(f *flag.Flag) { out = append(out, dashes+f.Name) })
	return out
}

// deviceCandidates offers the devices a command could be aimed at: those
// whose interview found the command's cluster, or every device when the
// command has no cluster of its own. A device that has not been interviewed
// is not offered as a light, because nothing is known about it — which is the
// point of drawing completion from the interviews rather than the registry.
func (g grammar) deviceCandidates(scope uint16) []string {
	pool := g.devices
	if scope != 0 {
		pool = withCluster(g.devices, scope)
	}
	out := make([]string, 0, len(pool))
	for _, d := range pool {
		out = append(out, deviceWord(d))
	}
	return out
}

// deviceWord is how a device is typed: by name where it has one, otherwise
// by the address that identifies it.
func deviceWord(d zigbee.Device) string {
	switch {
	case d.Name != "":
		return d.Name
	case d.IEEE != "":
		return d.IEEE
	default:
		return fmt.Sprintf("0x%04X", d.NodeID)
	}
}

// deviceAt resolves the device already typed for a command, or nil when it
// has not been typed yet or names nothing.
func (g grammar) deviceAt(cmd replCommand, positionals []string) *zigbee.Device {
	i := slices.Index(cmd.shape, argDevice)
	if i < 0 || i >= len(positionals) {
		return nil
	}
	r, err := resolveDevice(g.devices, positionals[i], cmd.scope)
	if err != nil {
		return nil
	}
	return r.device
}

// clusterAt reads the cluster already typed for a command.
func clusterAt(cmd replCommand, positionals []string) (uint16, bool) {
	i := slices.Index(cmd.shape, argCluster)
	if i < 0 || i >= len(positionals) {
		return 0, false
	}
	return zigbee.ParseCluster(positionals[i])
}

// attributesAt lists the attributes already typed, so that Tab does not offer
// one twice.
func attributesAt(cmd replCommand, positionals []string) []string {
	var used []string
	for i, p := range positionals {
		if kind, ok := cmd.kindAt(i); ok && kind == argAttribute {
			used = append(used, p)
		}
	}
	return used
}

// clusterCandidates offers the clusters a device carries, as its interview
// found them, or every cluster gzb can name for a device that has not been
// interviewed.
func clusterCandidates(device *zigbee.Device) []string {
	if device == nil || len(device.Endpoints) == 0 {
		var out []string
		for _, id := range zigbee.KnownClusters() {
			out = append(out, clusterWord(id))
		}
		return out
	}
	var out []string
	for _, ep := range device.Endpoints {
		for _, in := range ep.Input {
			out = append(out, clusterWord(in.ID))
		}
	}
	return out
}

// clusterWord is how a cluster is typed: by the name gzb prints for it where
// that name parses back, otherwise as the hex ParseCluster accepts. A
// manufacturer cluster prints as "manufacturer 0xFC11", which is a
// description rather than something to type.
func clusterWord(id uint16) string {
	name := zigbee.ClusterName(id)
	if parsed, ok := zigbee.ParseCluster(name); ok && parsed == id {
		return name
	}
	return fmt.Sprintf("0x%04X", id)
}

func attributeCandidates(cluster uint16, used []string) []string {
	var out []string
	for _, id := range zigbee.KnownAttributes(cluster) {
		name := zigbee.AttributeName(cluster, id)
		if slices.ContainsFunc(used, func(u string) bool { return strings.EqualFold(u, name) }) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// valueCandidates offers the values an attribute can take, where that is a
// short list: a boolean is, a level is not.
func valueCandidates(cluster uint16, attribute string) []string {
	attr, ok := zigbee.ParseAttribute(cluster, attribute)
	if !ok {
		return nil
	}
	if t, ok := zigbee.AttributeType(cluster, attr); ok && t == zigbee.TypeBool {
		return []string{"true", "false"}
	}
	return nil
}

// actionCandidates offers the words of the light grammar a device
// understands: "red" to a lamp with the Colour Control cluster, "brighter"
// to one that dims, "on" to anything with a switch. The patterns — a
// percentage, a hex colour — are not words and are not offered.
func actionCandidates(device *zigbee.Device) []string {
	var out []string
	for _, w := range zigbee.ActionWords() {
		action, ok := zigbee.ParseAction(w)
		if !ok {
			continue
		}
		if device != nil && len(device.Endpoints) > 0 {
			if _, ok := device.Endpoint(action.Cluster()); !ok {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// filterPrefix keeps the candidates that start with what was typed, ignoring
// case, in a stable order with no repeats.
func filterPrefix(candidates []string, typed string) []string {
	fold := strings.ToLower(typed)
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(strings.ToLower(c), fold) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// commonPrefix is how far a set of candidates agree, which is how far Tab
// can go without choosing between them.
func commonPrefix(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	prefix := candidates[0]
	for _, c := range candidates[1:] {
		for !strings.HasPrefix(c, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

// quoteWord writes a candidate the way tokenize will read it back.
func quoteWord(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + s + `"`
	}
	return s
}

// quotePartial writes an unfinished candidate: an opening quote if any of the
// candidates it may become needs one, so that the space inside "living room"
// does not end the word when the person keeps typing.
func quotePartial(prefix string, candidates []string) string {
	for _, c := range candidates {
		if strings.ContainsAny(c, " \t\"") {
			return `"` + prefix
		}
	}
	return prefix
}
