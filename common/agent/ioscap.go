package agent

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

// iOS device management capability probe.
//
// "Can this host enumerate iPhones?" is a host property, not a node role: it
// needs usbmuxd (the Apple device multiplexer) reachable from this process. On
// macOS and Linux that is the unix socket /var/run/usbmuxd; on Windows the
// Apple Mobile Device Service listens on TCP 127.0.0.1:27015. The probe is a
// bare socket dial — it deliberately does NOT import go-ios, so every node
// binary can run it without dragging the device stack in.
//
// The result becomes the ``ios_mgmt`` capability label, which the server gates
// iOS device management frames on (see node_server.service._require_online_ios_host).
// It is deliberately independent of the node role: an execution node on a Mac or
// Windows box with an iPhone attached advertises it exactly like the dedicated
// node-ios binary does.

// UsbmuxdSocketEnv mirrors go-ios's own override so an operator who moved the
// socket (or runs usbmuxd over TCP) can point the probe at it.
const UsbmuxdSocketEnv = "USBMUXD_SOCKET_ADDRESS"

// usbmuxdDialTimeout bounds the probe. A local socket accept is sub-millisecond;
// this only covers a hung service, and registration must never block on it.
const usbmuxdDialTimeout = 2 * time.Second

// usbmuxdProbeAddr resolves the address to dial, honouring USBMUXD_SOCKET_ADDRESS.
//
// The accepted override forms mirror go-ios (ios.GetUsbmuxdSocket): an explicit
// scheme is honoured as-is (unix:///path, tcp://host:port); without a scheme, a
// ":" means host:port and anything else is a unix socket path.
func usbmuxdProbeAddr() (network, address string) {
	if override := strings.TrimSpace(os.Getenv(UsbmuxdSocketEnv)); override != "" {
		if i := strings.Index(override, "://"); i >= 0 {
			return strings.ToLower(override[:i]), override[i+3:]
		}
		if strings.Contains(override, ":") {
			return "tcp", override
		}
		return "unix", override
	}
	if runtime.GOOS == "windows" {
		// Apple Mobile Device Service. Windows has no unix sockets for usbmuxd.
		return "tcp", "127.0.0.1:27015"
	}
	return "unix", "/var/run/usbmuxd"
}

// UsbmuxdAvailable reports whether the Apple device multiplexer is reachable,
// i.e. whether this host could enumerate attached iPhones.
//
// Any dial error means "no" — usbmuxd not installed, Apple Mobile Device Service
// not running, or the socket removed. That is the common case on a plain Linux
// server and is not an error worth surfacing: the capability label is simply
// absent, exactly like a missing docker CLI.
func UsbmuxdAvailable() bool {
	network, address := usbmuxdProbeAddr()
	conn, err := net.DialTimeout(network, address, usbmuxdDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ResolveIosMgmtCapability decides whether this node advertises iOS device
// management. "auto" (the default) probes for usbmuxd; "on"/"off" force the
// choice.
//
// "auto" is the default so a node that is merely upgraded on a machine with
// iTunes/Apple Devices installed starts offering the capability with no operator
// action — which is the whole point of folding iOS support into the execution
// node. "off" exists because a workstation with a phone plugged in should not
// have to let the platform drive it: same risk shape as --allow-system-env.
func ResolveIosMgmtCapability(value string) (bool, error) {
	switch normalizeIosMgmt(value) {
	case "auto":
		return UsbmuxdAvailable(), nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --ios %q: want auto|on|off", value)
	}
}

// normalizeIosMgmt maps every accepted --ios spelling onto auto|on|off. An empty
// value is "auto" (the default), so the ios_mgmt label is probed rather than
// asserted.
func normalizeIosMgmt(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return "auto"
	case "on", "true", "1", "yes":
		return "on"
	case "off", "false", "0", "no":
		return "off"
	default:
		return ""
	}
}

// IosMgmtProbed reports whether the ios_mgmt capability should be decided by
// probing usbmuxd *now* rather than by a verdict resolved at process start.
//
// This exists because the Apple Mobile Device Service is a host service the node
// does not control: it can be down (or simply not yet started) when the node
// boots and up a minute later. A single boot-time probe made that verdict
// permanent — the label disappeared from the register frame, every iOS surface in
// the console silently vanished, and only a node restart could bring it back. The
// caller distinguishes an explicit operator `--ios on/off` (sticky, deliberate)
// from `auto` (a fact about the host, so re-read every time facts are collected).
func IosMgmtProbed(mode string) bool {
	return normalizeIosMgmt(mode) == "auto"
}
