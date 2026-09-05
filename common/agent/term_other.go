//go:build !linux && !darwin && !windows

package agent

// fdIsTerminal has no portable implementation on other platforms. Reporting
// false is the safe default: an unattended rebind is refused unless the operator
// passes --yes.
func fdIsTerminal(uintptr) bool { return false }
