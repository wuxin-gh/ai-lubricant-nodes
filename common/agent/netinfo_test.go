package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolvePublicIPRequiresThreeMatchingSourcesSequentially(t *testing.T) {
	var active int32
	var maxActive int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)
		if n > atomic.LoadInt32(&maxActive) {
			atomic.StoreInt32(&maxActive, n)
		}
		_, _ = fmt.Fprint(w, "address=203.0.113.9")
	}))
	defer server.Close()

	ip, err := resolvePublicIP(context.Background(), []string{
		server.URL + "/one",
		server.URL + "/two",
		server.URL + "/three",
		server.URL + "/unused",
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "203.0.113.9" {
		t.Fatalf("got %q", ip)
	}
	if maxActive != 1 {
		t.Fatalf("requests overlapped: max active=%d", maxActive)
	}
}

func TestResolvePublicIPRejectsWrongFamilyAndNoConsensus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v6" {
			_, _ = fmt.Fprint(w, "2001:db8::1")
			return
		}
		_, _ = fmt.Fprint(w, "198.51.100.1")
	}))
	defer server.Close()

	_, err := resolvePublicIP(context.Background(), []string{
		server.URL + "/v6",
		server.URL + "/v4-a",
		server.URL + "/v4-b",
	}, 4)
	if err == nil {
		t.Fatal("expected no-consensus error")
	}
}

// newIPv6TestServer starts an httptest server bound to the IPv6 loopback so the
// tcp6 family dialer can actually reach it. The default httptest listener is
// 127.0.0.1, which tcp6 cannot dial.
func newIPv6TestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("ipv6 loopback unavailable: %v", err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Listener = ln
	s.Start()
	return s
}

// TestRefreshPublicIPsRunsFamiliesConcurrently asserts the two address families
// resolve at the same time while each family's own URL loop stays serial. A
// shared in-flight counter is bumped by every request on both servers; because
// each family alone allows only one in flight (serial loop), the shared counter
// can reach 2 only if an IPv4 and an IPv6 request overlap.
func TestRefreshPublicIPsRunsFamiliesConcurrently(t *testing.T) {
	var active int32
	var maxActive int32
	hold := func() func() {
		n := atomic.AddInt32(&active, 1)
		for {
			cur := atomic.LoadInt32(&maxActive)
			if n <= cur || atomic.CompareAndSwapInt32(&maxActive, cur, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		return func() { atomic.AddInt32(&active, -1) }
	}

	v4Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer hold()()
		_, _ = fmt.Fprint(w, "203.0.113.9")
	}))
	defer v4Server.Close()

	v6Server := newIPv6TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer hold()()
		_, _ = fmt.Fprint(w, "2001:db8::1")
	}))
	defer v6Server.Close()

	c := &Client{logger: slog.Default()}
	c.publicIP.configRevision = 1
	c.publicIP.ipv4URLs = []string{
		v4Server.URL + "/a", v4Server.URL + "/b", v4Server.URL + "/c",
	}
	c.publicIP.ipv6URLs = []string{
		v6Server.URL + "/a", v6Server.URL + "/b", v6Server.URL + "/c",
	}

	c.refreshPublicIPs(context.Background())

	if c.publicIP.ipv4 != "203.0.113.9" {
		t.Fatalf("ipv4 = %q, want 203.0.113.9", c.publicIP.ipv4)
	}
	if c.publicIP.ipv6 != "2001:db8::1" {
		t.Fatalf("ipv6 = %q, want 2001:db8::1", c.publicIP.ipv6)
	}
	if maxActive < 2 {
		t.Fatalf("address families did not overlap: max in-flight=%d", maxActive)
	}
}

// TestRefreshPublicIPsOneFamilyFailureDoesNotBlockOther asserts that when one
// family cannot reach consensus the other family's confirmed result still
// lands, and refreshPublicIPs returns rather than hanging on the failed family.
func TestRefreshPublicIPsOneFamilyFailureDoesNotBlockOther(t *testing.T) {
	v4Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.9")
	}))
	defer v4Server.Close()

	// Every v6 response is a distinct address → no three-source consensus.
	var v6Counter int32
	v6Server := newIPv6TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&v6Counter, 1)
		_, _ = fmt.Fprintf(w, "2001:db8::%x", n)
	}))
	defer v6Server.Close()

	c := &Client{logger: slog.Default()}
	c.publicIP.configRevision = 1
	c.publicIP.ipv4URLs = []string{
		v4Server.URL + "/a", v4Server.URL + "/b", v4Server.URL + "/c",
	}
	c.publicIP.ipv6URLs = []string{
		v6Server.URL + "/a", v6Server.URL + "/b", v6Server.URL + "/c",
		v6Server.URL + "/d",
	}

	done := make(chan struct{})
	go func() {
		c.refreshPublicIPs(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refreshPublicIPs blocked on failed family")
	}

	if c.publicIP.ipv4 != "203.0.113.9" {
		t.Fatalf("ipv4 = %q, want 203.0.113.9 (v4 must succeed despite v6 failure)", c.publicIP.ipv4)
	}
	if c.publicIP.ipv6 != "" {
		t.Fatalf("ipv6 = %q, want empty (no consensus)", c.publicIP.ipv6)
	}
}
