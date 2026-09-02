package zigbee

// A colour has to survive being asked for on a device nobody has seen yet.
//
// Zigbee lights disagree about how to be told one: some take hue and
// saturation, some take CIE 1931 xy, some can only shift a white point along
// the Planckian locus, and the same lamp often accepts more than one. So the
// colour a caller names is kept in a device-independent form and converted at
// the last moment, once the device has said what it can actually do.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Color is a colour to put a light to, in terms no device dictates.
//
// It is one of two things, and which one matters when the light turns out to
// be limited. A hue and saturation names a colour; Mireds names a white point,
// which is the only thing a colour-temperature lamp can render and the reason
// "warm" is not simply a pale orange.
type Color struct {
	// Mireds, when non-zero, makes this a white point: reciprocal megakelvin,
	// so 370 is 2700 K. Mireds rather than kelvin because it is the unit the
	// wire carries and the one in which equal steps look equally different.
	Mireds uint16
	// Hue is in degrees, 0-360. Saturation is 0-1. Both are ignored when
	// Mireds is set.
	Hue        float64
	Saturation float64

	// exactHue and exactSaturation carry a pair given in the cluster's own
	// 0-254 units, to be sent verbatim.
	//
	// They exist because hue is circular and the conversion through degrees
	// is therefore faithful to the colour but not to the number: 254 maps to
	// 360°, which is 0°, which comes back as 0. That is the same red, and it
	// is not the same attribute value — so a restore that has to put a device
	// back exactly as it was found needs the byte, not just the colour.
	exactHue        uint8
	exactSaturation uint8
	hasExact        bool
}

// ExactHueSaturation returns a colour to be sent as the given hue and
// saturation in the cluster's own 0-254 units, without conversion.
func ExactHueSaturation(hue, saturation uint8) Color {
	return Color{
		Hue:             float64(hue) * 360 / 254,
		Saturation:      float64(saturation) / 254,
		exactHue:        hue,
		exactSaturation: saturation,
		hasExact:        true,
	}
}

// IsWhitePoint reports whether the colour was named as a temperature.
func (c Color) IsWhitePoint() bool { return c.Mireds != 0 }

func (c Color) String() string {
	if c.IsWhitePoint() {
		return fmt.Sprintf("%d mireds (%.0f K)", c.Mireds, 1e6/float64(c.Mireds))
	}
	return fmt.Sprintf("hue %.0f°, saturation %.0f%%", c.Hue, c.Saturation*100)
}

// namedColors is the vocabulary a person actually uses.
//
// The saturated entries are full saturation on purpose: a light asked for red
// should be red, not a pastel. The whites are temperatures rather than hues,
// because that is what they are — and because naming them that way is what
// lets a lamp with no colour at all still honour "warm".
var namedColors = map[string]Color{
	"red":     {Hue: 0, Saturation: 1},
	"orange":  {Hue: 30, Saturation: 1},
	"yellow":  {Hue: 60, Saturation: 1},
	"green":   {Hue: 120, Saturation: 1},
	"cyan":    {Hue: 180, Saturation: 1},
	"blue":    {Hue: 240, Saturation: 1},
	"purple":  {Hue: 270, Saturation: 1},
	"magenta": {Hue: 300, Saturation: 1},
	"pink":    {Hue: 330, Saturation: 0.5},

	"candle": {Mireds: 500}, // 2000 K
	"warm":   {Mireds: 370}, // 2700 K
	"soft":   {Mireds: 313}, // 3200 K
	"white":  {Mireds: 250}, // 4000 K
	"cool":   {Mireds: 200}, // 5000 K
	"day":    {Mireds: 154}, // 6500 K
}

// ColorNames lists the named hues, and WhitePointNames the named
// temperatures, both in a stable order, for help text and for a REPL
// completing a half-typed word.
//
// They are separate because the difference is not cosmetic: a lamp that can
// only do colour temperature can render every name in the second list and
// none in the first.
func ColorNames() []string { return namesWhere(func(c Color) bool { return !c.IsWhitePoint() }) }

// WhitePointNames lists the named colour temperatures.
func WhitePointNames() []string { return namesWhere(Color.IsWhitePoint) }

func namesWhere(keep func(Color) bool) []string {
	names := make([]string, 0, len(namedColors))
	for name, color := range namedColors {
		if keep(color) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ParseColor resolves a colour written as a name ("red", "warm"), an sRGB hex
// triplet ("#ff8800"), or a temperature in kelvin ("2700k").
func ParseColor(s string) (Color, bool) {
	text := strings.ToLower(strings.TrimSpace(s))
	if color, ok := namedColors[text]; ok {
		return color, true
	}
	if strings.HasSuffix(text, "k") {
		kelvin, err := strconv.ParseFloat(strings.TrimSuffix(text, "k"), 64)
		if err == nil && kelvin >= 1000 && kelvin <= 40000 {
			return Color{Mireds: uint16(math.Round(1e6 / kelvin))}, true
		}
		return Color{}, false
	}
	if hex, ok := strings.CutPrefix(text, "#"); ok {
		return parseHexColor(hex)
	}
	return Color{}, false
}

// parseHexColor reads an sRGB triplet and keeps only its hue and saturation.
//
// The brightness of a hex colour is deliberately discarded: on a lamp,
// brightness is the level cluster's business, and folding it into the colour
// would mean "#800000" silently dimmed a light that someone else had just set.
func parseHexColor(hex string) (Color, bool) {
	if len(hex) != 6 {
		return Color{}, false
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return Color{}, false
	}
	r := float64((n>>16)&0xFF) / 255
	g := float64((n>>8)&0xFF) / 255
	b := float64(n&0xFF) / 255

	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	if max == 0 {
		// Black is not a colour a light can be. It is off, which is a
		// different cluster and a different intention.
		return Color{}, false
	}
	color := Color{Saturation: (max - min) / max}
	if max == min {
		return color, true // grey: no hue to speak of
	}
	switch max {
	case r:
		color.Hue = 60 * math.Mod((g-b)/(max-min)+6, 6)
	case g:
		color.Hue = 60 * ((b-r)/(max-min) + 2)
	default:
		color.Hue = 60 * ((r-g)/(max-min) + 4)
	}
	return color, true
}

// HueSaturation renders the colour in the units the Colour Control cluster's
// hue and saturation fields use: both 0 to 254.
//
// A white point has to travel through xy to get here, because a temperature is
// a point on the Planckian locus and not a hue anybody named.
func (c Color) HueSaturation() (hue, saturation uint8) {
	if c.hasExact {
		return c.exactHue, c.exactSaturation
	}
	source := c
	if c.IsWhitePoint() {
		source = fromXY(c.chromaticity())
	}
	// 254 rather than 255: the cluster's fields run 0x00 to 0xFE, and 0xFF is
	// out of range rather than "fully saturated".
	h := math.Mod(math.Mod(source.Hue, 360)+360, 360) * 254 / 360
	s := math.Max(0, math.Min(1, source.Saturation)) * 254
	return uint8(math.Round(h)), uint8(math.Round(s))
}

// XY renders the colour as CIE 1931 chromaticity, in the fractions-of-65536
// form the Move to Color command carries.
func (c Color) XY() (x, y uint16) {
	fx, fy := c.chromaticity()
	return uint16(math.Round(fx * 65536)), uint16(math.Round(fy * 65536))
}

// chromaticity is the colour as CIE 1931 xy in its own terms, 0 to 1.
func (c Color) chromaticity() (x, y float64) {
	if c.IsWhitePoint() {
		return planckianXY(1e6 / float64(c.Mireds))
	}
	r, g, b := hueToRGB(c.Hue, c.Saturation)
	// sRGB to CIE XYZ, D65, after undoing the transfer function. The matrix is
	// the sRGB specification's own.
	r, g, b = gammaExpand(r), gammaExpand(g), gammaExpand(b)
	bigX := 0.4124*r + 0.3576*g + 0.1805*b
	bigY := 0.2126*r + 0.7152*g + 0.0722*b
	bigZ := 0.0193*r + 0.1192*g + 0.9505*b
	sum := bigX + bigY + bigZ
	if sum == 0 {
		return 0, 0
	}
	return bigX / sum, bigY / sum
}

// hueToRGB converts hue and saturation at full value to sRGB, 0 to 1.
func hueToRGB(hue, saturation float64) (r, g, b float64) {
	h := math.Mod(math.Mod(hue, 360)+360, 360) / 60
	s := math.Max(0, math.Min(1, saturation))
	// Value is fixed at 1: how bright the lamp burns is the level cluster's
	// business, not the colour's.
	c := s
	x := c * (1 - math.Abs(math.Mod(h, 2)-1))
	m := 1 - c
	switch int(h) {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return r + m, g + m, b + m
}

// fromXY is the inverse trip, used only to render a white point as a hue for a
// lamp that has no other way to be told one.
func fromXY(x, y float64) Color {
	if y == 0 {
		return Color{Saturation: 0}
	}
	bigY := 1.0
	bigX := x * bigY / y
	bigZ := (1 - x - y) * bigY / y
	r := 3.2406*bigX - 1.5372*bigY - 0.4986*bigZ
	g := -0.9689*bigX + 1.8758*bigY + 0.0415*bigZ
	b := 0.0557*bigX - 0.2040*bigY + 1.0570*bigZ
	r, g, b = gammaCompress(r), gammaCompress(g), gammaCompress(b)

	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	if max <= 0 {
		return Color{Saturation: 0}
	}
	color := Color{Saturation: (max - min) / max}
	if max == min {
		return color
	}
	switch max {
	case r:
		color.Hue = 60 * math.Mod((g-b)/(max-min)+6, 6)
	case g:
		color.Hue = 60 * ((b-r)/(max-min) + 2)
	default:
		color.Hue = 60 * ((r-g)/(max-min) + 4)
	}
	return color
}

func gammaExpand(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func gammaCompress(c float64) float64 {
	c = math.Max(0, math.Min(1, c))
	if c <= 0.0031308 {
		return c * 12.92
	}
	return 1.055*math.Pow(c, 1/2.4) - 0.055
}

// planckianXY approximates the chromaticity of a black body at a colour
// temperature, by Kim et al.'s cubic fit to the Planckian locus. It holds from
// about 1667 K to 25000 K, which covers every white a lamp will offer.
func planckianXY(kelvin float64) (x, y float64) {
	kelvin = math.Max(1667, math.Min(25000, kelvin))
	t := kelvin
	switch {
	case t <= 4000:
		x = -0.2661239e9/(t*t*t) - 0.2343589e6/(t*t) + 0.8776956e3/t + 0.179910
	default:
		x = -3.0258469e9/(t*t*t) + 2.1070379e6/(t*t) + 0.2226347e3/t + 0.240390
	}
	switch {
	case t <= 2222:
		y = -1.1063814*x*x*x - 1.34811020*x*x + 2.18555832*x - 0.20219683
	case t <= 4000:
		y = -0.9549476*x*x*x - 1.37418593*x*x + 2.09137015*x - 0.16748867
	default:
		y = 3.0817580*x*x*x - 5.87338670*x*x + 3.75112997*x - 0.37001483
	}
	return x, y
}
