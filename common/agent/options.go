package agent

import (
	"fmt"
	"os"
	"strings"
)

// EnvOr returns the trimmed env value for key, or fallback when unset/empty.
func EnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// SplitAndTrim splits a comma-separated list, dropping empty entries.
func SplitAndTrim(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseLabels parses a comma-separated key=value capability label list.
func ParseLabels(value string) map[string]string {
	labels := map[string]string{}
	for _, part := range SplitAndTrim(value) {
		key, val, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(val)
	}
	return labels
}

// DefaultNodeName returns the host name, or "node" when it can't be read.
func DefaultNodeName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "node"
	}
	return strings.TrimSpace(host)
}

// ResolveDockerCapability decides whether this node offers the docker driver.
// "auto" probes for the docker CLI on PATH; "on"/"off" force the choice.
func ResolveDockerCapability(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return DockerAvailable(), nil
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --docker %q: want auto|on|off", value)
	}
}

// ResolveSystemEnvCapability decides whether this node accepts env_mode=system
// sessions (the editor runs against the operator's real HOME — ssh keys, CLI
// credentials — so the default must be deliberate). "auto" allows on a host
// install and refuses inside a container: a container's HOME is the image's,
// not the operator's, so system mode there is meaningless and only widens the
// blast radius. "on"/"off" force the choice either way.
func ResolveSystemEnvCapability(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return !RunningInContainer(), nil
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --allow-system-env %q: want auto|on|off", value)
	}
}
