// Command rsdport discovers the device's remoted (RSD) port via mDNS —
// exactly what go-ios's discover.go does (browse "_remoted._tcp" over every
// interface, IPv6). The PC can see the advertisement because the phone's USB
// networking interface (Apple Mobile Device Ethernet) is on this host's link.
//
// Usage: go run ./cmd/rsdport
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/grandcat/zeroconf"
)

func main() {
	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Fprintln(os.Stderr, "interfaces:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	for _, proto := range []zeroconf.IPType{zeroconf.IPv4AndIPv6} {
		r, err := zeroconf.NewResolver(zeroconf.SelectIfaces(ifaces), zeroconf.SelectIPTraffic(proto))
		if err != nil {
			continue
		}
		entries := make(chan *zeroconf.ServiceEntry, 16)
		if err := r.Browse(ctx, "_remoted._tcp", "local.", entries); err != nil {
			continue
		}
		go collect(entries)
	}
	<-ctx.Done()
}

var seen = map[string]bool{}

func collect(entries chan *zeroconf.ServiceEntry) {
	for e := range entries {
		if e == nil {
			continue
		}
		key := fmt.Sprintf("%s:%d", e.ServiceInstanceName(), e.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Printf("service=%q port=%d addrs6=%v addrs4=%v\n",
			e.ServiceInstanceName(), e.Port, e.AddrIPv6, e.AddrIPv4)
	}
}
