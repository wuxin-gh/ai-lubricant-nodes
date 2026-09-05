package agent

import (
	"context"
	"fmt"
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

// resolveDownloadURL rewrites the asset URL for url_prefix mode. network mode
// leaves the URL untouched (the transport handles routing).
func resolveDownloadURL(spec proxySpec, rawURL string) string {
	if spec.mode == "url_prefix" && spec.urlPrefix != "" {
		return strings.TrimRight(spec.urlPrefix, "/") + "/" + strings.TrimLeft(rawURL, "/")
	}
	return rawURL
}

// httpClientForProxy builds an *http.Client honoring the proxy spec. The
// default client is returned for empty/direct mode. An error means the spec
// was malformed (caller surfaces it, no partial download started).
func httpClientForProxy(spec proxySpec) (*http.Client, error) {
	if spec.mode == "" || spec.mode == "direct" {
		return http.DefaultClient, nil
	}
	if spec.mode == "url_prefix" {
		// Prefix mode downloads from the prefix host directly with no forwarding
		// proxy. Default client is fine; the URL was already rewritten.
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
		// the userinfo; Go injects them via Proxy-Authorization.
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}, nil
	case "socks5", "socks5h":
		// proxy.FromURL reads credentials from the URL userinfo itself; the second
		// argument is the *forwarding* dialer, so plain TCP goes there.
		dialer, derr := proxy.FromURL(u, proxy.Direct)
		if derr != nil {
			return nil, fmt.Errorf("download: socks5 dialer: %w", derr)
		}
		transport := &http.Transport{}
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
