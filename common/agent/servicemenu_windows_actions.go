//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strconv"
)

// serviceSpawn launches the durable launcher detached (cmd /c start /min) so
// the desktop entry can return immediately. start-node.cmd re-execs the node
// binary, which takes the host lock and reads the persisted config.
func serviceSpawn(launcher string) int {
	// "start" with no window title opens the launcher in a new console; /min
	// keeps it out of the way. cmd /c returns once start has launched it.
	cmd := exec.Command("cmd.exe", "/c", "start", "/min", "Ai Lubricant 节点", launcher)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Println("Ai Lubricant 节点：启动失败 —", err)
		return 1
	}
	_ = cmd.Wait() // cmd /c start returns immediately; wait reaps the short-lived cmd
	return 0
}

// serviceTerminate asks the running node's PID to exit. trustedOwner verified
// the PID belongs to our binary; taskkill /PID /T SIGTERM-equivalent is a
// graceful WM_CLOSE — but for a console app, /T sends a Ctrl+C. We use
// taskkill /PID (no /F) which sends WM_CLOSE; the node's SIGTERM handler
// (SetConsoleCtrlHandler) drains.
func serviceTerminate(info ownerInfo) int {
	if info.PID <= 0 {
		return 1
	}
	if out, err := exec.Command("taskkill.exe", "/PID", strconv.Itoa(info.PID)).CombinedOutput(); err != nil {
		fmt.Println("Ai Lubricant 节点：停止失败 —", string(out))
		return 1
	}
	return 0
}
