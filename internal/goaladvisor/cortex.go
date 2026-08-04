// Package goaladvisor connects the host-owned Goal Runtime to a semantic
// advisor without giving that advisor authority over local effects.
package goaladvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

var (
	ErrUnavailable = errors.New("cortex goal advisor is unavailable")
	ErrRejected    = errors.New("cortex goal request was rejected")
)

const (
	openTool           = "cortex_open_task"
	statusTool         = "cortex_status"
	handoffTool        = "cortex_handoff"
	answerDecisionTool = "cortex_answer_decision"
	gatewayTool        = "mcphub_call_tool"

	maxAdviceSummaryBytes = 4 * 1024
	maxAdviceTextBytes    = 1024
	maxAdviceItems        = 16
)

// Registry is the narrow MCP capability Cortex needs. ResolveToolName keeps
// direct Cortex and lazy MCPHub configurations interchangeable.
type Registry interface {
	ResolveToolName(remoteName string) (string, bool)
	CallTool(ctx context.Context, name string, args map[string]any) (*mcp.ToolResult, error)
}

// Cortex is a stateless adapter. sonar owns scheduling, cancellation,
// budgets and approvals; Cortex only returns durable semantic state.
type Cortex struct {
	registry    Registry
	workspace   string
	actor       string
	interpreter ContinuationInterpreter
	revision    func(context.Context, string) (WorkspaceRevision, error)
}

// ContinuationInterpreter is the exact host boundary for Cortex actions. The
// returned value is opaque and can only be consumed by the same Agent that
// created it.
type ContinuationInterpreter interface {
	InterpretContinuationResult(llm.ToolCall, *mcp.ToolResult) *agent.ContinuationContext
}

func NewCortex(registry Registry, workspace, actor string, interpreters ...ContinuationInterpreter) *Cortex {
	cortex := &Cortex{
		registry:  registry,
		workspace: strings.TrimSpace(workspace),
		actor:     strings.TrimSpace(actor),
		revision:  CurrentWorkspaceRevision,
	}
	if len(interpreters) > 0 {
		cortex.interpreter = interpreters[0]
	}
	return cortex
}

// OpenRequest is the explicit user-approved creation/link request. GoalID is
// reused as Cortex's idempotency key, so response loss is safe to retry.
type OpenRequest struct {
	GoalID             string
	Objective          string
	AcceptanceCriteria []goal.AcceptanceCriterion
}

// cortexAcceptanceCriterion is Cortex's immutable named-claim contract. Keep
// this adapter type separate from goal.AcceptanceCriterion so neither side can
// silently reinterpret the other's wire field names.
type cortexAcceptanceCriterion struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// Advice is the bounded portion of Cortex status sonar needs to decide
// whether to continue, pause, block, or complete.
type Advice struct {
	OK                  bool
	TaskID              string
	Revision            int64
	Phase               string
	Summary             string
	VerificationOutcome string
	VerificationDone    []string
	MissingVerification []string
	StaleVerification   []string
	CriterionEvidence   map[string]CriterionProof
	ProofRevision       WorkspaceRevision
	PendingDecision     bool
	Decision            *PendingDecision
	Degraded            bool
	Warnings            []string
	Continuation        *agent.ContinuationContext `json:"-"`
}

// CriterionProof is the exact Cortex named-claim receipt bound to one local
// acceptance ID and one verified workspace state.
type CriterionProof struct {
	Claim       string
	Evidence    []string
	Revision    string
	DirtyDigest string
}

// Open idempotently creates or resumes the Cortex case for a local goal.
func (c *Cortex) Open(ctx context.Context, request OpenRequest) (Advice, error) {
	if c == nil || c.registry == nil {
		return Advice{}, ErrUnavailable
	}
	if strings.TrimSpace(request.GoalID) == "" || strings.TrimSpace(request.Objective) == "" {
		return Advice{}, fmt.Errorf("%w: goal id and objective are required", ErrRejected)
	}
	criteria, err := cortexAcceptanceCriteria(request.AcceptanceCriteria)
	if err != nil {
		return Advice{}, err
	}
	advice, err := c.call(ctx, openTool, map[string]any{
		"goal":               strings.TrimSpace(request.Objective),
		"acceptanceCriteria": criteria,
		"workspace":          c.workspace,
		"mode":               "change",
		"surfaces":           []string{"code", "terminal"},
		"risk":               "medium",
		"actor":              defaultActor(c.actor),
		"idempotencyKey":     strings.TrimSpace(request.GoalID),
	})
	if err == nil {
		if strings.TrimSpace(advice.TaskID) == "" {
			return advice, fmt.Errorf("%w: Cortex open returned no task id", ErrRejected)
		}
		if !validCortexPhase(advice.Phase) || advice.Degraded {
			return advice, fmt.Errorf("%w: Cortex open returned an unknown or degraded phase", ErrRejected)
		}
	}
	return advice, err
}

// Status reads the current Cortex case. It is the only automatic host call.
// Downstream actions stay behind the parser boundary because this adapter does
// not own the current tool schemas or approval policy needed to validate them.
func (c *Cortex) Status(ctx context.Context, taskID string) (Advice, error) {
	if c == nil || c.registry == nil {
		return Advice{}, ErrUnavailable
	}
	if strings.TrimSpace(taskID) == "" {
		return Advice{}, fmt.Errorf("%w: cortex task id is required", ErrRejected)
	}
	advice, err := c.call(ctx, statusTool, map[string]any{
		"taskId":    taskID,
		"detail":    "standard",
		"workspace": c.workspace,
	})
	if err != nil {
		return advice, err
	}
	if advice.TaskID != strings.TrimSpace(taskID) || !validCortexPhase(advice.Phase) {
		return advice, fmt.Errorf("%w: Cortex status identity or phase is invalid", ErrRejected)
	}
	if advice.Degraded {
		return advice, fmt.Errorf("%w: Cortex status is degraded", ErrRejected)
	}
	if err := validateDecisionPhase(advice); err != nil {
		return advice, fmt.Errorf("%w: Cortex status decision state is inconsistent: %v", ErrRejected, err)
	}
	if !strings.EqualFold(advice.Phase, "complete") || !strings.EqualFold(advice.VerificationOutcome, "verified") {
		return advice, nil
	}
	evidence, proofRevision, evidenceErr := c.criterionEvidence(ctx, taskID, advice.Revision)
	if evidenceErr != nil {
		advice.Warnings = boundAdviceStrings(append(advice.Warnings, "exact acceptance evidence unavailable: "+evidenceErr.Error()))
		return advice, nil
	}
	advice.CriterionEvidence = evidence
	advice.ProofRevision = proofRevision
	return advice, nil
}

// AnswerDecision records one exact human-selected option. Cortex may leave its
// paused semantic phase when it accepts the answer, but this adapter never
// resumes sonar's Goal Runtime or dispatches provider work; the host must
// authorize those transitions separately.
func (c *Cortex) AnswerDecision(ctx context.Context, request AnswerDecisionRequest) (Advice, error) {
	if c == nil || c.registry == nil {
		return Advice{}, ErrUnavailable
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "task id", value: request.TaskID, limit: goal.MaxCorrelationIDBytes},
		{name: "decision id", value: request.DecisionID, limit: maxDecisionStableIDBytes},
		{name: "selected option id", value: request.OptionID, limit: maxDecisionStableIDBytes},
		{name: "responder", value: request.Responder, limit: maxDecisionRequesterBytes},
	} {
		if err := validateExactDecisionText(field.name, field.value, field.limit); err != nil {
			return Advice{}, fmt.Errorf("%w: invalid %s", ErrRejected, field.name)
		}
	}
	if ctx == nil {
		return Advice{}, fmt.Errorf("%w: context is nil", ErrRejected)
	}
	if err := ctx.Err(); err != nil {
		return Advice{}, fmt.Errorf("%s: %w", answerDecisionTool, err)
	}

	advice, err := c.call(ctx, answerDecisionTool, map[string]any{
		"taskId":     request.TaskID,
		"workspace":  c.workspace,
		"decisionId": request.DecisionID,
		"answer":     request.OptionID,
		"responder":  request.Responder,
	})
	if err != nil {
		return advice, err
	}
	if advice.TaskID != request.TaskID || !validCortexPhase(advice.Phase) {
		return advice, fmt.Errorf("%w: Cortex answer identity or phase is invalid", ErrRejected)
	}
	if advice.Degraded {
		return advice, fmt.Errorf("%w: Cortex answer response is degraded", ErrRejected)
	}
	if err := validateDecisionPhase(advice); err != nil {
		return advice, fmt.Errorf("%w: Cortex answer decision state is inconsistent: %v", ErrRejected, err)
	}
	if advice.PendingDecision || advice.Decision != nil {
		return advice, fmt.Errorf("%w: Cortex answer did not settle the pending decision", ErrRejected)
	}
	return advice, nil
}

func validateDecisionPhase(advice Advice) error {
	hasDecision := advice.Decision != nil
	if advice.PendingDecision != hasDecision {
		return errors.New("pending decision marker does not match typed decision")
	}
	wantsDecision := strings.EqualFold(strings.TrimSpace(advice.Phase), "needs_human_decision")
	if wantsDecision != hasDecision {
		return errors.New("phase does not match typed pending decision")
	}
	return nil
}

func validCortexPhase(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "new", "orienting", "investigating", "planned", "changing", "verifying", "persisting",
		"complete", "blocked", "abandoned", "needs_human_decision":
		return true
	default:
		return false
	}
}

func defaultActor(actor string) string {
	if actor == "" {
		return "sonar"
	}
	return actor
}

func cortexAcceptanceCriteria(criteria []goal.AcceptanceCriterion) ([]cortexAcceptanceCriterion, error) {
	if len(criteria) == 0 || len(criteria) > goal.MaxCriteria {
		return nil, fmt.Errorf("%w: acceptance criteria count must be between 1 and %d", ErrRejected, goal.MaxCriteria)
	}
	result := make([]cortexAcceptanceCriterion, 0, len(criteria))
	seen := make(map[string]struct{}, len(criteria))
	for _, criterion := range criteria {
		id := strings.TrimSpace(criterion.ID)
		statement := strings.TrimSpace(criterion.Description)
		if id == "" || !utf8.ValidString(id) || len(id) > goal.MaxCriterionIDBytes {
			return nil, fmt.Errorf("%w: acceptance criterion id is empty, invalid UTF-8, or too long", ErrRejected)
		}
		if statement == "" || !utf8.ValidString(statement) || len(statement) > goal.MaxCriterionBytes {
			return nil, fmt.Errorf("%w: acceptance criterion %q is empty, invalid UTF-8, or too long", ErrRejected, id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate acceptance criterion id %q", ErrRejected, id)
		}
		seen[id] = struct{}{}
		result = append(result, cortexAcceptanceCriterion{ID: id, Statement: statement})
	}
	return result, nil
}

func (c *Cortex) call(ctx context.Context, tool string, args map[string]any) (Advice, error) {
	if ctx == nil {
		return Advice{}, fmt.Errorf("%w: context is nil", ErrRejected)
	}
	exposed, direct := c.registry.ResolveToolName(tool)
	callArgs := args
	if !direct {
		var gateway bool
		exposed, gateway = c.registry.ResolveToolName(gatewayTool)
		if !gateway {
			return Advice{}, fmt.Errorf("%w: neither %s nor the MCPHub gateway is connected", ErrUnavailable, tool)
		}
		callArgs = map[string]any{
			"server":    "cortex",
			"tool":      tool,
			"arguments": args,
		}
	}

	result, err := c.registry.CallTool(ctx, exposed, callArgs)
	if err != nil {
		return Advice{}, fmt.Errorf("%s: %w", tool, err)
	}
	if result == nil {
		return Advice{}, fmt.Errorf("%w: %s returned no receipt", ErrUnavailable, tool)
	}
	var continuation *agent.ContinuationContext
	if c.interpreter != nil {
		continuation = c.interpreter.InterpretContinuationResult(llm.ToolCall{Name: exposed, Arguments: callArgs}, result)
	}
	document, documentErr := toolResultDocument(result)
	if documentErr != nil {
		return Advice{}, fmt.Errorf("%s response: %w", tool, documentErr)
	}
	advice, parseErr := parseAdvice(document)
	if parseErr != nil {
		return Advice{}, fmt.Errorf("%w: %s response is invalid: %v", ErrRejected, tool, parseErr)
	}
	advice.Continuation = continuation
	if result.IsError || !advice.OK {
		detail := advice.Summary
		if detail == "" {
			detail = "Cortex rejected the request"
		}
		return advice, fmt.Errorf("%w: %s", ErrRejected, detail)
	}
	return advice, nil
}

type adviceEnvelope struct {
	OK                  bool            `json:"ok"`
	TaskID              string          `json:"taskId"`
	Revision            int64           `json:"revision"`
	Phase               string          `json:"phase"`
	Summary             string          `json:"summary"`
	Error               string          `json:"error"`
	VerificationOutcome string          `json:"verificationOutcome"`
	VerificationDone    []string        `json:"verificationDone"`
	MissingVerification []string        `json:"missingVerification"`
	StaleVerification   []string        `json:"staleVerification"`
	PendingDecision     json.RawMessage `json:"pendingDecision"`
	Degraded            bool            `json:"degraded"`
	Warnings            []string        `json:"warnings"`
}

func parseAdvice(content string) (Advice, error) {
	content = primaryJSONContent(content)
	if !utf8.ValidString(content) {
		return Advice{}, fmt.Errorf("decode JSON envelope: invalid UTF-8")
	}
	var envelope adviceEnvelope
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		return Advice{}, fmt.Errorf("decode JSON envelope: %w", err)
	}
	if envelope.Revision < 0 {
		return Advice{}, fmt.Errorf("revision must not be negative")
	}
	taskID := strings.TrimSpace(envelope.TaskID)
	if taskID != envelope.TaskID || len(taskID) > goal.MaxCorrelationIDBytes {
		return Advice{}, fmt.Errorf("task id is not an exact bounded identity")
	}
	summary := strings.TrimSpace(envelope.Summary)
	if summary == "" {
		summary = strings.TrimSpace(envelope.Error)
	}
	decision, err := parsePendingDecision(envelope.PendingDecision)
	if err != nil {
		return Advice{}, err
	}
	return Advice{
		OK:                  envelope.OK,
		TaskID:              taskID,
		Revision:            envelope.Revision,
		Phase:               boundAdviceText(strings.TrimSpace(envelope.Phase), maxAdviceTextBytes),
		Summary:             boundAdviceText(summary, maxAdviceSummaryBytes),
		VerificationOutcome: boundAdviceText(strings.TrimSpace(envelope.VerificationOutcome), maxAdviceTextBytes),
		VerificationDone:    boundAdviceStrings(envelope.VerificationDone),
		MissingVerification: boundAdviceStrings(envelope.MissingVerification),
		StaleVerification:   boundAdviceStrings(envelope.StaleVerification),
		PendingDecision:     decision != nil,
		Decision:            decision,
		Degraded:            envelope.Degraded,
		Warnings:            boundAdviceStrings(envelope.Warnings),
	}, nil
}

type handoffEnvelope struct {
	TaskID       string `json:"taskId"`
	Revision     int64  `json:"revision"`
	Phase        string `json:"phase"`
	Verification struct {
		Outcome string `json:"outcome"`
	} `json:"verification"`
	Receipts []handoffReceipt `json:"receipts"`
}

type handoffReceipt struct {
	ID          string   `json:"id"`
	BatchID     string   `json:"batchId"`
	ClaimID     string   `json:"claimId"`
	Claim       string   `json:"claim"`
	Purpose     string   `json:"purpose"`
	Status      string   `json:"status"`
	Binding     string   `json:"binding"`
	Evidence    []string `json:"evidence"`
	Artifact    string   `json:"artifact"`
	Revision    string   `json:"revision"`
	DirtyDigest string   `json:"dirtyDigest"`
}

func (c *Cortex) criterionEvidence(ctx context.Context, taskID string, caseRevision int64) (map[string]CriterionProof, WorkspaceRevision, error) {
	content, err := c.callRaw(ctx, handoffTool, map[string]any{
		"taskId":    taskID,
		"workspace": c.workspace,
	})
	if err != nil {
		return nil, WorkspaceRevision{}, err
	}
	var handoff handoffEnvelope
	if err := json.Unmarshal([]byte(primaryJSONContent(content)), &handoff); err != nil {
		return nil, WorkspaceRevision{}, fmt.Errorf("decode handoff: %w", err)
	}
	if strings.TrimSpace(handoff.TaskID) != strings.TrimSpace(taskID) || handoff.Revision != caseRevision ||
		!strings.EqualFold(handoff.Phase, "complete") || !strings.EqualFold(handoff.Verification.Outcome, "verified") {
		return nil, WorkspaceRevision{}, fmt.Errorf("handoff identity, revision, phase, or verification does not match status")
	}
	revisionFn := c.revision
	if revisionFn == nil {
		revisionFn = CurrentWorkspaceRevision
	}
	current, err := revisionFn(ctx, c.workspace)
	if err != nil {
		return nil, WorkspaceRevision{}, fmt.Errorf("read current workspace revision: %w", err)
	}

	batchEvidence := make(map[string][]string)
	for _, receipt := range handoff.Receipts {
		if receipt.Purpose != "verifier_run" || strings.TrimSpace(receipt.BatchID) == "" ||
			!strings.EqualFold(receipt.Status, "passed") || receipt.Binding != "bound" || !receiptMatchesWorkspace(receipt, current) {
			continue
		}
		refs := receiptEvidenceRefs(taskID, receipt)
		if len(refs) > 0 {
			batchEvidence[receipt.BatchID] = appendUniqueAdviceRefs(batchEvidence[receipt.BatchID], refs...)
		}
	}

	result := make(map[string]CriterionProof)
	conflicts := make(map[string]struct{})
	for _, receipt := range handoff.Receipts {
		claimID := strings.TrimSpace(receipt.ClaimID)
		claim := strings.TrimSpace(receipt.Claim)
		if _, conflicted := conflicts[claimID]; conflicted {
			continue
		}
		batchRefs := batchEvidence[receipt.BatchID]
		if receipt.Purpose != "named_claim" || claimID == "" || len(claimID) > goal.MaxCriterionIDBytes ||
			claim == "" || len(claim) > goal.MaxCriterionBytes || len(batchRefs) == 0 ||
			!strings.EqualFold(receipt.Status, "passed") || receipt.Binding != "bound" || !receiptMatchesWorkspace(receipt, current) {
			continue
		}
		refs := receiptEvidenceRefs(taskID, receipt)
		refs = appendUniqueAdviceRefs(refs, batchRefs...)
		if len(refs) > 0 {
			proof := CriterionProof{
				Claim:       claim,
				Evidence:    refs,
				Revision:    receipt.Revision,
				DirtyDigest: receipt.DirtyDigest,
			}
			if previous, exists := result[claimID]; exists && !reflect.DeepEqual(previous, proof) {
				delete(result, claimID)
				conflicts[claimID] = struct{}{}
				continue
			}
			result[claimID] = proof
		}
	}
	return result, current, nil
}

func receiptMatchesWorkspace(receipt handoffReceipt, current WorkspaceRevision) bool {
	return current.Valid() && receipt.Revision == current.Commit && receipt.DirtyDigest == current.DirtyDigest
}

func receiptEvidenceRefs(taskID string, receipt handoffReceipt) []string {
	refs := make([]string, 0, len(receipt.Evidence)+2)
	if id := boundAdviceText(strings.TrimSpace(receipt.ID), maxAdviceTextBytes); id != "" {
		refs = append(refs, "case://"+strings.TrimSpace(taskID)+"/verification/"+id)
	}
	for _, ref := range receipt.Evidence {
		if ref = strings.TrimSpace(ref); ref != "" {
			refs = appendUniqueAdviceRefs(refs, boundAdviceText(ref, maxAdviceTextBytes))
		}
	}
	if artifact := strings.TrimSpace(receipt.Artifact); artifact != "" {
		refs = appendUniqueAdviceRefs(refs, boundAdviceText(artifact, maxAdviceTextBytes))
	}
	return refs
}

func appendUniqueAdviceRefs(destination []string, values ...string) []string {
	seen := make(map[string]struct{}, len(destination)+len(values))
	for _, value := range destination {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		destination = append(destination, value)
		if len(destination) >= maxAdviceItems {
			break
		}
	}
	return destination
}

func (c *Cortex) callRaw(ctx context.Context, tool string, args map[string]any) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("%w: context is nil", ErrRejected)
	}
	exposed, direct := c.registry.ResolveToolName(tool)
	callArgs := args
	if !direct {
		var gateway bool
		exposed, gateway = c.registry.ResolveToolName(gatewayTool)
		if !gateway {
			return "", fmt.Errorf("%w: neither %s nor the MCPHub gateway is connected", ErrUnavailable, tool)
		}
		callArgs = map[string]any{"server": "cortex", "tool": tool, "arguments": args}
	}
	result, err := c.registry.CallTool(ctx, exposed, callArgs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	if result == nil {
		return "", fmt.Errorf("%w: %s returned no receipt", ErrUnavailable, tool)
	}
	document, documentErr := toolResultDocument(result)
	if documentErr != nil {
		return "", fmt.Errorf("%s response: %w", tool, documentErr)
	}
	if result.IsError {
		return "", fmt.Errorf("%w: %s", ErrRejected, boundAdviceText(strings.TrimSpace(document), maxAdviceSummaryBytes))
	}
	return document, nil
}

func toolResultDocument(result *mcp.ToolResult) (string, error) {
	if result == nil {
		return "", errors.New("missing tool result")
	}
	if structured := strings.TrimSpace(string(result.Structured)); structured != "" && structured != "null" {
		if !json.Valid([]byte(structured)) {
			return "", errors.New("structuredContent is not valid JSON")
		}
		return structured, nil
	}
	content := primaryJSONContent(result.Content)
	if content == "" {
		return "", errors.New("tool result contains no JSON document")
	}
	return content, nil
}

func primaryJSONContent(content string) string {
	content = strings.TrimSpace(content)
	if marker := strings.Index(content, "\nstructured: "); marker >= 0 {
		return strings.TrimSpace(content[:marker])
	}
	if strings.HasPrefix(content, "structured: ") {
		return strings.TrimSpace(strings.TrimPrefix(content, "structured: "))
	}
	return content
}

func boundAdviceStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maxAdviceItems {
		values = values[:maxAdviceItems]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = boundAdviceText(strings.TrimSpace(value), maxAdviceTextBytes); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func boundAdviceText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}
