package main

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/chorankates/gzb/zigbee"
)

// The values in this file came off a real sensor: a measured temperature of
// 2640 alongside the range it can measure and a tolerance attribute it does not
// implement, and a reporting configuration of 5s/1h with a reportable change of
// 20. They are the same values the README quotes, and pinning the rendering to
// them is what stops a block there drifting from what the code prints.
//
// `make recapture` produces both: run it against a device to refresh the
// transcripts and these values together.

// capture collects what a printer writes to stdout.
func capture(t *testing.T, print func()) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	print()
	w.Close()
	os.Stdout = saved

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }

func TestRenderRead(t *testing.T) {
	got := capture(t, func() {
		printAttributeValues([]zigbee.AttributeValue{
			{ID: 0x0000, Name: "temperature", Type: zigbee.TypeInt16, Value: int64(2640), Scaled: f64(26.400000000000002), Unit: "°C"},
			{ID: 0x0001, Name: "minimum measurable", Type: zigbee.TypeInt16, Value: int64(-4000)},
			{ID: 0x0002, Name: "maximum measurable", Type: zigbee.TypeInt16, Value: int64(11500)},
			{ID: 0x0003, Name: "tolerance", Status: "unsupported attribute"},
		})
	})
	want := "" +
		"  temperature              26.40 °C   (raw 2640, int16)\n" +
		"  minimum measurable       -4000 (int16)\n" +
		"  maximum measurable       11500 (int16)\n" +
		"  tolerance                !  unsupported attribute\n"
	if got != want {
		t.Errorf("read rendered as\n%s\nwant\n%s", got, want)
	}
}

func TestRenderReportingStatus(t *testing.T) {
	got := capture(t, func() {
		printReportingStatus([]zigbee.ReportingStatus{
			{ID: 0x0000, Name: "temperature", Reporting: true, Min: 5 * time.Second, Max: time.Hour, Change: u64(20), Type: zigbee.TypeInt16},
		})
	})
	want := "  temperature              every 5s to 1h0m0s, on a change of 20 (int16)\n"
	if got != want {
		t.Errorf("configuration rendered as\n%s\nwant\n%s", got, want)
	}

	off := capture(t, func() {
		printReportingStatus([]zigbee.ReportingStatus{
			{ID: 0x0000, Name: "temperature", Reporting: false},
		})
	})
	// An attribute a device is not reporting has to say so, because the whole
	// point of asking was to tell that apart from having nothing to say.
	if want := "  temperature              not reported\n"; off != want {
		t.Errorf("silence rendered as %q, want %q", off, want)
	}
}

func TestRenderReportingPlan(t *testing.T) {
	// The threshold travels raw, so what it works out to has to be said back:
	// 50 hundredths of a degree is the mistake-by-a-hundred detector.
	plan := describeReporting(0x0402, zigbee.ReportConfig{
		ID: 0x0000, Type: zigbee.TypeInt16, Min: time.Minute, Max: time.Hour, Change: uint64(50),
	})
	if want := "every 1m0s to 1h0m0s, on a change of 50 (0.50 °C)"; plan != want {
		t.Errorf("plan = %q, want %q", plan, want)
	}

	// The two undos must not describe themselves the same way.
	revert := describeReporting(0x0402, zigbee.ReportDefaults(0x0000, zigbee.TypeInt16))
	silence := describeReporting(0x0402, zigbee.ReportConfig{
		ID: 0x0000, Type: zigbee.TypeInt16, Min: time.Minute, Max: zigbee.ReportingOff,
	})
	if revert != "reverting to the device's own default" {
		t.Errorf("revert = %q", revert)
	}
	if silence != "reporting off" {
		t.Errorf("off = %q", silence)
	}
	if revert == silence {
		t.Error("turning reporting off and restoring the default describe themselves identically")
	}
}
