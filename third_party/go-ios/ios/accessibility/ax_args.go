package accessibility

import (
	"encoding/base64"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/nskeyedarchiver"
)

// responseValues collects the decoded values carried by a DTX reply, looking in
// both the payload and the NSKeyedArchiver-encoded auxiliary arguments.
func responseValues(msg dtx.Message) []interface{} {
	out := make([]interface{}, 0, len(msg.Payload)+len(msg.Auxiliary.GetArguments()))
	out = append(out, msg.Payload...)
	for _, arg := range msg.Auxiliary.GetArguments() {
		if b, ok := arg.([]byte); ok {
			if decoded, err := nskeyedarchiver.Unarchive(b); err == nil {
				out = append(out, decoded...)
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

// findPlatformElementBase64 walks a decoded AX object tree and returns the first
// PlatformElementValue_v1 it finds, base64-encoded. The raw bytes appear either
// directly under the key or nested one level deeper under a "Value" key.
func findPlatformElementBase64(v interface{}) (string, bool) {
	switch t := v.(type) {
	case []interface{}:
		for _, it := range t {
			if be, ok := findPlatformElementBase64(it); ok {
				return be, true
			}
		}
	case map[string]interface{}:
		if pe, ok := t["PlatformElementValue_v1"]; ok {
			switch p := pe.(type) {
			case []byte:
				return base64.StdEncoding.EncodeToString(p), true
			case map[string]interface{}:
				if b, ok := p["Value"].([]byte); ok {
					return base64.StdEncoding.EncodeToString(b), true
				}
			}
		}
		for _, vv := range t {
			if be, ok := findPlatformElementBase64(vv); ok {
				return be, true
			}
		}
	}
	return "", false
}
