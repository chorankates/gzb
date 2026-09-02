package zigbee

import (
	"fmt"
	"testing"
	"time"
)

// The grammar's whole rule is that a plain word is a place and its comparative
// is a distance. "dim" and "dimmer" differ by one letter and by which ZCL
// command they become, so this is the test that keeps them apart.
func TestPlainWordsAreAbsoluteAndComparativesAreRelative(t *testing.T) {
	dim, ok := ParseAction("dim")
	if !ok || dim.Verb != VerbLevel {
		t.Fatalf("dim = %+v, want an absolute level", dim)
	}
	if dim.Level != MaxLevel/4 {
		t.Errorf("dim = level %d, want a quarter of %d", dim.Level, MaxLevel)
	}

	dimmer, ok := ParseAction("dimmer")
	if !ok || dimmer.Verb != VerbStep {
		t.Fatalf("dimmer = %+v, want a step", dimmer)
	}
	if dimmer.Step >= 0 {
		t.Errorf("dimmer steps by %d, which is not downwards", dimmer.Step)
	}
	if brighter, _ := ParseAction("brighter"); brighter.Step != -dimmer.Step {
		t.Errorf("brighter (%d) and dimmer (%d) are not opposites", brighter.Step, dimmer.Step)
	}
}

func TestParseActionsReadsAWholePhrase(t *testing.T) {
	actions, err := ParseActions([]string{"on", "red", "25%"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions))
	}
	if actions[0].Verb != VerbOn || actions[1].Verb != VerbColor || actions[2].Verb != VerbLevel {
		t.Errorf("phrase parsed as %v", actions)
	}
	// Order is preserved because it is acted on in order: "dim brighter" is a
	// person changing their mind, and the light should end where they left off.
	if actions[2].Level != levelFromPercent(25) {
		t.Errorf("25%% = level %d, want %d", actions[2].Level, levelFromPercent(25))
	}
}

func TestParseActionsRejectsWhatItCannotDo(t *testing.T) {
	if _, err := ParseActions([]string{"red", "sideways"}); err == nil {
		t.Error("accepted a word that is not a light action")
	}
	if _, err := ParseActions(nil); err == nil {
		t.Error("accepted an empty phrase")
	}
	// A percentage outside the scale is a mistake, not something to clamp
	// silently: 150% almost certainly means the person meant a raw level.
	if _, err := ParseActions([]string{"150%"}); err == nil {
		t.Error("accepted 150%")
	}
}

// Zero percent is the dimmest the lamp goes, not off. Conflating them would
// make "0%" and "off" differ in whether the light can be turned back up.
func TestLevelNeverLeavesTheClusterRange(t *testing.T) {
	if got := levelFromPercent(0); got != MinLevel {
		t.Errorf("0%% = %d, want %d", got, MinLevel)
	}
	if got := levelFromPercent(100); got != MaxLevel {
		t.Errorf("100%% = %d, want %d", got, MaxLevel)
	}
	for percent := 0.0; percent <= 100; percent += 0.5 {
		if got := levelFromPercent(percent); got < MinLevel || got > MaxLevel {
			t.Fatalf("%.1f%% = %d, outside %d-%d", percent, got, MinLevel, MaxLevel)
		}
	}
}

// A transition longer than the field holds must cap rather than wrap: a
// wrapped transition is a lamp that snaps instead of fading, which looks like
// the command was ignored.
func TestTransitionTimeCapsRatherThanWraps(t *testing.T) {
	if got := tenths(0); got != 0 {
		t.Errorf("no transition = %d, want 0", got)
	}
	if got := tenths(time.Second); got != 10 {
		t.Errorf("1s = %d tenths, want 10", got)
	}
	if got := tenths(24 * time.Hour); got != 65535 {
		t.Errorf("a day = %d, want the field's maximum", got)
	}
	if got := tenths(-time.Second); got != 0 {
		t.Errorf("a negative transition = %d, want 0", got)
	}
}

func TestColorCapabilityBits(t *testing.T) {
	// 31 is what both of the lights on this network report: everything but
	// the bits above colour temperature.
	caps := ColorCapability(31)
	for _, want := range []ColorCapability{ColorHueSaturation, ColorEnhancedHue, ColorLoop, ColorXY, ColorTemperature} {
		if !caps.Has(want) {
			t.Errorf("31 does not report %s", want)
		}
	}
	if ColorCapability(0).String() != "none" {
		t.Errorf("no capabilities rendered as %q", ColorCapability(0))
	}
	// A lamp that only does colour temperature must not claim hue/saturation,
	// which is the check that decides whether "red" is sent or refused.
	if ColorTemperature.Has(ColorHueSaturation) {
		t.Error("a temperature-only lamp claims to do hue and saturation")
	}
}

// The exact forms exist so a value read off a device can be put back exactly.
// A restore that rounds a hue to the nearest named colour has not restored
// anything, so the round trip has to be lossless for every value the cluster
// can hold.
func TestRawHueAndLevelRoundTripExactly(t *testing.T) {
	for hue := 0; hue <= 254; hue++ {
		for _, sat := range []int{0, 1, 133, 253, 254} {
			word := fmt.Sprintf("hue:%d/%d", hue, sat)
			action, ok := ParseAction(word)
			if !ok {
				t.Fatalf("%s did not parse", word)
			}
			gotHue, gotSat := action.Color.HueSaturation()
			if int(gotHue) != hue || int(gotSat) != sat {
				t.Fatalf("%s round-tripped to %d/%d", word, gotHue, gotSat)
			}
		}
	}
	for level := 0; level <= 254; level++ {
		word := fmt.Sprintf("level:%d", level)
		action, ok := ParseAction(word)
		if !ok {
			t.Fatalf("%s did not parse", word)
		}
		if int(action.Level) != level {
			t.Fatalf("%s round-tripped to %d", word, action.Level)
		}
	}
}

func TestRawFormsRejectWhatTheClusterCannotHold(t *testing.T) {
	for _, bad := range []string{"hue:255/254", "hue:169", "hue:169/255", "hue:a/b", "level:255", "level:x"} {
		if _, ok := ParseAction(bad); ok {
			t.Errorf("%q parsed, and it is outside what the cluster can hold", bad)
		}
	}
}
