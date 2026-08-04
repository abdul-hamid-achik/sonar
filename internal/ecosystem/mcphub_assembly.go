package ecosystem

import (
	"bytes"
	"encoding/json"
	"sync"
)

const (
	maxMCPHubAssembledResultBytes = 96 * 1024
	maxMCPHubActiveAssemblies     = 4
	maxMCPHubStoredErrorMetaBytes = 8 * 1024
)

type mcphubResultKey struct {
	namespace string
	callID    string
}

type mcphubResultAssembly struct {
	server   string
	tool     string
	total    int64
	next     int64
	sequence uint64
	data     []byte
}

// MCPHubResultObservation reports whether a page belonged to an exact
// host-bound stored call. Transient is model-only validated content derived
// before the raw assembly is zeroed; it must never be persisted.
type MCPHubResultObservation struct {
	Projection ToolProjection
	Transient  string
	// Workspace is an exact parser-validated Bob context workspace used only by
	// the host's ephemeral cache admission. It must never be persisted or sent to
	// presentation code.
	Workspace string
	// Actions is an ephemeral, bounded continuation projection derived before
	// the assembled raw payload is zeroed. It is never part of ToolProjection or
	// any persisted session shape.
	Actions []ContinuationAction
	// ContinuationSurface remains true even when an exact action fails closed,
	// allowing the agent to suppress raw command/reason prose from model input.
	ContinuationSurface bool
	Bound               bool
	Complete            bool
}

// MCPHubResultAssembler keeps a small, turn-scoped set of exact stored-result
// pages until the complete serialized CallToolResult can be passed through the
// same downstream parser as a direct result. Raw bytes never leave this type,
// and Reset zeroes any partial data before releasing it.
type MCPHubResultAssembler struct {
	mu       sync.Mutex
	entries  map[mcphubResultKey]*mcphubResultAssembly
	sequence uint64
}

// NewMCPHubResultAssembler creates an empty bounded result assembler.
func NewMCPHubResultAssembler() *MCPHubResultAssembler {
	return &MCPHubResultAssembler{entries: make(map[mcphubResultKey]*mcphubResultAssembly)}
}

// Remember binds a stored call ID to an exact route derived by the host from
// the original trusted dispatch. Response prose and page payload shape never
// grant parser authority.
func (a *MCPHubResultAssembler) Remember(namespace, server, tool string, projection ToolProjection) bool {
	if a == nil {
		return false
	}
	projection = projection.Normalize()
	digest := projection.Digest
	callID := projection.Route.CallID
	if namespace == "" || namespace != canonicalIdentifier(namespace) ||
		server == "" || server != canonicalIdentifier(server) ||
		tool == "" || tool != canonicalIdentifier(tool) || digest == nil ||
		digest.Kind != DigestMCPHubStored || callID == "" ||
		(projection.Route.Server != "" && projection.Route.Server != server) ||
		(projection.Route.Tool != "" && projection.Route.Tool != tool) ||
		digest.OriginalBytes <= 0 || digest.OriginalBytes > maxMCPHubAssembledResultBytes {
		return false
	}

	key := mcphubResultKey{namespace: namespace, callID: callID}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropLocked(key)
	if len(a.entries) >= maxMCPHubActiveAssemblies {
		a.dropOldestLocked()
	}
	a.sequence++
	a.entries[key] = &mcphubResultAssembly{
		server: server, tool: tool, total: digest.OriginalBytes, sequence: a.sequence,
		data: make([]byte, 0, min(int(digest.OriginalBytes), maxMCPHubResultPageBytes)),
	}
	return true
}

// ObservePage accepts only the exact requested cursor in a contiguous chain
// for a previously host-bound call. A complete chain is unwrapped as the real
// serialized MCP CallToolResult and routed to exactly one downstream parser.
func (a *MCPHubResultAssembler) ObservePage(namespace string, requestedCursor int64, projection ToolProjection, receipt RawReceipt) MCPHubResultObservation {
	observation := MCPHubResultObservation{Projection: projection}
	if a == nil || projection.Digest == nil || namespace == "" || namespace != canonicalIdentifier(namespace) || requestedCursor < 0 {
		return observation
	}
	key := mcphubResultKey{namespace: namespace, callID: projection.Route.CallID}
	switch projection.Digest.Kind {
	case DigestMCPHubUnavailable, DigestMCPHubCursorOutOfRange, DigestMCPHubError:
		observation.Bound = a.discard(key)
		return observation
	case DigestMCPHubPage:
	default:
		return observation
	}

	document, ok := receiptDocument(receipt)
	if !ok {
		return a.fail(key, observation)
	}
	var envelope mcphubResultPageEnvelope
	if json.Unmarshal(document, &envelope) != nil {
		return a.fail(key, observation)
	}
	page, ok := decodeMCPHubResultPage(envelope)
	if !ok || projection.Route.CallID == "" || envelope.CallID != projection.Route.CallID ||
		envelope.Cursor == nil || envelope.NextCursor == nil || envelope.Done == nil || envelope.TotalBytes == nil ||
		requestedCursor != *envelope.Cursor || projection.Digest.Cursor != *envelope.Cursor ||
		projection.Digest.NextCursor != *envelope.NextCursor || projection.Digest.TotalBytes != *envelope.TotalBytes ||
		projection.Digest.Done != *envelope.Done || projection.Digest.PageBytes != int64(len(page)) {
		return a.fail(key, observation)
	}

	a.mu.Lock()
	entry := a.entries[key]
	if entry == nil {
		a.mu.Unlock()
		return observation
	}
	observation.Bound = true
	if entry.total != *envelope.TotalBytes || entry.next != requestedCursor ||
		*envelope.NextCursor > maxMCPHubAssembledResultBytes ||
		int64(len(entry.data))+int64(len(page)) > entry.total {
		a.dropLocked(key)
		a.mu.Unlock()
		observation.Projection = assemblyFailure(projection)
		return observation
	}
	entry.data = append(entry.data, page...)
	entry.next = *envelope.NextCursor
	if !*envelope.Done {
		a.mu.Unlock()
		observation.Projection = assemblyAttention(projection)
		return observation
	}
	if entry.next != entry.total || int64(len(entry.data)) != entry.total {
		a.dropLocked(key)
		a.mu.Unlock()
		observation.Projection = assemblyFailure(projection)
		return observation
	}
	payload := entry.data
	server, tool := entry.server, entry.tool
	delete(a.entries, key)
	a.mu.Unlock()
	defer clear(payload)

	parsed, transient, workspace, actions, continuationSurface, typed := reparseStoredMCPHubResult(namespace, key.callID, server, tool, payload)
	observation.Complete = true
	if !typed {
		parsed.Domain = DomainUnknown
		parsed.DomainTyped = false
		parsed.Evidence = EvidenceNone
	}
	observation.Projection = parsed.Normalize()
	observation.Transient = transient
	observation.Workspace = workspace
	observation.Actions = actions
	observation.ContinuationSurface = continuationSurface
	return observation
}

// RejectPage tears down an exact bound retrieval after MCPHub answered with an
// application error or a malformed/future page. Transport failures intentionally
// do not call this method so a caller may retry the same cursor safely.
func (a *MCPHubResultAssembler) RejectPage(namespace, callID string, projection ToolProjection) MCPHubResultObservation {
	observation := MCPHubResultObservation{Projection: projection}
	if a == nil || namespace == "" || namespace != canonicalIdentifier(namespace) ||
		callID == "" || callID != canonicalIdentifier(callID) {
		return observation
	}
	if a.discard(mcphubResultKey{namespace: namespace, callID: callID}) {
		observation.Bound = true
		observation.Projection = assemblyFailure(projection)
	}
	return observation
}

// Reset discards every partial result and zeroes its in-memory payload.
func (a *MCPHubResultAssembler) Reset() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.entries {
		a.dropLocked(key)
	}
}

func (a *MCPHubResultAssembler) fail(key mcphubResultKey, observation MCPHubResultObservation) MCPHubResultObservation {
	if a.discard(key) {
		observation.Bound = true
		observation.Projection = assemblyFailure(observation.Projection)
	}
	return observation
}

func (a *MCPHubResultAssembler) discard(key mcphubResultKey) bool {
	if key.namespace == "" || key.callID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.entries[key] == nil {
		return false
	}
	a.dropLocked(key)
	return true
}

func (a *MCPHubResultAssembler) dropOldestLocked() {
	var oldestKey mcphubResultKey
	oldestSequence := ^uint64(0)
	for key, entry := range a.entries {
		if entry.sequence < oldestSequence {
			oldestKey, oldestSequence = key, entry.sequence
		}
	}
	if oldestKey != (mcphubResultKey{}) {
		a.dropLocked(oldestKey)
	}
}

func (a *MCPHubResultAssembler) dropLocked(key mcphubResultKey) {
	if entry := a.entries[key]; entry != nil {
		clear(entry.data)
		delete(a.entries, key)
	}
}

func assemblyAttention(projection ToolProjection) ToolProjection {
	projection.Domain = DomainAttention
	projection.DomainTyped = true
	projection.Evidence = EvidenceNone
	return projection.Normalize()
}

func assemblyFailure(projection ToolProjection) ToolProjection {
	projection.Domain = DomainFailed
	projection.DomainTyped = true
	projection.Evidence = EvidenceNone
	return projection.Normalize()
}

func reparseStoredMCPHubResult(namespace, callID, server, tool string, payload []byte) (ToolProjection, string, string, []ContinuationAction, bool, bool) {
	receipt, ok := storedCallToolReceipt(payload)
	parsed := ProjectToolCall(server+"__"+tool, nil)
	parsed.Route = ToolRoute{Gateway: namespace, Server: server, Tool: tool, CallID: callID, Lazy: true}
	if !ok || server == "" || tool == "" {
		parsed.Transport = TransportSucceeded
		parsed.Domain = DomainUnknown
		parsed.Evidence = EvidenceNone
		return parsed.Normalize(), "", "", nil, false, false
	}
	parsed = ProjectReceipt(parsed, receipt)
	parsed.Route = ToolRoute{Gateway: namespace, Server: server, Tool: tool, CallID: callID, Lazy: true}
	parsed = parsed.Normalize()
	// A real CallToolResult IsError bit is an exact downstream application
	// failure even when the downstream's StructuredContent is malformed or from
	// an unsupported schema. Preserve the same failed/untyped projection as the
	// direct parser instead of converting an answered error into unknown.
	answeredError := receipt.ToolError && parsed.Domain == DomainFailed
	if (!parsed.DomainTyped && !answeredError) || parsed.Domain == DomainUnknown {
		return parsed, "", "", nil, false, false
	}
	transient, _ := TransientModelContent(parsed, receipt)
	workspace, _ := BobContextWorkspace(parsed, receipt)
	actions := ProjectContinuationActions(parsed, receipt)
	return parsed, transient, workspace, actions, ReceiptHasContinuationActions(parsed, receipt), true
}

func storedCallToolReceipt(payload []byte) (RawReceipt, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || len(payload) > maxMCPHubAssembledResultBytes || payload[0] != '{' || !json.Valid(payload) ||
		!jsonObjectHasKey(payload, "content") || !jsonObjectHasKey(payload, "structuredContent") {
		return RawReceipt{}, false
	}
	var result struct {
		Meta              json.RawMessage `json:"_meta"`
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           *bool           `json:"isError"`
	}
	if json.Unmarshal(payload, &result) != nil || !jsonKind(result.Content, '[') || !jsonKind(result.StructuredContent, '{') ||
		(rawJSONPresent(result.Meta) && !jsonKind(result.Meta, '{')) {
		return RawReceipt{}, false
	}
	structured, ok := exactJSONDocument(result.StructuredContent)
	if !ok {
		return RawReceipt{}, false
	}
	toolError := result.IsError != nil && *result.IsError
	var errorMeta json.RawMessage
	if rawJSONPresent(result.Meta) {
		var meta struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(result.Meta, &meta) != nil {
			return RawReceipt{}, false
		}
		if len(meta.Error) > maxMCPHubStoredErrorMetaBytes {
			return RawReceipt{}, false
		}
		if exact, valid := exactJSONDocument(meta.Error); valid {
			errorMeta = exact
		}
	}
	return RawReceipt{Structured: structured, ErrorMeta: errorMeta, ToolError: toolError}, true
}
