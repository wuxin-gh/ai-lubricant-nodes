// Package totp implements the RFC 6238 time-based one-time password algorithm
// used to authenticate distributed execution nodes against the server.
//
// A node is provisioned with a per-node secret (minted by OnboardNode, stored
// encrypted on the server). At connect time the server sends its current time
// (NodeServerHello); the node derives a TOTP code from that time and the secret,
// and sends it in NodeRegister. The server re-derives the expected code over a
// small window of time steps (to absorb clock skew and network latency) and
// rejects reused codes via a short-TTL replay cache.
//
// Parameters match the Google Authenticator defaults — HMAC-SHA1, 6 digits,
// 30-second time step, base32-encoded secrets — so the same secret is also
// usable from a human authenticator app if an operator ever wants that.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// TimeStep is the TOTP time step in seconds (RFC 6238 / Google Authenticator default).
	TimeStep = 30
	// Digits is the number of digits in the code.
	Digits = 6
	// SecretBytes is the entropy length of a generated secret, in bytes.
	SecretBytes = 20
)

// StepWindow is the number of time steps on each side of the reference time the
// validator accepts. With TimeStep=30 this gives a ±windowStep*30s tolerance.
const StepWindow = 2

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a fresh random base32-encoded (no padding) secret.
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate totp secret: %w", err)
	}
	return base32NoPad.EncodeToString(buf), nil
}

// DecodeSecret parses a base32-encoded secret into raw bytes. Uppercasing and
// whitespace are tolerated; padding is optional.
func DecodeSecret(encoded string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.Join(strings.Fields(encoded), ""))
	decoded, err := base32NoPad.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("decode totp secret: %w", err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("decode totp secret: empty")
	}
	return decoded, nil
}

// Generate returns the TOTP code for the given secret at time t. The secret is
// the raw decoded bytes (see DecodeSecret).
func Generate(secret []byte, t time.Time) string {
	counter := uint64(t.UTC().Unix()) / TimeStep
	return generateAtCounter(secret, counter)
}

func generateAtCounter(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := int(sum[len(sum)-1] & 0x0f)
	bin := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	code := bin % mod
	return fmt.Sprintf("%0*d", Digits, code)
}

// Validate reports whether code is a valid TOTP for secret at now. It accepts
// codes within ±StepWindow time steps of now and, when used is non-nil, marks
// the accepted (node, code) as consumed so it cannot be replayed within the
// cache TTL. A code that has already been used within the TTL is rejected.
func Validate(secret []byte, code string, now time.Time, used *ReplayCache, nodeID string) bool {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return false
	}
	base := uint64(now.UTC().Unix()) / TimeStep
	for delta := -StepWindow; delta <= StepWindow; delta++ {
		step := int64(base) + int64(delta)
		if step < 0 {
			continue
		}
		if generateAtCounter(secret, uint64(step)) == code {
			if used != nil && !used.Claim(nodeID, code, now) {
				return false // replay
			}
			return true
		}
	}
	return false
}

// ReplayCache tracks recently accepted (nodeID, code) pairs to block replay
// within the acceptance window. Entries expire a little after the widest window
// that could still validate, so a code can never be accepted twice. Memory-only
// is fine: node counts are modest and the TTL is short.
type ReplayCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[string]time.Time
}

// NewReplayCache returns a cache whose TTL covers the largest skew window the
// validator will accept, with a small margin.
func NewReplayCache() *ReplayCache {
	ttl := time.Duration((StepWindow*2+1)*TimeStep) * time.Second
	ttl += 30 * time.Second // margin for processing delay
	return &ReplayCache{ttl: ttl, seen: map[string]time.Time{}}
}

// Claim reports whether (nodeID, code) is fresh — i.e. not already consumed
// within the TTL — and, if so, records it as consumed. It prunes expired
// entries opportunistically on each call.
func (c *ReplayCache) Claim(nodeID, code string, now time.Time) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	key := nodeID + "|" + code
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = now
	return true
}

func (c *ReplayCache) pruneLocked(now time.Time) {
	for k, at := range c.seen {
		if now.Sub(at) > c.ttl {
			delete(c.seen, k)
		}
	}
}
