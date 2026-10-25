package agent

import (
	"net/http"
	"testing"
)

// Every large-asset download must speak HTTP/1.1 *at the ALPN level*, not merely
// have Go's h2 upgrade switched off.
//
// The 2026-10-21 self-upgrade EOF: the transports set ForceAttemptHTTP2=false +
// an empty TLSNextProto, which only stops Go from *upgrading* a connection after
// the handshake. http.DefaultTransport.Clone() carries
// TLSClientConfig.NextProtos=["h2","http/1.1"], so ALPN still advertised h2; the
// peer picked it, Go had no h2 handler, wrote an HTTP/1.1 request onto an h2
// connection, and read the peer's SETTINGS frame back as a bare
// `Get "...": EOF` before a single body byte. forceH1 pins NextProtos to
// ["http/1.1"], which is the half that actually works.
//
// Asserting on the built clients is deliberate: a future edit that drops the
// NextProtos line still *looks* like it forces h1.

func wantH1(t *testing.T, name string, c *http.Client) {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("%s: transport is %T, expected *http.Transport", name, c.Transport)
	}
	if tr.ForceAttemptHTTP2 {
		t.Errorf("%s: ForceAttemptHTTP2 must be false", name)
	}
	if tr.TLSClientConfig == nil {
		t.Fatalf("%s: TLSClientConfig nil — Go will advertise h2 by default", name)
	}
	got := tr.TLSClientConfig.NextProtos
	if len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("%s: ALPN NextProtos = %v, want [http/1.1] — this is the EOF bug", name, got)
	}
}

func TestDownloadTransportsForceH1ALPN(t *testing.T) {
	// direct / "" mode → h1Client on the shared upgradeTransport.
	for _, mode := range []string{"", "direct"} {
		c, err := httpClientForProxy(proxySpec{mode: mode})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		wantH1(t, "mode="+mode, c)
	}

	// network mode, both http and https CONNECT proxies.
	for _, proxyURL := range []string{"http://127.0.0.1:7890", "https://proxy.example:8443"} {
		c, err := httpClientForProxy(proxySpec{mode: "network", url: proxyURL})
		if err != nil {
			t.Fatalf("network %s: %v", proxyURL, err)
		}
		wantH1(t, "network "+proxyURL, c)
	}

	// socks5 uses a custom DialContext (which alone would not auto-enable h2) but
	// still has to pin ALPN, so it goes through forceH1 too.
	c, err := httpClientForProxy(proxySpec{mode: "network", url: "socks5h://127.0.0.1:7891"})
	if err != nil {
		t.Fatalf("socks5: %v", err)
	}
	wantH1(t, "socks5", c)
}

// url_prefix mirrors are Cloudflare-fronted and speak h2; forcing h1 there is a
// known regression (2026-10-16: "malformed HTTP response", the h2 SETTINGS frame
// parsed as an h1 status line). Pin the opposite expectation.
func TestURLPrefixMirrorStaysH2Capable(t *testing.T) {
	c, err := httpClientForProxy(proxySpec{mode: "url_prefix", urlPrefix: "https://cf.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if c != http.DefaultClient {
		t.Errorf("url_prefix must return the default h2-capable client, got %+v", c)
	}
	// The default client's Transport is nil, which means http.DefaultTransport —
	// h2-capable, exactly what a Cloudflare-fronted mirror needs.
	if c.Transport != nil {
		t.Errorf("url_prefix must keep the default client's h2 transport, got %T", c.Transport)
	}
}

// The exported constructor used by the build runner / host-tool installer goes
// through the same choke point.
func TestExportedConstructorsForceH1(t *testing.T) {
	c, err := HTTPClientForProxyURL("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	wantH1(t, "HTTPClientForProxyURL", c)
}
