// FORK-ONLY file (not upstream go-ios). See nodes/go.mod's replace directive.
//
// Adds a data-plane switch to the LAN/RemotePairing tunnel so a callers' tunnel
// can terminate in the rootless userspace stack instead of a kernel TUN.
package tunnel

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/http"
	"github.com/danielpaulus/go-ios/ios/tunnel/tlspsk"
)

// ManualPairAndConnectToNetworkTunnel is ManualPairAndConnectToTunnelTCP with a
// selectable data-plane tail:
//
//	userspace=true  → connectToUserspaceTunnelLockdown (gVisor netstack behind a
//	localhost TCP listener; no root, no wintun, no netsh, no administrator)
//	userspace=false → connectToTunnelLockdown (kernel utun/wintun — identical to
//	upstream ManualPairAndConnectToTunnelTCP, which needs elevation on Windows)
//
// The RemotePairing half is unchanged and byte-identical to upstream: mDNS
// discovery of the device's ethernet interface, the untrusted tunnel service,
// pairing (the device shows its own Trust prompt on first use), a TCP tunnel
// listener and the TLS-PSK transport iOS 18.2+ uses in place of QUIC.
//
// ifacePort is only used when userspace is set; the caller owns choosing a port
// that does not collide with the ports TunnelManager allocates for its own
// userspace tunnels.
func ManualPairAndConnectToNetworkTunnel(ctx context.Context, device ios.DeviceEntry, p PairRecordManager, ifacePort int, userspace bool) (Tunnel, error) {
	addr, err := ios.FindDeviceInterfaceAddress(ctx, device)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to find device interface address: %w", err)
	}

	servicePort, err := getUntrustedTunnelServicePort(addr, device)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: could not find port for '%s'", untrustedTunnelServiceName)
	}
	conn, err := ios.ConnectTUNDevice(addr, servicePort, device)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to connect to tunnel service: %w", err)
	}
	h, err := http.NewHttpConnection(conn)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to create HTTP2 connection: %w", err)
	}
	xpcConn, err := ios.CreateXpcConnection(h)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to create RemoteXPC connection: %w", err)
	}
	ts := newTunnelServiceWithXpc(xpcConn, h, p)

	if err := ts.ManualPair(); err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to pair device: %w", err)
	}

	tunnelPort, err := ts.createTcpTunnelListener()
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to create tcp tunnel listener: %w", err)
	}

	tunnelAddr := fmt.Sprintf("[%s]:%d", addr, tunnelPort)
	tcpConn, err := ios.DialTunnelTCP(tunnelAddr)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: failed to dial tunnel port %s: %w", tunnelAddr, err)
	}
	tlsConn, err := tlspsk.Client(tcpConn, ts.sharedSecret)
	if err != nil {
		return Tunnel{}, fmt.Errorf("ManualPairAndConnectToNetworkTunnel: TLS-PSK handshake failed: %w", err)
	}

	if userspace {
		tun, err := connectToUserspaceTunnelLockdown(ctx, device, tlsConn, ifacePort)
		if err != nil {
			return Tunnel{}, err
		}
		// connectToUserspaceTunnelLockdown does not stamp these (its only other
		// caller, ConnectUserSpaceTunnelLockdown, gets them from the manager).
		// Consumers select the userspace data plane from the tunnel-info
		// response, so they MUST be set here or every dial takes the kernel path.
		tun.UserspaceTUN = true
		tun.UserspaceTUNPort = ifacePort
		// The RemoteXPC control channel (ts) is not part of the data plane and
		// upstream never closes it; tie it to the tunnel so retries don't leak
		// one control connection each.
		inner := tun.closer
		tun.closer = func() error { return errors.Join(inner(), ts.Close()) }
		return tun, nil
	}
	return connectToTunnelLockdown(ctx, device, tlsConn)
}
