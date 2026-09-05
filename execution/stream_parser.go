package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// pumpParsedStdout reads a session's stdout, preserving the raw byte stream (each
// line is still appended to the output queue verbatim) while ALSO parsing the
// runtime's structured output:
//
//   - stream mode emits NDJSON frames ({v,seq,type,...}); each is forwarded as a
//     NodeSessionEventStructured so the server sees tool calls / reasoning /
//     progress / todos / file changes without modeling every provider's schema.
//   - a terminal "result" stream frame or a one-shot "__AGENT_RESULT__<json>"
//     prompt line carries the final AgentResult; its JSON is captured and
//     returned so run() can fill NodeSessionResult.result_json.
//
// Structured parsing is best-effort: any line that is not recognized JSON is
// still delivered raw and otherwise ignored. The raw stream is never dropped, so
// existing consumers that read stdout bytes are unaffected.
func (m *sessionManager) pumpParsedStdout(session *nodeSession, r io.Reader) string {
	var resultJSON string
	scanner := bufio.NewScanner(r)
	// Provider output lines (reasoning, transcripts) can be large; raise the cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Preserve the raw byte stream: re-append the newline the scanner stripped.
		session.queue.append([]byte(line+"\n"), agentcomposev2.StdioStream_STDIO_STREAM_STDOUT)

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// One-shot prompt result: "__AGENT_RESULT__{...json...}".
		if strings.HasPrefix(trimmed, runtimeResultPrefix) {
			resultJSON = strings.TrimSpace(strings.TrimPrefix(trimmed, runtimeResultPrefix))
			continue
		}

		// Stream NDJSON frame.
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &frame); err != nil {
			continue
		}
		frameType := jsonStringField(frame, "type")
		if frameType == "" {
			continue
		}
		if frameType == "result" {
			// Terminal result of a stream turn/session: keep the whole frame as the
			// result payload (finalText/transcript/stopReason live inside it).
			resultJSON = trimmed
		}
		m.emitStructuredEvent(session, frameType, frame, trimmed)
	}
	return resultJSON
}

// emitStructuredEvent converts one parsed runtime frame into an upstream
// NodeSessionEventStructured. For "agent_event" frames it digs out the
// item.type so the server can distinguish agent_message/reasoning/command_execution/…,
// and the agent_id so sub-agent items are attributed (empty = root agent).
func (m *sessionManager) emitStructuredEvent(session *nodeSession, frameType string, frame map[string]json.RawMessage, rawLine string) {
	if m.emitStructured == nil {
		return
	}
	itemType := ""
	agentID := ""
	logicalEventID := jsonStringField(frame, "logical_event_id")
	eventName := jsonStringField(frame, "event_name")
	eventKind := jsonStringField(frame, "event_kind")
	toolName := jsonStringField(frame, "tool_name")
	subagentID := jsonStringField(frame, "subagent_id")
	phase := jsonStringField(frame, "phase")
	status := jsonStringField(frame, "status")
	if frameType == "agent_event" {
		// The runtime lifts agent_id to the frame top level (empty = root agent,
		// non-empty = the sub-agent spawned by that Task tool_use).
		agentID = jsonStringField(frame, "agent_id")
		subagentID = firstNonEmpty(subagentID, agentID)
		// Current normalized Claude/ACP envelope: item is top-level. Parse it once
		// and derive legacy fields only here at the node boundary; frontend never
		// guesses from provider payloads.
		if itemRaw, ok := frame["item"]; ok {
			var item map[string]json.RawMessage
			if json.Unmarshal(itemRaw, &item) == nil {
				itemType = jsonStringField(item, "type")
				logicalEventID = firstNonEmpty(logicalEventID, jsonStringField(item, "logical_event_id"), jsonStringField(item, "id"))
				eventKind = firstNonEmpty(eventKind, jsonStringField(item, "event_kind"))
				toolName = firstNonEmpty(toolName, jsonStringField(item, "tool_name"), jsonStringField(item, "title"))
				phase = firstNonEmpty(phase, jsonStringField(item, "phase"))
				status = firstNonEmpty(status, jsonStringField(item, "status"))
				subagentID = firstNonEmpty(subagentID, jsonStringField(item, "subagent_id"))
			}
		}

		// Legacy provider envelopes: message.type or event.item.type.
		if itemType == "" {
			if msgRaw, ok := frame["message"]; ok {
				var msg map[string]json.RawMessage
				if json.Unmarshal(msgRaw, &msg) == nil {
					itemType = jsonStringField(msg, "type")
				}
			}
		}
		if itemType == "" {
			if evtRaw, ok := frame["event"]; ok {
				var evt map[string]json.RawMessage
				if json.Unmarshal(evtRaw, &evt) == nil {
					if itemRaw, ok := evt["item"]; ok {
						var item map[string]json.RawMessage
						if json.Unmarshal(itemRaw, &item) == nil {
							itemType = jsonStringField(item, "type")
						}
					}
				}
			}
		}
	}
	if logicalEventID == "" {
		logicalEventID = jsonStringField(frame, "id")
	}
	if eventName == "" {
		eventName = frameType
	}
	if eventKind == "" {
		eventKind = canonicalEventKind(itemType)
	}
	if subagentID == "" {
		subagentID = agentID
	}

	session.mu.Lock()
	seq := session.structSeq
	session.structSeq++
	session.mu.Unlock()

	evt := &agentcomposev2.NodeSessionEventStructured{
		SessionId:       session.id,
		Seq:             seq,
		EventType:       frameType,
		ItemType:        itemType,
		AgentId:         agentID,
		LogicalEventId:  logicalEventID,
		EventName:       eventName,
		EventKind:       eventKind,
		ToolName:        toolName,
		SubagentId:      subagentID,
		Phase:           phase,
		Status:          status,
		PayloadJson:     rawLine,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
	upstream := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_SessionEvent{SessionEvent: evt},
	}
	if err := m.emitStructured(upstream); err != nil {
		// Structured events are additive; a stream-down drop is acceptable (the raw
		// byte stream is queued reliably and reconciles the session).
		m.logger.Debug("structured event not delivered (stream down)", "session_id", session.id, "error", err)
	}
}

func jsonStringField(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	return jsonStringUnmarshal(raw)
}

func jsonStringUnmarshal(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// canonicalEventKind maps the legacy item_type onto the canonical event_kind.
// Derivation happens ONLY here at the node boundary; downstream consumers read
// event_kind directly instead of guessing from provider vocabulary.
func canonicalEventKind(itemType string) string {
	switch itemType {
	case "agent_message":
		return "message"
	case "reasoning":
		return "reasoning"
	case "tool_call", "command_execution", "mcp_tool_call", "file_change", "web_search":
		return "tool"
	case "error":
		return "error"
	default:
		return ""
	}
}
