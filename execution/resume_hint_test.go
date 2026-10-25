package main

import (
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// The adopt-resume hint rides the tags map. Two things are worth locking: the
// tag KEY (it is a cross-language contract — the data service writes
// "resume_thread_id" in a Python file this test cannot see) and the
// empty-means-fresh behaviour, which is what every non-adopted task relies on.
func TestResumeThreadHint(t *testing.T) {
	cases := []struct {
		name string
		spec *agentcomposev2.NodeCreateSession
		want string
	}{
		{"nil spec starts fresh", nil, ""},
		{"no tags starts fresh", &agentcomposev2.NodeCreateSession{}, ""},
		{"unrelated tags start fresh",
			&agentcomposev2.NodeCreateSession{Tags: map[string]string{"env_id": "e-1"}}, ""},
		{"blank hint starts fresh",
			&agentcomposev2.NodeCreateSession{Tags: map[string]string{"resume_thread_id": "   "}}, ""},
		{"hint is carried verbatim",
			&agentcomposev2.NodeCreateSession{
				Tags: map[string]string{"resume_thread_id": "5a99d18c-8973-4fd4-a502-4edacba5930c"},
			},
			"5a99d18c-8973-4fd4-a502-4edacba5930c"},
		{"whitespace is trimmed",
			&agentcomposev2.NodeCreateSession{
				Tags: map[string]string{"resume_thread_id": " abc-123 "},
			},
			"abc-123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeThreadHint(tc.spec); got != tc.want {
				t.Fatalf("resumeThreadHint = %q, want %q", got, tc.want)
			}
		})
	}
}
