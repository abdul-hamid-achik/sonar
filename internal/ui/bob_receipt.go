package ui

import (
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

// isBobToolName reports whether a (possibly MCP-namespaced) tool name belongs
// to Bob, e.g. "bob__bob_plan" from a direct server or "mcphub__bob__bob_plan"
// through a gateway. Matching is by exact namespace segment or a "bob_" tool
// prefix so unrelated tools that merely contain "bob" stay untouched.
func isBobToolName(name string) bool {
	key := canonicalToolKey(safeToolIdentifier(name))
	if key == "bob" {
		return true
	}
	segments := strings.Split(key, "__")
	if strings.HasPrefix(segments[len(segments)-1], "bob_") {
		return true
	}
	for _, segment := range segments[:len(segments)-1] {
		if segment == "bob" {
			return true
		}
	}
	return false
}

// bobReceiptDigest summarizes a Bob JSON envelope for the tool receipt:
// stable error code, blocking conflicts with their codes, and Bob's
// copy-pasteable next actions. It returns an empty string for non-Bob tools,
// non-envelope output (including older bob binaries' plain output), and
// clean successes, so the receipt only gains lines when there is something
// to act on.
func bobReceiptDigest(name, result string) string {
	if !isBobToolName(name) {
		return ""
	}
	envelope, ok := ecosystem.ParseBobEnvelope(result)
	if !ok {
		return ""
	}
	return envelope.Digest()
}
