package accessibility

import (
	"context"
	"fmt"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/golog"
)

// NOTE on element geometry and coordinate hit-testing on real devices.
//
// Two further AX capabilities were investigated and confirmed unavailable on
// physical devices (tested on iPhone 13, iOS 18.4.1):
//
//   - Per-element frame via deviceElement:valueForAttribute: (AXFrame / AXBounds /
//     AXPosition / AXSize). The attribute-query path itself works (e.g. "Label"
//     resolves), but the geometry attributes return no value even for a live,
//     focused element. On real hardware an element's on-screen rect is only
//     delivered bundled in the audit result (ElementRectValue_v1, surfaced as
//     AXAuditIssue.ElementRect below).
//   - Coordinate hit-testing via deviceFetchElementAtNormalizedDeviceCoordinate:.
//     The call returns an empty reply on devices; the service advertises
//     deviceFetchResolvesElementsOnSimulator, i.e. coordinate resolution is
//     simulator-only.
//
// Hence no standalone "frame" or "element at point" command is exposed; the audit
// already reports each flagged element's rect.
//
// The audit works on iOS 14 through 18. iOS 15+ (AX API version >= 21) drives the
// audit with integer audit-type categories (deviceBeginAuditTypes:); iOS 14 (tested
// on an iPhone SE running 14.3, API v20) uses named string case IDs
// (deviceAllAuditCaseIDs / deviceBeginAuditCaseIDs:). The key subtlety found on
// device: iOS 14 still reports completion via the same
// hostDeviceDidCompleteAuditCategoriesWithAuditIssues: callback (not a "...CaseIDs..."
// variant) in the same AXAuditIssue_v1 format, so only the begin selector and the
// supported-types selector differ by version.

// AXAuditIssue is a single accessibility issue reported by the device's audit.
type AXAuditIssue struct {
	// IssueType is the human-readable audit category (e.g. "testTypeContrast"),
	// or the raw classification id when no mapping is known.
	IssueType string `json:"issueType"`
	// Label is the accessibility label of the offending element, when resolvable.
	Label string `json:"label,omitempty"`
	// ElementRect is the on-screen rect {x,y,width,height} of the element, if reported.
	ElementRect map[string]float64 `json:"elementRect,omitempty"`
	// PlatformElementValue is the base64 PlatformElementValue_v1 of the element,
	// which can be passed to QueryAttributeValue/PerformAction within the session.
	PlatformElementValue string `json:"platformElementValue,omitempty"`
}

// Audit issue classification ids reported in IssueClassificationValue_v1.
const (
	auditTypeContrast              = 12
	auditTypeContrastAlt           = 13
	auditTypeHitRegion             = 100
	auditTypeElementDetection      = 1000
	auditTypeDynamicText           = 3001
	auditTypeDynamicTextAlt        = 3002
	auditTypeTextClipped           = 3003
	auditTypeSufficientDescription = 5000
)

var auditTypeDescriptions = map[int]string{
	auditTypeContrast:              "testTypeContrast",
	auditTypeContrastAlt:           "testTypeContrast",
	auditTypeHitRegion:             "testTypeHitRegion",
	auditTypeElementDetection:      "testTypeElementDetection",
	auditTypeDynamicText:           "testTypeDynamicText",
	auditTypeDynamicTextAlt:        "testTypeDynamicText",
	auditTypeTextClipped:           "testTypeTextClipped",
	auditTypeSufficientDescription: "testTypeSufficientElementDescription",
}

// integerAuditTypesAPIVersion is the AX API version (21 == iOS 15) from which the
// audit is driven by integer audit-type categories (deviceBeginAuditTypes:). Older
// devices (e.g. iOS 14, API v20) use the named string case IDs
// (deviceAllAuditCaseIDs / deviceBeginAuditCaseIDs:). Both report results through
// the same hostDeviceDidCompleteAuditCategoriesWithAuditIssues: callback in the
// same AXAuditIssue_v1 format.
const integerAuditTypesAPIVersion = 21

// auditCompletedSelector is the callback the device invokes (on every iOS version
// tested, 14 through 18) once the audit finishes, carrying the issues.
const auditCompletedSelector = "hostDeviceDidCompleteAuditCategoriesWithAuditIssues:"

// auditTypes fetches the audit categories/case-ids the device supports, in the raw
// form the matching deviceBeginAudit* selector expects as its NSArray argument:
// integer categories on iOS 15+, named string case IDs on older devices.
func (a *ControlInterface) auditTypes(api uint64) ([]interface{}, error) {
	selector := "deviceAllAuditCaseIDs"
	if api >= integerAuditTypesAPIVersion {
		selector = "deviceAllSupportedAuditTypes"
	}
	resp, err := a.channel.MethodCall(selector)
	if err != nil {
		return nil, err
	}
	for _, v := range responseValues(resp) {
		if l, ok := v.([]interface{}); ok {
			return l, nil
		}
	}
	return nil, fmt.Errorf("unexpected %s reply format", selector)
}

// RunAudit runs the accessibility audit for every audit type the device supports
// against the currently focused application and returns the issues found. It
// blocks until the device reports completion or ctx is cancelled. Works on iOS 14+.
func (a *ControlInterface) RunAudit(ctx context.Context) ([]AXAuditIssue, error) {
	api, err := a.deviceAPIVersion()
	if err != nil {
		return nil, err
	}
	types, err := a.auditTypes(api)
	if err != nil {
		return nil, err
	}

	beginSelector := "deviceBeginAuditTypes:"
	if api < integerAuditTypesAPIVersion {
		beginSelector = "deviceBeginAuditCaseIDs:"
	}
	golog.Info("running accessibility audit", "module", logModule, "service", serviceName, "apiVersion", api, "auditTypes", len(types))
	a.channel.RegisterMethodForRemote(auditCompletedSelector)
	if err := a.channel.MethodCallAsync(beginSelector, types); err != nil {
		return nil, fmt.Errorf("failed to start audit: %w", err)
	}

	msg, err := a.channel.ReceiveMethodCallWithTimeout(ctx, auditCompletedSelector)
	if err != nil {
		return nil, fmt.Errorf("audit did not complete: %w", err)
	}

	issues := parseAuditIssues(msg)
	// Enrich with element labels where we have a platform element to query.
	for i := range issues {
		if issues[i].PlatformElementValue == "" {
			continue
		}
		if label, err := a.QueryLabelValue(ctx, issues[i].PlatformElementValue); err == nil && label != "" {
			issues[i].Label = label
		}
	}
	return issues, nil
}

// parseAuditIssues extracts the audit issues from the completion message, looking
// in both the archived auxiliary arguments and the payload.
func parseAuditIssues(msg dtx.Message) []AXAuditIssue {
	var issues []AXAuditIssue
	for _, v := range responseValues(msg) {
		issues = append(issues, extractIssues(deserializeObject(v))...)
	}
	return issues
}

// extractIssues walks a decoded object tree, finds the AXAuditIssue_v1 maps and
// converts them to AXAuditIssue.
func extractIssues(root interface{}) []AXAuditIssue {
	var out []AXAuditIssue
	for _, m := range findIssueMaps(root) {
		if val, ok := m["Value"].(map[string]interface{}); ok {
			m = val
		}
		var issue AXAuditIssue
		// Prefer the device-provided audit test type name; fall back to mapping the
		// numeric classification id.
		if t, ok := m["auditTestTypeValue_v1"].(string); ok && t != "" {
			issue.IssueType = t
		} else if ic, ok := m["IssueClassificationValue_v1"]; ok {
			if id, ok := toFloat(deserializeObject(ic)); ok {
				if lbl, ok := auditTypeDescriptions[int(id)]; ok {
					issue.IssueType = lbl
				} else {
					issue.IssueType = fmt.Sprintf("%d", int(id))
				}
			}
		}
		if r, ok := m["ElementRectValue_v1"]; ok {
			if rect, ok := tryExtractRect(deserializeObject(r)); ok {
				issue.ElementRect = rect
			}
		}
		if be, ok := findPlatformElementBase64(m); ok {
			issue.PlatformElementValue = be
		}
		if issue.IssueType != "" || issue.ElementRect != nil {
			out = append(out, issue)
		}
	}
	return out
}

// findIssueMaps locates the slice(s) of AXAuditIssue_v1-like maps in a decoded tree.
func findIssueMaps(v interface{}) []map[string]interface{} {
	var found []map[string]interface{}
	switch t := v.(type) {
	case []interface{}:
		if len(t) > 0 {
			if m, ok := t[0].(map[string]interface{}); ok &&
				hasAnyKey(m, "auditTestTypeValue_v1", "IssueClassificationValue_v1", "ElementRectValue_v1", "AuditElementValue_v1") {
				for _, it := range t {
					if mm, ok := it.(map[string]interface{}); ok {
						found = append(found, mm)
					}
				}
				return found
			}
		}
		for _, it := range t {
			found = append(found, findIssueMaps(it)...)
		}
	case map[string]interface{}:
		for _, vv := range t {
			found = append(found, findIssueMaps(vv)...)
		}
	}
	return found
}

func hasAnyKey(m map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
