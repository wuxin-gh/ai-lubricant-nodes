package agent

import "testing"

func TestProbeIosMgmtNow(t *testing.T) {
	ok, err := ResolveIosMgmtCapability("auto")
	t.Logf("ResolveIosMgmtCapability(auto) = %v, err=%v", ok, err)
	_, addr := usbmuxdProbeAddr()
	t.Logf("probe addr = %s", addr)
	t.Logf("UsbmuxdAvailable() = %v", UsbmuxdAvailable())
}
