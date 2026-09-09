package main

import (
	"os"
	"sort"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// TestProbeRealHome is a manual probe: point it at the operator's real HOME and
// print what inspectSystemEnv / the MCP + plugin sweeps return, so the console
// scan changes can be eyeballed before the binary ships. Skipped under -short.
func TestProbeRealHome(t *testing.T) {
	if testing.Short() {
		t.Skip("manual probe; run without -short")
	}
	if os.Getenv("AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV") == "" {
		t.Skip("set AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV=on to probe the real HOME")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HOME: %s", home)

	t.Log("\n=== MCP sources ===")
	for _, src := range systemEnvMCPSources(home) {
		t.Logf("  %-10s %-24s %s", src.provider, src.name, src.path)
	}

	t.Log("\n=== claude plugins ===")
	for _, e := range systemEnvClaudePlugins(home) {
		t.Logf("  %-22s v%-16s %s", e.GetName(), e.GetVersion(), e.GetPath())
		t.Logf("    desc: %s", e.GetDescription())
	}

	m := &sessionManager{opts: sessionOptions{systemEnvAllowed: true}}
	entries, err := m.inspectSystemEnv(&agentcomposev2.NodeInspectSystemEnv{})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.GetKind() != b.GetKind() {
			return a.GetKind() < b.GetKind()
		}
		return a.GetName() < b.GetName()
	})
	kindCount := map[string]int{}
	t.Log("\n=== inspectSystemEnv (full inventory) ===")
	for _, e := range entries {
		kindCount[e.GetKind()]++
		desc := e.GetDescription()
		if len(desc) > 55 {
			desc = desc[:55] + "…"
		}
		t.Logf("  %-6s %-26s prov=%-8s v=%-14s %s", e.GetKind(), e.GetName(), e.GetProvider(), e.GetVersion(), e.GetPath())
		t.Logf("    %s", desc)
	}
	t.Logf("\nkind counts: %v", kindCount)
}
