package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeOwner(cfg LocalConfig) error {
	path, err := ownerPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	body, err := json.Marshal(ownerInfo{
		PID:        os.Getpid(),
		Executable: exe,
		NodeID:     cfg.NodeID,
		Role:       cfg.Role,
	})
	if err != nil {
		return fmt.Errorf("encode lock owner: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write lock owner: %w", err)
	}
	return nil
}

func readOwner() (ownerInfo, error) {
	path, err := ownerPath()
	if err != nil {
		return ownerInfo{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ownerInfo{}, err
	}
	var info ownerInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return ownerInfo{}, err
	}
	return info, nil
}

func removeOwner() error {
	path, err := ownerPath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// trustedOwner refuses to terminate a PID unless both its recorded executable
// name and the currently running process are recognizably ours. On platforms
// where the process image path is available, it must match exactly; this closes
// the PID-reuse hole. macOS lacks procfs, so it falls back to a liveness check +
// known executable basename.
func trustedOwner(info ownerInfo) bool {
	if info.PID <= 0 || strings.TrimSpace(info.Executable) == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(info.Executable))
	known := strings.HasPrefix(base, "node-execution") ||
		strings.HasPrefix(base, "agent-compose-node-execution") ||
		strings.HasPrefix(base, "agent-compose-node-management")
	if !known || !processAlive(info.PID) {
		return false
	}
	actual, available := ownerProcessPath(info.PID)
	if !available {
		return true
	}
	recorded, err1 := filepath.Abs(info.Executable)
	actualAbs, err2 := filepath.Abs(actual)
	return err1 == nil && err2 == nil && strings.EqualFold(recorded, actualAbs)
}
