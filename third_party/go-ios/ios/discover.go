package ios

import (
	"context"
	"fmt"
	"net"

	"github.com/danielpaulus/go-ios/ios/golog"
	"github.com/grandcat/zeroconf"
)

// FindDeviceInterfaceAddress tries to find the address of the device by browsing through all network interfaces.
// It uses mDNS to discover  the "_remoted._tcp" service on the local. domain. Then tries to connect to the RemoteServiceDiscovery
// and checks if the udid of the device matches the udid of the device we are looking for.
func FindDeviceInterfaceAddress(ctx context.Context, device DeviceEntry) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("FindDeviceInterfaceAddress: failed to get network interfaces: %w", err)
	}

	result := make(chan string)

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	for _, iface := range ifaces {
		resolver, err := zeroconf.NewResolver(zeroconf.SelectIfaces([]net.Interface{iface}), zeroconf.SelectIPTraffic(zeroconf.IPv6))
		if err != nil {
			golog.Debug("failed to initialize resolver", "module", logModule, "udid", device.Properties.SerialNumber, "interface", iface.Name, "err", err)
			continue
		}
		entries := make(chan *zeroconf.ServiceEntry)
		resolver.Browse(ctx, "_remoted._tcp", "local.", entries)
		go checkEntry(ctx, device, iface.Name, entries, result)

	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-result:
		golog.Debug("found device address", "module", logModule, "udid", device.Properties.SerialNumber, "address", r)
		return r, nil
	}
}

// checkEntry connects to all remote service discoveries and tests which one belongs to this device' udid.
func checkEntry(ctx context.Context, device DeviceEntry, interfaceName string, entries chan *zeroconf.ServiceEntry, result chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-entries:
			if entry == nil {
				continue
			}
			fmt.Print(entry.ServiceInstanceName())
			for _, ip6 := range entry.AddrIPv6 {
				tryHandshake(ctx, ip6, entry.Port, interfaceName, device, result)
			}
		}
	}
}

func tryHandshake(ctx context.Context, ip6 net.IP, port int, interfaceName string, device DeviceEntry, result chan<- string) {
	addr := fmt.Sprintf("%s%%%s", ip6.String(), interfaceName)
	s, err := NewWithAddrPortDevice(addr, port, device)
	udid := device.Properties.SerialNumber
	if err != nil {
		golog.Error("failed to connect to remote service discovery", "module", logModule, "udid", udid, "error", err, "address", addr)
		return
	}
	defer s.Close()
	h, err := s.Handshake()
	if err != nil {
		return
	}
	if udid == h.Udid {
		select {
		case <-ctx.Done():
			golog.Error("failed sending handshake result", "module", logModule, "udid", udid, "error", ctx.Err())
		case result <- addr:
		}
	}
}
