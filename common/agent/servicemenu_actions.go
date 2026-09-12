// Service-menu actions: start / stop / restart of the node process, shared by
// the platform dialog files. All actions are best-effort with user-facing
// output (the desktop entry runs them in a visible console window); the exit
// code is 0 unless the action itself failed.
package agent

import (
	"fmt"
	"os"
	"time"
)

// serviceStart launches the durable launcher in the background. The host-wide
// single-instance lock inside the node makes a second start a no-op that
// prints "already running" and exits, so this never duplicates the process.
func serviceStart() int {
	launcher, err := autostartLauncherPath()
	if err != nil {
		fmt.Println("Ai Lubricant 节点：无法定位节点程序 —", err)
		return 1
	}
	if fileMissing(launcher) {
		fmt.Println("Ai Lubricant 节点：尚未在本机安装节点，请先按入驻指引完成安装后再启动。")
		return 1
	}
	return serviceSpawn(launcher)
}

// serviceStop sends a graceful termination to the running node (SIGTERM /
// taskkill), identified through the single-instance lock owner file. A missing
// owner means "not running" — a clean outcome, not an error.
func serviceStop() int {
	info, err := readOwner()
	if err != nil || !trustedOwner(info) {
		fmt.Println("Ai Lubricant 节点：节点当前未在运行。")
		return 0
	}
	if code := serviceTerminate(info); code != 0 {
		return code
	}
	fmt.Println("Ai Lubricant 节点：已请求节点停止（进行中的任务会在服务端保留，重新启动后可继续）。")
	serviceWaitLockReleased()
	return 0
}

// serviceWaitLockReleased blocks up to ~10s for the old node process to exit
// (the graceful-stop drain), so an immediate restart does not hit "already
// running".
func serviceWaitLockReleased() {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		info, err := readOwner()
		if err != nil || !processAlive(info.PID) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func fileMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}
