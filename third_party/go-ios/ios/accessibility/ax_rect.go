package accessibility

import (
	"regexp"
	"strconv"

	"github.com/danielpaulus/go-ios/ios/nskeyedarchiver"
)

var rectNumRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// tryExtractRect parses an on-screen rect {x, y, width, height} from the shapes the
// AX audit returns it in: an NSValue rect ("{{x, y}, {w, h}}"), a plain string, or
// a map with x/y/width/height keys.
func tryExtractRect(v interface{}) (map[string]float64, bool) {
	switch t := v.(type) {
	case nskeyedarchiver.NSValue:
		return parseNSRectString(t.NSRectval)
	case string:
		return parseNSRectString(t)
	case map[string]interface{}:
		for _, ks := range [][]string{
			{"x", "y", "width", "height"},
			{"X", "Y", "Width", "Height"},
			{"XValue", "YValue", "WidthValue", "HeightValue"},
		} {
			x, xok := toFloat(t[ks[0]])
			y, yok := toFloat(t[ks[1]])
			w, wok := toFloat(t[ks[2]])
			h, hok := toFloat(t[ks[3]])
			if xok && yok && wok && hok {
				return map[string]float64{"x": x, "y": y, "width": w, "height": h}, true
			}
		}
	}
	return nil, false
}

// parseNSRectString parses the first four numbers out of an NSRect/NSValue string
// rendering such as "{{0, 64}, {390, 44}}".
func parseNSRectString(s string) (map[string]float64, bool) {
	nums := rectNumRe.FindAllString(s, 4)
	if len(nums) < 4 {
		return nil, false
	}
	x, ok1 := parseFloat(nums[0])
	y, ok2 := parseFloat(nums[1])
	w, ok3 := parseFloat(nums[2])
	h, ok4 := parseFloat(nums[3])
	if ok1 && ok2 && ok3 && ok4 {
		return map[string]float64{"x": x, "y": y, "width": w, "height": h}, true
	}
	return nil, false
}

func parseFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	default:
		return 0, false
	}
}
