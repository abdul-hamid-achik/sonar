package ecosystem

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxProjectionIdentifierBytes = 96
	maxProjectionDigestItems     = 6
	maxProjectionMetric          = int64(1<<53 - 1)
)

// ToolRole is the stable product role a companion tool plays. It deliberately
// describes user intent rather than transport topology: MCPHub may route a
// Cortex call, but Cortex still owns coordination and evidence.
type ToolRole string

const (
	RoleGeneral       ToolRole = "general"
	RoleDiscovery     ToolRole = "discovery"
	RoleStructure     ToolRole = "structure"
	RoleCoordination  ToolRole = "coordination"
	RoleVerification  ToolRole = "verification"
	RoleSecurity      ToolRole = "security"
	RoleArtifact      ToolRole = "artifact"
	RoleObservability ToolRole = "observability"
	RoleBuild         ToolRole = "build"
	RoleGateway       ToolRole = "gateway"
)

// TransportState answers whether the invocation itself reached a terminal
// result. It must never be used as proof that the domain operation succeeded.
type TransportState string

const (
	TransportRunning   TransportState = "running"
	TransportSucceeded TransportState = "succeeded"
	TransportFailed    TransportState = "failed"
)

// DomainState is the interpreted outcome reported by a stable companion-tool
// contract. Unknown is intentionally distinct from success: a verifier whose
// envelope cannot be interpreted must not produce a green receipt.
type DomainState string

const (
	DomainPending   DomainState = "pending"
	DomainUnknown   DomainState = "unknown"
	DomainSucceeded DomainState = "succeeded"
	DomainAttention DomainState = "attention"
	DomainFailed    DomainState = "failed"
	DomainBlocked   DomainState = "blocked"
	DomainConflict  DomainState = "conflict"
	DomainDrift     DomainState = "drift"
)

// EvidenceState is the shared evidence grammar used by the transcript. A
// transport success alone never advances evidence to verified.
type EvidenceState string

const (
	EvidenceNone         EvidenceState = ""
	EvidenceCandidate    EvidenceState = "candidate"
	EvidenceSupported    EvidenceState = "supported"
	EvidenceVerified     EvidenceState = "verified"
	EvidenceContradicted EvidenceState = "contradicted"
	EvidenceStale        EvidenceState = "stale"
)

// ToolRoute is a bounded, non-secret routing receipt. Raw arguments and tool
// results do not belong here. Lazy marks a downstream tool reached through a
// gateway catalog instead of a tool pinned directly into the model prompt.
type ToolRoute struct {
	Gateway string `json:"gateway,omitempty"`
	Server  string `json:"server,omitempty"`
	Tool    string `json:"tool,omitempty"`
	CallID  string `json:"call_id,omitempty"`
	Lazy    bool   `json:"lazy,omitempty"`
}

// ReceiptDigestKind identifies a small, exact companion-tool contract whose
// useful non-secret fields can safely survive the raw MCP parser boundary.
// The values are deliberately product-specific: an unrecognized structured
// document remains DomainUnknown instead of becoming a generic success.
type ReceiptDigestKind string

const (
	DigestMCPHubServers          ReceiptDigestKind = "mcphub_servers"
	DigestMCPHubSearch           ReceiptDigestKind = "mcphub_search"
	DigestMCPHubDescribe         ReceiptDigestKind = "mcphub_describe"
	DigestMCPHubResolve          ReceiptDigestKind = "mcphub_resolve"
	DigestMCPHubStats            ReceiptDigestKind = "mcphub_stats"
	DigestMCPHubStored           ReceiptDigestKind = "mcphub_stored"
	DigestMCPHubPage             ReceiptDigestKind = "mcphub_page"
	DigestMCPHubUnavailable      ReceiptDigestKind = "mcphub_unavailable"
	DigestMCPHubCursorOutOfRange ReceiptDigestKind = "mcphub_cursor_out_of_range"
	DigestMCPHubError            ReceiptDigestKind = "mcphub_error"
	DigestMCPHubDetached         ReceiptDigestKind = "mcphub_detached"
	DigestHitspecSearch          ReceiptDigestKind = "hitspec_search"
	DigestCortexFailure          ReceiptDigestKind = "cortex_failure"
	DigestCortexReceipt          ReceiptDigestKind = "cortex_receipt"
	DigestBobContext             ReceiptDigestKind = "bob_context"
	DigestBobPath                ReceiptDigestKind = "bob_path"
	DigestBobPlaybook            ReceiptDigestKind = "bob_playbook"
	DigestBobFailure             ReceiptDigestKind = "bob_failure"
)

// ReceiptDigest is a bounded, product-specific allowlist of durable semantic
// metadata. It must never contain descriptions, schemas, queries, arguments,
// page data, previews, paths, commands, user values, error prose, or any other
// arbitrary server-controlled value. Identifier and digest fields are
// canonical and numeric fields are bounded. Kind controls which subset is
// authoritative; unknown combinations are discarded by ToolProjection.Normalize.
type ReceiptDigest struct {
	Kind           ReceiptDigestKind `json:"kind,omitempty"`
	Count          int64             `json:"count,omitempty"`
	Connected      int64             `json:"connected,omitempty"`
	TotalTools     int64             `json:"total_tools,omitempty"`
	Calls          int64             `json:"calls,omitempty"`
	Errors         int64             `json:"errors,omitempty"`
	Estimated      int64             `json:"estimated_tokens,omitempty"`
	Target         string            `json:"target,omitempty"`
	Items          []string          `json:"items,omitempty"`
	Required       []string          `json:"required,omitempty"`
	Ambiguous      bool              `json:"ambiguous,omitempty"`
	Expose         string            `json:"expose,omitempty"`
	OriginalBytes  int64             `json:"original_bytes,omitempty"`
	BudgetBytes    int64             `json:"budget_bytes,omitempty"`
	Cursor         int64             `json:"cursor,omitempty"`
	NextCursor     int64             `json:"next_cursor,omitempty"`
	TotalBytes     int64             `json:"total_bytes,omitempty"`
	PageBytes      int64             `json:"page_bytes,omitempty"`
	Done           bool              `json:"done,omitempty"`
	Truncated      bool              `json:"truncated,omitempty"`
	RecipeID       string            `json:"recipe_id,omitempty"`
	RecipeVersion  int64             `json:"recipe_version,omitempty"`
	State          string            `json:"state,omitempty"`
	Classification string            `json:"classification,omitempty"`
	Effect         string            `json:"effect,omitempty"`
	Scope          string            `json:"scope,omitempty"`
	Risk           string            `json:"risk,omitempty"`
	FirstAction    string            `json:"first_action,omitempty"`
	ContractDigest string            `json:"contract_digest,omitempty"`
	ContextDigest  string            `json:"context_digest,omitempty"`
	PlanDigest     string            `json:"plan_digest,omitempty"`
	ConflictCount  int64             `json:"conflict_count,omitempty"`
	ManagedFiles   int64             `json:"managed_files,omitempty"`
	Capabilities   int64             `json:"capabilities,omitempty"`
	ExtensionCount int64             `json:"extension_count,omitempty"`
	PlaybookCount  int64             `json:"playbook_count,omitempty"`
	Exists         bool              `json:"exists,omitempty"`
	Available      bool              `json:"available,omitempty"`
	Blocked        bool              `json:"blocked,omitempty"`
}

// ToolProjection is the semantic, persistable projection of one tool call.
// It contains no arbitrary argument or result values and is safe to keep in a
// session transcript after Normalize.
type ToolProjection struct {
	Specialist string         `json:"specialist,omitempty"`
	Operation  string         `json:"operation,omitempty"`
	Role       ToolRole       `json:"role,omitempty"`
	Transport  TransportState `json:"transport,omitempty"`
	Domain     DomainState    `json:"domain,omitempty"`
	// DomainTyped reports that Domain came from an exact host-trusted envelope
	// parser for the routed specialist. Generic transport or IsError coercion
	// never sets it, so only typed domains prove the effect owner answered.
	DomainTyped bool            `json:"domainTyped,omitempty"`
	Evidence    EvidenceState   `json:"evidence,omitempty"`
	Route       ToolRoute       `json:"route,omitempty"`
	Digest      *ReceiptDigest  `json:"digest,omitempty"`
	Artifact    *ArtifactDigest `json:"artifact,omitempty"`
}

// RawReceipt is the short-lived parser boundary between an MCP/tool transport
// and the semantic projection. Structured and ErrorMeta must be discarded
// after projection; they may contain large or sensitive downstream fields.
type RawReceipt struct {
	Text           string
	Structured     json.RawMessage
	ErrorMeta      json.RawMessage
	TransportError bool
	ToolError      bool
	// TrustedLocal is true only for sonar's own built-in and memory
	// tools. MCP transport success is never sufficient to set domain success.
	TrustedLocal bool
}

var specialistRoles = map[string]ToolRole{
	"mcphub":     RoleGateway,
	"cortex":     RoleCoordination,
	"bob":        RoleBuild,
	"monitor":    RoleObservability,
	"vecgrep":    RoleDiscovery,
	"veclite":    RoleDiscovery,
	"codemap":    RoleStructure,
	"glyphrun":   RoleVerification,
	"glyph":      RoleVerification,
	"cairntrace": RoleVerification,
	"cairn":      RoleVerification,
	"vidtrace":   RoleArtifact,
	"tinyvault":  RoleSecurity,
	"tvault":     RoleSecurity,
	"filecheap":  RoleArtifact,
	"fcheap":     RoleArtifact,
	"hitspec":    RoleDiscovery,
}

// ProjectToolCall creates a bounded semantic projection without retaining raw
// tool arguments. Only MCP routing identifiers are allowlisted from args.
func ProjectToolCall(name string, args map[string]any) ToolProjection {
	key := canonicalIdentifier(name)
	segments := splitToolName(key)
	operation := ""
	if len(segments) > 0 {
		operation = segments[len(segments)-1]
	}

	projection := ToolProjection{
		Operation: operation,
		Transport: TransportRunning,
		Domain:    DomainPending,
	}

	// Parser authority depends on the exact routed identity, not merely a lossy
	// canonicalization of attacker-controlled tool names or gateway arguments.
	// We still retain bounded canonical route metadata for display, but malformed
	// aliases never inherit a specialist's typed receipt parser.
	routeShapeValid := false
	routeIdentityExact := false
	rawRouteServer := ""
	if name == key && key == "mcphub__mcphub_call_tool" {
		routeShapeValid = true
		projection.Route.Gateway = "mcphub"
		projection.Route.Lazy = true
		rawServer := rawArgumentString(args, "server")
		rawTool := rawArgumentString(args, "tool")
		server := argumentIdentifier(args, "server")
		tool := argumentIdentifier(args, "tool")
		if server == "" {
			if before, after, ok := strings.Cut(tool, "__"); ok {
				server, tool = before, after
			}
			if before, after, ok := strings.Cut(rawTool, "__"); ok {
				rawServer, rawTool = before, after
			}
		} else {
			tool = strings.TrimPrefix(tool, server+"__")
			rawTool = strings.TrimPrefix(rawTool, rawServer+"__")
		}
		rawRouteServer = rawServer
		projection.Route.Server = canonicalIdentifier(server)
		projection.Route.Tool = canonicalIdentifier(tool)
		if projection.Route.Tool != "" {
			projection.Operation = projection.Route.Tool
		}
		routeIdentityExact = rawRouteServer == projection.Route.Server &&
			rawTool == projection.Route.Tool
	} else {
		routeShapeValid = projectNamedRoute(&projection, segments)
		rawRouteServer = rawNamedRouteServer(name, segments)
		routeIdentityExact = routeShapeValid && name == key
	}

	if projection.Operation == "mcphub_get_result" {
		projection.Route.CallID = argumentIdentifier(args, "callId", "call_id")
	}

	if projection.Route.Server != "" && routeIdentityExact {
		// Any explicit route server names the effect owner authoritatively. A
		// non-specialist direct server or gateway downstream must not gain a
		// specialist's parsers (or persisted attribution) merely by echoing that
		// specialist's operation name.
		projection.Specialist = exactSpecialistIdentity(rawRouteServer)
	} else if projection.Route.Server == "" && routeShapeValid && routeIdentityExact {
		projection.Specialist = inferSpecialist(projection.Route.Server, projection.Operation, segments)
	}
	projection.Role = specialistRoles[projection.Specialist]
	if projection.Specialist == "hitspec" && isHitspecCaptureOperation(projection.Operation) {
		projection.Role = RoleArtifact
	}
	if projection.Role == "" {
		projection.Role = RoleGeneral
	}
	if projection.Route.Server == "" && projection.Specialist != "" && projection.Specialist != "mcphub" {
		projection.Route.Server = projection.Specialist
	}
	if projection.Route.Tool == "" {
		projection.Route.Tool = projection.Operation
	}
	return projection.Normalize()
}

func projectNamedRoute(projection *ToolProjection, segments []string) bool {
	switch {
	case len(segments) == 1:
		return true
	case len(segments) == 3 && segments[0] == "mcphub":
		projection.Route.Gateway = "mcphub"
		projection.Route.Lazy = true
		projection.Route.Server = segments[1]
		return true
	case len(segments) == 2:
		projection.Route.Server = segments[0]
		return true
	default:
		return false
	}
}

func exactSpecialistIdentity(value string) string {
	if value == "" || value != canonicalIdentifier(value) {
		return ""
	}
	if _, ok := specialistRoles[value]; ok {
		return value
	}
	return ""
}

func rawNamedRouteServer(name string, segments []string) string {
	rawSegments := strings.Split(name, "__")
	switch {
	case len(segments) == 3 && segments[0] == "mcphub" && len(rawSegments) == 3 && rawSegments[0] == "mcphub":
		return rawSegments[1]
	case len(segments) == 2 && len(rawSegments) == 2:
		return rawSegments[0]
	default:
		return ""
	}
}

func inferSpecialist(server, operation string, segments []string) string {
	for _, candidate := range append([]string{server, operation}, segments...) {
		if identity := specialistIdentity(candidate); identity != "" {
			return identity
		}
	}
	return ""
}

func specialistIdentity(value string) string {
	value = canonicalIdentifier(value)
	if _, ok := specialistRoles[value]; ok {
		return value
	}
	for identity := range specialistRoles {
		if strings.HasPrefix(value, identity+"_") {
			return identity
		}
	}
	return ""
}

// ProjectToolResult applies transport state and authoritative parsers. Known
// verification/build tools fail closed to DomainUnknown when their stable
// envelope is absent, preventing a successful MCP exchange from masquerading
// as verified work.
func ProjectToolResult(projection ToolProjection, result string, isError bool) ToolProjection {
	return ProjectReceipt(projection, RawReceipt{Text: result, TransportError: isError, ToolError: isError})
}

// ProjectReceipt interprets one short-lived raw receipt using exact tool and
// schema contracts. Transport and tool/domain errors remain separate.
func ProjectReceipt(projection ToolProjection, receipt RawReceipt) ToolProjection {
	projection = projection.Normalize()
	if receipt.TransportError {
		projection.Transport = TransportFailed
		projection.Domain = DomainFailed
		projection.Evidence = EvidenceNone
		return projection.Normalize()
	}

	projection.Transport = TransportSucceeded
	if projection.Operation == "consult_experts" {
		if receipt.TrustedLocal {
			projection.Domain = projectExpertConsultationDomain(receipt.Text)
			projection.DomainTyped = projection.Domain != DomainUnknown
		} else {
			projection.Domain = DomainUnknown
		}
		projection.Evidence = EvidenceNone
		return finalizeToolReceipt(projection, receipt.ToolError)
	}
	if projected, recognized := projectMCPHubReceipt(projection, receipt); recognized {
		projected.DomainTyped = projected.Domain != DomainUnknown
		return finalizeToolReceipt(projected, receipt.ToolError)
	}
	if _, hasTypedError := exactJSONDocument(receipt.ErrorMeta); hasTypedError {
		projection.Domain = DomainFailed
		projection.Evidence = EvidenceNone
		return projection.Normalize()
	}
	switch projection.Specialist {
	case "bob":
		// Bob proves repository-contract state, never application behavior.
		// Clear any caller-seeded evidence before every success, failure, CLI
		// fallback, or malformed-receipt path.
		projection.Evidence = EvidenceNone
		if domain, digest, ok := projectBobReceipt(projection.Operation, receipt); ok {
			projection.Domain = domain
			projection.Digest = digest
			projection.DomainTyped = true
		} else if !hasStructuredReceipt(receipt) {
			if envelope, ok := ParseBobEnvelope(receipt.Text); ok {
				if domain, recognized := projectBobCLIReceipt(projection.Operation, envelope); recognized {
					projection.Domain = domain
					projection.DomainTyped = true
				} else {
					projection.Domain = DomainUnknown
				}
			} else {
				projection.Domain = DomainUnknown
			}
		} else {
			projection.Domain = DomainUnknown
		}
	case "glyphrun", "glyph", "cairntrace", "cairn":
		if domain, evidence, ok := projectVerifierReceipt(projection.Specialist, projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.DomainTyped = true
		} else {
			projection.Domain = DomainUnknown
			projection.Evidence = EvidenceNone
		}
	case "codemap":
		if domain, evidence, ok := projectCodemapReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
		}
	case "vecgrep":
		if domain, evidence, ok := projectVecgrepReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
		}
	case "monitor":
		if domain, evidence, artifact, ok := projectMonitorReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.Artifact = artifact
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
			projection.Artifact = nil
		}
	case "vidtrace":
		if domain, evidence, ok := projectVidtraceReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
		}
	case "filecheap", "fcheap":
		if domain, evidence, artifact, ok := projectFileCheapReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.Artifact = artifact
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
			projection.Artifact = nil
		}
	case "hitspec":
		if domain, evidence, digest, ok := projectHitspecSearchReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.Digest = digest
			projection.DomainTyped = true
		} else if domain, evidence, artifact, ok := projectHitspecReceipt(projection.Operation, receipt); ok {
			projection.Domain, projection.Evidence = domain, evidence
			projection.Artifact = artifact
			projection.DomainTyped = true
		} else if domain, ok := projectHitspecPreEffectFailure(projection.Operation, receipt); ok {
			projection.Domain = domain
			projection.Evidence = EvidenceNone
			projection.DomainTyped = true
		} else {
			projection.Domain, projection.Evidence = DomainUnknown, EvidenceNone
			projection.Digest = nil
			projection.Artifact = nil
		}
	case "cortex":
		// Cortex's shared envelope is authoritative for the catalogued
		// lifecycle operations: ok=true is coordination success (never
		// verification evidence) and ok=false is a typed rejection. Everything
		// else remains unknown until an operation-specific parser exists.
		if domain, digest, ok := projectCortexReceipt(projection.Operation, receipt); ok {
			projection.Domain = domain
			projection.DomainTyped = true
			projection.Evidence = EvidenceNone
			projection.Digest = digest
		} else if taskID, failed := projectCortexFailureReceipt(receipt); failed {
			projection.Domain = DomainFailed
			projection.DomainTyped = true
			projection.Evidence = EvidenceNone
			projection.Digest = &ReceiptDigest{Kind: DigestCortexFailure, Target: taskID}
		} else {
			projection.Domain = DomainUnknown
			projection.Evidence = EvidenceNone
		}
	default:
		if receipt.ToolError || len(receipt.ErrorMeta) > 0 {
			projection.Domain = DomainFailed
		} else if receipt.TrustedLocal {
			projection.Domain = DomainSucceeded
			projection.DomainTyped = true
		} else {
			projection.Domain = DomainUnknown
			projection.Evidence = EvidenceNone
		}
	}

	if projection.Domain == DomainSucceeded {
		switch projection.Role {
		case RoleDiscovery:
			projection.Evidence = EvidenceCandidate
		case RoleStructure:
			projection.Evidence = EvidenceSupported
		}
	}
	return finalizeToolReceipt(projection, receipt.ToolError)
}

// projectExpertConsultationDomain recognizes only the bounded summary line
// emitted by sonar's built-in expert runtime. Expert prose remains
// advisory text and is never interpreted as domain evidence.
func projectExpertConsultationDomain(text string) DomainState {
	const prefix = "experts: total="
	for range 5 {
		line, rest, more := strings.Cut(text, "\n")
		if strings.HasPrefix(line, prefix) {
			totalText, counts, ok := strings.Cut(strings.TrimPrefix(line, prefix), " · completed=")
			if !ok {
				return DomainUnknown
			}
			completedText, failedText, ok := strings.Cut(counts, " · failed=")
			if !ok {
				return DomainUnknown
			}
			total, totalErr := strconv.Atoi(totalText)
			completed, completedErr := strconv.Atoi(completedText)
			failed, failedErr := strconv.Atoi(failedText)
			if totalErr != nil || completedErr != nil || failedErr != nil ||
				total < 0 || total > 99 || completed < 0 || failed < 0 || completed+failed != total ||
				line != fmt.Sprintf("experts: total=%d · completed=%d · failed=%d", total, completed, failed) {
				return DomainUnknown
			}
			switch {
			case total == 0 || completed == 0:
				return DomainFailed
			case failed > 0:
				return DomainAttention
			default:
				return DomainSucceeded
			}
		}
		if !more {
			break
		}
		text = rest
	}
	return DomainUnknown
}

func finalizeToolReceipt(projection ToolProjection, toolError bool) ToolProjection {
	// MCP IsError is an application-level failure signal. Exact structured
	// detail may refine it to conflict/attention/contradicted, but it can never
	// be overridden by an optimistic envelope into success or verification.
	if toolError {
		if projection.Domain == DomainSucceeded || projection.Domain == DomainPending || projection.Domain == DomainUnknown || projection.Domain == "" {
			projection.Domain = DomainFailed
			projection.DomainTyped = false
			// The application-level error contradicts the optimistic typed
			// outcome, so candidate/supported evidence from that same envelope
			// cannot survive either. Parsers that already returned an explicit
			// attention/failed/contradicted state keep their bounded evidence.
			projection.Evidence = EvidenceNone
		}
	}
	return projection.Normalize()
}

// SafeReceiptText is the only model/UI fallback for structured-only tool
// results. Every value is a bounded allowlisted projection identifier; raw
// arguments, structured fields, and free-form server prose are excluded.
func SafeReceiptText(projection ToolProjection) string {
	projection = projection.Normalize()
	parts := []string{"tool receipt"}
	appendField := func(key, value string) {
		if value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	appendField("specialist", projection.Specialist)
	appendField("operation", projection.Operation)
	appendField("transport", string(projection.Transport))
	appendField("domain", string(projection.Domain))
	appendField("evidence", string(projection.Evidence))
	appendField("call_id", projection.Route.CallID)
	receipt := strings.Join(parts, " ")
	if summary := projection.SummaryText(); summary != "" {
		receipt += "\nsummary: " + summary
	}
	return receipt
}

// SummaryText renders only host-derived, bounded digest fields. It is safe for
// the model, transcript, and compact tool card; raw MCP content is never used.
func (p ToolProjection) SummaryText() string {
	p = p.Normalize()
	if p.Artifact != nil {
		return p.Artifact.summaryText()
	}
	if p.Digest == nil {
		return ""
	}
	digest := *p.Digest
	switch digest.Kind {
	case DigestMCPHubServers:
		parts := []string{
			metricLabel(digest.Count, "server", "servers"),
			fmt.Sprintf("%d connected", digest.Connected),
			metricLabel(digest.TotalTools, "tool", "tools"),
		}
		if digest.Expose != "" {
			parts = append(parts, digest.Expose+" exposure")
		}
		if items := digestItemSummary(digest.Items, digest.Count); items != "" {
			parts = append(parts, items)
		}
		return strings.Join(parts, " · ")
	case DigestMCPHubSearch:
		parts := []string{metricLabel(digest.Count, "match", "matches")}
		if items := digestItemSummary(digest.Items, digest.Count); items != "" {
			parts = append(parts, items)
		}
		return strings.Join(parts, " · ")
	case DigestMCPHubDescribe:
		if digest.Target == "" {
			return "tool contract unavailable"
		}
		if len(digest.Required) == 0 {
			return digest.Target + " · no required fields"
		}
		return digest.Target + " · requires " + strings.Join(digest.Required, ", ")
	case DigestMCPHubResolve:
		if digest.Target == "" {
			return "no matching tool"
		}
		parts := []string{"recommended " + digest.Target}
		if digest.Ambiguous {
			parts = append(parts, "ambiguous")
		}
		if len(digest.Required) > 0 {
			parts = append(parts, "requires "+strings.Join(digest.Required, ", "))
		}
		if len(digest.Items) > 0 {
			parts = append(parts, metricLabel(int64(len(digest.Items)), "alternative", "alternatives"))
		}
		return strings.Join(parts, " · ")
	case DigestMCPHubStats:
		return strings.Join([]string{
			metricLabel(digest.Calls, "call", "calls"),
			metricLabel(digest.Errors, "error", "errors"),
			fmt.Sprintf("~%d est. tokens", digest.Estimated),
			metricLabel(digest.Count, "server", "servers"),
		}, " · ")
	case DigestMCPHubStored:
		parts := []string{"result stored"}
		if digest.OriginalBytes > 0 {
			parts = append(parts, fmt.Sprintf("%d bytes", digest.OriginalBytes))
		}
		if p.Route.CallID != "" {
			parts = append(parts, "fetch "+p.Route.CallID)
		}
		return strings.Join(parts, " · ")
	case DigestMCPHubPage:
		end := digest.NextCursor
		if end < digest.Cursor {
			end = digest.Cursor
		}
		parts := []string{fmt.Sprintf("bytes %d–%d of %d", digest.Cursor, end, digest.TotalBytes)}
		if digest.Done {
			parts = append(parts, "final page")
		} else {
			parts = append(parts, fmt.Sprintf("continue at cursor %d", digest.NextCursor))
		}
		// Page bytes may contain arbitrary downstream data. The parser validates
		// and discards them instead of copying them into persistent model history.
		parts = append(parts, "payload omitted from persistent context")
		return strings.Join(parts, " · ")
	case DigestMCPHubUnavailable:
		return "stored result unavailable or expired"
	case DigestMCPHubCursorOutOfRange:
		return fmt.Sprintf("cursor %d is outside the stored result", digest.Cursor)
	case DigestMCPHubError:
		return "MCPHub reported an error"
	case DigestHitspecSearch:
		parts := []string{metricLabel(digest.Count, "candidate source", "candidate sources")}
		if items := digestItemSummary(digest.Items, digest.Count); items != "" {
			parts = append(parts, items)
		}
		if digest.Truncated {
			parts = append(parts, "more results omitted")
		}
		return strings.Join(parts, " · ")
	case DigestCortexFailure:
		if digest.Target != "" {
			return "Cortex rejected the request for " + digest.Target
		}
		return "Cortex rejected the request"
	case DigestCortexReceipt:
		parts := []string{"Cortex accepted the request"}
		if digest.Target != "" {
			parts[0] = "Cortex accepted the request for " + digest.Target
		}
		if len(digest.Items) == 1 {
			parts = append(parts, "phase "+digest.Items[0])
		}
		return strings.Join(parts, " · ")
	case DigestBobContext:
		parts := []string{"Bob repository " + digest.State}
		if digest.RecipeID != "" && digest.RecipeVersion > 0 {
			parts = append(parts, fmt.Sprintf("%s@%d", digest.RecipeID, digest.RecipeVersion))
		}
		parts = append(parts,
			metricLabel(digest.ManagedFiles, "managed file", "managed files"),
			metricLabel(digest.ConflictCount, "conflict", "conflicts"),
		)
		if digest.FirstAction != "" {
			parts = append(parts, "next "+digest.FirstAction)
		}
		if digest.Truncated {
			parts = append(parts, "bounded fields omitted")
		}
		return strings.Join(parts, " · ")
	case DigestBobPath:
		parts := []string{"Bob path " + digest.Classification, digest.State, digest.Effect}
		if digest.FirstAction != "" {
			parts = append(parts, "next "+digest.FirstAction)
		}
		if digest.Truncated {
			parts = append(parts, "related IDs omitted")
		}
		return strings.Join(parts, " · ")
	case DigestBobPlaybook:
		parts := []string{"Bob playbook " + digest.State}
		if digest.Target != "" {
			parts[0] += " " + digest.Target
		}
		if digest.Scope != "" {
			parts = append(parts, digest.Scope+" scope")
		}
		if digest.Risk != "" {
			parts = append(parts, digest.Risk+" risk")
		}
		if len(digest.Required) > 0 {
			parts = append(parts, "needs "+strings.Join(digest.Required, ", "))
		}
		if digest.FirstAction != "" {
			parts = append(parts, "first "+digest.FirstAction)
		}
		if digest.Truncated {
			parts = append(parts, "bounded fields omitted")
		}
		return strings.Join(parts, " · ")
	case DigestBobFailure:
		return "Bob rejected the request with " + digest.Target
	default:
		return ""
	}
}

func metricLabel(value int64, singular, plural string) string {
	label := plural
	if value == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", value, label)
}

func digestItemSummary(items []string, total int64) string {
	if len(items) == 0 {
		return ""
	}
	summary := strings.Join(items, ", ")
	if remaining := total - int64(len(items)); remaining > 0 {
		summary += fmt.Sprintf(" +%d", remaining)
	}
	return summary
}

func classifyBobDomain(envelope BobEnvelope) DomainState {
	if len(envelope.Conflicts()) > 0 {
		return DomainConflict
	}
	if info, ok := envelope.ErrorInfo(); ok {
		return classifyBobErrorCode(info.Code)
	}
	if count, present := envelope.ConflictCount(); present && count > 0 {
		return DomainConflict
	}
	if clean, present := envelope.CleanFlag(); present && !clean {
		return DomainDrift
	}
	if changed, present := envelope.LockChangedFlag(); present && changed {
		return DomainDrift
	}
	for _, action := range envelope.Actions() {
		if action.IsConflict() {
			return DomainConflict
		}
		if action.Code != "" && action.Code != "in_sync" && action.Code != "identical_content" {
			return DomainDrift
		}
		if action.Code == "" && action.Kind != "" && action.Kind != "unchanged" {
			return DomainDrift
		}
	}
	if !envelope.OK {
		return DomainFailed
	}
	return DomainSucceeded
}

func projectBobCLIReceipt(operation string, envelope BobEnvelope) (DomainState, bool) {
	expectedCommand := map[string]string{
		"bob_check":           "check",
		"bob_inspect":         "inspect",
		"bob_plan":            "plan",
		"bob_recipe_describe": "recipe show",
		"bob_stats":           "stats",
	}[operation]
	if expectedCommand == "" {
		return "", false
	}
	if info, hasError := envelope.ErrorInfo(); hasError {
		errorCommand := expectedCommand
		if operation == "bob_recipe_describe" {
			errorCommand = "recipe"
		}
		if envelope.Command != errorCommand {
			return "", false
		}
		if envelope.OK || strings.TrimSpace(info.Code) == "" || strings.TrimSpace(info.Code) != info.Code ||
			strings.TrimSpace(info.Message) == "" {
			return "", false
		}
		return classifyBobErrorCode(info.Code), true
	}
	if envelope.Command != expectedCommand {
		return "", false
	}
	if !validBobCLISuccess(operation, envelope) {
		return "", false
	}
	if operation == "bob_inspect" {
		inspection, ok := validBobCLIInspectReport(envelope.Data)
		if !ok {
			return "", false
		}
		return classifyBobInspection(inspection), true
	}
	if operation == "bob_plan" && len(envelope.Actions()) == 0 {
		// Bob's --conflicts-only projection can legitimately omit every
		// non-conflicting action. The envelope does not say whether filtering
		// was requested, so an empty plan is valid but cannot prove convergence.
		return DomainAttention, true
	}
	return classifyBobDomain(envelope), true
}

func validBobCLISuccess(operation string, envelope BobEnvelope) bool {
	switch operation {
	case "bob_inspect":
		_, reportOK := validBobCLIInspectReport(envelope.Data)
		return envelope.OK && reportOK
	case "bob_plan":
		return envelope.OK && validBobCLIPlan(envelope.Data, true)
	case "bob_check":
		var data struct {
			Clean *bool           `json:"clean"`
			Plan  json.RawMessage `json:"plan"`
		}
		if json.Unmarshal(envelope.Data, &data) != nil || data.Clean == nil || envelope.OK != *data.Clean ||
			!validBobCLIPlan(data.Plan, true) {
			return false
		}
		if !*data.Clean {
			return true
		}
		var plan struct {
			LockChanged *bool       `json:"lock_changed"`
			Actions     []BobAction `json:"actions"`
		}
		if json.Unmarshal(data.Plan, &plan) != nil || plan.LockChanged == nil || *plan.LockChanged {
			return false
		}
		for _, action := range plan.Actions {
			if action.Kind != "unchanged" {
				return false
			}
		}
		return true
	case "bob_recipe_describe":
		return envelope.OK && validBobCLIRecipeDescription(envelope.Data)
	case "bob_stats":
		var data struct {
			Enabled   *bool           `json:"enabled"`
			LocalOnly *bool           `json:"local_only"`
			Selection string          `json:"selection"`
			Stats     json.RawMessage `json:"stats"`
		}
		return envelope.OK && json.Unmarshal(envelope.Data, &data) == nil && data.Enabled != nil &&
			data.LocalOnly != nil && (data.Selection == "all" || validBobWorkspace(data.Selection)) &&
			*data.LocalOnly && validBobStats(data.Stats, *data.Enabled)
	default:
		return false
	}
}

func validBobCLIPlan(raw json.RawMessage, allowEmpty bool) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	var plan struct {
		SchemaVersion int             `json:"schema_version"`
		Recipe        json.RawMessage `json:"recipe"`
		LockChanged   *bool           `json:"lock_changed"`
		ConflictCount *int            `json:"conflict_count"`
		Actions       []BobAction     `json:"actions"`
	}
	if json.Unmarshal(raw, &plan) != nil || plan.SchemaVersion != 1 || plan.LockChanged == nil ||
		plan.ConflictCount == nil || *plan.ConflictCount < 0 || !jsonObjectHasKey(raw, "actions") {
		return false
	}
	_, recipeOK := validBobRecipeRef(plan.Recipe)
	if !recipeOK || (!allowEmpty && len(plan.Actions) == 0) {
		return false
	}
	conflicts := 0
	previousPath := ""
	for index, action := range plan.Actions {
		if !validBobActionPath(action.Path) || !validBobCLIAction(action) {
			return false
		}
		if index > 0 && action.Path <= previousPath {
			return false
		}
		previousPath = action.Path
		if action.Kind == "conflict" {
			conflicts++
		}
	}
	return conflicts == *plan.ConflictCount
}

func validBobCLIAction(action BobAction) bool {
	if action.Code == "" {
		switch action.Kind {
		case "create", "update", "adopt", "unchanged", "conflict":
			return true
		default:
			return false
		}
	}
	return validBobActionKindCode(action.Kind, action.Code)
}

func classifyBobErrorCode(code string) DomainState {
	switch code {
	case "conflicts":
		return DomainConflict
	case "missing_manifest", "manifest_invalid", "manifest_too_large", "recipe_invalid", "recipe_unknown", "input_invalid", "workspace_invalid", "workspace_unauthorized":
		return DomainBlocked
	default:
		return DomainFailed
	}
}

// Successful reports a semantically successful terminal outcome.
func (p ToolProjection) Successful() bool {
	return p.Transport == TransportSucceeded && p.Domain == DomainSucceeded
}

// NeedsAttention reports a terminal outcome that should not be painted as a
// successful green receipt even though the transport may have succeeded.
func (p ToolProjection) NeedsAttention() bool {
	switch p.Domain {
	case DomainUnknown, DomainAttention, DomainBlocked, DomainConflict, DomainDrift:
		return true
	default:
		return false
	}
}

// Normalize bounds all persisted identifiers and drops unknown enum values.
func (p ToolProjection) Normalize() ToolProjection {
	p.Specialist = canonicalIdentifier(p.Specialist)
	p.Operation = canonicalIdentifier(p.Operation)
	p.Route.Gateway = canonicalIdentifier(p.Route.Gateway)
	p.Route.Server = canonicalIdentifier(p.Route.Server)
	p.Route.Tool = canonicalIdentifier(p.Route.Tool)
	p.Route.CallID = canonicalIdentifier(p.Route.CallID)
	if !validRole(p.Role) {
		p.Role = RoleGeneral
	}
	if !validTransport(p.Transport) {
		p.Transport = TransportSucceeded
		p.Domain = DomainUnknown
	}
	if !validDomain(p.Domain) {
		p.Domain = DomainUnknown
	}
	if !validEvidence(p.Evidence) {
		p.Evidence = EvidenceNone
		if p.Transport != "" {
			p.Domain = DomainUnknown
		}
	}
	if p.Digest != nil {
		digest := normalizeReceiptDigest(*p.Digest)
		validContext := digest.Kind != ""
		if digest.Kind == DigestHitspecSearch {
			validContext = p.Specialist == "hitspec" && isHitspecSearchOperation(p.Operation) &&
				p.Role == RoleDiscovery && p.Transport == TransportSucceeded &&
				p.Domain == DomainSucceeded && p.Evidence == EvidenceCandidate
		} else if isBobDigestKind(digest.Kind) {
			validContext = p.Specialist == "bob" && isBobGuidanceOperation(p.Operation) &&
				p.Role == RoleBuild && p.Transport == TransportSucceeded && p.DomainTyped &&
				p.Evidence == EvidenceNone && validBobDigestContext(p.Operation, p.Domain, digest)
		}
		if !validContext {
			p.Digest = nil
		} else {
			p.Digest = &digest
		}
	}
	if p.Artifact != nil {
		artifact := normalizeArtifactDigest(*p.Artifact)
		baseContext := artifact.Kind != "" && p.Transport == TransportSucceeded &&
			(p.Domain == DomainSucceeded || p.Domain == DomainAttention)
		strictArtifactEvidence := p.Evidence == EvidenceSupported && p.Role == RoleArtifact
		fileCheapContext := artifact.Kind == ArtifactDigestFileCheapStash &&
			strictArtifactEvidence &&
			(p.Specialist == "filecheap" || p.Specialist == "fcheap") &&
			(p.Operation == "fcheap_save" || p.Operation == "filecheap_save")
		hitspecContext := artifact.Kind == ArtifactDigestHitspecCapture && p.Specialist == "hitspec" &&
			strictArtifactEvidence && isHitspecCaptureOperation(p.Operation)
		// Portable ArtifactRefV1 digests come from two typed sources: the
		// fcheap_artifact_ref emitter (RoleArtifact; cloud/link refs carry no
		// local evidence) and a completed monitor investigation that stashed
		// its incident bundle (RoleObservability).
		portableRefContext := artifact.Kind == ArtifactDigestPortableRef && p.DomainTyped &&
			(((p.Specialist == "filecheap" || p.Specialist == "fcheap") && p.Role == RoleArtifact &&
				(p.Operation == "fcheap_artifact_ref" || p.Operation == "filecheap_artifact_ref") &&
				(p.Evidence == EvidenceSupported || p.Evidence == EvidenceNone)) ||
				(p.Specialist == "monitor" && p.Role == RoleObservability &&
					p.Operation == "monitor_investigate" && p.Evidence == EvidenceSupported))
		validContext := baseContext && (fileCheapContext || hitspecContext || portableRefContext)
		if !validContext {
			p.Artifact = nil
		} else {
			p.Artifact = &artifact
		}
	}
	return p
}

func normalizeReceiptDigest(digest ReceiptDigest) ReceiptDigest {
	if !validReceiptDigestKind(digest.Kind) {
		return ReceiptDigest{}
	}
	digest.Target = canonicalIdentifier(digest.Target)
	digest.Expose = canonicalIdentifier(digest.Expose)
	digest.RecipeID = canonicalIdentifier(digest.RecipeID)
	digest.State = canonicalIdentifier(digest.State)
	digest.Classification = canonicalIdentifier(digest.Classification)
	digest.Effect = canonicalIdentifier(digest.Effect)
	digest.Scope = canonicalIdentifier(digest.Scope)
	digest.Risk = canonicalIdentifier(digest.Risk)
	digest.FirstAction = canonicalIdentifier(digest.FirstAction)
	digest.ContractDigest = canonicalIdentifier(digest.ContractDigest)
	digest.ContextDigest = canonicalIdentifier(digest.ContextDigest)
	digest.PlanDigest = canonicalIdentifier(digest.PlanDigest)
	digest.Items = normalizeDigestIdentifiers(digest.Items)
	digest.Required = normalizeDigestIdentifiers(digest.Required)
	digest.Count = boundedProjectionMetric(digest.Count)
	digest.Connected = boundedProjectionMetric(digest.Connected)
	digest.TotalTools = boundedProjectionMetric(digest.TotalTools)
	digest.Calls = boundedProjectionMetric(digest.Calls)
	digest.Errors = boundedProjectionMetric(digest.Errors)
	digest.Estimated = boundedProjectionMetric(digest.Estimated)
	digest.OriginalBytes = boundedProjectionMetric(digest.OriginalBytes)
	digest.BudgetBytes = boundedProjectionMetric(digest.BudgetBytes)
	digest.Cursor = boundedProjectionMetric(digest.Cursor)
	digest.NextCursor = boundedProjectionMetric(digest.NextCursor)
	digest.TotalBytes = boundedProjectionMetric(digest.TotalBytes)
	digest.PageBytes = boundedProjectionMetric(digest.PageBytes)
	digest.RecipeVersion = boundedProjectionMetric(digest.RecipeVersion)
	digest.ConflictCount = boundedProjectionMetric(digest.ConflictCount)
	digest.ManagedFiles = boundedProjectionMetric(digest.ManagedFiles)
	digest.Capabilities = boundedProjectionMetric(digest.Capabilities)
	digest.ExtensionCount = boundedProjectionMetric(digest.ExtensionCount)
	digest.PlaybookCount = boundedProjectionMetric(digest.PlaybookCount)
	return digest
}

func isBobDigestKind(kind ReceiptDigestKind) bool {
	switch kind {
	case DigestBobContext, DigestBobPath, DigestBobPlaybook, DigestBobFailure:
		return true
	default:
		return false
	}
}

func validBobDigestContext(operation string, domain DomainState, digest ReceiptDigest) bool {
	switch digest.Kind {
	case DigestBobContext:
		return operation == "bob_context" && digest.RecipeID != "" && digest.RecipeVersion > 0 &&
			digest.State != "" && validPrefixedSHA256(digest.ContractDigest) &&
			validPrefixedSHA256(digest.ContextDigest) && validPrefixedSHA256(digest.PlanDigest) &&
			(domain == DomainSucceeded || domain == DomainDrift || domain == DomainConflict)
	case DigestBobPath:
		return operation == "bob_path" && digest.Classification != "" && digest.State != "" &&
			digest.Effect != "" && (domain == DomainSucceeded || domain == DomainAttention)
	case DigestBobPlaybook:
		return operation == "bob_playbook" && digest.State != "" &&
			(domain == DomainSucceeded || domain == DomainAttention || domain == DomainBlocked)
	case DigestBobFailure:
		return isBobGuidanceOperation(operation) && digest.Target != "" &&
			(domain == DomainFailed || domain == DomainBlocked || domain == DomainConflict)
	default:
		return false
	}
}

func normalizeDigestIdentifiers(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, min(len(values), maxProjectionDigestItems))
	seen := make(map[string]struct{}, min(len(values), maxProjectionDigestItems))
	for _, value := range values {
		value = canonicalIdentifier(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == maxProjectionDigestItems {
			break
		}
	}
	return result
}

func boundedProjectionMetric(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > maxProjectionMetric {
		return maxProjectionMetric
	}
	return value
}

func validReceiptDigestKind(value ReceiptDigestKind) bool {
	switch value {
	case "", DigestMCPHubServers, DigestMCPHubSearch, DigestMCPHubDescribe,
		DigestMCPHubResolve, DigestMCPHubStats, DigestMCPHubStored,
		DigestMCPHubPage, DigestMCPHubUnavailable,
		DigestMCPHubCursorOutOfRange, DigestMCPHubError, DigestMCPHubDetached, DigestHitspecSearch, DigestCortexFailure,
		DigestCortexReceipt, DigestBobContext, DigestBobPath, DigestBobPlaybook, DigestBobFailure:
		return true
	default:
		return false
	}
}

func splitToolName(name string) []string {
	parts := strings.Split(name, "__")
	result := parts[:0]
	for _, part := range parts {
		if part = canonicalIdentifier(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func argumentIdentifier(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok {
			if value = canonicalIdentifier(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func rawArgumentString(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func canonicalIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		out, keep := r, true
		switch {
		case unicode.IsLetter(r), unicode.IsNumber(r):
		case strings.ContainsRune("_-.:/", r):
		case unicode.IsSpace(r):
			out = '_'
		default:
			keep = false
		}
		if !keep {
			continue
		}
		runeBytes := utf8.RuneLen(out)
		if runeBytes < 0 || builder.Len()+runeBytes > maxProjectionIdentifierBytes {
			break
		}
		builder.WriteRune(out)
	}
	return strings.Trim(builder.String(), "_-.:/")
}

func validRole(value ToolRole) bool {
	switch value {
	case RoleGeneral, RoleDiscovery, RoleStructure, RoleCoordination, RoleVerification, RoleSecurity, RoleArtifact, RoleObservability, RoleBuild, RoleGateway:
		return true
	default:
		return false
	}
}

func validTransport(value TransportState) bool {
	switch value {
	case "", TransportRunning, TransportSucceeded, TransportFailed:
		return true
	default:
		return false
	}
}

func validDomain(value DomainState) bool {
	switch value {
	case "", DomainPending, DomainUnknown, DomainSucceeded, DomainAttention, DomainFailed, DomainBlocked, DomainConflict, DomainDrift:
		return true
	default:
		return false
	}
}

func validEvidence(value EvidenceState) bool {
	switch value {
	case EvidenceNone, EvidenceCandidate, EvidenceSupported, EvidenceVerified, EvidenceContradicted, EvidenceStale:
		return true
	default:
		return false
	}
}
