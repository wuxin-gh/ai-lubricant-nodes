//go:build windows

package agent

import (
	"syscall"
	"unsafe"
)

// cpuModel reads the processor name from the registry
// (HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0\ProcessorNameString).
func cpuModel() string {
	const (
		hkeyLocalMachine = 0x80000002
		keyRead          = 0x20019
	)
	advapi := syscall.NewLazyDLL("advapi32.dll")
	regOpenKeyEx := advapi.NewProc("RegOpenKeyExW")
	regQueryValueEx := advapi.NewProc("RegQueryValueExW")
	regCloseKey := advapi.NewProc("RegCloseKey")

	subKey, _ := syscall.UTF16PtrFromString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`)
	var hKey syscall.Handle
	ret, _, _ := regOpenKeyEx.Call(
		uintptr(hkeyLocalMachine),
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyRead,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return ""
	}
	defer regCloseKey.Call(uintptr(hKey))

	valName, _ := syscall.UTF16PtrFromString("ProcessorNameString")
	var buf [256]uint16
	bufLen := uint32(len(buf) * 2)
	ret, _, _ = regQueryValueEx.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if ret != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

// memTotalBytes reads total physical memory via GlobalMemoryStatusEx.
func memTotalBytes() uint64 {
	type memoryStatusEx struct {
		dwLength                uint32
		dwMemoryLoad            uint32
		ullTotalPhys            uint64
		ullAvailPhys            uint64
		ullTotalPageFile        uint64
		ullAvailPageFile        uint64
		ullTotalVirtual         uint64
		ullAvailVirtual         uint64
		ullAvailExtendedVirtual uint64
	}
	kernel := syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx := kernel.NewProc("GlobalMemoryStatusEx")

	var stat memoryStatusEx
	stat.dwLength = uint32(unsafe.Sizeof(stat))
	ret, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&stat)))
	if ret == 0 {
		return 0
	}
	return stat.ullTotalPhys
}
