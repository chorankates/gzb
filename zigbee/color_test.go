package zigbee

import (
	"math"
	"testing"
)

// The sRGB primaries have published chromaticities, and the conversion chain —
// hue to RGB, gamma expansion, the sRGB matrix, normalisation — has to land on
// them. Checking against the published numbers rather than against the code's
// own output is the point: a wrong matrix or a forgotten gamma step is
// self-consistent and still wrong.
func TestNamedColorsLandOnTheSRGBPrimaries(t *testing.T) {
	for _, c := range []struct {
		name   string
		x, y   float64
		within float64
	}{
		{"red", 0.6400, 0.3300, 0.001},
		{"green", 0.3000, 0.6000, 0.001},
		{"blue", 0.1500, 0.0600, 0.001},
	} {
		color, ok := ParseColor(c.name)
		if !ok {
			t.Fatalf("%s is not a colour", c.name)
		}
		x, y := color.chromaticity()
		if math.Abs(x-c.x) > c.within || math.Abs(y-c.y) > c.within {
			t.Errorf("%s = (%.4f, %.4f), want (%.4f, %.4f)", c.name, x, y, c.x, c.y)
		}
	}
}

// The white points have to sit on the Planckian locus, or "warm" is merely a
// shade of orange somebody picked.
func TestWhitePointsFollowThePlanckianLocus(t *testing.T) {
	for _, c := range []struct {
		name string
		x, y float64
	}{
		{"warm", 0.4593, 0.4106}, // 2700 K
		{"cool", 0.3451, 0.3516}, // 5000 K
	} {
		color, ok := ParseColor(c.name)
		if !ok {
			t.Fatalf("%s is not a colour", c.name)
		}
		if !color.IsWhitePoint() {
			t.Errorf("%s is not a white point, so a temperature-only lamp cannot render it", c.name)
		}
		x, y := color.chromaticity()
		if math.Abs(x-c.x) > 0.002 || math.Abs(y-c.y) > 0.002 {
			t.Errorf("%s = (%.4f, %.4f), want (%.4f, %.4f)", c.name, x, y, c.x, c.y)
		}
	}
}

// The hue/saturation fields run 0x00 to 0xFE. Scaling by 255 instead of 254
// puts full saturation one step outside the range the cluster defines.
func TestHueSaturationUsesTheClusterRange(t *testing.T) {
	red, _ := ParseColor("red")
	hue, saturation := red.HueSaturation()
	if hue != 0 || saturation != 254 {
		t.Errorf("red = hue %d, saturation %d, want 0 and 254", hue, saturation)
	}

	// Blue is the value both of the lights on this network were sitting at
	// when the colour path was written, which is what makes it a fact and not
	// a preference: 240° over 360 of a 254-step scale is 169.
	blue, _ := ParseColor("blue")
	hue, saturation = blue.HueSaturation()
	if hue != 169 || saturation != 254 {
		t.Errorf("blue = hue %d, saturation %d, want 169 and 254", hue, saturation)
	}
}

func TestParseColorForms(t *testing.T) {
	if c, ok := ParseColor("2700k"); !ok || c.Mireds != 370 {
		t.Errorf("2700k = %+v (ok=%v), want 370 mireds", c, ok)
	}
	// A hex triplet keeps its hue and saturation and drops its brightness:
	// how bright a lamp burns belongs to the level cluster, and folding it in
	// here would let a colour silently dim a light.
	full, okFull := ParseColor("#ff0000")
	half, okHalf := ParseColor("#800000")
	if !okFull || !okHalf {
		t.Fatal("hex colours did not parse")
	}
	if full.Hue != half.Hue || full.Saturation != half.Saturation {
		t.Errorf("#ff0000 = %+v but #800000 = %+v; brightness leaked into the colour", full, half)
	}
	// Black is not a colour a lamp can be. It is off, which is a different
	// cluster and a different intention.
	if _, ok := ParseColor("#000000"); ok {
		t.Error("black parsed as a colour")
	}
	for _, bad := range []string{"", "reddish", "#ff", "#gggggg", "99999k"} {
		if _, ok := ParseColor(bad); ok {
			t.Errorf("%q parsed as a colour", bad)
		}
	}
}

// A lamp with only hue and saturation still has to be able to render "warm",
// which means the white point has to survive the trip through xy and back to
// something recognisably warm: an orange-ish hue, and not fully saturated.
func TestWhitePointRendersAsAHueForLampsThatNeedOne(t *testing.T) {
	warm, _ := ParseColor("warm")
	hue, saturation := warm.HueSaturation()
	if hue > 40 {
		t.Errorf("warm rendered as hue %d, which is not in the orange end of the scale", hue)
	}
	if saturation == 0 || saturation > 200 {
		t.Errorf("warm rendered at saturation %d, want partly saturated", saturation)
	}
}
