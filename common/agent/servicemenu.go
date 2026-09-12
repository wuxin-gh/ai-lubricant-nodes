// The "service" subcommand: a click-to-manage desktop entry for the node.
//
// The install script drops a desktop shortcut ("Ai Lubricant 节点" on
// Windows, an .app/.command on macOS) whose target is `<binary> service`.
// Running it pops a native three-button dialog (start / stop / restart) so an
// operator can manage the node service without a terminal — and without any
// credentials, because this command never reads or holds them.
//
// Process control reuses the existing single-instance lock owner file
// (lock_owner.go): the running node writes its PID there; "stop" sends
// SIGTERM (unix) / taskkill (windows) to that PID; "start" spawns the durable
// launcher; "restart" is stop-then-start with a short lock-release wait.
package agent

// ServiceMenuAction is one of the three dialog choices.
type ServiceMenuAction string

const (
	ServiceActionStart   ServiceMenuAction = "start"
	ServiceActionStop    ServiceMenuAction = "stop"
	ServiceActionRestart ServiceMenuAction = "restart"
	ServiceActionQuit    ServiceMenuAction = "quit"
)

// ServiceMenu runs the desktop service dialog and dispatches the chosen
// action. It is safe to call on a host where the node is not installed yet:
// start/stop/restart gracefully report "not installed" and quit exits 0.
func ServiceMenu() int {
	action, err := runServiceDialog()
	if err != nil {
		return 1
	}
	switch action {
	case ServiceActionStart:
		return serviceStart()
	case ServiceActionStop:
		return serviceStop()
	case ServiceActionRestart:
		if code := serviceStop(); code != 0 {
			return code
		}
		serviceWaitLockReleased()
		return serviceStart()
	case ServiceActionQuit, "":
		return 0
	}
	return 0
}
