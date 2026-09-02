//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// keepOutputProcessing puts back the one part of cooked mode the session
// still wants: the terminal turning "\n" into "\r\n" on the way out. Raw
// mode removes it along with everything else, and every printer in gzb
// writes plain newlines.
func keepOutputProcessing(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return err
	}
	termios.Oflag |= unix.OPOST | unix.ONLCR
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, termios)
}
