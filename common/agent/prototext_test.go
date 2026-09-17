package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// A Chinese Windows host emits command output in the console codepage (cp936 /
// GBK). Carrying that in a protobuf `string` field made the whole ack
// unmarshalable, so the server timed out and the operator saw a generic failure
// instead of npm's real message. SanitizeUTF8 must turn such bytes into valid
// UTF-8 without dropping the text.
func TestSanitizeUTF8DecodesGBK(t *testing.T) {
	// "安装失败" as GBK bytes — the shape npm/Windows error text arrives in.
	gbk := string([]byte{0xB0, 0xB2, 0xD7, 0xB0, 0xCA, 0xA7, 0xB0, 0xDC})
	if utf8.ValidString(gbk) {
		t.Fatal("fixture is already valid UTF-8; test would not exercise the decoder")
	}
	got := SanitizeUTF8(gbk)
	if !utf8.ValidString(got) {
		t.Fatalf("SanitizeUTF8 returned invalid UTF-8: %q", got)
	}
	if got != "安装失败" {
		t.Fatalf("GBK was not decoded to the expected text: got %q", got)
	}
}

func TestSanitizeUTF8LeavesValidTextAlone(t *testing.T) {
	for _, s := range []string{"", "npm error code EAI_AGAIN", "安装失败"} {
		if got := SanitizeUTF8(s); got != s {
			t.Fatalf("valid input was altered: %q -> %q", s, got)
		}
	}
}

// Undecodable bytes must still yield a sendable frame: replacing them is always
// better than failing the marshal, which is what produced the silent
// "editor ack send failed" and the server-side timeout.
func TestSanitizeUTF8ReplacesUndecodableBytes(t *testing.T) {
	raw := "prefix " + string([]byte{0xFF, 0xFE}) + " suffix"
	got := SanitizeUTF8(raw)
	if !utf8.ValidString(got) {
		t.Fatalf("still invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
		t.Fatalf("surrounding text was lost: %q", got)
	}
}

// The ack that actually broke: SendEditorAck carries the npm failure text in
// NodeCommandAck.Error. After sanitizing, the frame must marshal.
func TestSanitizeProtoStringsMakesEditorAckMarshalable(t *testing.T) {
	gbkErr := "D:\\nodejs\\npm.cmd install -g @anthropic-ai/claude-code failed: " +
		string([]byte{0xB0, 0xB2, 0xD7, 0xB0, 0xCA, 0xA7, 0xB0, 0xDC})

	ack := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{
			CommandAck: &agentcomposev2.NodeCommandAck{
				ServerFrameId: "frame-1",
				Error:         gbkErr,
			},
		},
	}

	// Without sanitizing this is exactly the marshal failure the client logged.
	if _, err := proto.Marshal(ack); err == nil {
		t.Fatal("fixture did not reproduce the marshal failure; test is not exercising the bug")
	}

	SanitizeProtoStrings(ack)

	raw, err := proto.Marshal(ack)
	if err != nil {
		t.Fatalf("frame still fails to marshal after sanitizing: %v", err)
	}
	out := &agentcomposev2.NodeUpstreamFrame{}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("sanitized frame does not round-trip: %v", err)
	}
	got := out.GetCommandAck().GetError()
	if !strings.Contains(got, "claude-code") {
		t.Fatalf("error text lost in round-trip: %q", got)
	}
	if !strings.Contains(got, "安装失败") {
		t.Fatalf("GBK portion was not decoded: %q", got)
	}
}

// Nested messages (the capability snapshot RefreshLabels acks) and maps must be
// walked too, not just top-level fields.
func TestSanitizeProtoStringsWalksNestedAndMaps(t *testing.T) {
	bad := string([]byte{0xB0, 0xB2})
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{
			CommandAck: &agentcomposev2.NodeCommandAck{
				ServerFrameId: "f",
				RefreshedCapabilities: &agentcomposev2.NodeCapabilities{
					Os: "windows",
					Labels: map[string]string{
						"editor_version_claude": bad,
					},
				},
			},
		},
	}

	SanitizeProtoStrings(frame)

	if _, err := proto.Marshal(frame); err != nil {
		t.Fatalf("nested/map values were not sanitized: %v", err)
	}
	if got := frame.GetCommandAck().GetRefreshedCapabilities().GetLabels()["editor_version_claude"]; got != "安" {
		t.Fatalf("map value not decoded: %q", got)
	}
}

// DetachStreamContext must keep values but drop cancellation — that is the
// whole point of the fix (a reconnect must not kill an in-flight install).
func TestDetachStreamContextDropsCancellationKeepsValues(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	cancel() // simulate the stream breaking

	if parent.Err() == nil {
		t.Fatal("parent should be canceled")
	}

	detached := DetachStreamContext(parent)
	if detached.Err() != nil {
		t.Fatalf("detached context is already canceled: %v", detached.Err())
	}
	if got := detached.Value(key{}); got != "v" {
		t.Fatalf("detached context lost values: %v", got)
	}
}
