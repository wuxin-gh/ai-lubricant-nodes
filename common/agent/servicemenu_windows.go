//go:build windows

package agent

import (
	"os/exec"
	"strings"
)

// runServiceDialog pops a native Windows message box with the three actions
// (Yes=启动服务, No=停止服务, Cancel=重启服务); closing the box = quit.
// PowerShell's System.Windows.Forms MessageBox renders a locale-independent
// Win32 dialog. It echoes the chosen action as an English word so the parser
// stays simple and locale-independent.
func runServiceDialog() (ServiceMenuAction, error) {
	ps := `Add-Type -AssemblyName System.Windows.Forms
$r = [System.Windows.Forms.MessageBox]::Show('是 = 启动服务  否 = 停止服务  取消 = 重启服务','Ai Lubricant 节点','YesNoCancel','Question')
switch ($r.ToString()) { 'Yes' { 'start' } 'No' { 'stop' } 'Cancel' { 'restart' } default { 'quit' } }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		// Console fallback so the feature still works on a GUI-less host
		// (e.g. Server Core over RDP).
		return consoleServiceChoice()
	}
	return parseDialogChoice(strings.TrimSpace(string(out))), nil
}
