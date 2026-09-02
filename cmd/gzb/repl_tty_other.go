//go:build !(linux || darwin || dragonfly || freebsd || netbsd || openbsd)

package main

// keepOutputProcessing has nothing to do where the terminal is not a Unix
// tty; the session runs with whatever line discipline raw mode left.
func keepOutputProcessing(int) error { return nil }
