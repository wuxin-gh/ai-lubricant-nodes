// Cross-platform helpers shared by the autostart_* platform files.

package agent

import (
	"os"
	"path/filepath"
)

// nodeExecutablePath returns the path of the running node binary. Used to
// locate the install root (parent of bin/) and the durable launcher written
// beside it by the install script.
func nodeExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// autostartLauncherPath resolves the durable start-node launcher next to the
// node binary, mirroring the install scripts' %ROOT%/start-node.sh /
// start-node.cmd layout. When the launcher is absent (a manual binary-only
// install), fall back to the binary itself, which reads the persisted config.
func autostartLauncherPath() (string, error) {
	bin, err := nodeExecutablePath()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(bin))
	candidates := []string{
		filepath.Join(root, "start-node.cmd"),
		filepath.Join(root, "start-node.sh"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return bin, nil
}

// autostartLogPath is where the launchd plist redirects agent stdout/stderr.
// Co-located with the node state dir's logs so a single place collects them.
func autostartLogPath(name string) string {
	dir, err := stateDir()
	if err != nil {
		return filepath.Join(os.TempDir(), name)
	}
	return filepath.Join(dir, "logs", name)
}
