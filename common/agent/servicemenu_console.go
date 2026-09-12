// Cross-platform console fallback for the service menu. The GUI dialog
// (osascript on macOS, Windows.Forms MessageBox on Windows) is preferred; when
// it is unavailable (Server Core, no display, headless box reached over SSH),
// the menu degrades to a stdin prompt that works in every shell.

package agent

import (
	"fmt"
	"strings"
)

func consoleServiceChoice() (ServiceMenuAction, error) {
	fmt.Println("Ai Lubricant 节点")
	fmt.Println("  1) 启动服务")
	fmt.Println("  2) 停止服务")
	fmt.Println("  3) 重启服务")
	fmt.Println("  0) 退出")
	fmt.Print("请选择: ")
	var line string
	if _, err := fmt.Scanln(&line); err != nil {
		return ServiceActionQuit, nil
	}
	return parseDialogChoice(strings.TrimSpace(line)), nil
}

func parseDialogChoice(s string) ServiceMenuAction {
	switch {
	case strings.HasPrefix(s, "启动"), s == "start", s == "1":
		return ServiceActionStart
	case strings.HasPrefix(s, "停止"), s == "stop", s == "2":
		return ServiceActionStop
	case strings.HasPrefix(s, "重启"), s == "restart", s == "3":
		return ServiceActionRestart
	}
	return ServiceActionQuit
}
