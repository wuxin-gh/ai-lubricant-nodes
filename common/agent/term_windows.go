//go:build windows

package agent

import "golang.org/x/sys/windows"

// fdIsTerminal reports whether fd is a console handle. GetConsoleMode fails for
// files, pipes and NUL, which is exactly the distinction we need: a redirected
// stdin must not be mistaken for an operator who can answer a prompt.
func fdIsTerminal(fd uintptr) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(fd), &mode) == nil
}
