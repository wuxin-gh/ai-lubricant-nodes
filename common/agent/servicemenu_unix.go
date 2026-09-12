//go:build !windows

package agent

import (
	"os/exec"
	"strings"
)

// runServiceDialog pops a native choice dialog on macOS / Linux. On macOS it
// uses osascript (choose from list); on Linux it uses zenity when present.
// Both fall through to the shared console prompt when no GUI is available
// (e.g. a headless box reached over SSH).
func runServiceDialog() (ServiceMenuAction, error) {
	if p, err := exec.LookPath("osascript"); err == nil {
		out, err := exec.Command(p, "-e",
			`choose from list {"启动服务","停止服务","重启服务","退出"} with title "Ai Lubricant 节点" with prompt "选择要执行的操作" default items {"退出"}`,
		).Output()
		if err == nil {
			return parseDialogChoice(strings.TrimSpace(string(out))), nil
		}
	}
	if p, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command(p, "--list", "--title=Ai Lubricant 节点",
			"--text=选择要执行的操作", "--column=操作", "--hide-header",
			"启动服务", "停止服务", "重启服务", "退出").Output()
		if err == nil {
			return parseDialogChoice(strings.TrimSpace(string(out))), nil
		}
	}
	return consoleServiceChoice()
}
