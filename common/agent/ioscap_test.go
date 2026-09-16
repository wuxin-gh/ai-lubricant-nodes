package agent

import (
	"net"
	"runtime"
	"testing"
)

// usbmuxdProbeAddr must resolve the same address shapes go-ios dials, so a probe
// and an actual enumeration never disagree about where usbmuxd lives.
func TestUsbmuxdProbeAddr(t *testing.T) {
	t.Run("explicit unix scheme honoured as-is", func(t *testing.T) {
		t.Setenv(UsbmuxdSocketEnv, "unix:///tmp/usbmuxd")
		network, address := usbmuxdProbeAddr()
		if network != "unix" || address != "/tmp/usbmuxd" {
			t.Fatalf("got %s %s", network, address)
		}
	})

	t.Run("explicit tcp scheme honoured as-is", func(t *testing.T) {
		t.Setenv(UsbmuxdSocketEnv, "tcp://127.0.0.1:27015")
		network, address := usbmuxdProbeAddr()
		if network != "tcp" || address != "127.0.0.1:27015" {
			t.Fatalf("got %s %s", network, address)
		}
	})

	t.Run("scheme is lower-cased for net.Dial", func(t *testing.T) {
		// net.Dial rejects "UNIX"; go-ios lower-cases for the same reason.
		t.Setenv(UsbmuxdSocketEnv, "UNIX:///tmp/usbmuxd")
		if network, _ := usbmuxdProbeAddr(); network != "unix" {
			t.Fatalf("got %s", network)
		}
	})

	t.Run("bare host:port becomes tcp", func(t *testing.T) {
		t.Setenv(UsbmuxdSocketEnv, "127.0.0.1:27015")
		network, address := usbmuxdProbeAddr()
		if network != "tcp" || address != "127.0.0.1:27015" {
			t.Fatalf("got %s %s", network, address)
		}
	})

	t.Run("bare path becomes unix", func(t *testing.T) {
		t.Setenv(UsbmuxdSocketEnv, "/var/run/usbmuxd")
		network, address := usbmuxdProbeAddr()
		if network != "unix" || address != "/var/run/usbmuxd" {
			t.Fatalf("got %s %s", network, address)
		}
	})

	t.Run("no override uses the platform default", func(t *testing.T) {
		t.Setenv(UsbmuxdSocketEnv, "")
		network, address := usbmuxdProbeAddr()
		if runtime.GOOS == "windows" {
			if network != "tcp" || address != "127.0.0.1:27015" {
				t.Fatalf("windows default: got %s %s", network, address)
			}
			return
		}
		if network != "unix" || address != "/var/run/usbmuxd" {
			t.Fatalf("posix default: got %s %s", network, address)
		}
	})
}

// UsbmuxdAvailable must report true only when something is actually listening.
func TestUsbmuxdAvailableAgainstRealSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	t.Setenv(UsbmuxdSocketEnv, "tcp://"+ln.Addr().String())
	if !UsbmuxdAvailable() {
		t.Fatal("expected available against a listening socket")
	}
}

func TestUsbmuxdUnavailableWhenNothingListens(t *testing.T) {
	// Bind then immediately close to get a port nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	t.Setenv(UsbmuxdSocketEnv, "tcp://"+addr)
	if UsbmuxdAvailable() {
		t.Fatal("expected unavailable with nothing listening")
	}
}

// ResolveIosMgmtCapability is the tri-state resolver: auto probes, on/off force,
// anything else is a usage error rather than a silent fallback.
func TestResolveIosMgmtCapability(t *testing.T) {
	// Point auto at a dead port so the probe verdict is deterministic.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	t.Setenv(UsbmuxdSocketEnv, "tcp://"+dead)

	cases := []struct {
		value string
		want  bool
	}{
		{"", false}, // auto, nothing listening
		{"auto", false},
		{"on", true},
		{"true", true},
		{"1", true},
		{"yes", true},
		{"off", false},
		{"false", false},
		{"0", false},
		{"no", false},
		{"ON", true},  // case-insensitive
		{" off", false}, // trimmed
	}
	for _, tc := range cases {
		got, err := ResolveIosMgmtCapability(tc.value)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", tc.value, err)
		}
		if got != tc.want {
			t.Errorf("%q: got %v want %v", tc.value, got, tc.want)
		}
	}

	if _, err := ResolveIosMgmtCapability("maybe"); err == nil {
		t.Fatal("expected a usage error for an invalid value")
	}
}

// auto must be true when the probe succeeds — this is what makes "upgrade the
// node on a machine with iTunes installed" light up the capability with no
// operator action.
func TestResolveIosMgmtCapabilityAutoProbes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Setenv(UsbmuxdSocketEnv, "tcp://"+ln.Addr().String())

	if got, err := ResolveIosMgmtCapability("auto"); err != nil || !got {
		t.Fatalf("auto with a live socket: got %v err %v, want true", got, err)
	}
}
