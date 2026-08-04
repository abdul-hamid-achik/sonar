package ecosystem

// TransientModelContent returns useful content from an exact, validated
// transient contract. The content is intentionally not part of ToolProjection:
// callers may feed it to the active provider turn but must pair it with
// SafeReceiptText as the durable replacement.
func TransientModelContent(projection ToolProjection, receipt RawReceipt) (string, bool) {
	projection = projection.Normalize()
	if projection.Digest == nil {
		return "", false
	}
	switch projection.Digest.Kind {
	case DigestMCPHubPage:
		return transientMCPHubResultPage(projection, receipt)
	case DigestHitspecSearch:
		return transientHitspecSearch(projection, receipt)
	case DigestBobContext, DigestBobPath, DigestBobPlaybook:
		return transientBobGuidanceContent(projection, receipt)
	default:
		return "", false
	}
}
