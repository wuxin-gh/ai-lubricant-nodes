package agent

import (
	"strings"
	"testing"
)

// TestResolveSystemEnvCapability covers the tri-state resolution of
// --allow-system-env. "auto" flips on the container probe (inverted: a host
// install allows, a container refuses), on/off force it, anything else errors.
func TestResolveSystemEnvCapability(t *testing.T) {
	// The probe answer this host would give — used for the auto expectations.
	auto := !RunningInContainer()

	cases := []struct {
		value string
		want  bool
		error bool
	}{
		{"", auto, false},
		{"auto", auto, false},
		{"AUTO", auto, false},   // case-insensitive
		{" auto ", auto, false}, // trimmed
		{"on", true, false},
		{"true", true, false},
		{"1", true, false},
		{"yes", true, false},
		{"off", false, false},
		{"false", false, false},
		{"0", false, false},
		{"no", false, false},
		{"maybe", false, true},
		{"enable", false, true},
	}
	for _, tc := range cases {
		got, err := ResolveSystemEnvCapability(tc.value)
		if tc.error {
			if err == nil {
				t.Errorf("ResolveSystemEnvCapability(%q) = %v, want error", tc.value, got)
			} else if !strings.Contains(err.Error(), "auto|on|off") {
				t.Errorf("ResolveSystemEnvCapability(%q) error = %v, want usage hint", tc.value, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveSystemEnvCapability(%q) unexpected error: %v", tc.value, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveSystemEnvCapability(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
