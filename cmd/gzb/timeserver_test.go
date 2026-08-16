package main

import (
	"testing"
	"time"
)

func TestTimeAttributeLocalTimeSupportsNegativeOffset(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	now := time.Date(2026, 8, 16, 12, 30, 0, 0, zone)

	rec, ok := timeAttribute(attrLocalTime, now)
	if !ok {
		t.Fatal("LocalTime was not supported")
	}
	want := uint64(time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC).Sub(zigbeeEpoch) / time.Second)
	if rec.Value != want {
		t.Fatalf("LocalTime = %v, want %d", rec.Value, want)
	}
}

func TestTimeAttributeClampsBeforeZigbeeEpoch(t *testing.T) {
	now := zigbeeEpoch.Add(-time.Hour)
	rec, _ := timeAttribute(attrTime, now)
	if rec.Value != uint64(0) {
		t.Fatalf("Time = %v, want 0", rec.Value)
	}
}
