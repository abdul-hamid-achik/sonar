package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

const (
	executionRecoveryActor    = "local-user"
	executionRecoveryAllLimit = 100
)

type executionRecoveryStore interface {
	InspectStandaloneExecutionReconciliation(context.Context, int64, string, string) (db.StandaloneExecutionReconciliationInspection, error)
	AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error)
	ResolveStandaloneExecutionReconciliation(context.Context, *db.ExecutionSessionLease, db.ResolveStandaloneExecutionReconciliationRequest) (db.StandaloneExecutionReconciliationReceipt, error)
	ListStandaloneExecutionReconciliationPending(context.Context, int64, string, int) ([]execution.State, error)
	ResolveSessionRef(context.Context, string) (db.Session, error)
	SessionHandle(context.Context, int64) (string, error)
}

type executionRecoveryView struct {
	SessionID       int64  `json:"session_id"`
	WorkspaceID     string `json:"workspace_id"`
	SessionRevision int64  `json:"session_revision"`
	ExecutionID     string `json:"execution_id"`
	TurnID          string `json:"turn_id"`
	ToolName        string `json:"tool_name"`
	EventID         int64  `json:"event_id"`
	EventType       string `json:"event_type"`
	EffectClass     string `json:"effect_class"`
	ArgumentsSHA256 string `json:"arguments_sha256"`
	ItemID          string `json:"item_id"`
	Resolved        bool   `json:"resolved"`
	ResolutionID    string `json:"resolution_id,omitempty"`
	ApplyTemplate   string `json:"apply_template,omitempty"`
}

type executionRecoveryApplyView struct {
	SessionID       int64  `json:"session_id"`
	SessionRevision int64  `json:"session_revision"`
	ExecutionID     string `json:"execution_id"`
	EventID         int64  `json:"event_id"`
	ItemID          string `json:"item_id"`
	ResolutionID    string `json:"resolution_id"`
	Inserted        bool   `json:"inserted"`
	Notice          string `json:"notice"`
}

func handleExecutionCommand(args []string) int {
	return handleExecutionCommandIO(args, os.Stdout, os.Stderr)
}

func handleExecutionCommandIO(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeExecutionUsage(stdout)
		return 0
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			executionFprintf(stderr, "execution: unexpected argument %q after %s\n", args[1], args[0])
			return 2
		}
		writeExecutionUsage(stdout)
		return 0
	}
	if args[0] != "recover" {
		executionFprintf(stderr, "execution: unknown command %q\n", args[0])
		writeExecutionUsage(stderr)
		return 2
	}
	if hasHelpFlag(args[1:]) {
		return handleExecutionRecover(nil, "", []string{"--help"}, stdout, stderr)
	}
	workspace := currentWorkspace()
	if workspace == "" {
		executionFprintln(stderr, "execution: workspace identity is unavailable")
		return 1
	}
	store, err := db.Open()
	if err != nil {
		executionFprintf(stderr, "execution: open durable store: %v\n", err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			executionFprintf(stderr, "execution: close durable store: %v\n", err)
		}
	}()
	return handleExecutionRecover(store, workspace, args[1:], stdout, stderr)
}

func handleExecutionRecover(store executionRecoveryStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("execution recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeExecutionRecoverUsage(stdout) }
	apply := flags.Bool("apply", false, "append typed reconciliation evidence")
	all := flags.Bool("all", false, "target every execution pending reconciliation in the session")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	revision := flags.Int64("revision", -1, "exact session revision shown by inspection")
	eventID := flags.Int64("event-id", -1, "exact latest event ID shown by inspection")
	observation := flags.String("observation", "", "effect_applied, effect_not_applied, or effect_compensated")
	source := flags.String("source", "", "external_receipt, workspace_artifact, verification_check, or operator_observation")
	reference := flags.String("reference", "", "redacted evidence reference")
	summary := flags.String("summary", "", "bounded inspection summary")
	observedAtText := flags.String("observed-at", "", "evidence observation time in RFC3339")
	setDigest := flags.String("set-digest", "", "exact pending-set digest shown by --all inspection")
	normalized, err := normalizeExecutionRecoverArgs(args)
	if err != nil {
		executionFprintf(stderr, "execution recover: %v\n", err)
		return 2
	}
	if code, done := flagParseExitCode(flags.Parse(normalized)); done {
		return code
	}
	wantArgs := 2
	if *all {
		wantArgs = 1
	}
	if flags.NArg() != wantArgs {
		if *all {
			executionFprintln(stderr, "execution recover: provide SESSION_ID only with --all")
		} else {
			executionFprintln(stderr, "execution recover: provide SESSION_ID and EXECUTION_ID")
		}
		return 2
	}
	session, err := resolveSessionArg(context.Background(), store, flags.Arg(0))
	if err != nil {
		executionFprintf(stderr, "execution recover: %v\n", err)
		return 2
	}
	sessionID := session.ID
	sessionHandle := sessionDisplayHandle(session)
	provided := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { provided[value.Name] = true })
	if !*apply {
		for name := range provided {
			if name != "json" && name != "apply" && name != "all" {
				executionFprintf(stderr, "execution recover: --%s requires --apply\n", name)
				return 2
			}
		}
		if *all {
			return listExecutionRecoveryPending(store, workspace, sessionID, sessionHandle, *jsonOutput, stdout, stderr)
		}
		inspection, err := store.InspectStandaloneExecutionReconciliation(context.Background(), sessionID, workspace, flags.Arg(1))
		if err != nil {
			executionFprintf(stderr, "execution recover: inspect immutable receipt: %v\n", err)
			return 1
		}
		view := projectExecutionRecovery(inspection)
		if *jsonOutput {
			if err := writeExecutionJSON(stdout, view); err != nil {
				executionFprintf(stderr, "execution recover: %v\n", err)
				return 1
			}
		} else {
			writeExecutionRecovery(stdout, view, sessionHandle)
		}
		return 0
	}

	required := []string{"observation", "source", "reference", "summary", "observed-at"}
	if *all {
		required = append(required, "set-digest")
		// Batch apply derives the exact revision/event tokens from a fresh
		// inspection of each execution under the held session lease.
		for _, name := range []string{"revision", "event-id"} {
			if provided[name] {
				executionFprintf(stderr, "execution recover: --%s cannot be combined with --all; exact tokens are inspected per execution\n", name)
				return 2
			}
		}
	} else {
		if provided["set-digest"] {
			executionFprintln(stderr, "execution recover: --set-digest can be used only with --all --apply")
			return 2
		}
		required = append([]string{"revision", "event-id"}, required...)
	}
	for _, name := range required {
		if !provided[name] {
			executionFprintf(stderr, "execution recover: --apply requires --%s from a prior inspection\n", name)
			return 2
		}
	}
	if !*all && (*revision < 0 || *eventID <= 0) {
		executionFprintln(stderr, "execution recover: --revision must be non-negative and --event-id must be positive")
		return 2
	}
	if *all && !validExecutionRecoverySetDigest(*setDigest) {
		executionFprintln(stderr, "execution recover: --set-digest must be the 64-character lowercase digest from a prior --all inspection")
		return 2
	}
	observedAt, err := time.Parse(time.RFC3339, *observedAtText)
	if err != nil {
		executionFprintf(stderr, "execution recover: invalid --observed-at RFC3339 value: %v\n", err)
		return 2
	}
	evidence := reconciliation.Request{
		Disposition: reconciliation.Disposition(*observation),
		Source: reconciliation.Source{
			Kind: reconciliation.SourceKind(*source), Reference: *reference,
			ObservedAt: observedAt.UTC(),
		},
		Summary: *summary,
	}
	if err := evidence.Validate(); err != nil {
		executionFprintf(stderr, "execution recover: invalid typed evidence: %v\n", err)
		return 2
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), sessionID, workspace)
	if err != nil {
		executionFprintf(stderr, "execution recover: acquire exact session lease: %v\n", err)
		return 1
	}
	defer func() {
		if err := lease.Close(); err != nil {
			executionFprintf(stderr, "execution recover: release session lease: %v\n", err)
		}
	}()
	if *all {
		return applyAllExecutionRecoveries(store, lease, workspace, sessionID, *setDigest, evidence, *jsonOutput, stdout, stderr)
	}
	receipt, err := store.ResolveStandaloneExecutionReconciliation(context.Background(), lease, db.ResolveStandaloneExecutionReconciliationRequest{
		SessionID: sessionID, WorkspaceID: workspace, ExecutionID: flags.Arg(1),
		ExpectedSessionRevision: *revision, ExpectedEventID: *eventID,
		Actor: executionRecoveryActor, Evidence: evidence,
	})
	if err != nil {
		executionFprintf(stderr, "execution recover: append typed evidence: %v\n", err)
		return 1
	}
	view := recoveryApplyView(receipt)
	if *jsonOutput {
		if err := writeExecutionJSON(stdout, view); err != nil {
			executionFprintf(stderr, "execution recover: %v\n", err)
			return 1
		}
	} else {
		writeExecutionRecoveryReceipt(stdout, view)
	}
	return 0
}

func recoveryApplyView(receipt db.StandaloneExecutionReconciliationReceipt) executionRecoveryApplyView {
	return executionRecoveryApplyView{
		SessionID: receipt.SessionID, SessionRevision: receipt.SessionRevision,
		ExecutionID: receipt.ExecutionID, EventID: receipt.EventID,
		ItemID: receipt.ItemID, ResolutionID: receipt.ResolutionID, Inserted: receipt.Inserted,
		Notice: "Evidence recorded; the immutable outcome-unknown event was not changed and no tool was retried.",
	}
}

func writeExecutionRecoveryReceipt(writer io.Writer, view executionRecoveryApplyView) {
	executionFprintf(writer, "Reconciled execution %s at event %d.\nReceipt: %s\n%s\n", terminalSafeGoalText(view.ExecutionID), view.EventID, terminalSafeGoalText(view.ResolutionID), view.Notice)
}

type executionRecoveryPendingView struct {
	SessionID int64                       `json:"session_id"`
	Workspace string                      `json:"workspace_id"`
	Count     int                         `json:"count"`
	SetDigest string                      `json:"set_digest"`
	Pending   []executionRecoveryItemView `json:"pending"`
}

type executionRecoveryItemView struct {
	ExecutionID    string `json:"execution_id"`
	ToolName       string `json:"tool_name"`
	EventID        int64  `json:"event_id"`
	EventType      string `json:"event_type"`
	EffectClass    string `json:"effect_class"`
	TurnID         string `json:"turn_id"`
	InspectCommand string `json:"inspect_command"`
}

func listExecutionRecoveryPending(store executionRecoveryStore, workspace string, sessionID int64, sessionHandle string, jsonOutput bool, stdout, stderr io.Writer) int {
	if sessionHandle == "" {
		sessionHandle = sessionHandleOrLookup(context.Background(), store, sessionID, "")
	}
	states, err := store.ListStandaloneExecutionReconciliationPending(context.Background(), sessionID, workspace, executionRecoveryAllLimit)
	if err != nil {
		writeExecutionRecoveryPendingError(stderr, err, false)
		return 1
	}
	view := executionRecoveryPendingView{
		SessionID: sessionID, Workspace: workspace,
		Count: len(states), SetDigest: executionRecoverySetDigest(sessionID, workspace, states),
		Pending: make([]executionRecoveryItemView, 0, len(states)),
	}
	for _, state := range states {
		handle := sessionHandle
		if handle == "" {
			handle = fmt.Sprintf("%d", sessionID)
		}
		view.Pending = append(view.Pending, executionRecoveryItemView{
			ExecutionID: state.Identity.ExecutionID, ToolName: state.Identity.ToolName,
			EventID: state.Latest.ID, EventType: string(state.Latest.Type),
			EffectClass: string(state.Identity.EffectClass), TurnID: state.Identity.TurnID,
			InspectCommand: fmt.Sprintf("sonar execution recover %s %s", handle, state.Identity.ExecutionID),
		})
	}
	if jsonOutput {
		if err := writeExecutionJSON(stdout, view); err != nil {
			executionFprintf(stderr, "execution recover: %v\n", err)
			return 1
		}
		return 0
	}
	if view.Count == 0 {
		handle := sessionHandle
		if handle == "" {
			handle = "?"
		}
		executionFprintf(stdout, "No executions are pending reconciliation in session %s.\n", handle)
		return 0
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	executionFprintf(table, "EXECUTION\tTOOL\tEVENT\tEFFECT\n")
	for _, item := range view.Pending {
		executionFprintf(table, "%s\t%s\t%d · %s\t%s\n",
			terminalSafeGoalText(item.ExecutionID), terminalSafeGoalText(item.ToolName),
			item.EventID, item.EventType, item.EffectClass)
	}
	_ = table.Flush()
	executionFprintf(stdout, "\n%d execution(s) pending reconciliation.\n", view.Count)
	executionFprintln(stdout, "Inspect each with the read-only command, or reconcile all with one verified observation:")
	executionFprintf(stdout, "Reviewed-set digest: %s\n", view.SetDigest)
	handle := sessionHandle
	if handle == "" {
		handle = "?"
	}
	executionFprintf(stdout, "  sonar execution recover %s --all --apply --set-digest %s --observation effect_not_applied --source verification_check --reference REF --summary SUMMARY --observed-at RFC3339\n", handle, view.SetDigest)
	executionFprintln(stdout, "Only assert one shared disposition after verifying every listed execution independently.")
	return 0
}

func applyAllExecutionRecoveries(store executionRecoveryStore, lease *db.ExecutionSessionLease, workspace string, sessionID int64, expectedSetDigest string, evidence reconciliation.Request, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	states, err := store.ListStandaloneExecutionReconciliationPending(ctx, sessionID, workspace, executionRecoveryAllLimit)
	if err != nil {
		writeExecutionRecoveryPendingError(stderr, err, true)
		return 1
	}
	actualSetDigest := executionRecoverySetDigest(sessionID, workspace, states)
	if actualSetDigest != expectedSetDigest {
		executionFprintf(stderr, "execution recover: --all aborted: pending reconciliation set changed after inspection (reviewed %s, current %s). No evidence was recorded. Run the read-only --all inspection again.\n", expectedSetDigest, actualSetDigest)
		return 1
	}
	receipts := make([]executionRecoveryApplyView, 0, len(states))
	emit := func() int {
		if !jsonOutput {
			executionFprintf(stdout, "Reconciled %d execution(s).\n", len(receipts))
			return 0
		}
		if err := writeExecutionJSON(stdout, receipts); err != nil {
			executionFprintf(stderr, "execution recover: %v\n", err)
			return 1
		}
		return 0
	}
	for _, state := range states {
		// Each resolution re-inspects under the held lease so the exact
		// revision/event tokens are always fresh and CAS-verified.
		inspection, err := store.InspectStandaloneExecutionReconciliation(ctx, sessionID, workspace, state.Identity.ExecutionID)
		if err != nil {
			executionFprintf(stderr, "execution recover: inspect %s: %v\n", state.Identity.ExecutionID, err)
			_ = emit()
			return 1
		}
		if inspection.Resolved {
			continue
		}
		receipt, err := store.ResolveStandaloneExecutionReconciliation(ctx, lease, db.ResolveStandaloneExecutionReconciliationRequest{
			SessionID: sessionID, WorkspaceID: workspace, ExecutionID: inspection.ExecutionID,
			ExpectedSessionRevision: inspection.SessionRevision, ExpectedEventID: inspection.EventID,
			Actor: executionRecoveryActor, Evidence: evidence,
		})
		if err != nil {
			executionFprintf(stderr, "execution recover: append typed evidence for %s: %v\n", inspection.ExecutionID, err)
			_ = emit()
			return 1
		}
		view := recoveryApplyView(receipt)
		receipts = append(receipts, view)
		if !jsonOutput {
			writeExecutionRecoveryReceipt(stdout, view)
		}
	}
	return emit()
}

func executionRecoverySetDigest(sessionID int64, workspace string, states []execution.State) string {
	type digestItem struct {
		ExecutionID     string `json:"execution_id"`
		EventID         int64  `json:"event_id"`
		EventType       string `json:"event_type"`
		ArgumentsSHA256 string `json:"arguments_sha256,omitempty"`
	}
	items := make([]digestItem, 0, len(states))
	for _, state := range states {
		items = append(items, digestItem{
			ExecutionID: state.Identity.ExecutionID, EventID: state.Latest.ID,
			EventType: string(state.Latest.Type), ArgumentsSHA256: state.Latest.ArgumentsSHA256,
		})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].ExecutionID != items[right].ExecutionID {
			return items[left].ExecutionID < items[right].ExecutionID
		}
		return items[left].EventID < items[right].EventID
	})
	payload, _ := json.Marshal(struct {
		Version   int          `json:"version"`
		SessionID int64        `json:"session_id"`
		Workspace string       `json:"workspace_id"`
		Pending   []digestItem `json:"pending"`
	}{Version: 1, SessionID: sessionID, Workspace: workspace, Pending: items})
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:])
}

func validExecutionRecoverySetDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func writeExecutionRecoveryPendingError(stderr io.Writer, err error, applying bool) {
	if !errors.Is(err, db.ErrExecutionHazardOverflow) {
		executionFprintf(stderr, "execution recover: list pending reconciliations: %v\n", err)
		return
	}
	executionFprintf(stderr, "execution recover: --all aborted: pending reconciliation backlog exceeds the safe limit of %d; recover executions individually before retrying --all", executionRecoveryAllLimit)
	if applying {
		executionFprintf(stderr, ". No evidence was recorded")
	}
	executionFprintln(stderr, ".")
}

func normalizeExecutionRecoverArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{
		"revision": true, "event-id": true, "observation": true,
		"source": true, "reference": true, "summary": true, "observed-at": true,
		"set-digest": true,
	}
	var flags, positional []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--apply" || argument == "-apply" || argument == "--json" || argument == "-json" ||
			argument == "--all" || argument == "-all" {
			flags = append(flags, argument)
			continue
		}
		if strings.HasPrefix(argument, "--") || strings.HasPrefix(argument, "-") {
			nameValue := strings.TrimLeft(argument, "-")
			name, _, hasValue := strings.Cut(nameValue, "=")
			if valueFlags[name] && !hasValue {
				if index+1 >= len(args) {
					return nil, fmt.Errorf("--%s requires a value", name)
				}
				flags = append(flags, argument, args[index+1])
				index++
				continue
			}
			flags = append(flags, argument)
			continue
		}
		positional = append(positional, argument)
	}
	return append(flags, positional...), nil
}

func projectExecutionRecovery(inspection db.StandaloneExecutionReconciliationInspection) executionRecoveryView {
	view := executionRecoveryView{
		SessionID: inspection.SessionID, WorkspaceID: inspection.WorkspaceID,
		SessionRevision: inspection.SessionRevision, ExecutionID: inspection.ExecutionID,
		TurnID: inspection.TurnID, ToolName: inspection.ToolName,
		EventID: inspection.EventID, EventType: string(inspection.EventType),
		EffectClass: string(inspection.EffectClass), ArgumentsSHA256: inspection.ArgumentsSHA256,
		ItemID: inspection.ItemID, Resolved: inspection.Resolved, ResolutionID: inspection.ResolutionID,
	}
	if !inspection.Resolved {
		// JSON apply template keeps the durable numeric session id so machine
		// consumers have a stable contract; the human text path uses the public handle.
		view.ApplyTemplate = executionRecoveryApplyTemplate(
			fmt.Sprintf("%d", inspection.SessionID), inspection.SessionRevision, inspection.EventID, inspection.ExecutionID,
		)
	}
	return view
}

func executionRecoveryApplyTemplate(sessionReference string, revision, eventID int64, executionID string) string {
	return fmt.Sprintf(
		"sonar execution recover --apply --revision %d --event-id %d --observation effect_not_applied --source verification_check --reference REF --summary SUMMARY --observed-at RFC3339 %s %s",
		revision, eventID, sessionReference, executionID,
	)
}

func writeExecutionRecovery(writer io.Writer, view executionRecoveryView, sessionHandle string) {
	if sessionHandle == "" {
		sessionHandle = "?"
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	executionFprintf(table, "SESSION\t%s @ revision %d\n", sessionHandle, view.SessionRevision)
	executionFprintf(table, "EXECUTION\t%s\n", terminalSafeGoalText(view.ExecutionID))
	executionFprintf(table, "TOOL\t%s\n", terminalSafeGoalText(view.ToolName))
	executionFprintf(table, "EVENT\t%d · %s · %s\n", view.EventID, view.EventType, view.EffectClass)
	executionFprintf(table, "TURN\t%s\n", terminalSafeGoalText(view.TurnID))
	executionFprintf(table, "ARGUMENTS\t%s\n", terminalSafeGoalText(view.ArgumentsSHA256))
	executionFprintf(table, "ITEM\t%s\n", terminalSafeGoalText(view.ItemID))
	_ = table.Flush()
	if view.Resolved {
		executionFprintf(writer, "Already reconciled by immutable receipt %s. No tool was retried.\n", terminalSafeGoalText(view.ResolutionID))
		return
	}
	executionFprintln(writer, "Inspection is read-only. Verify the external effect independently before applying one disposition.")
	executionFprintln(writer, "Apply template:")
	executionFprintln(writer, "  "+executionRecoveryApplyTemplate(
		sessionHandle, view.SessionRevision, view.EventID, terminalSafeGoalText(view.ExecutionID),
	))
}

func writeExecutionUsage(writer io.Writer) {
	executionFprintln(writer, "Usage:")
	executionFprintln(writer, "  sonar execution recover [--json] SESSION_ID EXECUTION_ID")
	executionFprintln(writer, "  sonar execution recover [--json] SESSION_ID --all")
	executionFprintln(writer, "  sonar execution recover --apply --revision N ... SESSION_ID EXECUTION_ID")
	executionFprintln(writer, "  sonar execution recover --all --apply --set-digest HASH [evidence options] SESSION_ID")
	executionFprintln(writer)
	executionFprintln(writer, "SESSION_ID accepts a 7-character hex handle such as a1b2c3d.")
	executionFprintln(writer)
	executionFprintln(writer, "Safety:")
	executionFprintln(writer, "  Inspection is read-only.")
	executionFprintln(writer, "  Apply requires exact inspection tokens and typed evidence.")
	executionFprintln(writer, "  Batch apply also requires the reviewed --all set digest.")
	executionFprintln(writer, "  It never retries a tool or rewrites the immutable execution ledger.")
	executionFprintln(writer)
	executionFprintln(writer, "Run `sonar execution recover --help` for all options.")
}

func writeExecutionRecoverUsage(writer io.Writer) {
	executionFprintln(writer, "Usage:")
	executionFprintln(writer, "  sonar execution recover [--json] SESSION_ID EXECUTION_ID")
	executionFprintln(writer, "  sonar execution recover [--json] SESSION_ID --all")
	executionFprintln(writer, "  sonar execution recover --apply [evidence options] SESSION_ID EXECUTION_ID")
	executionFprintln(writer, "  sonar execution recover --all --apply --set-digest HASH [evidence options] SESSION_ID")
	executionFprintln(writer)
	executionFprintln(writer, "Options:")
	executionFprintln(writer, "  --json               Print machine-readable JSON")
	executionFprintln(writer, "  --apply              Append typed reconciliation evidence")
	executionFprintln(writer, "  --all                Target every pending reconciliation in the session.")
	executionFprintln(writer, "                       Listing is read-only; with --apply, one shared typed")
	executionFprintln(writer, "                       observation is recorded per execution and exact")
	executionFprintln(writer, "                       revision/event tokens are inspected under the lease.")
	executionFprintln(writer, "                       The exact pending set must match --set-digest")
	executionFprintln(writer, "                       from the prior read-only listing (omit --revision and --event-id).")
	executionFprintln(writer, "  --set-digest HASH    Exact pending-set digest shown by --all inspection")
	executionFprintln(writer, "  -h, --help           Show this help")
	executionFprintln(writer)
	executionFprintln(writer, "Required with --apply:")
	executionFprintln(writer, "  --revision N         Exact session revision shown by inspection")
	executionFprintln(writer, "  --event-id N         Exact latest event ID shown by inspection")
	executionFprintln(writer, "  --observation VALUE")
	executionFprintln(writer, "      effect_applied, effect_not_applied, or effect_compensated")
	executionFprintln(writer, "  --source VALUE")
	executionFprintln(writer, "      external_receipt, workspace_artifact, verification_check, or")
	executionFprintln(writer, "      operator_observation")
	executionFprintln(writer, "  --reference TEXT     Redacted evidence reference")
	executionFprintln(writer, "  --summary TEXT       Bounded inspection summary")
	executionFprintln(writer, "  --observed-at RFC3339")
	executionFprintln(writer, "      Evidence observation time")
	executionFprintln(writer)
	executionFprintln(writer, "Safety:")
	executionFprintln(writer, "  Inspection is read-only unless --apply is explicit.")
	executionFprintln(writer, "  Applying requires every evidence option shown above; --all also requires --set-digest.")
	executionFprintln(writer, "  It never retries a tool or rewrites the immutable execution ledger.")
}

func executionFprintf(writer io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(writer, format, args...)
}

func executionFprintln(writer io.Writer, args ...any) {
	_, _ = fmt.Fprintln(writer, args...)
}

func writeExecutionJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
