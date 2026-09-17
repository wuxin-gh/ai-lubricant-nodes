package agent

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// SanitizeUTF8 makes an externally-sourced string safe to carry in a protobuf
// `string` field.
//
// Why: protobuf requires string fields to be valid UTF-8 and fails the entire
// frame marshal when one is not. Command output on a Chinese Windows host is
// emitted in the console codepage (cp936 / GBK), so a single npm error line made
// the whole NodeCommandAck unsendable — the client logged "editor ack send
// failed: marshal message: string field contains invalid UTF-8" and the server
// then timed out, so the operator saw a generic failure instead of the real
// npm message. Decode GBK first (the dominant non-UTF-8 source on the Windows
// hosts this runs on); anything still undecodable is replaced rather than
// throwing the whole frame away.
func SanitizeUTF8(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	if decoded, _, err := transform.String(simplifiedchinese.GBK.NewDecoder(), s); err == nil && utf8.ValidString(decoded) {
		return decoded
	}
	return strings.ToValidUTF8(s, "�")
}

// SanitizeProtoStrings repairs every invalid-UTF-8 string field reachable from
// a protobuf message, in place. It is the structural choke point that keeps an
// ack/result frame sendable regardless of what a child process wrote to stderr.
//
// String scalars, repeated strings, string-keyed and string-valued maps are all
// covered; nested messages are walked. The walk only allocates when it finds a
// field that needs fixing, so the hot path (session output, whose payload is a
// `bytes` field and rarely contains an invalid string) stays cheap.
func SanitizeProtoStrings(m proto.Message) {
	if m == nil {
		return
	}
	sanitizeReflect(m.ProtoReflect())
}

func sanitizeReflect(msg protoreflect.Message) {
	if !msg.IsValid() {
		return
	}
	// protobuf forbids mutation during Range; collect fixes and apply after.
	type setter func()
	var fixes []setter
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() != protoreflect.StringKind {
				return true
			}
			mp := v.Map()
			type pair struct{ k, s string }
			var changed []pair
			mp.Range(func(mk protoreflect.MapKey, mv protoreflect.Value) bool {
				if s := mv.String(); !utf8.ValidString(s) {
					changed = append(changed, pair{mk.String(), SanitizeUTF8(s)})
				}
				return true
			})
			if len(changed) > 0 {
				fixes = append(fixes, func() {
					for _, c := range changed {
						mk := protoreflect.ValueOfString(c.k).MapKey()
						mp.Set(mk, protoreflect.ValueOfString(c.s))
					}
				})
			}
		case fd.IsList():
			if fd.Kind() != protoreflect.StringKind {
				return true
			}
			l := v.List()
			type idx struct{ i int; s string }
			var changed []idx
			for i := 0; i < l.Len(); i++ {
				if s := l.Get(i).String(); !utf8.ValidString(s) {
					changed = append(changed, idx{i, SanitizeUTF8(s)})
				}
			}
			if len(changed) > 0 {
				fixes = append(fixes, func() {
					for _, c := range changed {
						l.Set(c.i, protoreflect.ValueOfString(c.s))
					}
				})
			}
		case fd.Kind() == protoreflect.StringKind:
			if s := v.String(); !utf8.ValidString(s) {
				field, fixed := fd, SanitizeUTF8(s)
				fixes = append(fixes, func() { msg.Set(field, protoreflect.ValueOfString(fixed)) })
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sanitizeReflect(v.Message())
		}
		return true
	})
	for _, f := range fixes {
		f()
	}
}
