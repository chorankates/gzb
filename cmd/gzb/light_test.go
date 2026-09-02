package main

import (
	"testing"

	"github.com/chorankates/gzb/zigbee"
)

// --persist writes the level a light returns to, which needs an absolute
// brightness to write. A phrase that only stepped the level named no such
// number, and picking one would mean inventing the answer.
func TestLastAbsoluteLevel(t *testing.T) {
	actions, err := zigbee.ParseActions([]string{"on", "red", "half", "dim"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	// The last one wins, for the same reason the actions are applied in order:
	// someone who said two brightnesses changed their mind about the first.
	level, ok := lastAbsoluteLevel(actions)
	if !ok || level != zigbee.MaxLevel/4 {
		t.Errorf("level = %d (ok=%v), want the quarter that `dim` named", level, ok)
	}

	stepped, err := zigbee.ParseActions([]string{"red", "dimmer"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	if _, ok := lastAbsoluteLevel(stepped); ok {
		t.Error("a phrase that only stepped the level offered a level to persist")
	}
}

// The values here are the ones both lights on this network actually returned:
// blue at hue 169 / saturation 254 and level 127 before the change, red at hue
// 0 and level 63 after it. They are the same values the README quotes, and
// pinning the rendering to them is what stops a block there drifting from what
// the code prints.
//
// `make recapture DEVICE=<a light>` produces the transcripts these back.

func TestRenderLightPlan(t *testing.T) {
	actions, err := zigbee.ParseActions([]string{"red", "dim"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	got := capture(t, func() {
		printLightPlan("light1", zigbee.Light{Node: 0xA489, Endpoint: 1}, actions)
	})
	want := "" +
		"light1 (0xA489) endpoint 1\n" +
		"  hue 0°, saturation 100%\n" +
		"  level 63 (25%)\n"
	if got != want {
		t.Errorf("plan rendered as\n%s\nwant\n%s", got, want)
	}
}

// A step and a level must not render alike, or the echo cannot do the one job
// it has: showing which of "dim" and "dimmer" was heard.
func TestRenderLightPlanDistinguishesStepsFromLevels(t *testing.T) {
	actions, err := zigbee.ParseActions([]string{"dimmer", "brighter"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	got := capture(t, func() {
		printLightPlan("light1", zigbee.Light{Node: 0xA489, Endpoint: 1}, actions)
	})
	want := "" +
		"light1 (0xA489) endpoint 1\n" +
		"  dimmer by 64\n" +
		"  brighter by 64\n"
	if got != want {
		t.Errorf("steps rendered as\n%s\nwant\n%s", got, want)
	}
}

func TestRenderWhitePointAndSwitching(t *testing.T) {
	actions, err := zigbee.ParseActions([]string{"on", "warm", "40%"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	got := capture(t, func() {
		printLightPlan("hallway lamp", zigbee.Light{Node: 0x919A, Endpoint: 1}, actions)
	})
	want := "" +
		"hallway lamp (0x919A) endpoint 1\n" +
		"  on\n" +
		"  370 mireds (2703 K)\n" +
		"  level 102 (40%)\n"
	if got != want {
		t.Errorf("phrase rendered as\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOnLevel(t *testing.T) {
	got := capture(t, func() {
		printOnLevel(63)
		printLightDone()
	})
	want := "" +
		"  on level 63 — what it returns to when something else switches it on\n" +
		"  ok\n"
	if got != want {
		t.Errorf("on level rendered as\n%s\nwant\n%s", got, want)
	}
}
