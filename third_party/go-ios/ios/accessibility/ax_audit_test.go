package accessibility

import (
	"encoding/base64"
	"testing"

	"github.com/danielpaulus/go-ios/ios/nskeyedarchiver"
)

// TestExtractIssues covers parsing the AXAuditIssue_v1 maps the device delivers in
// the audit completion callback, using the two shapes observed on real hardware:
// iOS 15+ issues carry the named auditTestTypeValue_v1, while iOS 14 issues carry
// only the numeric IssueClassificationValue_v1.
func TestExtractIssues(t *testing.T) {
	platform := []byte{0xf1, 0x00, 0x01, 0x02, 0x03}
	// Shape after deserializeObject: passthrough envelopes already unwrapped, so the
	// issue keys map directly to their values (as logged from the iPhone 13/SE).
	tree := []interface{}{
		map[string]interface{}{
			"auditTestTypeValue_v1": "testTypeDynamicText",
			"ElementRectValue_v1":   nskeyedarchiver.NSValue{NSRectval: "{{84, 485}, {86, 20}}"},
			"AuditElementValue_v1": map[string]interface{}{
				"PlatformElementValue_v1": platform,
			},
		},
		map[string]interface{}{
			// iOS 14 style: numeric classification, no named type.
			"IssueClassificationValue_v1": uint64(auditTypeContrast),
			"ElementRectValue_v1":         nskeyedarchiver.NSValue{NSRectval: "{{1, 2}, {3, 4}}"},
		},
		map[string]interface{}{
			// Unknown numeric id falls back to the raw number.
			"IssueClassificationValue_v1": uint64(9999),
		},
	}

	issues := extractIssues(tree)
	if len(issues) != 3 {
		t.Fatalf("want 3 issues, got %d: %+v", len(issues), issues)
	}

	if issues[0].IssueType != "testTypeDynamicText" {
		t.Errorf("issue[0] type = %q, want testTypeDynamicText", issues[0].IssueType)
	}
	if got := issues[0].ElementRect; got["x"] != 84 || got["y"] != 485 || got["width"] != 86 || got["height"] != 20 {
		t.Errorf("issue[0] rect = %v", got)
	}
	if want := base64.StdEncoding.EncodeToString(platform); issues[0].PlatformElementValue != want {
		t.Errorf("issue[0] platformElementValue = %q, want %q", issues[0].PlatformElementValue, want)
	}

	if issues[1].IssueType != "testTypeContrast" {
		t.Errorf("issue[1] type = %q, want testTypeContrast (mapped from %d)", issues[1].IssueType, auditTypeContrast)
	}
	if issues[2].IssueType != "9999" {
		t.Errorf("issue[2] type = %q, want raw 9999", issues[2].IssueType)
	}
}

func TestTryExtractRect(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want map[string]float64
		ok   bool
	}{
		{"NSValue", nskeyedarchiver.NSValue{NSRectval: "{{10.5, 20}, {30, 40.25}}"}, map[string]float64{"x": 10.5, "y": 20, "width": 30, "height": 40.25}, true},
		{"string", "{{0, 64}, {390, 44}}", map[string]float64{"x": 0, "y": 64, "width": 390, "height": 44}, true},
		{"map", map[string]interface{}{"x": 1.0, "y": 2.0, "width": 3.0, "height": 4.0}, map[string]float64{"x": 1, "y": 2, "width": 3, "height": 4}, true},
		{"too few numbers", "{{1, 2}}", nil, false},
		{"unrelated", 42, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := tryExtractRect(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestFindPlatformElementBase64(t *testing.T) {
	raw := []byte{0x01, 0x02, 0x03}
	want := base64.StdEncoding.EncodeToString(raw)

	// Direct form (audit issues): PlatformElementValue_v1 -> []byte.
	direct := map[string]interface{}{
		"AuditElementValue_v1": map[string]interface{}{"PlatformElementValue_v1": raw},
	}
	if got, ok := findPlatformElementBase64(direct); !ok || got != want {
		t.Errorf("direct form: got %q ok=%v, want %q", got, ok, want)
	}

	// Nested form: PlatformElementValue_v1 -> {Value: []byte}.
	nested := map[string]interface{}{
		"PlatformElementValue_v1": map[string]interface{}{"Value": raw},
	}
	if got, ok := findPlatformElementBase64(nested); !ok || got != want {
		t.Errorf("nested form: got %q ok=%v, want %q", got, ok, want)
	}

	if _, ok := findPlatformElementBase64(map[string]interface{}{"unrelated": 1}); ok {
		t.Error("expected no element for unrelated map")
	}
}
