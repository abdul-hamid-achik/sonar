package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

const goalCommandLimit = 100

const (
	goalRecoveryActor           = "local-user"
	goalRecoveryNoResumeWarning = "Recovery records evidence only. It never resumes execution; resume the paused goal separately after review."
)

type goalSessionStore interface {
	ListSessions(context.Context, db.ListSessionsParams) ([]db.Session, error)
	GetSession(context.Context, int64) (db.Session, error)
	GetSessionState(context.Context, int64) (string, error)
	ResolveSessionRef(context.Context, string) (db.Session, error)
	SessionHandle(context.Context, int64) (string, error)
}

type goalControlStore interface {
	goalSessionStore
	ListControlStates(context.Context, controlplane.Query) ([]controlplane.State, error)
}

type goalRecoveryStore interface {
	GetSessionStateRecord(context.Context, int64) (db.SessionStateRecord, error)
	InspectReconciliationGroup(context.Context, int64, string) (db.ReconciliationGroupInspection, error)
	AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error)
	EnsureReconciliationGroup(context.Context, *db.ExecutionSessionLease, db.EnsureReconciliationGroupRequest) (db.ReconciliationGroup, bool, error)
	ResolveExecutionReconciliation(context.Context, *db.ExecutionSessionLease, db.ResolveExecutionReconciliationRequest) (db.ReconciliationCommitReceipt, error)
	ResolveReconciliationParent(context.Context, *db.ExecutionSessionLease, db.ResolveReconciliationParentRequest) (db.ReconciliationCommitReceipt, error)
	ResolveSessionRef(context.Context, string) (db.Session, error)
	SessionHandle(context.Context, int64) (string, error)
}

// goalSummary is the stable read model printed by `sonar goal list`.
// The full validated snapshot remains available to the show command.
type goalSummary struct {
	SessionID       int64         `json:"session_id"`
	PublicID        string        `json:"-"`
	GoalID          string        `json:"goal_id"`
	State           goal.State    `json:"state"`
	Objective       string        `json:"objective"`
	CortexTaskID    string        `json:"cortex_task_id,omitempty"`
	CortexRevision  int64         `json:"cortex_revision,omitempty"`
	ContinuationUse int64         `json:"continuation_turns"`
	EvalTokenUse    int64         `json:"eval_tokens"`
	UpdatedAt       time.Time     `json:"goal_updated_at"`
	SessionUpdated  string        `json:"session_updated_at"`
	Snapshot        goal.Snapshot `json:"-"`
}

type goalSessionEnvelope struct {
	Goal *goal.Snapshot `json:"goal"`
}

// pendingControlSummary is the least-privilege CLI projection. Payload and
// evidence envelopes remain in the private durable store; inspection commands
// expose only the identity and explanation needed to choose a next action.
type pendingControlSummary struct {
	SessionID   int64             `json:"session_id"`
	ItemID      string            `json:"item_id"`
	Kind        controlplane.Kind `json:"kind"`
	GoalID      string            `json:"goal_id,omitempty"`
	ExecutionID string            `json:"execution_id,omitempty"`
	TurnID      string            `json:"turn_id,omitempty"`
	Summary     string            `json:"summary"`
	CreatedAt   time.Time         `json:"created_at"`
}

type goalRecoveryMemberSummary struct {
	ItemID      string `json:"item_id"`
	ExecutionID string `json:"execution_id"`
	EventID     int64  `json:"event_id"`
	EventType   string `json:"event_type"`
	EffectClass string `json:"effect_class"`
	Resolved    bool   `json:"resolved"`
}

type goalRecoveryParentSummary struct {
	ItemID         string `json:"item_id"`
	Required       bool   `json:"required"`
	Resolved       bool   `json:"resolved"`
	Ready          bool   `json:"ready"`
	BlockedByCount int    `json:"blocked_by_count"`
}

// goalRecoveryDryRun is deliberately redacted. Never encode the repository
// inspection directly: it contains canonical payload and evidence envelopes.
type goalRecoveryDryRun struct {
	DryRun                   bool                        `json:"dry_run"`
	SessionID                int64                       `json:"session_id"`
	SessionHandle            string                      `json:"-"`
	SessionRevision          int64                       `json:"session_revision"`
	GoalID                   string                      `json:"goal_id"`
	GoalState                goal.State                  `json:"goal_state"`
	GroupItemID              string                      `json:"group_item_id"`
	TurnID                   string                      `json:"turn_id"`
	BlockerReference         string                      `json:"blocker_reference"`
	SnapshotCursor           int64                       `json:"snapshot_cursor"`
	GroupPayloadSHA256       string                      `json:"group_payload_sha256"`
	MemberSetSHA256          string                      `json:"member_set_sha256"`
	ExecutionMemberCount     int                         `json:"execution_member_count"`
	Members                  []goalRecoveryMemberSummary `json:"members"`
	UnresolvedExecutionItems []string                    `json:"unresolved_execution_items"`
	Parent                   goalRecoveryParentSummary   `json:"parent"`
	NoResumeWarning          string                      `json:"no_resume_warning"`
}

type goalRecoveryApplyResult struct {
	Applied             bool       `json:"applied"`
	Inserted            bool       `json:"inserted"`
	SessionID           int64      `json:"session_id"`
	SessionHandle       string     `json:"-"`
	SessionRevision     int64      `json:"session_revision"`
	GroupItemID         string     `json:"group_item_id"`
	ItemID              string     `json:"item_id"`
	ResolutionID        string     `json:"resolution_id"`
	GoalState           goal.State `json:"goal_state"`
	GoalCleared         bool       `json:"goal_cleared"`
	RemainingExecutions int        `json:"remaining_executions"`
	ParentPending       bool       `json:"parent_pending"`
	ExecutionCursor     int64      `json:"execution_cursor"`
	NoResumeWarning     string     `json:"no_resume_warning"`
}

type goalOpenResult struct {
	SessionID int64         `json:"session_id"`
	Workspace string        `json:"workspace"`
	Goal      goal.Snapshot `json:"goal"`
}

type goalRunInvocation struct {
	SessionID int64
	Prompt    string
}

type goalRunOptions struct {
	SessionPublicID string
	SessionID       int64
	Prompt          string
	SkipApprovals   bool
	Model           string
	AgentProfile    string
}

var activeGoalRun *goalRunInvocation

func handleGoalCommand(args []string) int {
	return handleGoalCommandIO(args, os.Stdout, os.Stderr)
}

func handleGoalCommandIO(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeGoalUsage(stdout)
		return 0
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			goalFprintf(stderr, "goal: unexpected argument %q after %s\n", args[1], args[0])
			return 2
		}
		writeGoalUsage(stdout)
		return 0
	}
	command := args[0]
	switch command {
	case "list", "show", "pending", "recover", "open", "run":
	default:
		goalFprintf(stderr, "goal: unknown command %q\n", command)
		writeGoalUsage(stderr)
		return 2
	}
	if hasHelpFlag(args[1:]) {
		helpArgs := []string{"--help"}
		switch command {
		case "list":
			return handleGoalList(nil, "", helpArgs, stdout, stderr)
		case "show":
			return handleGoalShow(nil, "", helpArgs, stdout, stderr)
		case "pending":
			return handleGoalPending(nil, "", helpArgs, stdout, stderr)
		case "recover":
			return handleGoalRecover(nil, "", helpArgs, stdout, stderr)
		case "open":
			return handleGoalOpen(nil, "", helpArgs, stdout, stderr)
		case "run":
			return handleGoalRun(args[1:], stdout, stderr)
		}
	}
	if command == "run" {
		return handleGoalRun(args[1:], stdout, stderr)
	}
	workspace := currentWorkspace()
	if workspace == "" {
		goalFprintln(stderr, "goal: workspace identity is unavailable")
		return 1
	}
	store, err := db.Open()
	if err != nil {
		goalFprintf(stderr, "goal: open durable store: %v\n", err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			goalFprintf(stderr, "goal: close durable store: %v\n", err)
		}
	}()

	switch command {
	case "open":
		return handleGoalOpen(store, workspace, args[1:], stdout, stderr)
	case "list":
		return handleGoalList(store, workspace, args[1:], stdout, stderr)
	case "show":
		return handleGoalShow(store, workspace, args[1:], stdout, stderr)
	case "pending":
		return handleGoalPending(store, workspace, args[1:], stdout, stderr)
	case "recover":
		return handleGoalRecover(store, workspace, args[1:], stdout, stderr)
	}
	return 2
}

func handleGoalRun(args []string, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		writeGoalRunUsage(stdout)
		return 0
	}
	options, code := parseGoalRunArgs(args, stdout, stderr)
	if code != 0 {
		return code
	}
	store, err := db.Open()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal run: open durable store: %v\n", err)
		return 1
	}
	session, err := resolveSessionArg(context.Background(), store, options.SessionPublicID)
	closeErr := store.Close()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal run: %v\n", err)
		return 1
	}
	if closeErr != nil {
		_, _ = fmt.Fprintf(stderr, "goal run: close durable store: %v\n", closeErr)
		return 1
	}
	options.SessionID = session.ID
	rootArgs := []string{os.Args[0], "--prompt", options.Prompt, "--mode", "auto"}
	if options.SkipApprovals {
		rootArgs = append(rootArgs, "--skip-approvals")
	}
	if options.Model != "" {
		rootArgs = append(rootArgs, "--model", options.Model)
	}
	if options.AgentProfile != "" {
		rootArgs = append(rootArgs, "--agent", options.AgentProfile)
	}
	originalArgs, originalInvocation := os.Args, activeGoalRun
	os.Args = rootArgs
	activeGoalRun = &goalRunInvocation{SessionID: options.SessionID, Prompt: options.Prompt}
	defer func() {
		os.Args = originalArgs
		activeGoalRun = originalInvocation
	}()
	return run()
}

func parseGoalRunArgs(args []string, stdout, stderr io.Writer) (goalRunOptions, int) {
	normalized, err := reorderFlagsBeforePositionals(args, map[string]bool{
		"prompt": true,
		"model":  true,
		"agent":  true,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal run: %v\n", err)
		return goalRunOptions{}, 2
	}
	flags := flag.NewFlagSet("goal run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalRunUsage(stdout) }
	prompt := flags.String("prompt", "", "instruction for this goal turn")
	skipApprovals := flags.Bool("skip-approvals", false, "skip approval prompts")
	model := flags.String("model", "", "override the Ollama model")
	agentProfile := flags.String("agent", "", "override the agent profile")
	if code, done := flagParseExitCode(flags.Parse(normalized)); done {
		return goalRunOptions{}, code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "goal run: provide exactly one session ID")
		return goalRunOptions{}, 2
	}
	publicID, err := sessionref.Parse(flags.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal run: invalid session reference %q\n", flags.Arg(0))
		return goalRunOptions{}, 2
	}
	promptText := strings.TrimSpace(*prompt)
	if promptText == "" {
		_, _ = fmt.Fprintln(stderr, "goal run: --prompt is required")
		return goalRunOptions{}, 2
	}
	return goalRunOptions{
		SessionPublicID: publicID, Prompt: promptText, SkipApprovals: *skipApprovals,
		Model: strings.TrimSpace(*model), AgentProfile: strings.TrimSpace(*agentProfile),
	}, 0
}

func handleGoalOpen(store *db.Store, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goal open", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalOpenUsage(stdout) }
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	objective := flags.String("objective", "", "bounded goal objective")
	criterion := flags.String("criterion", "", "acceptance criterion (defaults to the objective)")
	maxTurns := flags.Int64("max-continuation-turns", goal.DefaultContinuationTurns, "maximum automatic continuation turns")
	maxTokens := flags.Int64("max-eval-tokens", goal.DefaultEvalTokens, "maximum evaluation tokens")
	maxWall := flags.Duration("max-wall-time", goal.DefaultWallTime, "maximum wall time")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if store == nil {
		_, _ = fmt.Fprintln(stderr, "goal open: durable store is unavailable")
		return 1
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "goal open: use --objective and no positional arguments")
		return 2
	}
	objectiveText := strings.TrimSpace(*objective)
	if objectiveText == "" {
		_, _ = fmt.Fprintln(stderr, "goal open: --objective is required")
		return 2
	}
	criterionText := strings.TrimSpace(*criterion)
	if criterionText == "" {
		criterionText = objectiveText
	}
	if *maxTurns <= 0 || *maxTokens <= 0 {
		_, _ = fmt.Fprintln(stderr, "goal open: budgets must be positive")
		return 2
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: headlessSessionTitle(objectiveText), Model: "headless-goal", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal open: create session: %v\n", err)
		return 1
	}
	runtime, err := goal.New(goal.Spec{
		SessionID: session.ID, Objective: objectiveText,
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "criterion_1", Description: criterionText}},
		Budget: goal.BudgetLimits{
			MaxContinuationTurns: *maxTurns,
			MaxEvalTokens:        *maxTokens,
			MaxWallTime:          *maxWall,
		},
	})
	if err != nil {
		_ = store.DeleteSession(context.Background(), session.ID)
		_, _ = fmt.Fprintf(stderr, "goal open: create runtime: %v\n", err)
		return 1
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		_ = store.DeleteSession(context.Background(), session.ID)
		_, _ = fmt.Fprintf(stderr, "goal open: snapshot runtime: %v\n", err)
		return 1
	}
	state, err := json.Marshal(map[string]any{"version": 2, "execution_cursor": 0, "goal": snapshot})
	if err != nil {
		_ = store.DeleteSession(context.Background(), session.ID)
		_, _ = fmt.Fprintf(stderr, "goal open: encode state: %v\n", err)
		return 1
	}
	if err := store.SaveSessionState(context.Background(), session.ID, string(state)); err != nil {
		_ = store.DeleteSession(context.Background(), session.ID)
		_, _ = fmt.Fprintf(stderr, "goal open: persist state: %v\n", err)
		return 1
	}
	result := goalOpenResult{SessionID: session.ID, Workspace: workspace, Goal: snapshot}
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal open: %v\n", err)
			return 1
		}
		return 0
	}
	handle := sessionDisplayHandle(session)
	goalFprintf(stdout, "Opened goal %s in session %s.\n", terminalSafeGoalText(snapshot.ID), handle)
	goalFprintf(stdout, "Inspect it with: sonar goal show --json %s\n", handle)
	return 0
}

func handleGoalPending(store goalControlStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goal pending", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalPendingUsage(stdout) }
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	limit := flags.Int("limit", 20, "maximum pending items to print (1-100)")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "goal pending: provide exactly one session ID from `sonar goal list`")
		return 2
	}
	session, err := resolveSessionArg(context.Background(), store, flags.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal pending: %v\n", err)
		return 2
	}
	sessionID := session.ID
	if *limit <= 0 || *limit > controlplane.MaxListLimit {
		_, _ = fmt.Fprintf(stderr, "goal pending: limit must be between 1 and %d\n", controlplane.MaxListLimit)
		return 2
	}
	if _, err := getGoalSummary(context.Background(), store, workspace, sessionID); err != nil {
		_, _ = fmt.Fprintf(stderr, "goal pending: %v\n", err)
		return 1
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: sessionID, WorkspaceID: workspace, PendingOnly: true, Limit: *limit,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal pending: list durable control items: %v\n", err)
		return 1
	}
	pending, err := projectPendingControlItems(states, sessionID, workspace)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal pending: validate durable control items: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, pending); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal pending: %v\n", err)
			return 1
		}
		return 0
	}
	writePendingControlItems(stdout, pending)
	return 0
}

func handleGoalRecover(store goalRecoveryStore, workspace string, args []string, stdout, stderr io.Writer) int {
	normalized, err := normalizeGoalRecoverArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: %v\n", err)
		return 2
	}
	flags := flag.NewFlagSet("goal recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalRecoverUsage(stdout) }
	apply := flags.Bool("apply", false, "persist the exact typed evidence")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	itemID := flags.String("item", "", "exact execution-member or parent item ID")
	observation := flags.String("observation", "", "effect_applied, effect_not_applied, effect_compensated, or turn_abandoned_after_inspection")
	source := flags.String("source", "", "external_receipt, workspace_artifact, verification_check, or operator_observation")
	reference := flags.String("reference", "", "redacted evidence reference")
	summary := flags.String("summary", "", "bounded inspection summary")
	observedAtText := flags.String("observed-at", "", "evidence observation time in RFC3339")
	if code, done := flagParseExitCode(flags.Parse(normalized)); done {
		return code
	}
	if store == nil {
		_, _ = fmt.Fprintln(stderr, "goal recover: durable store is unavailable")
		return 1
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "goal recover: provide exactly one session ID")
		return 2
	}
	provided := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { provided[value.Name] = true })
	// Validate flag shape before touching the durable session ledger so users
	// get actionable --apply errors even when the session handle is unknown.
	if !*apply {
		for name := range provided {
			if name != "json" && name != "apply" {
				_, _ = fmt.Fprintf(stderr, "goal recover: --%s requires --apply\n", name)
				return 2
			}
		}
	} else {
		required := map[string]string{
			"item": *itemID, "observation": *observation, "source": *source,
			"reference": *reference, "summary": *summary, "observed-at": *observedAtText,
		}
		for _, name := range []string{"item", "observation", "source", "reference", "summary", "observed-at"} {
			if !provided[name] || strings.TrimSpace(required[name]) == "" {
				_, _ = fmt.Fprintf(stderr, "goal recover: --apply requires non-empty --%s\n", name)
				return 2
			}
		}
	}
	session, err := resolveSessionArg(context.Background(), store, flags.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: %v\n", err)
		return 2
	}
	sessionID := session.ID
	sessionHandle := sessionDisplayHandle(session)
	if !*apply {
		inspection, err := store.InspectReconciliationGroup(context.Background(), sessionID, workspace)
		if err != nil {
			if errors.Is(err, db.ErrReconciliationGroupNotFound) {
				_, _ = fmt.Fprintln(stderr, "goal recover: no existing reconciliation group; dry-run never creates one")
			} else {
				_, _ = fmt.Fprintf(stderr, "goal recover: inspect durable recovery group: %v\n", err)
			}
			return 1
		}
		projection := projectGoalRecoveryDryRun(inspection)
		projection.SessionHandle = sessionHandle
		if *jsonOutput {
			if err := writeJSON(stdout, projection); err != nil {
				_, _ = fmt.Fprintf(stderr, "goal recover: %v\n", err)
				return 1
			}
		} else {
			writeGoalRecoveryDryRun(stdout, projection)
		}
		return 0
	}
	observedAt, err := time.Parse(time.RFC3339, *observedAtText)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: invalid --observed-at RFC3339 value: %v\n", err)
		return 2
	}
	sourceValue := reconciliation.Source{
		Kind: reconciliation.SourceKind(*source), Reference: *reference, ObservedAt: observedAt.UTC(),
	}
	if err := sourceValue.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: invalid evidence source: %v\n", err)
		return 2
	}
	var memberEvidence *reconciliation.Request
	var parentEvidence *reconciliation.TurnRequest
	if conclusion := reconciliation.TurnConclusion(*observation); conclusion.Valid() {
		request := reconciliation.TurnRequest{Conclusion: conclusion, Source: sourceValue, Summary: *summary}
		if err := request.Validate(); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: invalid parent evidence: %v\n", err)
			return 2
		}
		parentEvidence = &request
	} else if disposition := reconciliation.Disposition(*observation); disposition.Valid() {
		request := reconciliation.Request{Disposition: disposition, Source: sourceValue, Summary: *summary}
		if err := request.Validate(); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: invalid execution evidence: %v\n", err)
			return 2
		}
		memberEvidence = &request
	} else {
		_, _ = fmt.Fprintf(stderr, "goal recover: invalid --observation %q\n", *observation)
		return 2
	}

	ctx := context.Background()
	lease, err := store.AcquireExecutionSessionLease(ctx, sessionID, workspace)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: acquire exact session lease: %v\n", err)
		return 1
	}
	defer func() {
		if err := lease.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: release session lease: %v\n", err)
		}
	}()
	record, err := store.GetSessionStateRecord(ctx, sessionID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: load revisioned session: %v\n", err)
		return 1
	}
	inspection, inspectErr := store.InspectReconciliationGroup(ctx, sessionID, workspace)
	var group db.ReconciliationGroup
	switch {
	case inspectErr == nil:
		if inspection.SessionRevision != record.Revision {
			_, _ = fmt.Fprintf(stderr, "goal recover: %v: inspected revision %d differs from loaded revision %d\n", db.ErrSessionStateConflict, inspection.SessionRevision, record.Revision)
			return 1
		}
		group = inspection.Group
	case errors.Is(inspectErr, db.ErrReconciliationGroupNotFound):
		group, _, err = store.EnsureReconciliationGroup(ctx, lease, db.EnsureReconciliationGroupRequest{
			SessionID: sessionID, WorkspaceID: workspace, ExpectedSessionRevision: record.Revision,
		})
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: ensure exact reconciliation group: %v\n", err)
			return 1
		}
	default:
		_, _ = fmt.Fprintf(stderr, "goal recover: inspect durable recovery group: %v\n", inspectErr)
		return 1
	}

	var receipt db.ReconciliationCommitReceipt
	if *itemID == group.GroupItemID {
		if parentEvidence == nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: parent item %q requires observation %q\n", *itemID, reconciliation.TurnAbandonedAfterInspection)
			return 2
		}
		receipt, err = store.ResolveReconciliationParent(ctx, lease, db.ResolveReconciliationParentRequest{
			SessionID: sessionID, WorkspaceID: workspace, GroupItemID: group.GroupItemID,
			ExpectedSessionRevision: record.Revision, Actor: goalRecoveryActor, Evidence: *parentEvidence,
		})
	} else if member, ok := reconciliationMemberByItemID(group, *itemID); ok {
		if memberEvidence == nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: execution item %q requires an effect observation\n", *itemID)
			return 2
		}
		receipt, err = store.ResolveExecutionReconciliation(ctx, lease, db.ResolveExecutionReconciliationRequest{
			SessionID: sessionID, WorkspaceID: workspace, GroupItemID: group.GroupItemID,
			ControlItemID: member.ControlItemID, ExpectedSessionRevision: record.Revision,
			Actor: goalRecoveryActor, Evidence: *memberEvidence,
		})
	} else {
		_, _ = fmt.Fprintf(stderr, "goal recover: item %q is not the exact parent or execution member of group %q\n", *itemID, group.GroupItemID)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "goal recover: apply typed evidence: %v\n", err)
		return 1
	}
	result := projectGoalRecoveryApply(sessionID, receipt)
	result.SessionHandle = sessionHandle
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			_, _ = fmt.Fprintf(stderr, "goal recover: %v\n", err)
			return 1
		}
	} else {
		writeGoalRecoveryApply(stdout, result)
	}
	return 0
}

func normalizeGoalRecoverArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{
		"item": true, "observation": true, "source": true,
		"reference": true, "summary": true, "observed-at": true,
	}
	flagArgs := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals = append(positionals, argument)
			continue
		}
		flagArgs = append(flagArgs, argument)
		nameValue := strings.TrimLeft(argument, "-")
		name, _, hasValue := strings.Cut(nameValue, "=")
		if !valueFlags[name] || hasValue {
			continue
		}
		if index+1 >= len(args) {
			return nil, fmt.Errorf("--%s requires a value", name)
		}
		index++
		flagArgs = append(flagArgs, args[index])
	}
	return append(flagArgs, positionals...), nil
}

func reconciliationMemberByItemID(group db.ReconciliationGroup, itemID string) (db.ReconciliationGroupMember, bool) {
	for _, member := range group.Members {
		if member.ControlItemID == itemID {
			return member, true
		}
	}
	return db.ReconciliationGroupMember{}, false
}

func projectGoalRecoveryDryRun(inspection db.ReconciliationGroupInspection) goalRecoveryDryRun {
	group := inspection.Group
	members := make([]goalRecoveryMemberSummary, 0, len(group.Members))
	unresolved := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		members = append(members, goalRecoveryMemberSummary{
			ItemID: member.ControlItemID, ExecutionID: member.ExecutionID,
			EventID: member.EventID, EventType: string(member.EventType),
			EffectClass: string(member.EffectClass), Resolved: member.Resolved,
		})
		if !member.Resolved {
			unresolved = append(unresolved, member.ControlItemID)
		}
	}
	parentResolved := group.ParentResolution != nil
	return goalRecoveryDryRun{
		DryRun: true, SessionID: inspection.SessionID, SessionRevision: inspection.SessionRevision,
		GoalID: inspection.GoalID, GoalState: inspection.GoalState,
		GroupItemID: group.GroupItemID, TurnID: group.TurnID, BlockerReference: group.BlockerReference,
		SnapshotCursor: group.SnapshotCursor, GroupPayloadSHA256: group.PayloadSHA256,
		MemberSetSHA256: group.MemberSetSHA256, ExecutionMemberCount: group.ExecutionMemberCount,
		Members: members, UnresolvedExecutionItems: unresolved,
		Parent: goalRecoveryParentSummary{
			ItemID: group.GroupItemID, Required: true, Resolved: parentResolved,
			Ready: !parentResolved && len(unresolved) == 0, BlockedByCount: len(unresolved),
		},
		NoResumeWarning: goalRecoveryNoResumeWarning,
	}
}

func projectGoalRecoveryApply(sessionID int64, receipt db.ReconciliationCommitReceipt) goalRecoveryApplyResult {
	state := goal.StateBlocked
	if receipt.Goal != nil {
		state = receipt.Goal.State
	} else if !receipt.ParentPending {
		state = ""
	}
	return goalRecoveryApplyResult{
		Applied: true, Inserted: receipt.Inserted, SessionID: sessionID,
		SessionRevision: receipt.SessionRevision, GroupItemID: receipt.GroupItemID,
		ItemID: receipt.ItemID, ResolutionID: receipt.ResolutionID, GoalState: state,
		GoalCleared: receipt.GoalCleared, RemainingExecutions: receipt.RemainingExecutions,
		ParentPending: receipt.ParentPending, ExecutionCursor: receipt.ExecutionCursor,
		NoResumeWarning: goalRecoveryNoResumeWarning,
	}
}

func writeGoalRecoveryDryRun(writer io.Writer, view goalRecoveryDryRun) {
	goalFprintln(writer, "Recovery dry run (read-only)")
	handle := view.SessionHandle
	if handle == "" {
		handle = "?"
	}
	goalFprintf(writer, "Session: %s @ revision %d\n", handle, view.SessionRevision)
	goalFprintf(writer, "Goal: %s · %s\n", terminalSafeGoalText(view.GoalID), view.GoalState)
	goalFprintf(writer, "Group: %s\n", terminalSafeGoalText(view.GroupItemID))
	goalFprintf(writer, "Turn: %s · snapshot cursor %d\n", terminalSafeGoalText(view.TurnID), view.SnapshotCursor)
	goalFprintf(writer, "Members: %d total · %d unresolved\n", view.ExecutionMemberCount, len(view.UnresolvedExecutionItems))
	for _, member := range view.Members {
		status := "pending"
		if member.Resolved {
			status = "resolved"
		}
		goalFprintf(writer, "  %s · %s · %s · %s\n",
			terminalSafeGoalText(member.ItemID), terminalSafeGoalText(member.ExecutionID), member.EventType, status)
	}
	parentStatus := "blocked"
	switch {
	case view.Parent.Resolved:
		parentStatus = "resolved"
	case view.Parent.Ready:
		parentStatus = "ready"
	}
	goalFprintf(writer, "Parent: %s · %s", terminalSafeGoalText(view.Parent.ItemID), parentStatus)
	if view.Parent.BlockedByCount > 0 {
		goalFprintf(writer, " · waiting on %d execution member(s)", view.Parent.BlockedByCount)
	}
	goalFprintln(writer)
	goalFprintf(writer, "Warning: %s\n", view.NoResumeWarning)
}

func writeGoalRecoveryApply(writer io.Writer, result goalRecoveryApplyResult) {
	action := "replayed"
	if result.Inserted {
		action = "recorded"
	}
	goalFprintf(writer, "Recovery evidence %s: %s\n", action, terminalSafeGoalText(result.ResolutionID))
	goalFprintf(writer, "Group: %s · item %s\n", terminalSafeGoalText(result.GroupItemID), terminalSafeGoalText(result.ItemID))
	handle := result.SessionHandle
	if handle == "" {
		handle = "?"
	}
	goalFprintf(writer, "Session: %s @ revision %d\n", handle, result.SessionRevision)
	if result.GoalCleared {
		goalFprintf(writer, "Goal state: %s\n", result.GoalState)
	} else {
		goalFprintf(writer, "Remaining execution members: %d · parent pending: %t\n", result.RemainingExecutions, result.ParentPending)
	}
	goalFprintf(writer, "Warning: %s\n", result.NoResumeWarning)
}

func handleGoalList(store goalSessionStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goal list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalListUsage(stdout) }
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	limit := flags.Int("limit", 20, "maximum durable sessions to inspect (1-100)")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "goal list: unexpected positional arguments")
		return 2
	}
	if *limit <= 0 || *limit > goalCommandLimit {
		goalFprintf(stderr, "goal list: limit must be between 1 and %d\n", goalCommandLimit)
		return 2
	}
	summaries, warnings, err := listGoalSummaries(context.Background(), store, workspace, int64(*limit))
	if err != nil {
		goalFprintf(stderr, "goal list: %v\n", err)
		return 1
	}
	for _, warning := range warnings {
		goalFprintf(stderr, "goal list: warning: %v\n", warning)
	}
	if *jsonOutput {
		if err := writeJSON(stdout, summaries); err != nil {
			goalFprintf(stderr, "goal list: %v\n", err)
			return 1
		}
		return 0
	}
	writeGoalList(stdout, summaries)
	return 0
}

func handleGoalShow(store goalSessionStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goal show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeGoalShowUsage(stdout) }
	jsonOutput := flags.Bool("json", false, "print the complete validated snapshot as JSON")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "goal show: provide exactly one session ID from `sonar goal list`")
		return 2
	}
	session, err := resolveSessionArg(context.Background(), store, flags.Arg(0))
	if err != nil {
		goalFprintf(stderr, "goal show: %v\n", err)
		return 2
	}
	sessionID := session.ID
	summary, err := getGoalSummary(context.Background(), store, workspace, sessionID)
	if err != nil {
		goalFprintf(stderr, "goal show: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, summary.Snapshot); err != nil {
			goalFprintf(stderr, "goal show: %v\n", err)
			return 1
		}
		return 0
	}
	writeGoalDetail(stdout, summary)
	return 0
}

func listGoalSummaries(ctx context.Context, store goalSessionStore, workspace string, limit int64) ([]goalSummary, []error, error) {
	if store == nil {
		return nil, nil, errors.New("durable store is unavailable")
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, nil, errors.New("workspace identity is required")
	}
	if limit <= 0 || limit > goalCommandLimit {
		return nil, nil, fmt.Errorf("limit must be between 1 and %d", goalCommandLimit)
	}
	sessions, err := store.ListSessions(ctx, db.ListSessionsParams{WorkspaceID: workspace, Limit: limit})
	if err != nil {
		return nil, nil, fmt.Errorf("list durable sessions: %w", err)
	}
	summaries := make([]goalSummary, 0, len(sessions))
	warnings := make([]error, 0)
	for _, session := range sessions {
		raw, stateErr := store.GetSessionState(ctx, session.ID)
		if errors.Is(stateErr, db.ErrSessionStateNotFound) {
			continue
		}
		if stateErr != nil {
			warnings = append(warnings, fmt.Errorf("session %d: %w", session.ID, stateErr))
			continue
		}
		summary, present, decodeErr := decodeGoalSummary(session, raw)
		if decodeErr != nil {
			warnings = append(warnings, fmt.Errorf("session %d: %w", session.ID, decodeErr))
			continue
		}
		if present {
			summaries = append(summaries, summary)
		}
	}
	return summaries, warnings, nil
}

func getGoalSummary(ctx context.Context, store goalSessionStore, workspace string, sessionID int64) (goalSummary, error) {
	if store == nil {
		return goalSummary{}, errors.New("durable store is unavailable")
	}
	session, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return goalSummary{}, fmt.Errorf("read session %d: %w", sessionID, err)
	}
	if session.WorkspaceID != workspace {
		return goalSummary{}, fmt.Errorf("session %d belongs to a different workspace", sessionID)
	}
	raw, err := store.GetSessionState(ctx, sessionID)
	if err != nil {
		return goalSummary{}, fmt.Errorf("read session %d state: %w", sessionID, err)
	}
	summary, present, err := decodeGoalSummary(session, raw)
	if err != nil {
		return goalSummary{}, err
	}
	if !present {
		return goalSummary{}, fmt.Errorf("session %d has no durable goal", sessionID)
	}
	return summary, nil
}

func decodeGoalSummary(session db.Session, raw string) (goalSummary, bool, error) {
	var envelope goalSessionEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return goalSummary{}, false, fmt.Errorf("decode durable session: %w", err)
	}
	if envelope.Goal == nil {
		return goalSummary{}, false, nil
	}
	if envelope.Goal.SessionID != session.ID {
		return goalSummary{}, false, fmt.Errorf("goal %q belongs to session %d, not %d", envelope.Goal.ID, envelope.Goal.SessionID, session.ID)
	}
	runtime, err := goal.Restore(*envelope.Goal)
	if err != nil {
		return goalSummary{}, false, fmt.Errorf("validate durable goal: %w", err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		return goalSummary{}, false, fmt.Errorf("refresh durable goal: %w", err)
	}
	return goalSummary{
		SessionID:       session.ID,
		PublicID:        session.PublicID,
		GoalID:          snapshot.ID,
		State:           snapshot.State,
		Objective:       snapshot.Objective,
		CortexTaskID:    snapshot.Cortex.TaskID,
		CortexRevision:  snapshot.Cortex.Revision,
		ContinuationUse: snapshot.Usage.ContinuationTurns,
		EvalTokenUse:    snapshot.Usage.EvalTokens,
		UpdatedAt:       snapshot.UpdatedAt,
		SessionUpdated:  session.UpdatedAt,
		Snapshot:        snapshot,
	}, true, nil
}

func writeGoalUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal <command> [options]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Commands:")
	_, _ = fmt.Fprintln(writer, "  list      List durable goal sessions")
	_, _ = fmt.Fprintln(writer, "  show      Show one validated goal snapshot")
	_, _ = fmt.Fprintln(writer, "  pending   List pending control items for a session")
	_, _ = fmt.Fprintln(writer, "  recover   Inspect or reconcile paused goal work")
	_, _ = fmt.Fprintln(writer, "  open      Open and persist a new headless goal")
	_, _ = fmt.Fprintln(writer, "  run       Execute one durable headless goal turn")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Recovery:")
	_, _ = fmt.Fprintln(writer, "  sonar goal recover [--json] <session-id>")
	_, _ = fmt.Fprintln(writer, "  sonar goal recover --apply --item ID ... --observed-at RFC3339 <session-id>")
	_, _ = fmt.Fprintln(writer)
	writeGoalSessionReferenceHelp(writer)
	_, _ = fmt.Fprintln(writer, "Safety:")
	_, _ = fmt.Fprintln(writer, "  Recovery is read-only unless --apply is explicit.")
	_, _ = fmt.Fprintln(writer, "  Recovery never resumes execution and has no force override.")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Run `sonar goal <command> --help` for command options.")
}

func writeGoalOpenUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal open --objective TEXT [options]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --objective TEXT             Bounded goal objective (required)")
	_, _ = fmt.Fprintln(writer, "  --criterion TEXT             Acceptance criterion (defaults to objective)")
	_, _ = fmt.Fprintf(writer, "  --max-continuation-turns N   Automatic continuation budget (default %d)\n", goal.DefaultBudget.MaxContinuationTurns)
	_, _ = fmt.Fprintf(writer, "  --max-eval-tokens N          Evaluation token budget (default %d)\n", goal.DefaultBudget.MaxEvalTokens)
	_, _ = fmt.Fprintf(writer, "  --max-wall-time DURATION     Wall-time budget (default %s)\n", goal.DefaultBudget.MaxWallTime)
	_, _ = fmt.Fprintln(writer, "  --json                       Print machine-readable JSON")
	_, _ = fmt.Fprintln(writer, "  -h, --help                   Show this help")
}

func writeGoalRunUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal run <session-id> --prompt TEXT [options]")
	_, _ = fmt.Fprintln(writer)
	writeGoalSessionReferenceHelp(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --prompt TEXT       Instruction for this goal turn (required)")
	_, _ = fmt.Fprintln(writer, "  --skip-approvals    Skip approval prompts; explicit denies still win")
	_, _ = fmt.Fprintln(writer, "  --model NAME        Override the Ollama model")
	_, _ = fmt.Fprintln(writer, "  --agent NAME        Override the agent profile")
	_, _ = fmt.Fprintln(writer, "  -h, --help          Show this help")
}

func writeGoalListUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal list [options]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --limit N    Maximum durable sessions to inspect (default 20; range 1-100)")
	_, _ = fmt.Fprintln(writer, "  --json       Print machine-readable JSON")
	_, _ = fmt.Fprintln(writer, "  -h, --help   Show this help")
}

func writeGoalShowUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal show [options] <session-id>")
	_, _ = fmt.Fprintln(writer)
	writeGoalSessionReferenceHelp(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --json       Print the complete validated snapshot as JSON")
	_, _ = fmt.Fprintln(writer, "  -h, --help   Show this help")
}

func writeGoalPendingUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal pending [options] <session-id>")
	_, _ = fmt.Fprintln(writer)
	writeGoalSessionReferenceHelp(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --limit N    Maximum pending items to print (default 20; range 1-100)")
	_, _ = fmt.Fprintln(writer, "  --json       Print machine-readable JSON")
	_, _ = fmt.Fprintln(writer, "  -h, --help   Show this help")
}

func writeGoalRecoverUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar goal recover [--json] <session-id>")
	_, _ = fmt.Fprintln(writer, "  sonar goal recover --apply [evidence options] [--json] <session-id>")
	_, _ = fmt.Fprintln(writer)
	writeGoalSessionReferenceHelp(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --json               Print machine-readable JSON")
	_, _ = fmt.Fprintln(writer, "  --apply              Persist the exact typed evidence")
	_, _ = fmt.Fprintln(writer, "  -h, --help           Show this help")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Required with --apply:")
	_, _ = fmt.Fprintln(writer, "  --item ID            Exact execution-member or parent item ID")
	_, _ = fmt.Fprintln(writer, "  --observation VALUE")
	_, _ = fmt.Fprintln(writer, "      effect_applied, effect_not_applied, effect_compensated, or")
	_, _ = fmt.Fprintln(writer, "      turn_abandoned_after_inspection")
	_, _ = fmt.Fprintln(writer, "  --source VALUE")
	_, _ = fmt.Fprintln(writer, "      external_receipt, workspace_artifact, verification_check, or")
	_, _ = fmt.Fprintln(writer, "      operator_observation")
	_, _ = fmt.Fprintln(writer, "  --reference TEXT     Redacted evidence reference")
	_, _ = fmt.Fprintln(writer, "  --summary TEXT       Bounded inspection summary")
	_, _ = fmt.Fprintln(writer, "  --observed-at RFC3339")
	_, _ = fmt.Fprintln(writer, "      Evidence observation time")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Safety:")
	_, _ = fmt.Fprintln(writer, "  Inspection is read-only unless --apply is explicit.")
	_, _ = fmt.Fprintln(writer, "  Applying requires every evidence option shown above.")
	_, _ = fmt.Fprintln(writer, "  Recovery never resumes execution and has no force override.")
}

func writeGoalSessionReferenceHelp(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Session references accept a 7-character hex handle such as a1b2c3d.")
	_, _ = fmt.Fprintln(writer)
}

func projectPendingControlItems(states []controlplane.State, sessionID int64, workspaceID string) ([]pendingControlSummary, error) {
	if sessionID <= 0 || strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("pending control scope is invalid")
	}
	if len(states) > controlplane.MaxListLimit {
		return nil, fmt.Errorf("pending control projection exceeds %d items", controlplane.MaxListLimit)
	}
	result := make([]pendingControlSummary, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for index, state := range states {
		if !state.Pending() {
			return nil, fmt.Errorf("item %d is already resolved", index)
		}
		if err := state.Item.Validate(); err != nil {
			return nil, fmt.Errorf("item %d: %w", index, err)
		}
		if state.Item.Identity.SessionID != sessionID || state.Item.Identity.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("item %q is outside the requested session scope", state.Item.ItemID)
		}
		if _, exists := seen[state.Item.ItemID]; exists {
			return nil, fmt.Errorf("duplicate item id %q", state.Item.ItemID)
		}
		seen[state.Item.ItemID] = struct{}{}
		result = append(result, pendingControlSummary{
			SessionID: sessionID, ItemID: state.Item.ItemID, Kind: state.Item.Kind,
			GoalID: state.Item.Identity.GoalID, ExecutionID: state.Item.Identity.ExecutionID,
			TurnID: state.Item.Identity.TurnID, Summary: state.Item.Summary,
			CreatedAt: state.Item.CreatedAt,
		})
	}
	return result, nil
}

func writePendingControlItems(writer io.Writer, items []pendingControlSummary) {
	if len(items) == 0 {
		goalFprintln(writer, "No pending decisions, approvals, or recovery items.")
		return
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	goalFprintln(table, "KIND\tITEM\tGOAL\tSUMMARY")
	for _, item := range items {
		goalFprintf(table, "%s\t%s\t%s\t%s\n",
			item.Kind,
			terminalSafeGoalText(item.ItemID),
			terminalSafeGoalText(fallbackGoalCLIText(item.GoalID, "—")),
			compactGoalObjective(item.Summary, 72),
		)
	}
	_ = table.Flush()
}

func fallbackGoalCLIText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func writeGoalList(writer io.Writer, summaries []goalSummary) {
	if len(summaries) == 0 {
		goalFprintln(writer, "No durable goals found in this workspace.")
		return
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	goalFprintln(table, "SESSION\tSTATE\tUPDATED\tOBJECTIVE")
	for _, summary := range summaries {
		handle := sessionref.Format(summary.PublicID)
		if handle == "" {
			handle = "?"
		}
		goalFprintf(table, "%s\t%s\t%s\t%s\n",
			handle, summary.State, summary.UpdatedAt.Local().Format("2006-01-02 15:04"),
			compactGoalObjective(summary.Objective, 72),
		)
	}
	_ = table.Flush()
}

func writeGoalDetail(writer io.Writer, summary goalSummary) {
	handle := sessionref.Format(summary.PublicID)
	if handle == "" {
		handle = "?"
	}
	goalFprintf(writer, "Goal: %s\n", terminalSafeGoalText(summary.GoalID))
	goalFprintf(writer, "Session: %s\n", handle)
	goalFprintf(writer, "State: %s\n", summary.State)
	goalFprintf(writer, "Objective: %s\n", terminalSafeGoalText(summary.Objective))
	if summary.Snapshot.StateReason != "" {
		goalFprintf(writer, "Reason: %s\n", terminalSafeGoalText(summary.Snapshot.StateReason))
	}
	goalFprintf(writer, "Acceptance: %d criteria\n", len(summary.Snapshot.AcceptanceCriteria))
	goalFprintf(writer, "Usage: %d continuation turns, %d eval tokens\n", summary.ContinuationUse, summary.EvalTokenUse)
	if summary.CortexTaskID != "" {
		goalFprintf(writer, "Cortex: %s @ revision %d\n", terminalSafeGoalText(summary.CortexTaskID), summary.CortexRevision)
	}
	goalFprintf(writer, "Updated: %s\n", summary.UpdatedAt.Local().Format(time.RFC3339))
}

func goalFprintf(writer io.Writer, format string, arguments ...any) {
	_, _ = fmt.Fprintf(writer, format, arguments...)
}

func goalFprintln(writer io.Writer, arguments ...any) {
	_, _ = fmt.Fprintln(writer, arguments...)
}

func compactGoalObjective(value string, limit int) string {
	value = strings.Join(strings.Fields(terminalSafeGoalText(value)), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func terminalSafeGoalText(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return -1
			}
			return r
		}
	}, strings.ToValidUTF8(value, "�"))
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
