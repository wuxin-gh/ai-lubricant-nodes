//go:build linux

package agent

import "golang.org/x/sys/unix"

// fdIsTerminal reports whether fd is a real terminal. A termios query is the
// reliable test: os.ModeCharDevice is also true for /dev/null, which would make
// a redirected stdin look interactive and turn a refusal into a silent "no".
func fdIsTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}
