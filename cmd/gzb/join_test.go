package main

import "testing"

// The window travels as one byte of whole seconds, and zero means close: a
// person who typed 0 wanted permit-join, and is told so before the port is
// touched.
func TestJoinWindowDefaultsAndBounds(t *testing.T) {
	if seconds, err := parseJoinWindow(nil); err != nil || seconds != 60 {
		t.Errorf("no argument = %d, %v; want the 60-second default", seconds, err)
	}
	if seconds, err := parseJoinWindow([]string{"30"}); err != nil || seconds != 30 {
		t.Errorf("30 = %d, %v", seconds, err)
	}
	for _, bad := range []string{"0", "300", "soon", "-5"} {
		if _, err := parseJoinWindow([]string{bad}); err == nil {
			t.Errorf("%q was accepted as a window", bad)
		}
	}
}
