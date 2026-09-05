package totp

import (
	"encoding/base32"
	"testing"
	"time"
)

// TestGenerateRFC6238Vector checks the algorithm against a known RFC 6238 test
// vector. The RFC uses the ASCII secret "12345678901234567890" (SHA1 variant).
// At Unix time 59 (time step 1) the 8-digit code is 94287082; truncated to our
// 6 digits that is 287082.
func TestGenerateRFC6238Vector(t *testing.T) {
	secret := []byte("12345678901234567890")
	got := Generate(secret, time.Unix(59, 0).UTC())
	if want := "287082"; got != want {
		t.Fatalf("Generate at t=59 = %q, want %q", got, want)
	}
	// t=1111111109 → 8-digit 07081804 → 6-digit 081804.
	got = Generate(secret, time.Unix(1111111109, 0).UTC())
	if want := "081804"; got != want {
		t.Fatalf("Generate at t=1111111109 = %q, want %q", got, want)
	}
}

func TestValidateWithinWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111109, 0).UTC()
	// Code generated one step in the past must still validate at now (±window).
	past := Generate(secret, now.Add(-TimeStep*time.Second))
	if !Validate(secret, past, now, nil, "node-1") {
		t.Fatalf("code from previous step should validate within window")
	}
	// A code far outside the window must not validate.
	stale := Generate(secret, now.Add(-10*TimeStep*time.Second))
	if Validate(secret, stale, now, nil, "node-1") {
		t.Fatalf("code well outside window must not validate")
	}
}

func TestValidateRejectsWrongCode(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Now().UTC()
	if Validate(secret, "000000", now, nil, "node-1") && Generate(secret, now) != "000000" {
		t.Fatalf("clearly wrong code must not validate")
	}
	if Validate(secret, "12345", now, nil, "node-1") {
		t.Fatalf("wrong-length code must not validate")
	}
}

func TestReplayCacheBlocksReuse(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Now().UTC()
	code := Generate(secret, now)
	cache := NewReplayCache()
	if !Validate(secret, code, now, cache, "node-1") {
		t.Fatalf("first use of a valid code must succeed")
	}
	if Validate(secret, code, now, cache, "node-1") {
		t.Fatalf("second use of the same code must be rejected (replay)")
	}
	// A different node using the same code value is independent.
	if !Validate(secret, code, now, cache, "node-2") {
		t.Fatalf("same code from a different node must be allowed")
	}
}

func TestReplayCacheExpires(t *testing.T) {
	cache := NewReplayCache()
	now := time.Now().UTC()
	if !cache.Claim("node-1", "111111", now) {
		t.Fatalf("first claim must succeed")
	}
	if cache.Claim("node-1", "111111", now) {
		t.Fatalf("immediate re-claim must fail")
	}
	// After the TTL the entry is pruned and the code can be claimed again.
	later := now.Add(cache.ttl + time.Second)
	if !cache.Claim("node-1", "111111", later) {
		t.Fatalf("claim after TTL must succeed again")
	}
}

func TestGenerateAndDecodeSecretRoundTrip(t *testing.T) {
	enc, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	raw, err := DecodeSecret(enc)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	if len(raw) != SecretBytes {
		t.Fatalf("decoded secret len = %d, want %d", len(raw), SecretBytes)
	}
	// Lowercase + whitespace should still decode.
	if _, err := DecodeSecret("  " + enc + "  "); err != nil {
		t.Fatalf("DecodeSecret with surrounding whitespace: %v", err)
	}
}

func TestDecodeSecretPaddedInput(t *testing.T) {
	// A padded base32 secret (as some tools emit) must also decode.
	raw := []byte("12345678901234567890")
	padded := base32.StdEncoding.EncodeToString(raw)
	if _, err := DecodeSecret(padded); err != nil {
		t.Fatalf("DecodeSecret padded: %v", err)
	}
}
