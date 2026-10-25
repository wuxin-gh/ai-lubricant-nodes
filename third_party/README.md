# go-ios fork

This directory is a **local, minimally-patched copy of `github.com/danielpaulus/go-ios`
upstream `v1.3.2`** (the latest release at the time of forking). The nodes module
pins it with:

```
replace github.com/danielpaulus/go-ios => ./third_party/go-ios
```

## Why

The node controls iPhones over **LAN (no USB)** via go-ios's RemotePairing tunnel.
Upstream ships the building blocks but not the wiring:

- the automatic `TunnelManager` only tunnels usbmuxd (USB) devices and refuses
  Network devices (`tunnel_api.go:shouldSkipDevice`);
- the TLS-PSK LAN tunnel (`tunnel_tcp.go`) ends in a **kernel** utun/wintun, which
  needs administrator rights — but the node runs the rootless **userspace** data
  plane;
- `TunnelManager` has no way to register a tunnel it did not create.

## The patch (6 hunks, all `FORK-ONLY`-marked, all inert for USB entries)

| File | Change |
|---|---|
| `ios/tunnel/network_tunnel.go` | **new** — `ManualPairAndConnectToNetworkTunnel(ctx, device, pm, ifacePort, userspace)`: the LAN tunnel with a selectable data-plane tail (userspace gVisor, no root). |
| `ios/tunnel/tunnel_api.go` | `TunnelManager.external` set + `AddExternalTunnel`/`RemoveExternalTunnel`; the two teardown loops in `UpdateTunnels` skip external UDIDs. |
| `ios/rsd.go` | `RsdHandshakeResponse.OSVersion` (the device reports its iOS version in the handshake). |
| `ios/lockdown_value.go` | `GetProductVersion`: for a `DeviceID==0` entry, read the version back off the RSD provider instead of dialling usbmuxd. |
| `ios/installationproxy/installationproxy.go` | `New`: for `DeviceID==0 && SupportsRsd()`, connect over the RSD shim (`com.apple.mobile.installation_proxy.shim.remote`) — mirrors zipconduit's proven pattern. |
| `ios/forward/forward.go` | `Forward` accepts the whole device entry; for `DeviceID==0 && SupportsRsd()`, dial the device port through the tunnel (`ConnectTUNDevice`) instead of usbmuxd. |

Every hunk is gated on `device.DeviceID == 0` (or a flag only `AddExternalTunnel`
sets), which real USB entries never have — so the USB path is byte-for-byte
unchanged.

## Rebasing on a new upstream release

1. `rm -rf third_party/go-ios && cp -r <module-cache>/go-ios@<newver> third_party/go-ios`
   (prefer the module **zip** over the cache dir — the cache dir carries a stray
   `go.work` + `cmd/cdc-ncm` that do not exist in a release).
2. `rm -f third_party/go-ios/go.work*`; drop `cmd/cdc-ncm` if present.
3. Re-apply the 6 hunks above mechanically.
4. `cd third_party/go-ios && go build ./...`
5. `go test ./... ` — the `ios/imagemounter` tests download a real DDI and need
   network; they also fail on upstream in a network-restricted box (not a fork
   regression).

`ios/tunnel` and `ios/forward` tests pass locally without a device. The
LAN/RemotePairing path itself has NOT been verified against a physical iPhone —
that is the outstanding real-device step.