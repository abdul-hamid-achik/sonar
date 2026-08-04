package ecosystem

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MCPHub caps result pages at 8 KiB. Enforce the same parser bound so a
// malicious or incompatible gateway cannot smuggle an unbounded transient
// payload into the next provider request.
const maxMCPHubResultPageBytes = 8 * 1024

type mcphubResultPageEnvelope struct {
	Status     string `json:"status"`
	CallID     string `json:"callId"`
	MediaType  string `json:"mediaType"`
	Data       string `json:"data"`
	Cursor     *int64 `json:"cursor"`
	NextCursor *int64 `json:"nextCursor"`
	Done       *bool  `json:"done"`
	TotalBytes *int64 `json:"totalBytes"`
}

func projectMCPHubReceipt(projection ToolProjection, receipt RawReceipt) (ToolProjection, bool) {
	document, ok := receiptDocument(receipt)
	if !ok {
		return projection, false
	}

	if projection.Operation == "mcphub_get_result" {
		if !exactMCPHubManagementRoute(projection) {
			return projection, false
		}
		return projectMCPHubResultPage(projection, document), true
	}
	if projected, stored := projectMCPHubStoredReceipt(projection, document); stored {
		return projected, true
	}
	if projected, detached := projectMCPHubDetachedReceipt(projection, document); detached {
		return projected, true
	}
	if !isMCPHubManagementOperation(projection.Operation) || !exactMCPHubManagementRoute(projection) {
		return projection, false
	}
	if projection.Operation == "mcphub_resolve_tool" {
		return projectMCPHubResolve(projection, document), true
	}
	if jsonObjectHasValue(document, "error") {
		projection.Domain = DomainFailed
		projection.Evidence = EvidenceNone
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubError}
		return projection, true
	}

	switch projection.Operation {
	case "mcphub_list_servers":
		return projectMCPHubServers(projection, document)
	case "mcphub_search_tools":
		return projectMCPHubSearch(projection, document)
	case "mcphub_describe_tool":
		return projectMCPHubDescribe(projection, document)
	case "mcphub_stats":
		return projectMCPHubStats(projection, document)
	default:
		return projection, false
	}
}

func exactMCPHubManagementRoute(projection ToolProjection) bool {
	return projection.Specialist == "mcphub" && projection.Route.Gateway == "" &&
		projection.Route.Server == "mcphub" && projection.Route.Tool == projection.Operation
}

func isMCPHubManagementOperation(operation string) bool {
	switch operation {
	case "mcphub_list_servers", "mcphub_search_tools", "mcphub_describe_tool",
		"mcphub_resolve_tool", "mcphub_stats":
		return true
	default:
		return false
	}
}

func projectMCPHubStoredReceipt(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	// A stored envelope has parser authority only on the exact canonical MCPHub
	// gateway route. Agent-side trusted aliases are canonicalized to this route
	// before entering ProjectReceipt, then restored for display afterwards.
	if projection.Route.Gateway != "mcphub" {
		return projection, false
	}
	var envelope struct {
		Status        string `json:"status"`
		CallID        string `json:"callId"`
		Server        string `json:"server"`
		Tool          string `json:"tool"`
		Namespaced    string `json:"namespaced"`
		OriginalBytes int64  `json:"originalBytes"`
		BudgetBytes   int64  `json:"budgetBytes"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.Status != "stored" {
		return projection, false
	}
	callID := canonicalIdentifier(envelope.CallID)
	server := canonicalIdentifier(envelope.Server)
	tool := canonicalIdentifier(envelope.Tool)
	namespaced := canonicalIdentifier(envelope.Namespaced)
	if callID == "" || callID != envelope.CallID || envelope.OriginalBytes <= 0 || envelope.BudgetBytes <= 0 ||
		envelope.OriginalBytes <= envelope.BudgetBytes ||
		(envelope.Server != "" && server != envelope.Server) || (envelope.Tool != "" && tool != envelope.Tool) ||
		(envelope.Namespaced != "" && namespaced != envelope.Namespaced) ||
		(server != "" && projection.Route.Server != "" && server != projection.Route.Server) ||
		(tool != "" && projection.Route.Tool != "" && tool != projection.Route.Tool) ||
		(namespaced != "" && server != "" && tool != "" && namespaced != server+"__"+tool) ||
		(namespaced != "" && projection.Route.Server != "" && projection.Route.Tool != "" &&
			namespaced != projection.Route.Server+"__"+projection.Route.Tool) {
		projection.Domain = DomainUnknown
		projection.Evidence = EvidenceNone
		return projection, true
	}
	projection.Route.CallID = callID
	if projection.Route.Server == "" {
		projection.Route.Server = server
	}
	if projection.Route.Tool == "" {
		projection.Route.Tool = tool
	}
	projection.Domain = DomainAttention
	projection.Evidence = EvidenceNone
	projection.Digest = &ReceiptDigest{
		Kind: DigestMCPHubStored, OriginalBytes: envelope.OriginalBytes, BudgetBytes: envelope.BudgetBytes,
	}
	return projection.Normalize(), true
}

// projectMCPHubDetachedReceipt recognizes the detached-call lifecycle:
// acceptance on the gateway route and the poll states on the management
// route. Detached work is durably pending — never success — until its
// finalized downstream result is collected and parsed under the downstream
// tool's own contract.
func projectMCPHubDetachedReceipt(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	var envelope struct {
		Status     string          `json:"status"`
		CallID     string          `json:"callId"`
		Namespaced string          `json:"namespaced"`
		TimeoutMS  *int64          `json:"timeoutMs"`
		ElapsedMS  *int64          `json:"elapsedMs"`
		Reason     string          `json:"reason"`
		Error      json.RawMessage `json:"error"`
	}
	if json.Unmarshal(document, &envelope) != nil {
		return projection, false
	}
	callID := canonicalIdentifier(envelope.CallID)
	if callID == "" || callID != envelope.CallID {
		return projection, false
	}
	settle := func(domain DomainState, state string) (ToolProjection, bool) {
		projection.Route.CallID = callID
		projection.Domain = domain
		projection.Evidence = EvidenceNone
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubDetached, State: state}
		return projection.Normalize(), true
	}
	switch envelope.Status {
	case "accepted":
		// Acceptance is emitted by mcphub_call_tool with detach=true and is
		// only trusted on the exact gateway route.
		if projection.Route.Gateway != "mcphub" || envelope.TimeoutMS == nil || *envelope.TimeoutMS <= 0 {
			return projection, false
		}
		return settle(DomainPending, "accepted")
	case "pending":
		if projection.Operation != "mcphub_poll_result" || !exactMCPHubManagementRoute(projection) ||
			envelope.ElapsedMS == nil || *envelope.ElapsedMS < 0 {
			return projection, false
		}
		return settle(DomainPending, "pending")
	case "failed":
		if projection.Operation != "mcphub_poll_result" || !exactMCPHubManagementRoute(projection) ||
			!rawJSONPresent(envelope.Error) {
			return projection, false
		}
		return settle(DomainFailed, "failed")
	case "unknown":
		// The gateway no longer knows this call: the outcome is genuinely
		// unresolved and needs attention, not a silent retry.
		if projection.Operation != "mcphub_poll_result" || !exactMCPHubManagementRoute(projection) ||
			strings.TrimSpace(envelope.Reason) == "" {
			return projection, false
		}
		return settle(DomainAttention, "unknown")
	default:
		return projection, false
	}
}

func projectMCPHubResultPage(projection ToolProjection, document json.RawMessage) ToolProjection {
	var envelope mcphubResultPageEnvelope
	if json.Unmarshal(document, &envelope) != nil {
		projection.Domain = DomainUnknown
		projection.Evidence = EvidenceNone
		return projection
	}

	// Retrieval is bound to the exact opaque identifier the model supplied. A
	// mismatched or absent response ID never replaces it and can never become a
	// follow-up fetch target.
	requestedID := canonicalIdentifier(projection.Route.CallID)
	receivedID := canonicalIdentifier(envelope.CallID)
	if requestedID == "" || receivedID == "" || receivedID != envelope.CallID || requestedID != receivedID {
		projection.Domain = DomainFailed
		projection.Evidence = EvidenceNone
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubError}
		return projection.Normalize()
	}
	projection.Route.CallID = requestedID

	switch envelope.Status {
	case "ok":
		page, ok := decodeMCPHubResultPage(envelope)
		if !ok {
			projection.Domain = DomainUnknown
			projection.Evidence = EvidenceNone
			return projection.Normalize()
		}
		projection.Digest = &ReceiptDigest{
			Kind: DigestMCPHubPage, Cursor: *envelope.Cursor, NextCursor: *envelope.NextCursor,
			TotalBytes: *envelope.TotalBytes, PageBytes: int64(len(page)), Done: *envelope.Done,
		}
		if *envelope.Done {
			projection.Domain = DomainSucceeded
		} else {
			// The page call completed, but the stored result still has a precise
			// continuation cursor. Attention is terminal and avoids a fake spinner.
			projection.Domain = DomainAttention
		}
	case "unavailable":
		projection.Domain = DomainBlocked
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubUnavailable}
	case "cursor_out_of_range":
		projection.Domain = DomainBlocked
		cursor := int64(0)
		if envelope.Cursor != nil {
			cursor = *envelope.Cursor
		}
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubCursorOutOfRange, Cursor: cursor}
	default:
		projection.Domain = DomainUnknown
	}
	projection.Evidence = EvidenceNone
	return projection.Normalize()
}

func decodeMCPHubResultPage(envelope mcphubResultPageEnvelope) ([]byte, bool) {
	page, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil || envelope.MediaType != "application/json" || envelope.Cursor == nil ||
		envelope.NextCursor == nil || envelope.Done == nil || envelope.TotalBytes == nil ||
		*envelope.Cursor < 0 || *envelope.NextCursor < *envelope.Cursor ||
		*envelope.TotalBytes < *envelope.NextCursor ||
		int64(len(page)) != *envelope.NextCursor-*envelope.Cursor ||
		len(page) > maxMCPHubResultPageBytes ||
		*envelope.Done != (*envelope.NextCursor == *envelope.TotalBytes) ||
		(!*envelope.Done && *envelope.NextCursor == *envelope.Cursor) {
		return nil, false
	}
	return page, true
}

func transientMCPHubResultPage(projection ToolProjection, receipt RawReceipt) (string, bool) {
	if projection.Operation != "mcphub_get_result" {
		return "", false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", false
	}
	var envelope mcphubResultPageEnvelope
	if json.Unmarshal(document, &envelope) != nil || envelope.Status != "ok" {
		return "", false
	}
	requestedID := canonicalIdentifier(projection.Route.CallID)
	if requestedID == "" || canonicalIdentifier(envelope.CallID) != requestedID {
		return "", false
	}
	page, ok := decodeMCPHubResultPage(envelope)
	if !ok || envelope.Cursor == nil || envelope.NextCursor == nil || envelope.Done == nil || envelope.TotalBytes == nil {
		return "", false
	}
	digest := projection.Digest
	if digest.Cursor != *envelope.Cursor || digest.NextCursor != *envelope.NextCursor ||
		digest.TotalBytes != *envelope.TotalBytes || digest.Done != *envelope.Done ||
		digest.PageBytes != int64(len(page)) {
		return "", false
	}
	// A byte page may split a UTF-8 code point. Keep the provider request valid
	// while preserving the useful JSON fragment; the durable receipt contains
	// no page data at all.
	payload := strings.ToValidUTF8(string(page), "�")
	return fmt.Sprintf(
		"MCPHub result page (transient; not saved)\ncall_id: %s\nbytes: %d-%d of %d\ndone: %t\npayload_fragment:\n%s",
		requestedID, *envelope.Cursor, *envelope.NextCursor, *envelope.TotalBytes, *envelope.Done, payload,
	), true
}

func projectMCPHubServers(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	var envelope struct {
		Servers []struct {
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		} `json:"servers"`
		TotalTools *int64 `json:"total_tools"`
		Expose     string `json:"expose"`
	}
	if json.Unmarshal(document, &envelope) != nil || !jsonObjectHasValue(document, "servers") || envelope.TotalTools == nil {
		return projection, false
	}
	items := make([]string, 0, len(envelope.Servers))
	connected := int64(0)
	for _, server := range envelope.Servers {
		items = append(items, server.Name)
		if server.Connected {
			connected++
		}
	}
	projection.Domain = DomainSucceeded
	projection.Evidence = EvidenceNone
	projection.Digest = &ReceiptDigest{
		Kind: DigestMCPHubServers, Count: int64(len(envelope.Servers)), Connected: connected,
		TotalTools: *envelope.TotalTools, Items: items, Expose: envelope.Expose,
	}
	return projection.Normalize(), true
}

func projectMCPHubSearch(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	var envelope struct {
		Count   *int64 `json:"count"`
		Matches []struct {
			Namespaced string `json:"namespaced"`
		} `json:"matches"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.Count == nil || !jsonObjectHasValue(document, "matches") {
		return projection, false
	}
	items := make([]string, 0, len(envelope.Matches))
	for _, match := range envelope.Matches {
		items = append(items, match.Namespaced)
	}
	projection.Domain = DomainSucceeded
	projection.Evidence = EvidenceNone
	projection.Digest = &ReceiptDigest{Kind: DigestMCPHubSearch, Count: *envelope.Count, Items: items}
	return projection.Normalize(), true
}

func projectMCPHubDescribe(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	var envelope struct {
		Server     string `json:"server"`
		Tool       string `json:"tool"`
		Namespaced string `json:"namespaced"`
		Input      struct {
			Required []string `json:"required"`
		} `json:"input_schema"`
	}
	if json.Unmarshal(document, &envelope) != nil || !jsonObjectHasValue(document, "input_schema") {
		return projection, false
	}
	target := envelope.Namespaced
	if target == "" && envelope.Server != "" && envelope.Tool != "" {
		target = envelope.Server + "__" + envelope.Tool
	}
	if canonicalIdentifier(target) == "" {
		return projection, false
	}
	projection.Domain = DomainSucceeded
	projection.Evidence = EvidenceNone
	projection.Digest = &ReceiptDigest{Kind: DigestMCPHubDescribe, Target: target, Required: envelope.Input.Required}
	return projection.Normalize(), true
}

func projectMCPHubResolve(projection ToolProjection, document json.RawMessage) ToolProjection {
	// Resolver receipts are advisory. Start from the fail-closed projection so
	// any contract mismatch remains transport success without domain success.
	projection.Domain = DomainUnknown
	projection.Evidence = EvidenceNone
	projection.Digest = nil
	if !validMCPHubResolverDocument(document) {
		return projection.Normalize()
	}
	if jsonObjectHasValue(document, "error") {
		projection.Domain = DomainFailed
		projection.Digest = &ReceiptDigest{Kind: DigestMCPHubError}
		return projection.Normalize()
	}

	var envelope struct {
		ContractVersion *int            `json:"contract_version"`
		CatalogRevision *string         `json:"catalog_revision"`
		Status          *string         `json:"status"`
		Recommendation  json.RawMessage `json:"recommendation"`
		Ambiguous       *bool           `json:"ambiguous"`
		Alternatives    json.RawMessage `json:"alternatives"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.ContractVersion == nil ||
		*envelope.ContractVersion != mcphubResolverContractVersion || envelope.CatalogRevision == nil ||
		!validMCPHubCatalogRevision(*envelope.CatalogRevision) || envelope.Status == nil || envelope.Ambiguous == nil {
		return projection.Normalize()
	}
	alternatives, ok := parseMCPHubResolverAlternatives(envelope.Alternatives)
	if !ok {
		return projection.Normalize()
	}
	recommendation := bytes.TrimSpace(envelope.Recommendation)
	hasRecommendation := jsonKind(recommendation, '{')
	noRecommendation := bytes.Equal(recommendation, []byte("null"))
	if !hasRecommendation && !noRecommendation {
		return projection.Normalize()
	}

	digest := ReceiptDigest{Kind: DigestMCPHubResolve, Ambiguous: *envelope.Ambiguous}
	var domain DomainState
	switch *envelope.Status {
	case "no_match":
		if hasRecommendation || *envelope.Ambiguous || len(alternatives) != 0 {
			return projection.Normalize()
		}
		domain = DomainAttention
	case "confident", "ambiguous":
		wantAmbiguous := *envelope.Status == "ambiguous"
		if !hasRecommendation || *envelope.Ambiguous != wantAmbiguous {
			return projection.Normalize()
		}
		target, required, ok := parseMCPHubResolverRecommendation(recommendation)
		if !ok {
			return projection.Normalize()
		}
		digest.Target = target
		digest.Required = required
		if wantAmbiguous {
			domain = DomainAttention
		} else {
			domain = DomainSucceeded
		}
	default:
		return projection.Normalize()
	}
	seen := map[string]struct{}{digest.Target: {}}
	for _, alternative := range alternatives {
		if _, duplicate := seen[alternative]; duplicate {
			return projection.Normalize()
		}
		seen[alternative] = struct{}{}
		digest.Items = append(digest.Items, alternative)
	}
	projection.Domain = domain
	projection.Evidence = EvidenceNone
	projection.Digest = &digest
	return projection.Normalize()
}

func validMCPHubResolverDocument(document json.RawMessage) bool {
	document = bytes.TrimSpace(document)
	return len(document) > 0 && len(document) <= maxMCPHubResolverDocumentBytes &&
		document[0] == '{' && json.Valid(document)
}

func validMCPHubCatalogRevision(value string) bool {
	if value == "" || len(value) > maxMCPHubCatalogRevisionBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_-.:/", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func parseMCPHubResolverRecommendation(raw json.RawMessage) (string, []string, bool) {
	var recommendation struct {
		Server     string          `json:"server"`
		Tool       string          `json:"tool"`
		Namespaced string          `json:"namespaced"`
		Required   json.RawMessage `json:"required_fields"`
	}
	if json.Unmarshal(raw, &recommendation) != nil ||
		!validMCPHubResolverIdentifier(recommendation.Server, false) ||
		!validMCPHubResolverIdentifier(recommendation.Tool, true) ||
		!validMCPHubResolverNamespacedIdentifier(recommendation.Namespaced) ||
		recommendation.Namespaced != recommendation.Server+"__"+recommendation.Tool {
		return "", nil, false
	}
	required, ok := parseMCPHubResolverRequiredFields(recommendation.Required)
	if !ok {
		return "", nil, false
	}
	return recommendation.Namespaced, required, true
}

func parseMCPHubResolverAlternatives(raw json.RawMessage) ([]string, bool) {
	var alternatives []struct {
		Namespaced string `json:"namespaced"`
	}
	if !jsonKind(raw, '[') || json.Unmarshal(raw, &alternatives) != nil || alternatives == nil ||
		len(alternatives) > maxMCPHubResolverAlternatives {
		return nil, false
	}
	seen := make(map[string]struct{}, len(alternatives))
	result := make([]string, 0, len(alternatives))
	for _, alternative := range alternatives {
		if !validMCPHubResolverNamespacedIdentifier(alternative.Namespaced) {
			return nil, false
		}
		if _, duplicate := seen[alternative.Namespaced]; duplicate {
			return nil, false
		}
		seen[alternative.Namespaced] = struct{}{}
		result = append(result, alternative.Namespaced)
	}
	return result, true
}

func parseMCPHubResolverRequiredFields(raw json.RawMessage) ([]string, bool) {
	var required []string
	if !jsonKind(raw, '[') || json.Unmarshal(raw, &required) != nil || required == nil ||
		len(required) > maxMCPHubResolverRequiredFields {
		return nil, false
	}
	seen := make(map[string]struct{}, len(required))
	total := 0
	for _, field := range required {
		if field == "" || len(field) > maxMCPHubResolverRequiredFieldBytes || !utf8.ValidString(field) || strings.TrimSpace(field) != field {
			return nil, false
		}
		for _, character := range field {
			if unicode.IsLetter(character) || unicode.IsNumber(character) || strings.ContainsRune("_-.:/$[]", character) {
				continue
			}
			return nil, false
		}
		if _, duplicate := seen[field]; duplicate {
			return nil, false
		}
		seen[field] = struct{}{}
		total += len(field)
		if total > maxMCPHubResolverRequiredFieldNameBytes {
			return nil, false
		}
	}
	return append([]string(nil), required...), true
}

func validMCPHubResolverNamespacedIdentifier(value string) bool {
	server, tool, found := strings.Cut(value, "__")
	return found && validMCPHubResolverIdentifier(server, false) && validMCPHubResolverIdentifier(tool, true)
}

func validMCPHubResolverIdentifier(value string, allowDoubleUnderscore bool) bool {
	if value == "" || len(value) > maxMCPHubResolverIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	if !allowDoubleUnderscore && strings.Contains(value, "__") {
		return false
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsNumber(character) || strings.ContainsRune("_-.:/", character) {
			continue
		}
		return false
	}
	return true
}

func projectMCPHubStats(projection ToolProjection, document json.RawMessage) (ToolProjection, bool) {
	var envelope struct {
		Totals *struct {
			Calls     int64 `json:"calls"`
			Errors    int64 `json:"errors"`
			EstTokens int64 `json:"est_tokens"`
		} `json:"totals"`
		Servers []struct {
			Server string `json:"server"`
		} `json:"servers"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.Totals == nil || !jsonObjectHasValue(document, "servers") {
		return projection, false
	}
	items := make([]string, 0, len(envelope.Servers))
	for _, server := range envelope.Servers {
		items = append(items, server.Server)
	}
	projection.Domain = DomainSucceeded
	projection.Evidence = EvidenceNone
	projection.Digest = &ReceiptDigest{
		Kind: DigestMCPHubStats, Count: int64(len(envelope.Servers)), Calls: envelope.Totals.Calls,
		Errors: envelope.Totals.Errors, Estimated: envelope.Totals.EstTokens, Items: items,
	}
	return projection.Normalize(), true
}

const (
	mcphubResolverContractVersion           = 1
	maxMCPHubCatalogRevisionBytes           = 128
	maxMCPHubResolverDocumentBytes          = 64 * 1024
	maxMCPHubResolverIdentifierBytes        = 256
	maxMCPHubResolverRequiredFields         = 48
	maxMCPHubResolverRequiredFieldBytes     = 128
	maxMCPHubResolverRequiredFieldNameBytes = 2048
	maxMCPHubResolverAlternatives           = 5
)
