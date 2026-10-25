package ioshost

import (
	"errors"
	"sync/atomic"
	"testing"

	ios "github.com/danielpaulus/go-ios/ios"
)

// ── 设备集合键（只在变化时对账的判据） ────────────────────────────────────
//
// 常驻隧道被误拆的根因是 go-ios 每次 ListDevices 抖动就拆隧道。我们的对策是
// 平时根本不调 UpdateTunnels —— 判据就是这个集合键：两次探测的键相同且隧道
// 齐 → 什么都不做。所以键必须：顺序无关、内容变化才变。

func TestDeviceSetKeyIsOrderIndependent(t *testing.T) {
	mk := func(udids ...string) []ios.DeviceEntry {
		out := make([]ios.DeviceEntry, 0, len(udids))
		for _, u := range udids {
			var d ios.DeviceEntry
			d.Properties.SerialNumber = u
			out = append(out, d)
		}
		return out
	}
	a := deviceSetKey(mk("UDID-B", "UDID-A"))
	b := deviceSetKey(mk("UDID-A", "UDID-B"))
	if a != b {
		t.Fatalf("key must not depend on enumeration order: %q vs %q", a, b)
	}
	if deviceSetKey(mk("UDID-A")) == deviceSetKey(mk("UDID-A", "UDID-B")) {
		t.Fatal("a plugged device must change the key (else it never gets a tunnel)")
	}
	if deviceSetKey(mk("UDID-A")) == deviceSetKey(mk()) {
		t.Fatal("an unplug must change the key (else the stale tunnel never goes)")
	}
	if got := deviceSetKey(mk()); got != "" {
		t.Fatalf("empty list key = %q, want empty string", got)
	}
}

// ── TunnelAgent Pause/Resume ───────────────────────────────────────────────
//
// go-ios 的 UpdateTunnels 在一次 ListDevices 失败时拆掉所有健康隧道，会把
// 进行中的 WDA 启动腰斩（真实日志：install 100% → 3s 后 wda_unreachable）。
// Pause 必须让 ticker 整轮跳过（连 ListDevices 都不发），Resume 恢复。

func TestTunnelAgentPauseResumeToggle(t *testing.T) {
	a := &TunnelAgent{paused: new(atomic.Bool)}
	if a.paused.Load() {
		t.Fatal("agent must start unpaused")
	}
	a.Pause()
	if !a.paused.Load() {
		t.Fatal("Pause must set the flag")
	}
	a.Resume()
	if a.paused.Load() {
		t.Fatal("Resume must clear the flag")
	}
}

func TestTunnelAgentNilSafePauseResume(t *testing.T) {
	// Degraded starts return &TunnelAgent{} (paused==nil) or even nil — Pause/
	// Resume must be safe no-ops there, since the wiring always calls them.
	var a *TunnelAgent
	a.Pause()
	a.Resume()
	empty := &TunnelAgent{}
	empty.Pause()
	empty.Resume()
}

// ── Launch 重试的瞬态判定 ─────────────────────────────────────────────────

func TestIsTransientUsbmuxdError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New(`dial tcp 127.0.0.1:27015: connectex: A connection attempt failed because the connected party did not properly respond`), true},
		{errors.New("read tcp ...: i/o timeout"), true},
		{errors.New("connection reset by peer"), true},
		{errors.New("no connection could be made because the target machine actively refused it"), true},
		{errors.New("EOF"), true},
		// 终态错误：语义不同，绝不重试。
		{errors.New("developer mode is disabled"), false},
		{errors.New("device not present: udid xyz"), false},
		{errors.New("codesign: signature invalid"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isTransientUsbmuxdError(c.err); got != c.want {
			t.Errorf("isTransientUsbmuxdError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
