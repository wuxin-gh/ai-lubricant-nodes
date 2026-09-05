//go:build darwin

package agent

import "golang.org/x/sys/unix"

// fdIsTerminal reports whether fd is a real terminal (see term_linux.go).
// Darwin's termios get request is TIOCGETA rather than TCGETS.
func fdIsTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
	return err == nil
}
