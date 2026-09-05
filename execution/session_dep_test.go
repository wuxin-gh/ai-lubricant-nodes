package main

import (
	"path/filepath"
	"testing"
)

// The gateway sanitizes subdir names, but the node must not trust a value that
// arrived over the wire: a separator or traversal segment would let an
// associated repo be cloned outside the session workspace.
func TestSafeSubdirSegmentRejectsTraversal(t *testing.T) {
	for _, in := range []string{
		"..",
		".",
		"../escape",
		"a/b",
		`a\b`,
		"/abs",
		"nested/..",
		"..hidden/..",
		"",
		"   ",
	} {
		if got := safeSubdirSegment(in); got != "" {
			t.Fatalf("%q must be rejected; got %q", in, got)
		}
	}
}

func TestSafeSubdirSegmentAcceptsPlainNames(t *testing.T) {
	for _, in := range []string{
		"dep",
		"Ai-Lubricant_v2.0",
		"lib.core",
		"  padded  ",
	} {
		got := safeSubdirSegment(in)
		if got == "" {
			t.Fatalf("%q must be accepted", in)
		}
		if filepath.Base(got) != got {
			t.Fatalf("%q must stay a single segment; got %q", in, got)
		}
	}
}

// A rejected segment must never be joined into a path: verify the guard holds
// for the case that motivated it (escaping the workspace root).
func TestSafeSubdirSegmentBlocksWorkspaceEscape(t *testing.T) {
	workDir := filepath.Join("tmp", "tasks", "task-1")
	if seg := safeSubdirSegment("../../etc"); seg != "" {
		joined := filepath.Join(workDir, seg)
		t.Fatalf("traversal segment must be rejected before Join; would have produced %q", joined)
	}
}
