//go:build !linux && !windows && !darwin

package agent

// cpuModel / memTotalBytes have no probe on unsupported platforms; the shared
// SystemLabels still reports cpu_cores from the Go runtime.
func cpuModel() string { return "" }

func memTotalBytes() uint64 { return 0 }
