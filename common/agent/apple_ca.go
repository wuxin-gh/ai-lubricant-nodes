// Apple CA trust for outbound HTTPS requests to Apple's private-CA endpoints
// (gsa.apple.com is the one GSA auth endpoint in current use). The chain
// terminates at Apple Root CA — a private CA absent from every public trust
// store (certifi / system roots) — so a stock http.Transport would fail the
// handshake with "certificate signed by unknown authority". The pinned bundle
// is appended ON TOP of the system pool: system trust stays intact, nothing is
// weakened, Apple's private chain simply becomes verifiable too.
package agent

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"sync"
)

//go:embed certs/apple_gsa_ca.pem
var appleGSACAPEM []byte

var (
	appleCAPoolOnce sync.Once
	appleCAPool     *x509.CertPool
	appleCAPoolErr  error
)

// AppleCATLSConfig returns a TLS config whose root pool is the system pool
// PLUS the pinned Apple CA bundle. Returns (nil, nil) when the system pool
// cannot be read — callers then fall back to Go's default (system) behavior
// rather than downgrading to InsecureSkipVerify.
func AppleCATLSConfig() *tls.Config {
	appleCAPoolOnce.Do(func() {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if ok := pool.AppendCertsFromPEM(appleGSACAPEM); !ok {
			appleCAPoolErr = fmt.Errorf("embedded apple_gsa_ca.pem parsed no certificates")
			return
		}
		appleCAPool = pool
	})
	if appleCAPoolErr != nil {
		return nil
	}
	return &tls.Config{RootCAs: appleCAPool} //nolint:gosec // G402 is about InsecureSkipVerify; this config only widens trust, never disables verification
}
