//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// serviceSpawn launches the durable launcher detached in its own session so
// the desktop entry can return immediately. start-node.sh re-execs the node
// binary, which takes the host lock and reads the persisted config.
func serviceSpawn(launcher string) int {
	cmd := exec.Command(launcher)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Println("Ai Lubricant 节点：启动失败 —", err)
		return 1
	}
	// Detach: we don't wait — the node is long-running. Setsid puts the child
	// in its own session so this process's exit won't take the node down.
	return 0
}

// serviceTerminate sends SIGTERM to the running node's PID. trustedOwner has
// already verified the PID belongs to our binary.
func serviceTerminate(info ownerInfo) int {
	proc, err := os.FindProcess(info.PID)
	if err != nil {
		return 1
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return 1
	}
	return 0
}
