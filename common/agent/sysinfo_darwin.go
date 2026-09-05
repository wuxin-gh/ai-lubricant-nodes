//go:build darwin

package agent

import (
	"os/exec"
	"strconv"
	"strings"
)

// cpuModel reads machdep.cpu.brand_string via sysctl.
func cpuModel() string {
	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// memTotalBytes reads hw.memsize via sysctl.
func memTotalBytes() uint64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
