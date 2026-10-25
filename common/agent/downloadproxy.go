package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// proxySpec is the server-supplied egress route for a download. It mirrors the
// proto NodeSelfUpgrade/NodeRuntimeUpgrade proxy_* fields. Zero value means
// download directly (no proxy).
type proxySpec struct {
	mode      string // "" | "network" | "url_prefix"
	url       string // network mode: full proxy URL with credentials pre-injected
	urlPrefix string // url_prefix mode: reverse-proxy prefix base
}

// String renders the spec for a log line, masking any userinfo credentials in
// the network-mode URL so a proxy password never reaches the log. url_prefix is
// printed in full — it is a mirror base, not a credential.
func (p proxySpec) String() string {
	if p.mode == "" && p.url == "" && p.urlPrefix == "" {
		return "direct (no proxy)"
	}
	mode := p.mode
	if mode == "" {
		mode = "direct"
	}
	switch p.mode {
	case "network":
		u, err := url.Parse(p.url)
		if err != nil || u.Host == "" {
			return fmt.Sprintf("mode=%s url=<invalid: %q>", mode, p.url)
		}
		if u.User != nil {
			u.User = url.UserPassword(u.User.Username(), "****")
		}
		return fmt.Sprintf("mode=%s url=%s", mode, u.String())
	case "url_prefix":
		return fmt.Sprintf("mode=%s url_prefix=%s", mode, p.urlPrefix)
	default:
		return fmt.Sprintf("mode=%s", mode)
	}
}

// ProxySpec is the exported form of proxySpec, for callers outside this
// package (the build runner) that must route a `git clone` through the node's
// egress: url_prefix rewrites the clone URL; network passes a git http(s).proxy.
type ProxySpec struct {
	Mode      string // "" | "direct" | "network" | "url_prefix"
	URL       string // network mode: full proxy URL
	URLPrefix string // url_prefix mode: reverse-proxy prefix base
}

// resolveDownloadURL rewrites the asset URL for url_prefix mode. network mode
// leaves the URL untouched (the transport handles routing).
func resolveDownloadURL(spec proxySpec, rawURL string) string {
	if spec.mode == "url_prefix" && spec.urlPrefix != "" {
		return strings.TrimRight(spec.urlPrefix, "/") + "/" + strings.TrimLeft(rawURL, "/")
	}
	return rawURL
}

// forceH1 pins a transport to HTTP/1.1. Setting ForceAttemptHTTP2=false and an
// empty TLSNextProto only stops Go from *upgrading* a connection to h2 after the
// handshake — it does NOT change the ALPN list advertised during the handshake.
// Clone() inherits http.DefaultTransport's TLSClientConfig, which Go's own h2
// setup has already mutated to NextProtos=["h2","http/1.1"]; the server then
// picks h2, Go has no h2 handler, writes an HTTP/1.1 request onto a connection
// the peer treats as h2, and surfaces the peer's h2 frames as a bare
// `Get "...": EOF` before a single body byte (observed 2026-10-21 through the
// 127.0.0.1:7890 CONNECT proxy: h1-forced ALPN → 302, h2-negotiated → empty
// reply). Overriding NextProtos is the part that actually forces h1.
func forceH1(t *http.Transport) *http.Transport {
	t.ForceAttemptHTTP2 = false
	// Empty non-nil TLSNextProto disables the automatic h2 upgrade for https://
	// dials (Go only negotiates h2 when a next-protocol hook exists).
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tlsCfg := t.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	} else {
		tlsCfg = tlsCfg.Clone()
	}
	tlsCfg.NextProtos = []string{"http/1.1"}
	t.TLSClientConfig = tlsCfg
	return t
}

// upgradeTransport forces HTTP/1.1 onto a clone of the default transport. The
// release-asset downloads (the ~188MB node-runtime.tar.gz especially) go straight
// to GitHub; Go's DefaultTransport negotiates HTTP/2 there, and a CDN/middlebox
// that resets the h2 stream mid-body surfaces as
// "stream error: stream ID 1; INTERNAL_ERROR; received from peer" with no way to
// resume — the exact self-upgrade failure this guards against. Forcing h1 makes
// the transfer a plain chunked body that the retry in progressDownload can
// restart cleanly. The socks5 branch already implies h1 (its custom DialContext
// disables auto-h2), which is why proxied upgrades never hit this.
//
// Built from a value-copy of http.DefaultTransport so it inherits the sane
// dial/pool settings without mutating the process-global default.
var upgradeTransport = forceH1(http.DefaultTransport.(*http.Transport).Clone())

// h1Client returns a fresh *http.Client on the forced-h1 transport. A new client
// per call so a caller setting .Timeout never touches a shared instance; the
// Transport itself is concurrency-safe and reused.
func h1Client() *http.Client {
	return &http.Client{Transport: upgradeTransport}
}

// httpClientForProxy builds an *http.Client honoring the proxy spec. Direct
// mode uses the forced-HTTP/1.1 client (see upgradeTransport) to avoid h2
// mid-body stream resets on GitHub CDN. url_prefix mode uses the default
// (h2-capable) client because prefix mirrors (ghfast.top, cf.comic.xin) are
// Cloudflare-fronted and speak HTTP/2 — forcing h1 against them produces
// "malformed HTTP response" (h2 SETTINGS frame parsed as h1 status line).
func httpClientForProxy(spec proxySpec) (*http.Client, error) {
	// Single choke point for EVERY Go-side download (self-upgrade, runtime-upgrade,
	// host-tool, Xcode job, WDA fetch): log the egress route once here so "which
	// proxy did the node actually use" is answerable from the node log for all of
	// them, not just the upgrade path. Credentials are masked by String().
	slog.Default().Info("download: egress proxy", "proxy", spec.String())
	if spec.mode == "" || spec.mode == "direct" {
		return h1Client(), nil
	}
	if spec.mode == "url_prefix" {
		// Prefix mirror is Cloudflare-fronted (h2). The URL was already rewritten
		// by resolveDownloadURL; the client just GETs it directly. Default client
		// auto-negotiates h2 and works; h1Client() does not (regression fixed
		// 2026-10-16 — was returning h1Client() which broke all url_prefix
		// downloads through Cloudflare mirrors).
		return http.DefaultClient, nil
	}
	if spec.mode != "network" {
		return nil, fmt.Errorf("download: unsupported proxy_mode %q", spec.mode)
	}
	u, err := url.Parse(spec.url)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("download: invalid proxy_url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		// http.Transport.Proxy accepts an http(s) proxy URL with credentials in
		// the userinfo; Go injects them via Proxy-Authorization. Force h1 here
		// too: an https CONNECT tunnel to GitHub over an h2-optimized proxy is
		// exactly the middlebox that resets the stream.
		//
		// The ALPN half matters as much as the transport half (see forceH1):
		// through a CONNECT proxy the ALPN handshake happens on the tunneled
		// TLS session, so a server that picks h2 makes Go's h1 writer produce
		// the bare `EOF` this branch is called out for.
		transport := forceH1(http.DefaultTransport.(*http.Transport).Clone())
		transport.Proxy = http.ProxyURL(u)
		return &http.Client{Transport: transport}, nil
	case "socks5", "socks5h":
		// proxy.FromURL reads credentials from the URL userinfo itself; the second
		// argument is the *forwarding* dialer, so plain TCP goes there.
		dialer, derr := proxy.FromURL(u, proxy.Direct)
		if derr != nil {
			return nil, fmt.Errorf("download: socks5 dialer: %w", derr)
		}
		transport := forceH1(&http.Transport{})
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = ctxDialer.DialContext
		} else {
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
		return &http.Client{Transport: transport}, nil
	default:
		return nil, fmt.Errorf("download: unsupported proxy scheme %q", u.Scheme)
	}
}

// ResolveDownloadURL rewrites rawURL through the node's persisted egress-proxy
// snapshot (url_prefix mode prepends the prefix; other modes return the URL
// as-is). Generic downloads (e.g. the WDA artifact fetch) use this so they
// follow the same egress route as self-upgrade.
func (c *Client) ResolveDownloadURL(rawURL string) string {
	return resolveDownloadURL(c.DownloadProxy(), rawURL)
}

// DownloadProxySpec returns the node's persisted egress-proxy snapshot in the
// exported form (the build runner's clone step reads this per build, so a
// runtime proxy-config update applies to the next build without a restart).
func (c *Client) DownloadProxySpec() ProxySpec {
	p := c.DownloadProxy()
	return ProxySpec{Mode: p.mode, URL: p.url, URLPrefix: p.urlPrefix}
}

// HTTPClientForProxyURL builds an *http.Client that routes through the given
// proxy URL (http(s):// or socks5(h)://). It is the exported form of the
// network-mode branch of httpClientForProxy, for callers outside this package
// (the build runner's archive download) that must honor the node's egress route
// for a plain HTTP GET rather than a git command.
func HTTPClientForProxyURL(proxyURL string) (*http.Client, error) {
	return httpClientForProxy(proxySpec{mode: "network", url: proxyURL})
}

// DownloadHTTPClient builds an *http.Client for a large download through the
// node's persisted egress-proxy snapshot (direct client when unset). timeout
// bounds the whole download. Rebuilt per call so a runtime proxy-config update
// applies to the next fetch. The returned client is never the shared
// http.DefaultClient, so mutating its Timeout is safe.
func (c *Client) DownloadHTTPClient(timeout time.Duration) (*http.Client, error) {
	client, err := httpClientForProxy(c.DownloadProxy())
	if err != nil {
		return nil, err
	}
	if client == http.DefaultClient {
		client = &http.Client{}
	}
	client.Timeout = timeout
	return client, nil
}

// DirectHTTPClient builds a plain direct (no proxy) *http.Client with the given
// timeout. It is the fallback for a proxy that mangles the stream: the egress
// route stays the default, direct egress the safety net. Never the shared
// http.DefaultClient.
func (c *Client) DirectHTTPClient(timeout time.Duration) (*http.Client, error) {
	return &http.Client{Timeout: timeout}, nil
}
