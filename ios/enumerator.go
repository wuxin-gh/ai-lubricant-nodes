package main

import (
	"context"

	ios "github.com/danielpaulus/go-ios/ios"
)

// goiosEnumerator is the production Enumerator backed by go-ios's usbmuxd
// device list and listen socket.
//
// REAL-DEVICE GATE: the ListDevices / Listen API surface is stable in go-ios
// v1.3.2 and matches what the reference CLI uses, but the attach/detach event
// stream has not been verified against a physical iPhone here.
type goiosEnumerator struct{}

// List implements Enumerator by calling ios.ListDevices and flattening each
// DeviceEntry to the fields the manager needs.
func (goiosEnumerator) List(_ context.Context) ([]EnumeratedDevice, error) {
	list, err := ios.ListDevices()
	if err != nil {
		return nil, err
	}
	out := make([]EnumeratedDevice, 0, len(list.DeviceList))
	for _, d := range list.DeviceList {
		out = append(out, EnumeratedDevice{
			UDID:           d.Properties.SerialNumber,
			ConnectionType: d.Properties.ConnectionType,
			DeviceID:       d.DeviceID,
		})
	}
	return out, nil
}

// Subscribe implements Enumerator by opening go-ios's usbmuxd Listen socket.
// The returned event channel yields DeviceEvent values (attach/detach); the
// error channel yields any read error from the socket; stop closes the socket.
//
// On iOS 17+ and on hosts where the listen socket cannot be opened (usbmuxd
// not running, Apple Mobile Device Service missing on Windows) Subscribe
// returns an error and the manager falls back to periodic polling.
func (goiosEnumerator) Subscribe(ctx context.Context) (<-chan DeviceEvent, <-chan error, func(), error) {
	next, closeSocket, err := ios.Listen()
	if err != nil {
		return nil, nil, nil, err
	}
	events := make(chan DeviceEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		for {
			msg, err := next()
			if err != nil {
				errs <- err
				return
			}
			select {
			case <-ctx.Done():
				return
			case events <- DeviceEvent{
				UDID:     msg.Properties.SerialNumber,
				Attached: msg.DeviceAttached(),
			}:
			}
		}
	}()
	// ios.Listen's closer returns an error; the Enumerator contract is a bare
	// stop func (nothing useful to do with a socket-close failure at teardown).
	stop := func() { _ = closeSocket() }
	return events, errs, stop, nil
}

// lookupDeviceInfo reads lockdown values for one device to get its name,
// hardware model and iOS version. Best-effort: a freshly plugged device may
// not have answered lockdown yet, in which case the manager keeps the UDID
// and fills these in on the next snapshot.
func lookupDeviceInfo(d EnumeratedDevice) (DeviceInfo, error) {
	entry := ios.DeviceEntry{
		DeviceID: d.DeviceID,
		Properties: ios.DeviceProperties{
			SerialNumber:   d.UDID,
			ConnectionType: d.ConnectionType,
		},
	}
	v, err := ios.GetValues(entry)
	if err != nil {
		return DeviceInfo{}, err
	}
	return DeviceInfo{
		Name:           v.Value.DeviceName,
		Model:          v.Value.HardwareModel,
		ProductVersion: v.Value.ProductVersion,
	}, nil
}
