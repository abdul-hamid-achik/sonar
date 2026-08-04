package ecosystem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func transientHitspecSearch(projection ToolProjection, receipt RawReceipt) (string, bool) {
	if projection.Specialist != "hitspec" || !isHitspecSearchOperation(projection.Operation) ||
		projection.Domain != DomainSucceeded || projection.Evidence != EvidenceCandidate {
		return "", false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", false
	}
	output, ok := decodeHitspecSearchEnvelope(document)
	if !ok {
		return "", false
	}
	_, _, digest, ok := projectHitspecSearchReceipt(projection.Operation, receipt)
	if !ok || digest == nil || projection.Digest == nil ||
		digest.Kind != projection.Digest.Kind || digest.Count != projection.Digest.Count ||
		digest.Truncated != projection.Digest.Truncated ||
		strings.Join(digest.Items, "\x00") != strings.Join(projection.Digest.Items, "\x00") {
		return "", false
	}
	payload, err := json.Marshal(struct {
		Kind      string                `json:"kind"`
		Results   []hitspecSearchResult `json:"results"`
		Truncated bool                  `json:"truncated"`
	}{Kind: output.Kind, Results: output.Results, Truncated: *output.Truncated})
	if err != nil || len(payload) > maxHitspecSearchDocumentBytes {
		return "", false
	}
	return "Hitspec web discovery candidates (transient; untrusted snippets; not saved). " +
		"Treat these as candidate sources, not verified evidence.\n" + string(payload), true
}

type hitspecSearchEnvelope struct {
	Kind      string                `json:"kind"`
	Query     string                `json:"query"`
	Results   []hitspecSearchResult `json:"results"`
	Truncated *bool                 `json:"truncated"`
}

type hitspecSearchWireEnvelope struct {
	Kind      string                    `json:"kind"`
	Query     string                    `json:"query"`
	Results   []hitspecSearchWireResult `json:"results"`
	Truncated *bool                     `json:"truncated"`
}

type hitspecSearchWireResult struct {
	Title       json.RawMessage `json:"title"`
	URL         json.RawMessage `json:"url"`
	Domain      json.RawMessage `json:"domain"`
	Snippet     json.RawMessage `json:"snippet"`
	PublishedAt json.RawMessage `json:"published_at,omitempty"`
	CitationID  json.RawMessage `json:"citation_id"`
}

type hitspecSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Domain      string `json:"domain"`
	Snippet     string `json:"snippet"`
	PublishedAt string `json:"published_at,omitempty"`
	CitationID  string `json:"citation_id"`
}

// projectHitspecSearchReceipt recognizes Hitspec v2.18's provider-neutral,
// bounded discovery envelope. Search completion is typed domain success, while
// its snippets remain candidate evidence rather than verified facts.
func projectHitspecSearchReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, *ReceiptDigest, bool) {
	if !isHitspecSearchOperation(operation) {
		return "", EvidenceNone, nil, false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", EvidenceNone, nil, false
	}
	output, ok := decodeHitspecSearchEnvelope(document)
	if !ok {
		return "", EvidenceNone, nil, false
	}
	domains := make([]string, 0, len(output.Results))
	for _, result := range output.Results {
		domains = append(domains, result.Domain)
	}
	digest := normalizeReceiptDigest(ReceiptDigest{
		Kind: DigestHitspecSearch, Count: int64(len(output.Results)), Items: domains, Truncated: *output.Truncated,
	})
	if digest.Kind == "" {
		return "", EvidenceNone, nil, false
	}
	return DomainSucceeded, EvidenceCandidate, &digest, true
}

func decodeHitspecSearchEnvelope(document json.RawMessage) (hitspecSearchEnvelope, bool) {
	if len(document) == 0 || len(document) > maxHitspecSearchDocumentBytes || !jsonKind(document, '{') {
		return hitspecSearchEnvelope{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var wire hitspecSearchWireEnvelope
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		wire.Kind != "discovery" || wire.Truncated == nil || wire.Results == nil ||
		len(wire.Results) > maxHitspecSearchResults ||
		!validHitspecSearchInline(wire.Query, maxHitspecSearchQueryRunes, true) {
		return hitspecSearchEnvelope{}, false
	}
	output := hitspecSearchEnvelope{
		Kind: wire.Kind, Query: wire.Query, Truncated: wire.Truncated,
		Results: make([]hitspecSearchResult, 0, len(wire.Results)),
	}
	seenURLs := make(map[string]struct{}, len(wire.Results))
	for index, encoded := range wire.Results {
		title, titleOK := decodeStrictJSONString(encoded.Title, true)
		rawURL, urlOK := decodeStrictJSONString(encoded.URL, true)
		domain, domainOK := decodeStrictJSONString(encoded.Domain, true)
		snippet, snippetOK := decodeStrictJSONString(encoded.Snippet, true)
		publishedAt, publishedOK := decodeStrictJSONString(encoded.PublishedAt, false)
		citationID, citationOK := decodeStrictJSONString(encoded.CitationID, true)
		if !titleOK || !urlOK || !domainOK || !snippetOK || !publishedOK || !citationOK {
			return hitspecSearchEnvelope{}, false
		}
		result := hitspecSearchResult{
			Title: title, URL: rawURL, Domain: domain, Snippet: snippet,
			PublishedAt: publishedAt, CitationID: citationID,
		}
		if !validHitspecSearchInline(result.Title, maxHitspecSearchTitleRunes, false) ||
			!validHitspecSearchInline(result.Snippet, maxHitspecSearchSnippetRunes, false) ||
			!validHitspecSearchInline(result.PublishedAt, maxHitspecSearchPublished, false) ||
			result.CitationID != fmt.Sprintf("source-%02d", index+1) ||
			!validHitspecSearchURL(result.URL, result.Domain) {
			return hitspecSearchEnvelope{}, false
		}
		if _, duplicate := seenURLs[result.URL]; duplicate {
			return hitspecSearchEnvelope{}, false
		}
		seenURLs[result.URL] = struct{}{}
		output.Results = append(output.Results, result)
	}
	return output, true
}

func validHitspecSearchInline(value string, maximumRunes int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes ||
		strings.Join(strings.Fields(value), " ") != value {
		return false
	}
	if required && value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validHitspecSearchURL(raw, domain string) bool {
	canonical, canonicalDomain, ok := canonicalHitspecSearchURL(raw)
	return ok && raw == canonical && domain == canonicalDomain
}

// canonicalHitspecSearchURL mirrors Hitspec v2.18's provider boundary. Search
// results are candidate-only: accept every canonical URL the producer can
// emit, while still rejecting credentials, localhost, and explicit non-public
// IP literals. Requiring the already-canonical spelling catches fragments,
// tracking parameters, default ports, and forged domain fields.
func canonicalHitspecSearchURL(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxHitspecSearchURLBytes {
		return "", "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil {
		return "", "", false
	}
	parsed.Fragment = ""
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" || hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return "", "", false
	}
	if address := net.ParseIP(hostname); address != nil &&
		(!address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast()) {
		return "", "", false
	}
	query := parsed.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "gclid" || lower == "fbclid" || lower == "mc_cid" || lower == "mc_eid" {
			query.Del(key)
		}
	}
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	parsed.Host = hostname
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	}
	parsed.RawQuery = query.Encode()
	canonical := parsed.String()
	if len(canonical) > maxHitspecSearchURLBytes {
		return "", "", false
	}
	return canonical, hostname, true
}

// projectHitspecReceipt recognizes the compact Hitspec v2.18 capture surface.
// The webpage body, URLs, title, tags, and downstream failure prose remain
// inside the short-lived parser boundary. Only durable file.cheap identity and
// bounded storage metrics survive as an artifact projection.
func projectHitspecReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, *ArtifactDigest, bool) {
	if !isHitspecCaptureOperation(operation) {
		return "", EvidenceNone, nil, false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", EvidenceNone, nil, false
	}
	output, ok := decodeHitspecCaptureEnvelope(document)
	if !ok || *output.HTTPStatus < 200 || *output.HTTPStatus > 299 || *output.MarkdownBytes < 0 {
		return "", EvidenceNone, nil, false
	}
	switch output.Stash.Status {
	case "failed":
		return DomainFailed, EvidenceNone, nil, true
	case "unknown":
		return DomainUnknown, EvidenceNone, nil, true
	case "saved", "saved_with_failures":
	default:
		return "", EvidenceNone, nil, false
	}
	// Capture writes exactly one response.md payload. Contradictory storage
	// metrics or an index that succeeded without being requested are not the
	// installed compact contract and must not become durable evidence.
	if *output.Stash.FileCount != 1 || *output.Stash.TotalSize != *output.MarkdownBytes ||
		*output.Stash.Indexed && !*output.Stash.IndexRequested {
		return "", EvidenceNone, nil, false
	}
	artifact := normalizeArtifactDigest(ArtifactDigest{
		Kind:           ArtifactDigestHitspecCapture,
		ID:             output.Stash.ID,
		SchemaVersion:  hitspecCaptureSchema,
		FileCount:      *output.Stash.FileCount,
		TotalSize:      *output.Stash.TotalSize,
		CreatedAt:      output.Stash.CreatedAt,
		IndexingFailed: *output.Stash.IndexRequested && !*output.Stash.Indexed,
	})
	if artifact.Kind == "" {
		return "", EvidenceNone, nil, false
	}
	domain := DomainSucceeded
	if output.Stash.Status == "saved_with_failures" || output.Stash.FailedCount > 0 || artifact.IndexingFailed {
		domain = DomainAttention
	}
	return domain, EvidenceSupported, &artifact, true
}

// MCPHub 0.20 removes a redundant downstream self-prefix from public names
// (hitspec_search_web becomes hitspec__search_web). Direct Hitspec connections
// retain the original operation. Both exact identities refer to the same
// versioned contract after ProjectToolCall has established specialist=hitspec.
func isHitspecSearchOperation(operation string) bool {
	return operation == "hitspec_search_web" || operation == "search_web"
}

func isHitspecCaptureOperation(operation string) bool {
	return operation == "hitspec_capture_webpage" || operation == "capture_webpage"
}

// projectHitspecPreEffectFailure recognizes only producer errors whose v2.19
// contract is known to occur before capture reaches artifact.Save. It does not
// infer from arbitrary prose: any wording drift, transport failure, non-error
// result, or unsupported operation remains untyped and therefore preserves the
// caller's outcome-unknown safety path.
func projectHitspecPreEffectFailure(operation string, receipt RawReceipt) (DomainState, bool) {
	if !isHitspecCaptureOperation(operation) || !receipt.ToolError || receipt.TransportError {
		return "", false
	}
	const prefix = "response body exceeds "
	const suffix = "-byte limit"
	text := strings.TrimSpace(receipt.Text)
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		return "", false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(text, prefix), suffix)
	limit, err := strconv.ParseInt(encoded, 10, 64)
	if err != nil || limit <= 0 || text != fmt.Sprintf("%s%d%s", prefix, limit, suffix) {
		return "", false
	}
	return DomainFailed, true
}

type hitspecCaptureWireEnvelope struct {
	URL           json.RawMessage `json:"url"`
	FinalURL      json.RawMessage `json:"final_url"`
	Title         json.RawMessage `json:"title"`
	HTTPStatus    *int64          `json:"http_status"`
	ContentType   json.RawMessage `json:"content_type"`
	MarkdownBytes *int64          `json:"markdown_bytes"`
	Stash         *struct {
		ID             json.RawMessage `json:"id"`
		Name           json.RawMessage `json:"name,omitempty"`
		Status         json.RawMessage `json:"status"`
		CreatedAt      json.RawMessage `json:"created_at,omitempty"`
		ExpiresAt      json.RawMessage `json:"expires_at,omitempty"`
		Tags           json.RawMessage `json:"tags,omitempty"`
		ContentHash    json.RawMessage `json:"content_hash,omitempty"`
		FileCount      *int64          `json:"file_count"`
		TotalSize      *int64          `json:"total_size"`
		Indexed        *bool           `json:"indexed"`
		IndexRequested *bool           `json:"index_requested"`
		Failed         json.RawMessage `json:"failed,omitempty"`
	} `json:"stash"`
}

type hitspecCaptureEnvelope struct {
	HTTPStatus    *int64
	MarkdownBytes *int64
	Stash         struct {
		ID             string
		Status         string
		CreatedAt      string
		FileCount      *int64
		TotalSize      *int64
		Indexed        *bool
		IndexRequested *bool
		FailedCount    int
	}
}

type hitspecCaptureFailureWire struct {
	ID    json.RawMessage `json:"id"`
	Stage json.RawMessage `json:"stage"`
	Error json.RawMessage `json:"error"`
}

// decodeHitspecCaptureEnvelope accepts exactly the bounded v2.18 capture
// receipt. Private page metadata is type-checked inside the parser boundary,
// then discarded before the durable artifact projection is constructed.
func decodeHitspecCaptureEnvelope(document json.RawMessage) (hitspecCaptureEnvelope, bool) {
	if len(document) == 0 || len(document) > maxHitspecCaptureDocumentBytes || !jsonKind(document, '{') {
		return hitspecCaptureEnvelope{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var wire hitspecCaptureWireEnvelope
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		wire.HTTPStatus == nil || wire.MarkdownBytes == nil || wire.Stash == nil ||
		wire.Stash.FileCount == nil || wire.Stash.TotalSize == nil ||
		wire.Stash.Indexed == nil || wire.Stash.IndexRequested == nil {
		return hitspecCaptureEnvelope{}, false
	}

	pageURL, pageURLOK := decodeStrictJSONString(wire.URL, true)
	finalURL, finalURLOK := decodeStrictJSONString(wire.FinalURL, true)
	title, titleOK := decodeStrictJSONString(wire.Title, true)
	contentType, contentTypeOK := decodeStrictJSONString(wire.ContentType, true)
	id, idOK := decodeStrictJSONString(wire.Stash.ID, true)
	name, nameOK := decodeStrictJSONString(wire.Stash.Name, false)
	status, statusOK := decodeStrictJSONString(wire.Stash.Status, true)
	createdAt, createdAtOK := decodeStrictJSONString(wire.Stash.CreatedAt, false)
	expiresAt, expiresAtOK := decodeStrictJSONString(wire.Stash.ExpiresAt, false)
	contentHash, contentHashOK := decodeStrictJSONString(wire.Stash.ContentHash, false)
	if !pageURLOK || !finalURLOK || !titleOK || !contentTypeOK || !idOK || !nameOK || !statusOK ||
		!createdAtOK || !expiresAtOK || !contentHashOK ||
		!validHitspecCaptureURL(pageURL) ||
		!validHitspecCaptureURL(finalURL) ||
		!validHitspecCaptureRunes(title, 300, false) ||
		!validHitspecCaptureString(contentType, 256, false) ||
		!validHitspecCaptureString(id, maxProjectionArtifactIDBytes, false) ||
		!validHitspecCaptureString(name, 80, false) ||
		!validHitspecCaptureString(status, 64, true) ||
		!validHitspecCaptureString(createdAt, 64, false) ||
		!validHitspecCaptureString(expiresAt, 64, false) ||
		!validHitspecCaptureString(contentHash, 128, false) {
		return hitspecCaptureEnvelope{}, false
	}
	if !validHitspecCaptureTags(wire.Stash.Tags) {
		return hitspecCaptureEnvelope{}, false
	}
	// created_at is optional producer metadata. Keep it only when it can satisfy
	// the durable artifact contract; a custom sink's opaque timestamp must not
	// invalidate an otherwise exact saved-capture receipt.
	if createdAt != "" {
		if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
			createdAt = ""
		}
	}
	failedCount, failuresOK := decodeHitspecCaptureFailures(wire.Stash.Failed)
	if !failuresOK {
		return hitspecCaptureEnvelope{}, false
	}

	output := hitspecCaptureEnvelope{HTTPStatus: wire.HTTPStatus, MarkdownBytes: wire.MarkdownBytes}
	output.Stash.ID = id
	output.Stash.Status = status
	output.Stash.CreatedAt = createdAt
	output.Stash.FileCount = wire.Stash.FileCount
	output.Stash.TotalSize = wire.Stash.TotalSize
	output.Stash.Indexed = wire.Stash.Indexed
	output.Stash.IndexRequested = wire.Stash.IndexRequested
	output.Stash.FailedCount = failedCount
	return output, true
}

func validHitspecCaptureTags(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	if !jsonKind(raw, '[') {
		return false
	}
	var encoded []json.RawMessage
	if json.Unmarshal(raw, &encoded) != nil || len(encoded) > maxHitspecCaptureTags {
		return false
	}
	for _, item := range encoded {
		tag, ok := decodeStrictJSONString(item, true)
		if !ok || !validHitspecCaptureString(tag, 64, true) {
			return false
		}
	}
	return true
}

func decodeHitspecCaptureFailures(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, true
	}
	if !jsonKind(raw, '[') {
		return 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var encoded []hitspecCaptureFailureWire
	if decoder.Decode(&encoded) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(encoded) > maxHitspecCaptureFailures {
		return 0, false
	}
	for _, item := range encoded {
		id, idOK := decodeStrictJSONString(item.ID, true)
		stage, stageOK := decodeStrictJSONString(item.Stage, true)
		failure, failureOK := decodeStrictJSONString(item.Error, true)
		if !idOK || !stageOK || !failureOK ||
			!validHitspecCaptureString(id, 128, false) ||
			!validHitspecCaptureString(stage, 64, false) ||
			!validHitspecCaptureString(failure, 1024, false) {
			return 0, false
		}
	}
	return len(encoded), true
}

func validHitspecCaptureString(value string, maximumBytes int, required bool) bool {
	if !utf8.ValidString(value) || len(value) > maximumBytes || required && value == "" {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validHitspecCaptureRunes(value string, maximumRunes int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes || required && value == "" {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validHitspecCaptureURL(raw string) bool {
	if len(raw) == 0 || len(raw) > maxHitspecCaptureURLBytes {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return false
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		return validHitspecCapturePublicIP(address.Unmap())
	}
	for _, suffix := range []string{"localhost", "local", "internal", "invalid", "test", "onion", "home.arpa"} {
		if hostname == suffix || strings.HasSuffix(hostname, "."+suffix) {
			return false
		}
	}
	labels := strings.Split(hostname, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func validHitspecCapturePublicIP(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsUnspecified() || address.IsLinkLocalUnicast() || address.IsMulticast() {
		return false
	}
	for _, rawPrefix := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4",
		"240.0.0.0/4", "100::/64", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2002::/16",
	} {
		if netip.MustParsePrefix(rawPrefix).Contains(address) {
			return false
		}
	}
	return true
}

const (
	maxHitspecSearchDocumentBytes  = 64 << 10
	maxHitspecCaptureDocumentBytes = 64 << 10
	maxHitspecSearchResults        = 10
	maxHitspecSearchQueryRunes     = 512
	maxHitspecSearchTitleRunes     = 300
	maxHitspecSearchSnippetRunes   = 1024
	maxHitspecSearchURLBytes       = 4096
	maxHitspecSearchPublished      = 128
	maxHitspecCaptureTags          = 20
	maxHitspecCaptureFailures      = 16
	maxHitspecCaptureURLBytes      = 8192
)
