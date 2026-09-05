package agent

import (
	"fmt"
	"runtime"
)

// SystemLabels gathers host CPU/memory specs as capability labels. Every probe
// is best-effort: a value that can't be read is simply omitted rather than
// failing registration. These are merged into NodeCapabilities.Labels at
// register time so the management console can show real machine specs
// (cpu / cpu_cores / memory_total) beside the reported OS/arch/hostname.
//
// The per-OS probes (cpuModel / memTotalBytes) live in sysinfo_<goos>.go.
func SystemLabels() map[string]string {
	labels := map[string]string{
		"cpu_cores": fmt.Sprintf("%d", runtime.NumCPU()),
	}
	if model := cpuModel(); model != "" {
		labels["cpu"] = model
	}
	if total := memTotalBytes(); total > 0 {
		labels["memory_total"] = fmt.Sprintf("%d", total)
	}
	return labels
}
