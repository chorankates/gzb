package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/chorankates/gzb/zigbee"
)

func fixtureGrammar() grammar {
	return grammar{commands: replCommands(), devices: fixtureDevices()}
}

// candidatesFor is what Tab would offer at the end of line.
func candidatesFor(t *testing.T, line string) []string {
	t.Helper()
	return fixtureGrammar().complete(line).candidates
}

func TestTokenizeKeepsQuotedNamesTogether(t *testing.T) {
	words := tokenize(`light  "living room thermo" on`)
	texts := make([]string, 0, len(words))
	for _, w := range words {
		texts = append(texts, w.text)
	}
	if want := []string{"light", "living room thermo", "on"}; !slices.Equal(texts, want) {
		t.Errorf("tokenize = %q, want %q", texts, want)
	}
	// The name's start is the quote, since that is what gets replaced.
	if words[1].start != 7 || words[1].end != 27 || words[1].open {
		t.Errorf("quoted word = %+v", words[1])
	}

	// A quote still open at the end of the line is a word still being typed.
	words = tokenize(`read "living`)
	if len(words) != 2 || words[1].text != "living" || !words[1].open {
		t.Errorf("open quote tokenized as %+v", words)
	}
}

func TestCommandsCompleteFirst(t *testing.T) {
	all := candidatesFor(t, "")
	for _, want := range []string{"light", "read", "help", "quit"} {
		if !slices.Contains(all, want) {
			t.Errorf("an empty line does not offer %q: %v", want, all)
		}
	}
	if slices.Contains(all, "exit") {
		t.Error("the exit alias is offered alongside quit")
	}
	if got := candidatesFor(t, "li"); !slices.Equal(got, []string{"light"}) {
		t.Errorf("\"li\" completes to %v", got)
	}
	if got := candidatesFor(t, "help re"); !slices.Equal(got, []string{"read", "reporting"}) {
		t.Errorf("\"help re\" completes to %v", got)
	}
}

// `light <tab>` is the headline: the lights, and only the lights, and only
// the ones an interview has said are lights.
func TestLightOffersOnlyInterviewedLights(t *testing.T) {
	if got := candidatesFor(t, "light "); !slices.Equal(got, []string{"light1", "light2"}) {
		t.Errorf("light <tab> = %v, want the two lights", got)
	}
	if got := candidatesFor(t, "light LIGHT2"); !slices.Equal(got, []string{"light2"}) {
		t.Errorf("case-insensitive prefix gave %v", got)
	}
}

// What a light can be told follows what its interview found. A colour lamp
// gets the colours; a plain switch gets on and off and nothing about red.
func TestLightWordsFollowTheDevicesClusters(t *testing.T) {
	words := candidatesFor(t, "light 1 ")
	for _, want := range []string{"on", "off", "toggle", "dim", "brighter", "red", "warm"} {
		if !slices.Contains(words, want) {
			t.Errorf("light 1 <tab> lacks %q: %v", want, words)
		}
	}
	for _, pattern := range []string{"N%", "#rrggbb", "level:N"} {
		if slices.Contains(words, pattern) {
			t.Errorf("light 1 <tab> offers the pattern %q, which is not a word", pattern)
		}
	}

	plug := zigbee.Device{
		Name: "plug", NodeID: 0x0033, IEEE: "00:00:00:00:00:00:00:33", Interviewed: fixtureDevices()[0].Interviewed,
		Endpoints: []zigbee.Endpoint{endpoint(1, 0x0000, zigbee.ClusterOnOff)},
	}
	g := grammar{commands: replCommands(), devices: append(fixtureDevices(), plug)}
	if got := g.complete("light plug ").candidates; !slices.Equal(got, []string{"off", "on", "toggle"}) {
		t.Errorf("a plain switch is offered %v", got)
	}
	// After one word, another: the phrase is any number of them.
	if got := g.complete("light plug on to").candidates; !slices.Equal(got, []string{"toggle"}) {
		t.Errorf("second word completes to %v", got)
	}
}

func TestReadOffersTheClustersAnInterviewFound(t *testing.T) {
	got := candidatesFor(t, `read "living room thermo" `)
	if want := []string{"basic", "humidity", "power", "temperature"}; !slices.Equal(got, want) {
		t.Errorf("clusters = %v, want %v", got, want)
	}
	// A device nothing has interviewed could have anything, so everything
	// gzb can name is offered.
	got = candidatesFor(t, "read door1 ")
	if !slices.Contains(got, "on/off") || !slices.Contains(got, "temperature") {
		t.Errorf("an uninterviewed device is offered %v", got)
	}
}

// A cluster gzb has no name for is offered as the hex ParseCluster accepts,
// not as the description ClusterName prints.
func TestUnnamedClustersAreOfferedAsHex(t *testing.T) {
	if got := clusterWord(0xFC11); got != "0xFC11" {
		t.Errorf("clusterWord(0xFC11) = %q", got)
	}
	if got := clusterWord(zigbee.ClusterOnOff); got != "on/off" {
		t.Errorf("clusterWord(on/off) = %q", got)
	}
}

func TestAttributesFollowTheClusterAndAreNotOfferedTwice(t *testing.T) {
	got := candidatesFor(t, "read light1 color ")
	for _, want := range []string{"hue", "saturation", "color temperature"} {
		if !slices.Contains(got, want) {
			t.Errorf("color <tab> lacks %q: %v", want, got)
		}
	}
	got = candidatesFor(t, "read light1 color hue ")
	if slices.Contains(got, "hue") {
		t.Errorf("hue is offered again after being typed: %v", got)
	}
	if !slices.Contains(got, "saturation") {
		t.Errorf("saturation is missing after hue: %v", got)
	}
}

// write alternates attribute and value; a boolean's values are worth
// offering, and after a value comes the next attribute.
func TestWriteAlternatesAttributesAndValues(t *testing.T) {
	if got := candidatesFor(t, "write light1 on/off on/off "); !slices.Equal(got, []string{"false", "true"}) {
		t.Errorf("a boolean's values = %v", got)
	}
	if got := candidatesFor(t, "write light1 level level "); got != nil {
		t.Errorf("a level offers %v, want nothing", got)
	}
	got := candidatesFor(t, "write light1 on/off on/off true ")
	if !slices.Contains(got, "on time") || slices.Contains(got, "on/off") {
		t.Errorf("after a pair, the next attribute = %v", got)
	}
}

// Flags come before the device and are the command's own; a flag that takes
// a value swallows the next word, and a switch does not.
func TestFlagsCompleteBeforeTheDevice(t *testing.T) {
	got := candidatesFor(t, "light -")
	if want := []string{"-endpoint", "-persist", "-timeout", "-transition"}; !slices.Equal(got, want) {
		t.Errorf("light -<tab> = %v, want %v", got, want)
	}
	if got := candidatesFor(t, "light --tr"); !slices.Equal(got, []string{"--transition"}) {
		t.Errorf("double-dash form = %v", got)
	}
	if got := candidatesFor(t, "light -transition "); got != nil {
		t.Errorf("a flag's value is offered %v", got)
	}
	if got := candidatesFor(t, "light -transition 2s "); !slices.Equal(got, []string{"light1", "light2"}) {
		t.Errorf("after a valued flag = %v", got)
	}
	if got := candidatesFor(t, "light -persist "); !slices.Equal(got, []string{"light1", "light2"}) {
		t.Errorf("after a switch = %v", got)
	}
	if got := candidatesFor(t, "light -transition=2s l"); !slices.Equal(got, []string{"light1", "light2"}) {
		t.Errorf("after a flag with its value attached = %v", got)
	}
	// Commands with no positional arguments still have their flags.
	if got := candidatesFor(t, "join -"); !slices.Equal(got, []string{"-verbose"}) {
		t.Errorf("join -<tab> = %v", got)
	}
	if got := candidatesFor(t, "monitor -"); !slices.Equal(got, []string{"-for", "-raw"}) {
		t.Errorf("monitor -<tab> = %v", got)
	}
}

func TestCompletionStartsAtTheWordIncludingItsQuote(t *testing.T) {
	c := fixtureGrammar().complete(`read "liv`)
	if c.start != 5 {
		t.Errorf("start = %d, want 5, the opening quote", c.start)
	}
	if !slices.Equal(c.candidates, []string{"living room thermo"}) {
		t.Errorf("candidates = %v", c.candidates)
	}
}

// What Tab does to the line: one candidate replaces the word, quoted if it
// needs to be, and moves on; several go as far as they agree; and when that
// is nowhere, they are shown instead.
func TestCompleteAtEditsTheLine(t *testing.T) {
	one := completion{start: 5, candidates: []string{"living room thermo"}}
	line, pos, changed, show := completeAt(`read "liv`, 9, one)
	if !changed || show || line != `read "living room thermo" ` || pos != len(line) {
		t.Errorf("one candidate: %q at %d (changed %v, show %v)", line, pos, changed, show)
	}

	// The cursor in the middle of a line: what is after it stays.
	line, pos, changed, _ = completeAt("light l off", 7, completion{start: 6, candidates: []string{"light1"}})
	if !changed || line != "light light1  off" || pos != 13 {
		t.Errorf("mid-line: %q at %d", line, pos)
	}

	several := completion{start: 6, candidates: []string{"light1", "light2"}}
	line, pos, changed, show = completeAt("light l", 7, several)
	if !changed || show || line != "light light" || pos != 11 {
		t.Errorf("common prefix: %q at %d (changed %v, show %v)", line, pos, changed, show)
	}
	_, _, changed, show = completeAt("light light", 11, several)
	if changed || !show {
		t.Errorf("no further to go: changed %v, show %v", changed, show)
	}

	// Candidates with spaces open a quote as soon as the prefix is shared.
	names := completion{start: 5, candidates: []string{"living room lamp", "living room thermo"}}
	line, _, changed, _ = completeAt("read l", 6, names)
	if !changed || line != `read "living room ` {
		t.Errorf("shared prefix with spaces: %q", line)
	}
	// But a quote on its own is not progress: candidates that share nothing
	// are shown, not quoted. The colour attributes are the case that found
	// this — "hue" and "color temperature" agree on no letter at all.
	attrs := completion{start: 13, candidates: []string{"color temperature", "hue", "saturation"}}
	_, _, changed, show = completeAt("read 1 color ", 13, attrs)
	if changed || !show {
		t.Errorf("no shared prefix: changed %v, show %v; want the list", changed, show)
	}
	_, _, changed, show = completeAt(`read 1 color "`, 14, attrs)
	if changed || !show {
		t.Errorf("after a lone quote: changed %v, show %v; want the list", changed, show)
	}
}

func TestColumnsFillDownThenAcross(t *testing.T) {
	got := columns([]string{"a", "bb", "ccc", "dddd", "e"}, 14)
	want := []string{
		"  a     dddd",
		"  bb    e",
		"  ccc",
	}
	if !slices.Equal(got, want) {
		t.Errorf("columns =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The prompt's own commands need no coordinator, and neither does telling
// someone a command does not exist.
func TestRunHandlesWhatNeedsNoDevice(t *testing.T) {
	s := &session{g: &globals{}, commands: replCommands()}
	ctx := context.Background()

	if err := s.run(ctx, "quit"); !errors.Is(err, errQuit) {
		t.Errorf("quit = %v", err)
	}
	if err := s.run(ctx, "exit"); !errors.Is(err, errQuit) {
		t.Errorf("exit = %v", err)
	}
	if err := s.run(ctx, "   "); err != nil {
		t.Errorf("a blank line = %v", err)
	}
	if err := s.run(ctx, "bogus"); err == nil || !strings.Contains(err.Error(), "help") {
		t.Errorf("an unknown command = %v, want a pointer at help", err)
	}

	out := capture(t, func() {
		if err := s.run(ctx, "help"); err != nil {
			t.Errorf("help = %v", err)
		}
	})
	for _, want := range []string{"light", "read", "interview", "join", "Tab completes"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %q:\n%s", want, out)
		}
	}

	// A join window that would close the network is refused before the
	// window is opened, so it needs no coordinator to be refused by.
	if err := s.run(ctx, "join 0"); err == nil || !strings.Contains(err.Error(), "permit-join") {
		t.Errorf("join 0 = %v, want a pointer at permit-join", err)
	}
	out = capture(t, func() {
		if err := s.run(ctx, "join 1 2"); err != nil {
			t.Errorf("join with two arguments = %v", err)
		}
	})
	if !strings.Contains(out, "usage: gzb join") {
		t.Errorf("join with two arguments printed:\n%s", out)
	}

	// A command short of its arguments shows its usage rather than dialling.
	out = capture(t, func() {
		if err := s.run(ctx, "light"); err != nil {
			t.Errorf("light with no arguments = %v", err)
		}
	})
	if !strings.Contains(out, "usage: gzb light") {
		t.Errorf("light with no arguments printed:\n%s", out)
	}
	out = capture(t, func() {
		if err := s.run(ctx, "read -h"); err != nil {
			t.Errorf("read -h = %v", err)
		}
	})
	if !strings.Contains(out, "usage: gzb read") {
		t.Errorf("read -h printed:\n%s", out)
	}
}
