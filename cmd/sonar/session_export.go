package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	sessionListLimit    = 100
	sessionExportLimit  = 1000
	sessionExportSchema = "sonar.session-export.v2"
)

type sessionExportStore interface {
	AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error)
	ListSessions(context.Context, db.ListSessionsParams) ([]db.Session, error)
	GetSession(context.Context, int64) (db.Session, error)
	ResolveSessionRef(context.Context, string) (db.Session, error)
	SessionHandle(context.Context, int64) (string, error)
	GetSessionStateForExport(context.Context, int64, int) (string, error)
	ListSessionExecutionEvents(context.Context, int64, string, int) ([]execution.Event, error)
	SessionExecutionEventExists(context.Context, int64, string, int64) (bool, error)
	ListExecutionRecoveryHazards(context.Context, int64, string, int64, int) ([]execution.State, error)
	ListControlStates(context.Context, controlplane.Query) ([]controlplane.State, error)
	ListRecentSessionTokenStats(context.Context, int64, int) ([]db.TokenStat, error)
	ListRecentSessionFileChanges(context.Context, int64, int) ([]db.FileChange, error)
	ListRecentSessionCheckpoints(context.Context, int64, int) ([]db.Checkpoint, error)
}

// sessionExportBundle is the machine-readable audit projection of one session.
// Source records are persisted; authority and bound fields are derived
// read-only metadata. Export does not change durable state.
type sessionExportBundle struct {
	Schema          string                          `json:"schema"`
	ExportedBy      string                          `json:"exported_by"`
	GoalOwned       bool                            `json:"goal_owned"`
	Session         db.Session                      `json:"session"`
	StateJSON       json.RawMessage                 `json:"state_json,omitempty"`
	ExecutionEvents []sessionExportExecutionEvent   `json:"execution_events"`
	ControlStates   []sessionExportControlState     `json:"control_states"`
	TokenStats      []db.TokenStat                  `json:"token_stats"`
	FileChanges     []db.FileChange                 `json:"file_changes"`
	Checkpoints     []sessionExportCheckpoint       `json:"checkpoints"`
	OpenIssues      []sessionExportOpenIssue        `json:"open_issues"`
	Bounds          map[string]sessionExportBounded `json:"bounds"`
}

type sessionExportMetadata struct {
	Schema              string                          `json:"schema"`
	ExportedBy          string                          `json:"exported_by"`
	Projection          string                          `json:"projection"`
	CollectedUnderLease bool                            `json:"collected_under_session_lease"`
	GoalOwned           bool                            `json:"goal_owned"`
	StateJSONIncluded   bool                            `json:"state_json_included"`
	ReviewBeforeSharing bool                            `json:"review_before_sharing"`
	Disclosure          []string                        `json:"disclosure"`
	Bounds              map[string]sessionExportBounded `json:"bounds"`
}

type sessionExportExecutionEvent struct {
	EventID         int64  `json:"event_id"`
	ExecutionID     string `json:"execution_id"`
	TurnID          string `json:"turn_id"`
	ToolName        string `json:"tool_name"`
	Kind            string `json:"kind"`
	EffectClass     string `json:"effect_class"`
	EventType       string `json:"event_type"`
	Approval        string `json:"approval"`
	ArgumentsSHA256 string `json:"arguments_sha256,omitempty"`
	ResultSHA256    string `json:"result_sha256,omitempty"`
	ResultReceipt   string `json:"result_receipt,omitempty"`
	Detail          string `json:"detail,omitempty"`
	OccurredAt      string `json:"occurred_at"`
}

type sessionExportControlState struct {
	ItemID      string `json:"item_id"`
	Kind        string `json:"kind"`
	ExecutionID string `json:"execution_id,omitempty"`
	TurnID      string `json:"turn_id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Resolved    bool   `json:"resolved"`
	Outcome     string `json:"outcome,omitempty"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type sessionExportCheckpoint struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	MsgCount int64  `json:"msg_count"`
	// Full checkpoint message bodies are intentionally omitted; they duplicate
	// the transcript and can be large. The metadata is enough for an audit.
	CreatedAt string `json:"created_at"`
}

type sessionExportOpenIssue struct {
	ExecutionID string `json:"execution_id"`
	ToolName    string `json:"tool_name"`
	EventType   string `json:"event_type"`
	EffectClass string `json:"effect_class"`
	Status      string `json:"status"`
	Remedy      string `json:"remedy"`
}

type sessionExportBounded struct {
	Returned                      int    `json:"returned"`
	Limit                         int    `json:"limit"`
	Selection                     string `json:"selection"`
	BoundReached                  bool   `json:"bound_reached"`
	AdditionalRecordsMayBeOmitted bool   `json:"additional_records_may_be_omitted"`
}

var sessionExportDisclosure = []string{
	"This is a bounded audit projection, not a complete session transcript or full timeline.",
	"Sections are separate bounded database reads collected while the cooperative exclusive session execution lease is held; this is not a single database transaction snapshot.",
	"The JSONL includes raw durable session state (which may contain transcript content), execution receipt/detail text, and workspace or file paths.",
	"The export layer does not perform general-purpose secret or path redaction; review every file before sharing it.",
}

func handleSessionList(store sessionExportStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("session list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeSessionUsage(stdout) }
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	limit := flags.Int("limit", sessionListLimit, "maximum sessions to list")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if *limit <= 0 || *limit > sessionListLimit {
		executionFprintf(stderr, "session list: --limit must be between 1 and %d\n", sessionListLimit)
		return 2
	}
	sessions, err := store.ListSessions(context.Background(), db.ListSessionsParams{
		WorkspaceID: workspace, Limit: int64(*limit),
	})
	if err != nil {
		executionFprintf(stderr, "session list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if sessions == nil {
			sessions = []db.Session{}
		}
		if err := writeExecutionJSON(stdout, sessions); err != nil {
			executionFprintf(stderr, "session list: %v\n", err)
			return 1
		}
		return 0
	}
	if len(sessions) == 0 {
		executionFprintf(stdout, "No sessions in workspace %s.\n", relativizeHome(workspace))
		return 0
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	executionFprintf(table, "SESSION\tMODEL\tMODE\tUPDATED\tTITLE\n")
	for _, session := range sessions {
		executionFprintf(table, "%s\t%s\t%s\t%s\t%s\n",
			sessionDisplayHandle(session), terminalSafeGoalText(session.Model), terminalSafeGoalText(session.Mode),
			terminalSafeGoalText(session.UpdatedAt), terminalSafeGoalText(sessionListTitle(session.Title)))
	}
	_ = table.Flush()
	executionFprintln(stdout, "\nExport one with: sonar session export SESSION_ID")
	return 0
}

func handleSessionExport(store sessionExportStore, workspace string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("session export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeSessionUsage(stdout) }
	format := flags.String("format", "both", "output format: jsonl, md, or both")
	outDir := flags.String("out", "", "output directory (default: ./sonar-audit-<id>)")
	normalized, err := reorderFlagsBeforePositionals(args, map[string]bool{"format": true, "out": true})
	if err != nil {
		executionFprintf(stderr, "session export: %v\n", err)
		return 2
	}
	if code, done := flagParseExitCode(flags.Parse(normalized)); done {
		return code
	}
	if flags.NArg() != 1 {
		executionFprintln(stderr, "session export: provide SESSION_ID")
		return 2
	}
	session, err := resolveSessionArg(context.Background(), store, flags.Arg(0))
	if err != nil {
		executionFprintf(stderr, "session export: %v\n", err)
		return 2
	}
	sessionID := session.ID
	wantJSONL, wantMD := false, false
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "both", "":
		wantJSONL, wantMD = true, true
	case "jsonl":
		wantJSONL = true
	case "md", "markdown":
		wantMD = true
	default:
		executionFprintf(stderr, "session export: unknown --format %q (want jsonl, md, or both)\n", *format)
		return 2
	}

	bundle, err := collectSessionExport(context.Background(), store, workspace, sessionID)
	if err != nil {
		executionFprintf(stderr, "session export: %v\n", err)
		return 1
	}

	directory := strings.TrimSpace(*outDir)
	if directory == "" {
		directory = fmt.Sprintf("sonar-audit-%d", sessionID)
	}
	if err := prepareSessionExportDirectory(directory); err != nil {
		executionFprintf(stderr, "session export: prepare output directory: %v\n", err)
		return 1
	}
	written := make([]string, 0, 2)
	if wantJSONL {
		path := filepath.Join(directory, fmt.Sprintf("session-%d.jsonl", sessionID))
		if err := writeSessionExportJSONL(path, bundle); err != nil {
			executionFprintf(stderr, "session export: %v\n", err)
			return 1
		}
		written = append(written, path)
	}
	if wantMD {
		path := filepath.Join(directory, fmt.Sprintf("session-%d-summary.md", sessionID))
		if err := writeSessionExportMarkdown(path, bundle); err != nil {
			executionFprintf(stderr, "session export: %v\n", err)
			return 1
		}
		written = append(written, path)
	}
	executionFprintf(stdout, "Exported session %d (%d execution events, %d open issue(s)).\n",
		sessionID, len(bundle.ExecutionEvents), len(bundle.OpenIssues))
	for _, path := range written {
		executionFprintf(stdout, "  %s\n", path)
	}
	if len(bundle.OpenIssues) > 0 {
		if wantMD {
			executionFprintln(stdout, "This session has execution recovery issues; see the Open Issues section of the summary.")
		} else {
			executionFprintln(stdout, "This session has execution recovery issues; see the open_issue records in the JSONL export.")
		}
	}
	executionFprintln(stdout, "Review before sharing: exports may contain raw session content, receipt/detail text, and paths.")
	if sessionExportMayBeTruncated(bundle) {
		switch {
		case wantJSONL && wantMD:
			executionFprintln(stdout, "One or more export bounds were reached; additional records may be omitted. See JSONL metadata and Markdown Export bounds.")
		case wantJSONL:
			executionFprintln(stdout, "One or more export bounds were reached; additional records may be omitted. See the JSONL metadata record.")
		default:
			executionFprintln(stdout, "One or more export bounds were reached; additional records may be omitted. See the Markdown Export bounds section.")
		}
	}
	return 0
}

func collectSessionExport(ctx context.Context, store sessionExportStore, workspace string, sessionID int64) (bundle sessionExportBundle, err error) {
	lease, err := store.AcquireExecutionSessionLease(ctx, sessionID, workspace)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("acquire exact session lease: %w", err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			bundle = sessionExportBundle{}
			err = errors.Join(err, fmt.Errorf("release exact session lease: %w", closeErr))
		}
	}()

	session, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("read session %d: %w", sessionID, err)
	}
	if session.WorkspaceID != workspace {
		return sessionExportBundle{}, fmt.Errorf("session %d belongs to a different workspace", sessionID)
	}
	bundle = sessionExportBundle{
		Schema: sessionExportSchema, ExportedBy: "sonar session export",
		Session: session, Bounds: map[string]sessionExportBounded{},
	}
	executionCursor := int64(0)
	stateAvailable := false

	if raw, stateErr := store.GetSessionStateForExport(ctx, sessionID, db.MaxSessionExportStateBytes); stateErr == nil {
		state, inspectErr := db.InspectSessionRecoveryState(sessionID, raw)
		if inspectErr != nil {
			return sessionExportBundle{}, fmt.Errorf("read session %d state: %w", sessionID, inspectErr)
		}
		bundle.StateJSON = json.RawMessage(raw)
		bundle.Bounds["state_json_bytes"] = newFailClosedSessionExportBound(
			len(raw), db.MaxSessionExportStateBytes, "complete durable state bytes; oversize state fails the export",
		)
		bundle.GoalOwned = state.GoalOwned
		executionCursor = state.ExecutionCursor
		stateAvailable = true
	} else if !errors.Is(stateErr, db.ErrSessionStateNotFound) {
		return sessionExportBundle{}, fmt.Errorf("read session %d state: %w", sessionID, stateErr)
	} else {
		bundle.Bounds["state_json_bytes"] = newFailClosedSessionExportBound(
			0, db.MaxSessionExportStateBytes, "no durable state record",
		)
	}

	events, err := store.ListSessionExecutionEvents(ctx, sessionID, workspace, sessionExportLimit)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("list execution events: %w", err)
	}
	latestExecutionEventID := int64(0)
	for _, event := range events {
		if event.ID > latestExecutionEventID {
			latestExecutionEventID = event.ID
		}
	}
	if executionCursor > latestExecutionEventID {
		return sessionExportBundle{}, fmt.Errorf(
			"session execution cursor %d exceeds latest durable execution event %d",
			executionCursor, latestExecutionEventID,
		)
	}
	if executionCursor > 0 {
		exists, existsErr := store.SessionExecutionEventExists(ctx, sessionID, workspace, executionCursor)
		if existsErr != nil {
			return sessionExportBundle{}, fmt.Errorf("validate session execution cursor %d: %w", executionCursor, existsErr)
		}
		if !exists {
			return sessionExportBundle{}, fmt.Errorf(
				"session execution cursor %d does not identify a durable execution event in this session/workspace",
				executionCursor,
			)
		}
	}
	bundle.Bounds["execution_events"] = newSessionExportBound(
		len(events), sessionExportLimit, "most recent events, returned in chronological order",
	)
	for _, event := range events {
		bundle.ExecutionEvents = append(bundle.ExecutionEvents, sessionExportExecutionEvent{
			EventID: event.ID, ExecutionID: event.Identity.ExecutionID, TurnID: event.Identity.TurnID,
			ToolName: event.Identity.ToolName, Kind: string(event.Identity.Kind),
			EffectClass: string(event.Identity.EffectClass), EventType: string(event.Type),
			Approval: string(event.Approval), ArgumentsSHA256: event.ArgumentsSHA256,
			ResultSHA256: event.ResultSHA256, ResultReceipt: event.ResultReceipt,
			Detail: event.Detail, OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}

	states, err := store.ListControlStates(ctx, controlplane.Query{
		SessionID: sessionID, WorkspaceID: workspace, Limit: controlplane.MaxListLimit,
	})
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("list control states: %w", err)
	}
	bundle.Bounds["control_states"] = newSessionExportBound(
		len(states), controlplane.MaxListLimit, "newest records first",
	)
	for _, state := range states {
		view := sessionExportControlState{
			ItemID: state.Item.ItemID, Kind: string(state.Item.Kind),
			ExecutionID: state.Item.Identity.ExecutionID, TurnID: state.Item.Identity.TurnID,
			Summary: state.Item.Summary, Resolved: !state.Pending(),
			CreatedAt: state.Item.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		if state.Resolution != nil {
			view.Outcome = string(state.Resolution.Outcome)
			view.ResolvedBy = state.Resolution.ResolvedBy
		}
		bundle.ControlStates = append(bundle.ControlStates, view)
	}

	stats, err := store.ListRecentSessionTokenStats(ctx, sessionID, db.MaxSessionExportReadLimit)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("list token stats: %w", err)
	}
	bundle.TokenStats = stats
	bundle.Bounds["token_stats"] = newSessionExportBound(
		len(stats), db.MaxSessionExportReadLimit, "most recent records, returned in chronological order",
	)
	changes, err := store.ListRecentSessionFileChanges(ctx, sessionID, db.MaxSessionExportReadLimit)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("list file changes: %w", err)
	}
	bundle.FileChanges = changes
	bundle.Bounds["file_changes"] = newSessionExportBound(
		len(changes), db.MaxSessionExportReadLimit, "most recent records, returned in chronological order",
	)
	checkpoints, err := store.ListRecentSessionCheckpoints(ctx, sessionID, db.MaxSessionExportReadLimit)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("list checkpoints: %w", err)
	}
	bundle.Bounds["checkpoints"] = newSessionExportBound(
		len(checkpoints), db.MaxSessionExportReadLimit, "most recent records, newest first",
	)
	for _, cp := range checkpoints {
		bundle.Checkpoints = append(bundle.Checkpoints, sessionExportCheckpoint{
			ID: cp.ID, Label: cp.Label, Kind: cp.Kind, MsgCount: cp.MsgCount, CreatedAt: cp.CreatedAt,
		})
	}

	// Open issues come from the SAME authoritative hazard projection the agent
	// runtime uses to decide whether to block: it applies the snapshot cursor
	// (so a completed/failed effect newer than the saved transcript is flagged),
	// the reconciliation overlay (so only validly-reconciled executions are
	// cleared), and is a bounded latest-state-per-execution view (immune to the
	// timeline's event truncation).
	hazards, err := store.ListExecutionRecoveryHazards(ctx, sessionID, workspace, executionCursor, maxSessionExportHazards)
	if err != nil {
		return sessionExportBundle{}, fmt.Errorf("project execution recovery hazards: %w", err)
	}
	if !stateAvailable && len(hazards) > 0 {
		return sessionExportBundle{}, errors.New("durable session state is missing, so recovery authority and exact remedies cannot be established")
	}
	bundle.Bounds["open_issues"] = newFailClosedSessionExportBound(
		len(hazards), maxSessionExportHazards, "authoritative effective recovery hazards; overflow fails the export",
	)
	for _, hazard := range hazards {
		status, remedy := openIssueStatus(hazard.Latest.Type, hazard.Identity.EffectClass, sessionID, bundle.GoalOwned)
		if status == "" {
			continue
		}
		bundle.OpenIssues = append(bundle.OpenIssues, sessionExportOpenIssue{
			ExecutionID: hazard.Identity.ExecutionID, ToolName: hazard.Identity.ToolName,
			EventType: string(hazard.Latest.Type), EffectClass: string(hazard.Identity.EffectClass),
			Status: status, Remedy: remedy,
		})
	}
	return bundle, nil
}

const maxSessionExportHazards = 100

// openIssueStatus classifies a recovery hazard the same way the agent runtime's
// executionRuntime does, and names the correct recovery authority. Goal-owned
// hazards always route first to the universally available read-only goal
// summary. For ordinary
// sessions, unknown/started hazards need reconciliation evidence while an
// answered terminal newer than the snapshot needs projection repair.
func openIssueStatus(eventType execution.EventType, effect execution.EffectClass, sessionID int64, goalOwned bool) (status, remedy string) {
	if goalOwned {
		remedy = fmt.Sprintf("sonar goal show %d", sessionID)
		switch {
		case eventType == execution.EventOutcomeUnknown:
			return "GOAL-OWNED — outcome unknown; inspect the owning goal before recovery", remedy
		case eventType == execution.EventStarted && effect != execution.EffectReadOnly:
			return "GOAL-OWNED — dispatch has no terminal receipt; inspect the owning goal before recovery", remedy
		case (eventType == execution.EventCompleted || eventType == execution.EventFailed) && effect != execution.EffectReadOnly:
			return "GOAL-OWNED — answered effect is newer than saved goal state; inspect the owning goal before recovery", remedy
		default:
			return "", ""
		}
	}
	switch {
	case eventType == execution.EventOutcomeUnknown:
		return "UNRESOLVED — outcome unknown, needs reconciliation",
			fmt.Sprintf("sonar execution recover %d --all", sessionID)
	case eventType == execution.EventStarted && effect != execution.EffectReadOnly:
		return "UNRESOLVED — started without a terminal receipt",
			fmt.Sprintf("sonar execution recover %d --all", sessionID)
	case (eventType == execution.EventCompleted || eventType == execution.EventFailed) && effect != execution.EffectReadOnly:
		return "UNPROJECTED — answered effect newer than the saved transcript",
			fmt.Sprintf("sonar session repair %d", sessionID)
	default:
		return "", ""
	}
}

func newSessionExportBound(returned, limit int, selection string) sessionExportBounded {
	reached := returned >= limit
	return sessionExportBounded{
		Returned: returned, Limit: limit, Selection: selection,
		BoundReached: reached, AdditionalRecordsMayBeOmitted: reached,
	}
}

func newFailClosedSessionExportBound(returned, limit int, selection string) sessionExportBounded {
	bound := newSessionExportBound(returned, limit, selection)
	bound.AdditionalRecordsMayBeOmitted = false
	return bound
}

func sessionExportMayBeTruncated(bundle sessionExportBundle) bool {
	for _, bound := range bundle.Bounds {
		if bound.AdditionalRecordsMayBeOmitted {
			return true
		}
	}
	return false
}

func writeSessionExportJSONL(path string, bundle sessionExportBundle) error {
	return writePrivateSessionExport(path, "JSONL export", func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		emit := func(kind string, value any) error {
			return encoder.Encode(map[string]any{"kind": kind, "value": value})
		}
		metadata := sessionExportMetadata{
			Schema: bundle.Schema, ExportedBy: bundle.ExportedBy,
			Projection: "bounded_audit_projection", CollectedUnderLease: true, GoalOwned: bundle.GoalOwned,
			StateJSONIncluded:   len(bundle.StateJSON) > 0,
			ReviewBeforeSharing: true, Disclosure: append([]string(nil), sessionExportDisclosure...),
			Bounds: bundle.Bounds,
		}
		if err := emit("metadata", metadata); err != nil {
			return err
		}
		if err := emit("session", bundle.Session); err != nil {
			return err
		}
		if len(bundle.StateJSON) > 0 {
			if err := emit("state_json", bundle.StateJSON); err != nil {
				return err
			}
		}
		for _, event := range bundle.ExecutionEvents {
			if err := emit("execution_event", event); err != nil {
				return err
			}
		}
		for _, state := range bundle.ControlStates {
			if err := emit("control_state", state); err != nil {
				return err
			}
		}
		for _, stat := range bundle.TokenStats {
			if err := emit("token_stat", stat); err != nil {
				return err
			}
		}
		for _, change := range bundle.FileChanges {
			if err := emit("file_change", change); err != nil {
				return err
			}
		}
		for _, checkpoint := range bundle.Checkpoints {
			if err := emit("checkpoint", checkpoint); err != nil {
				return err
			}
		}
		for _, issue := range bundle.OpenIssues {
			if err := emit("open_issue", issue); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeSessionExportMarkdown(path string, bundle sessionExportBundle) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Session %d audit\n\n", bundle.Session.ID)
	b.WriteString("> **Review before sharing.** This is a bounded audit projection, not a complete transcript or full timeline. ")
	b.WriteString("Sections are separate bounded reads collected while the session execution lease is held, not one database transaction snapshot. ")
	b.WriteString("This summary can include session metadata and recorded paths. A JSONL export additionally includes raw durable session state ")
	b.WriteString("(which may contain transcript content), execution receipt/detail text, and paths. ")
	b.WriteString("The export layer does not perform general-purpose secret or path redaction.\n\n")
	fmt.Fprintf(&b, "- **Title:** %s\n", markdownSafe(bundle.Session.Title))
	fmt.Fprintf(&b, "- **Workspace:** %s\n", markdownSafe(relativizeHome(bundle.Session.WorkspaceID)))
	fmt.Fprintf(&b, "- **Model:** %s\n", markdownSafe(bundle.Session.Model))
	fmt.Fprintf(&b, "- **Mode:** %s\n", markdownSafe(bundle.Session.Mode))
	fmt.Fprintf(&b, "- **Created:** %s\n", markdownSafe(bundle.Session.CreatedAt))
	fmt.Fprintf(&b, "- **Updated:** %s\n", markdownSafe(bundle.Session.UpdatedAt))
	fmt.Fprintf(&b, "- **Execution events:** %d\n", len(bundle.ExecutionEvents))
	fmt.Fprintf(&b, "- **Schema:** `%s`\n\n", bundle.Schema)

	b.WriteString("## Open issues\n\n")
	if len(bundle.OpenIssues) == 0 {
		b.WriteString("None — the authoritative recovery projection found no unresolved outcome, ")
		b.WriteString("effectful dispatch without a receipt, or unprojected answered effect that blocks continuation.\n\n")
	} else {
		b.WriteString("These executions block continuation. Status and the safe next step per row:\n\n")
		if bundle.GoalOwned {
			b.WriteString("For goal-owned rows, start with the read-only `goal show` command below. If a durable reconciliation group already exists, `sonar goal recover SESSION_ID` inspects it; otherwise reopen the owning session so its recovery coordinator can establish the group.\n\n")
		}
		b.WriteString("| Execution | Tool | Latest event | Status | Remedy |\n|---|---|---|---|---|\n")
		for _, issue := range bundle.OpenIssues {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | `%s` |\n",
				markdownSafe(issue.ExecutionID), markdownSafe(issue.ToolName),
				markdownSafe(issue.EventType), markdownSafe(issue.Status), markdownSafe(issue.Remedy))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Execution events (bounded)\n\n")
	if bound, ok := bundle.Bounds["execution_events"]; ok && bound.AdditionalRecordsMayBeOmitted {
		fmt.Fprintf(&b, "The %d-event export bound was reached; earlier events may be omitted.\n\n", bound.Limit)
	}
	if len(bundle.ExecutionEvents) == 0 {
		b.WriteString("No execution events recorded.\n\n")
	} else {
		b.WriteString("| Event | Tool | Type | Effect | Approval | Occurred |\n|---|---|---|---|---|---|\n")
		for _, event := range bundle.ExecutionEvents {
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s |\n",
				event.EventID, markdownSafe(event.ToolName), markdownSafe(event.EventType),
				markdownSafe(event.EffectClass), markdownSafe(event.Approval), markdownSafe(event.OccurredAt))
		}
		b.WriteString("\n")
	}

	if len(bundle.ControlStates) > 0 {
		b.WriteString("## Control-plane records\n\n")
		b.WriteString("| Item | Kind | Resolved | Outcome |\n|---|---|---|---|\n")
		for _, state := range bundle.ControlStates {
			fmt.Fprintf(&b, "| `%s` | %s | %t | %s |\n",
				markdownSafe(state.ItemID), markdownSafe(state.Kind), state.Resolved, markdownSafe(state.Outcome))
		}
		b.WriteString("\n")
	}

	if len(bundle.FileChanges) > 0 {
		b.WriteString("## File changes\n\n")
		b.WriteString("| Path | Tool | +Added | -Removed |\n|---|---|---|---|\n")
		for _, change := range bundle.FileChanges {
			fmt.Fprintf(&b, "| %s | %s | %d | %d |\n",
				markdownSafe(relativizeHome(change.FilePath)), markdownSafe(change.ToolName), change.Added, change.Removed)
		}
		b.WriteString("\n")
	}

	if len(bundle.Bounds) > 0 {
		b.WriteString("## Export bounds\n\n")
		b.WriteString("| Section | Returned | Limit | Selection | Truncation |\n|---|---:|---:|---|---|\n")
		for _, section := range []string{
			"state_json_bytes", "execution_events", "control_states", "token_stats", "file_changes", "checkpoints", "open_issues",
		} {
			bound, ok := bundle.Bounds[section]
			if !ok {
				continue
			}
			truncation := "bound not reached"
			switch {
			case bound.AdditionalRecordsMayBeOmitted:
				truncation = "bound reached; additional records may be omitted"
			case bound.BoundReached:
				truncation = "bound reached; complete because overflow fails the export"
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %s | %s |\n",
				markdownSafe(section), bound.Returned, bound.Limit,
				markdownSafe(bound.Selection), markdownSafe(truncation))
		}
		b.WriteString("\n")
	}

	b.WriteString("---\nUse `--format jsonl` or `--format both` to generate the bounded machine-readable audit projection. Review every export before sharing.\n")
	return writePrivateSessionExport(path, "markdown summary", func(writer io.Writer) error {
		_, err := io.WriteString(writer, b.String())
		return err
	})
}

func prepareSessionExportDirectory(path string) error {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", safeio.ErrSymlink, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("output path is not a directory: %s", path)
	}
	if created {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure output directory: %w", err)
		}
	}
	return nil
}

func writePrivateSessionExport(path, label string, write func(io.Writer) error) (err error) {
	if err := safeio.ValidatePublishPath(path); err != nil {
		return fmt.Errorf("validate %s path: %w", label, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create private %s: %w", label, err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure private %s: %w", label, err)
	}
	if err := write(temporary); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", label, err), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", label, err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", label, err)
	}
	if err := safeio.ValidatePublishPath(path); err != nil {
		return fmt.Errorf("revalidate %s path: %w", label, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", label, err)
	}
	published = true
	return nil
}

func sessionListTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "(untitled)"
	}
	if len([]rune(title)) > 48 {
		return string([]rune(title)[:45]) + "..."
	}
	return title
}

// reorderFlagsBeforePositionals moves flags ahead of positional arguments so
// `SESSION_ID --flag` parses like `--flag SESSION_ID`, while keeping each
// value-taking flag bound to its value. valueFlags names the flags that consume
// the following argument (unless given as --flag=value).
func reorderFlagsBeforePositionals(args []string, valueFlags map[string]bool) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			positional = append(positional, argument)
			continue
		}
		name := strings.TrimLeft(argument, "-")
		bare, _, hasValue := strings.Cut(name, "=")
		flags = append(flags, argument)
		if valueFlags[bare] && !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--%s requires a value", bare)
			}
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...), nil
}

// relativizeHome shortens the caller's exact home-directory prefix for
// Markdown display. It is a readability aid, not general path redaction.
func relativizeHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// markdownSafe escapes characters that would break a table cell or inject
// Markdown/HTML formatting from a value that may contain server-authored text.
func markdownSafe(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\", "|", "\\|", "\n", " ", "\r", " ", "`", "'",
		"[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "!", "\\!",
		"<", "\\<", ">", "\\>", "&", "\\&", "*", "\\*",
		"#", "\\#", "~", "\\~", "://", "\\://",
	)
	return replacer.Replace(terminalSafeGoalText(value))
}
