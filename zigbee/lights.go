package zigbee

// Operating a light, in the words a person uses for it.
//
// This layer exists so that "light1 red" and "light1 brighter" have somewhere
// to land that is not a command-line flag parser. The grammar is parsed once,
// here, into Actions; a caller — the CLI today, a REPL later — supplies the
// words and does not need to know that "brighter" is a Step command on cluster
// 0x0008 while "red" may be any of three different commands on 0x0300
// depending on what the lamp turns out to support.
//
// Nothing here is specific to a model. Every command is standard ZCL, and the
// one decision that varies between lamps — how to be told a colour — is made
// by asking the lamp rather than by keeping a table of them.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chorankates/gzb/internal/zcl"
)

// Light addresses a light: a node, and the endpoint its control clusters live
// on. It names no cluster, unlike a Target, because a light is several of them
// — on/off, level and colour are three clusters on one endpoint.
type Light struct {
	Node     uint16
	Endpoint uint8
}

func (l Light) String() string { return fmt.Sprintf("0x%04X endpoint %d", l.Node, l.Endpoint) }

// target addresses one of the light's clusters.
func (l Light) target(cluster uint16) Target {
	return Target{Node: l.Node, Endpoint: l.Endpoint, Cluster: cluster}
}

// The clusters a light is operated through. They are exported because
// resolving which endpoint of a device is the light means asking which one
// carries them, and a caller doing that should not have to hardcode 0x0006.
const (
	ClusterOnOff        = zcl.ClusterOnOff
	ClusterLevelControl = zcl.ClusterLevelControl
	ClusterColorControl = zcl.ClusterColorControl
)

// Level bounds. The Level Control cluster runs 1 to 254: 0 is not "off", and
// 255 is not "full", both of which are easier to get wrong than to notice.
const (
	MinLevel uint8 = 1
	MaxLevel uint8 = 254
)

// levelStep is how far "brighter" and "dimmer" move, a quarter of the scale.
// Big enough to see, small enough that saying it twice is still useful.
const levelStep = 64

// ColorCapability is a bit of the Colour Control cluster's ColorCapabilities
// attribute, which is how a lamp says what it can be told.
type ColorCapability uint16

const (
	ColorHueSaturation ColorCapability = 1 << 0
	ColorEnhancedHue   ColorCapability = 1 << 1
	ColorLoop          ColorCapability = 1 << 2
	ColorXY            ColorCapability = 1 << 3
	ColorTemperature   ColorCapability = 1 << 4
)

// Has reports whether a capability is present.
func (c ColorCapability) Has(want ColorCapability) bool { return c&want != 0 }

func (c ColorCapability) String() string {
	var parts []string
	for _, named := range []struct {
		bit  ColorCapability
		name string
	}{
		{ColorHueSaturation, "hue/saturation"},
		{ColorEnhancedHue, "enhanced hue"},
		{ColorLoop, "colour loop"},
		{ColorXY, "xy"},
		{ColorTemperature, "colour temperature"},
	} {
		if c.Has(named.bit) {
			parts = append(parts, named.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// colorCapabilitiesAttribute is ColorCapabilities (0x400A).
const colorCapabilitiesAttribute uint16 = 0x400A

// ColorCapabilities asks a light how it can be told a colour.
//
// A lamp too old to implement the attribute — it was optional before ZCL 6 —
// is taken to do hue and saturation, which is what such a lamp did.
func (c *Coordinator) ColorCapabilities(ctx context.Context, l Light) (ColorCapability, error) {
	values, err := c.ReadAttributes(ctx, l.target(zcl.ClusterColorControl), []uint16{colorCapabilitiesAttribute})
	if err != nil {
		return 0, err
	}
	for _, value := range values {
		if value.ID != colorCapabilitiesAttribute {
			continue
		}
		if value.Status != "" {
			return ColorHueSaturation, nil
		}
		if bits, ok := value.Value.(uint64); ok {
			return ColorCapability(bits), nil
		}
	}
	return ColorHueSaturation, nil
}

// Switch turns a light on or off.
func (c *Coordinator) Switch(ctx context.Context, l Light, on bool) error {
	cmd := zcl.CmdOff
	if on {
		cmd = zcl.CmdOn
	}
	return c.SendCommand(ctx, l.target(zcl.ClusterOnOff), cmd, nil)
}

// Toggle inverts a light's on/off state, without having to know it first.
func (c *Coordinator) Toggle(ctx context.Context, l Light) error {
	return c.SendCommand(ctx, l.target(zcl.ClusterOnOff), zcl.CmdToggle, nil)
}

// SetLevel puts a light to a brightness.
//
// This is Move to Level, not Move to Level (with On/Off): it sets how bright
// the lamp is without deciding whether it is lit. A light that is off stays
// off, which is what makes it possible to set up a motion light in daylight.
func (c *Coordinator) SetLevel(ctx context.Context, l Light, level uint8, transition time.Duration) error {
	payload := zcl.MoveToLevelPayload(clampLevel(level), tenths(transition))
	return c.SendCommand(ctx, l.target(zcl.ClusterLevelControl), zcl.CmdMoveToLevel, payload)
}

// StepLevel moves a light's brightness by an amount, which is what "brighter"
// means. The device does the arithmetic against whatever it is currently at,
// so this needs no read first and two in a row compose.
func (c *Coordinator) StepLevel(ctx context.Context, l Light, delta int, transition time.Duration) error {
	mode, size := zcl.StepUp, delta
	if delta < 0 {
		mode, size = zcl.StepDown, -delta
	}
	if size > int(MaxLevel) {
		size = int(MaxLevel)
	}
	payload := zcl.StepLevelPayload(mode, uint8(size), tenths(transition))
	return c.SendCommand(ctx, l.target(zcl.ClusterLevelControl), zcl.CmdStepLevel, payload)
}

// SetOnLevel writes the brightness a light returns to when something turns it
// on — including something that is not gzb, such as the lamp's own motion
// sensor.
//
// This is the difference between a light that is dim now and one that is dim
// the next time it lights up. Move to Level changes what the lamp is doing;
// OnLevel changes what it will do. A motion light configured while it is dark
// needs the second, because nothing gzb sends is what turns it on.
func (c *Coordinator) SetOnLevel(ctx context.Context, l Light, level uint8) error {
	results, err := c.WriteAttributes(ctx, l.target(zcl.ClusterLevelControl), []AttributeWrite{
		{ID: onLevelAttribute, Type: TypeUint8, Value: uint64(clampLevel(level))},
	})
	if err != nil {
		return err
	}
	for _, result := range results {
		if !result.OK {
			return fmt.Errorf("zigbee: %s refused a write to %s: %s", l, result.Name, result.Status)
		}
	}
	return nil
}

// onLevelAttribute is OnLevel (0x0011) on the Level Control cluster.
const onLevelAttribute uint16 = 0x0011

// SetColor puts a light to a colour, choosing the command from what the lamp
// says it can do rather than from what model it is.
//
// The order of preference is the order of fidelity. Hue and saturation is what
// most lamps implement and what the colour was probably named in; xy says the
// same thing in absolute terms; a colour temperature can only express a white
// point, so a lamp offering nothing else is told so rather than being sent an
// approximation of red that would come out beige.
func (c *Coordinator) SetColor(ctx context.Context, l Light, color Color, transition time.Duration) error {
	caps, err := c.ColorCapabilities(ctx, l)
	if err != nil {
		return err
	}
	t := l.target(zcl.ClusterColorControl)
	switch {
	case color.IsWhitePoint() && caps.Has(ColorTemperature):
		// A white point on a lamp that has the cluster for it: say it in the
		// terms it was meant in rather than converting to a hue first.
		return c.SendCommand(ctx, t, zcl.CmdMoveToColorTemperature,
			zcl.MoveToColorTemperaturePayload(color.Mireds, tenths(transition)))

	case caps.Has(ColorHueSaturation):
		hue, saturation := color.HueSaturation()
		return c.SendCommand(ctx, t, zcl.CmdMoveToHueAndSaturation,
			zcl.MoveToHueAndSaturationPayload(hue, saturation, tenths(transition)))

	case caps.Has(ColorXY):
		x, y := color.XY()
		return c.SendCommand(ctx, t, zcl.CmdMoveToColor,
			zcl.MoveToColorPayload(x, y, tenths(transition)))

	case caps.Has(ColorTemperature):
		return fmt.Errorf("zigbee: %s can only be told a colour temperature, and %s is not one (try warm, cool, or a value like 2700k)", l, color)

	default:
		return fmt.Errorf("zigbee: %s reports no colour capability at all (%s)", l, caps)
	}
}

// Verb is what an Action does.
type Verb int

const (
	VerbOn Verb = iota
	VerbOff
	VerbToggle
	// VerbLevel sets an absolute brightness; VerbStep moves it by an amount.
	VerbLevel
	VerbStep
	VerbColor
)

// Action is one thing to do to a light, in the terms a person said it.
//
// Word is kept so that a caller can echo back what it understood. In a REPL
// that is most of the interface: the difference between "dim" and "dimmer" is
// one letter and two quite different commands, and showing which one was heard
// is cheaper than explaining the grammar.
type Action struct {
	Verb  Verb
	Word  string
	Level uint8
	Step  int
	Color Color
}

// Cluster reports which cluster an action is sent to, which is also what a
// device has to implement for the word to mean anything to it. A completion
// of "light1 <tab>" uses this to offer "red" only to a lamp that carries the
// Colour Control cluster, and "brighter" only to one that dims.
func (a Action) Cluster() uint16 {
	switch a.Verb {
	case VerbOn, VerbOff, VerbToggle:
		return ClusterOnOff
	case VerbLevel, VerbStep:
		return ClusterLevelControl
	case VerbColor:
		return ClusterColorControl
	}
	return 0
}

func (a Action) String() string {
	switch a.Verb {
	case VerbOn:
		return "on"
	case VerbOff:
		return "off"
	case VerbToggle:
		return "toggle"
	case VerbLevel:
		return fmt.Sprintf("level %d (%.0f%%)", a.Level, float64(a.Level)*100/float64(MaxLevel))
	case VerbStep:
		direction := "brighter"
		if a.Step < 0 {
			direction = "dimmer"
		}
		return fmt.Sprintf("%s by %d", direction, abs(a.Step))
	case VerbColor:
		return a.Color.String()
	}
	return a.Word
}

// levelWords are the absolute brightnesses that have names.
//
// The pattern is worth stating because it is the whole grammar: a plain
// adjective is a place to go to, and its comparative is a distance to move.
// "dim" puts a light at a quarter; "dimmer" takes a quarter off wherever it
// happens to be.
var levelWords = map[string]uint8{
	"full":   MaxLevel,
	"bright": MaxLevel,
	"half":   MaxLevel / 2,
	"dim":    MaxLevel / 4,
	"low":    MaxLevel / 4,
	"min":    MinLevel,
	"faint":  MinLevel,
}

// stepWords are the relative ones.
var stepWords = map[string]int{
	"brighter": levelStep,
	"up":       levelStep,
	"dimmer":   -levelStep,
	"darker":   -levelStep,
	"down":     -levelStep,
}

// switchWords are the on/off ones.
var switchWords = map[string]Verb{
	"on":     VerbOn,
	"off":    VerbOff,
	"toggle": VerbToggle,
}

// ParseAction resolves a single word of the light grammar.
func ParseAction(word string) (Action, bool) {
	text := strings.ToLower(strings.TrimSpace(word))
	if text == "" {
		return Action{}, false
	}
	if verb, ok := switchWords[text]; ok {
		return Action{Verb: verb, Word: text}, true
	}
	if level, ok := levelWords[text]; ok {
		return Action{Verb: VerbLevel, Word: text, Level: level}, true
	}
	if step, ok := stepWords[text]; ok {
		return Action{Verb: VerbStep, Word: text, Step: step}, true
	}
	// Exact forms, in the cluster's own units. They exist so a value read off
	// a device can be put back exactly: "blue" is a colour someone chose, but
	// hue 169 is the colour this lamp was actually at, and a restore that
	// rounds it to the nearest name has not restored anything.
	if raw, ok := strings.CutPrefix(text, "level:"); ok {
		n, err := strconv.ParseUint(raw, 10, 8)
		if err != nil || n > uint64(MaxLevel) {
			return Action{}, false
		}
		return Action{Verb: VerbLevel, Word: text, Level: uint8(n)}, true
	}
	if raw, ok := strings.CutPrefix(text, "hue:"); ok {
		color, ok := parseRawHueSaturation(raw)
		if !ok {
			return Action{}, false
		}
		return Action{Verb: VerbColor, Word: text, Color: color}, true
	}
	if percent, ok := strings.CutSuffix(text, "%"); ok {
		n, err := strconv.ParseFloat(percent, 64)
		if err != nil || n < 0 || n > 100 {
			return Action{}, false
		}
		return Action{Verb: VerbLevel, Word: text, Level: levelFromPercent(n)}, true
	}
	if color, ok := ParseColor(text); ok {
		return Action{Verb: VerbColor, Word: text, Color: color}, true
	}
	return Action{}, false
}

// parseRawHueSaturation reads "169/254": a hue and a saturation in the 0-254
// units the cluster itself uses.
//
// The pair is converted into the device-independent form and back rather than
// carried specially, which works because the round trip is exact by
// construction: a hue of h·360/254 degrees renders as h again.
func parseRawHueSaturation(s string) (Color, bool) {
	hue, saturation, found := strings.Cut(s, "/")
	if !found {
		return Color{}, false
	}
	h, err := strconv.ParseUint(hue, 10, 8)
	if err != nil || h > 254 {
		return Color{}, false
	}
	sat, err := strconv.ParseUint(saturation, 10, 8)
	if err != nil || sat > 254 {
		return Color{}, false
	}
	return ExactHueSaturation(uint8(h), uint8(sat)), true
}

// ParseActions resolves a whole phrase, so that "red dim" is two actions and
// not an error about a colour called "red dim".
//
// Order is preserved and acted on in order, because that is the only reading
// that stays true as the vocabulary grows: "dim brighter" is a person changing
// their mind, and the light should end up where they left off.
func ParseActions(words []string) ([]Action, error) {
	actions := make([]Action, 0, len(words))
	for _, word := range words {
		action, ok := ParseAction(word)
		if !ok {
			return nil, fmt.Errorf("zigbee: %q is not something a light can be told (try one of: %s)", word, strings.Join(ActionWords(), ", "))
		}
		actions = append(actions, action)
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("zigbee: nothing to do")
	}
	return actions, nil
}

// ActionWords lists the vocabulary, for help text and for a REPL completing a
// half-typed word. Colours are included because to a person they are the same
// kind of thing.
func ActionWords() []string {
	words := make([]string, 0, len(switchWords)+len(levelWords)+len(stepWords))
	for word := range switchWords {
		words = append(words, word)
	}
	for word := range levelWords {
		words = append(words, word)
	}
	for word := range stepWords {
		words = append(words, word)
	}
	words = append(words, ColorNames()...)
	words = append(words, WhitePointNames()...)
	words = append(words, "N%", "#rrggbb", "NNNNk", "level:N", "hue:H/S")
	sort.Strings(words)
	return words
}

// Apply carries out a phrase against a light, in the order it was said.
//
// A failure stops the rest: a light that refused to change colour is in a
// state nobody asked for, and pressing on to also dim it makes that harder to
// see rather than easier.
func (c *Coordinator) Apply(ctx context.Context, l Light, actions []Action, transition time.Duration) error {
	for _, action := range actions {
		var err error
		switch action.Verb {
		case VerbOn:
			err = c.Switch(ctx, l, true)
		case VerbOff:
			err = c.Switch(ctx, l, false)
		case VerbToggle:
			err = c.Toggle(ctx, l)
		case VerbLevel:
			err = c.SetLevel(ctx, l, action.Level, transition)
		case VerbStep:
			err = c.StepLevel(ctx, l, action.Step, transition)
		case VerbColor:
			err = c.SetColor(ctx, l, action.Color, transition)
		default:
			err = fmt.Errorf("zigbee: no such action %q", action.Word)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	return nil
}

// levelFromPercent maps a percentage onto the cluster's 1-254.
//
// Zero percent is the dimmest the lamp goes, not off: turning a light off is
// the on/off cluster's business, and quietly conflating the two would make
// "0%" and "off" differ in whether the lamp can be turned back up remotely.
func levelFromPercent(percent float64) uint8 {
	level := math.Round(percent / 100 * float64(MaxLevel))
	return clampLevel(uint8(math.Max(0, math.Min(float64(MaxLevel), level))))
}

func clampLevel(level uint8) uint8 {
	if level < MinLevel {
		return MinLevel
	}
	if level > MaxLevel {
		return MaxLevel
	}
	return level
}

// tenths converts a transition to the tenths of a second the wire carries.
// Anything longer than the field holds is capped rather than wrapped, since a
// wrapped transition is a lamp that snaps instead of fading.
func tenths(d time.Duration) uint16 {
	if d <= 0 {
		return 0
	}
	t := d.Round(100*time.Millisecond) / (100 * time.Millisecond)
	if t > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(t)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
