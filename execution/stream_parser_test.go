package main

import (
	"encoding/json"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

func TestEmitStructuredEventExtractsAgentID(t *testing.T) {
	var got *agentcomposev2.NodeUpstreamFrame
	m := newSessionManager(sessionOptions{}, testLogger(), noopEmit, noopEmit, func(frame *agentcomposev2.NodeUpstreamFrame) error {
		got = frame
		return nil
	}, noopEmit)
	session := &nodeSession{id: "session-1"}
	// Verbatim-SDK-message shape (Claude): agent_id lifted to the frame top
	// level, the untouched SDK message under "message".
	rawLine := `{"type":"agent_event","agent_id":"call_abc","message":{"type":"assistant","parent_tool_use_id":"call_abc","subagent_type":"claude","uuid":"u1"}}`
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawLine), &frame); err != nil {
		t.Fatal(err)
	}

	m.emitStructuredEvent(session, "agent_event", frame, rawLine)

	if got == nil || got.GetSessionEvent() == nil {
		t.Fatal("expected structured session event")
	}
	evt := got.GetSessionEvent()
	if evt.GetSessionId() != "session-1" {
		t.Fatalf("session id = %q, want session-1", evt.GetSessionId())
	}
	if evt.GetEventType() != "agent_event" {
		t.Fatalf("event type = %q, want agent_event", evt.GetEventType())
	}
	if evt.GetItemType() != "assistant" {
		t.Fatalf("item type = %q, want assistant (SDK message type)", evt.GetItemType())
	}
	if evt.GetAgentId() != "call_abc" {
		t.Fatalf("agent id = %q, want call_abc", evt.GetAgentId())
	}
	if evt.GetPayloadJson() != rawLine {
		t.Fatalf("payload json changed")
	}
}

// Root-agent frames carry an empty agent_id; Codex-shaped item frames still
// resolve item_type from event.item.type.
func TestEmitStructuredEventRootAndCodexShapes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rawLine      string
		wantAgentID  string
		wantItemType string
	}{
		{
			name:         "root agent verbatim message",
			rawLine:      `{"type":"agent_event","agent_id":"","message":{"type":"stream_event","parent_tool_use_id":null}}`,
			wantAgentID:  "",
			wantItemType: "stream_event",
		},
		{
			name:         "codex shaped item",
			rawLine:      `{"type":"agent_event","event":{"item":{"id":"i1","type":"command_execution"}}}`,
			wantAgentID:  "",
			wantItemType: "command_execution",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *agentcomposev2.NodeUpstreamFrame
			m := newSessionManager(sessionOptions{}, testLogger(), noopEmit, noopEmit, func(frame *agentcomposev2.NodeUpstreamFrame) error {
				got = frame
				return nil
			}, noopEmit)
			var frame map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.rawLine), &frame); err != nil {
				t.Fatal(err)
			}

			m.emitStructuredEvent(&nodeSession{id: "s"}, "agent_event", frame, tc.rawLine)

			if got == nil || got.GetSessionEvent() == nil {
				t.Fatal("expected structured session event")
			}
			if id := got.GetSessionEvent().GetAgentId(); id != tc.wantAgentID {
				t.Fatalf("agent id = %q, want %q", id, tc.wantAgentID)
			}
			if it := got.GetSessionEvent().GetItemType(); it != tc.wantItemType {
				t.Fatalf("item type = %q, want %q", it, tc.wantItemType)
			}
		})
	}
}

// The runtime fills a canonical envelope (logical_event_id / event_kind /
// tool_name / subagent_id / phase / status) once, and the parser carries it
// verbatim into the proto so downstream consumers route on protocol fields
// instead of re-deriving intent from a provider payload.
func TestEmitStructuredEventExtractsCanonicalEnvelope(t *testing.T) {
	var got *agentcomposev2.NodeUpstreamFrame
	m := newSessionManager(sessionOptions{}, testLogger(), noopEmit, noopEmit, func(frame *agentcomposev2.NodeUpstreamFrame) error {
		got = frame
		return nil
	}, noopEmit)

	// Flat Claude envelope: canonical fields at the frame top level, the
	// normalized item underneath. This is the opening frame of a tool call.
	rawLine := `{"type":"agent_event","agent_id":"","subagent_id":"","logical_event_id":"toolu_01","event_name":"agent_event","event_kind":"tool","tool_name":"Edit","phase":"start","item":{"id":"toolu_01","type":"tool_call","title":"Edit","tool_name":"Edit","input":{"file_path":"a.ts"},"status":"running"}}`
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawLine), &frame); err != nil {
		t.Fatal(err)
	}

	m.emitStructuredEvent(&nodeSession{id: "s"}, "agent_event", frame, rawLine)

	evt := got.GetSessionEvent()
	if evt.GetLogicalEventId() != "toolu_01" {
		t.Fatalf("logical event id = %q, want toolu_01", evt.GetLogicalEventId())
	}
	if evt.GetEventKind() != "tool" {
		t.Fatalf("event kind = %q, want tool", evt.GetEventKind())
	}
	if evt.GetToolName() != "Edit" {
		t.Fatalf("tool name = %q, want Edit", evt.GetToolName())
	}
	if evt.GetSubagentId() != "" {
		t.Fatalf("subagent id = %q, want empty (root)", evt.GetSubagentId())
	}
	if evt.GetPhase() != "start" {
		t.Fatalf("phase = %q, want start", evt.GetPhase())
	}
	if evt.GetItemType() != "tool_call" {
		t.Fatalf("item type = %q, want tool_call", evt.GetItemType())
	}
}

// A sparse completion frame carries ONLY {id, type, output, status} in its item;
// the canonical fields at the frame top level are what make it routable alone.
func TestEmitStructuredEventSparseCompletionCarriesCanonicalFields(t *testing.T) {
	var got *agentcomposev2.NodeUpstreamFrame
	m := newSessionManager(sessionOptions{}, testLogger(), noopEmit, noopEmit, func(frame *agentcomposev2.NodeUpstreamFrame) error {
		got = frame
		return nil
	}, noopEmit)

	rawLine := `{"type":"agent_event","agent_id":"call_child","logical_event_id":"toolu_02","event_kind":"tool","tool_name":"WebSearch","phase":"complete","item":{"id":"toolu_02","type":"tool_call","title":"WebSearch","output":"results","status":"done"}}`
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawLine), &frame); err != nil {
		t.Fatal(err)
	}

	m.emitStructuredEvent(&nodeSession{id: "s"}, "agent_event", frame, rawLine)

	evt := got.GetSessionEvent()
	if evt.GetLogicalEventId() != "toolu_02" {
		t.Fatalf("logical event id = %q, want toolu_02", evt.GetLogicalEventId())
	}
	if evt.GetToolName() != "WebSearch" {
		t.Fatalf("tool name = %q, want WebSearch", evt.GetToolName())
	}
	if evt.GetSubagentId() != "call_child" {
		t.Fatalf("subagent id = %q, want call_child", evt.GetSubagentId())
	}
	if evt.GetPhase() != "complete" {
		t.Fatalf("phase = %q, want complete", evt.GetPhase())
	}
	if evt.GetStatus() != "done" {
		t.Fatalf("status = %q, want done (from item)", evt.GetStatus())
	}
}

// Frames that predate the canonical envelope still get a derived event_kind
// (item_type → canonical kind) at the node boundary; the fallback never leaks
// provider vocabulary downstream.
func TestEmitStructuredEventDerivesEventKindFromItemType(t *testing.T) {
	for _, tc := range []struct {
		itemType string
		wantKind string
	}{
		{"agent_message", "message"},
		{"reasoning", "reasoning"},
		{"tool_call", "tool"},
		{"command_execution", "tool"},
		{"web_search", "tool"},
		{"error", "error"},
	} {
		t.Run(tc.itemType, func(t *testing.T) {
			var got *agentcomposev2.NodeUpstreamFrame
			m := newSessionManager(sessionOptions{}, testLogger(), noopEmit, noopEmit, func(frame *agentcomposev2.NodeUpstreamFrame) error {
				got = frame
				return nil
			}, noopEmit)
			rawLine := `{"type":"agent_event","agent_id":"","item":{"id":"x","type":"` + tc.itemType + `"}}`
			var frame map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rawLine), &frame); err != nil {
				t.Fatal(err)
			}

			m.emitStructuredEvent(&nodeSession{id: "s"}, "agent_event", frame, rawLine)

			if kind := got.GetSessionEvent().GetEventKind(); kind != tc.wantKind {
				t.Fatalf("event kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}
