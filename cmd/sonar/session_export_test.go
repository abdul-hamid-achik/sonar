package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

type fakeSessionExportStore struct {
	sessions []db.Session
	session  db.Session
	stateErr error
	stateRaw string
	events   []execution.Event
	hazards  []execution.State
	cursor   int64
	states   []controlplane.State
	tokens   []db.TokenStat
	tokenErr error
	files    []db.FileChange
	fileErr  error
	cpErr    error
	leaseErr error
	leases   int
}

func (s *fakeSessionExportStore) ResolveSessionRef(_ context.Context, ref string) (db.Session, error) {
	publicID, err := sessionref.Parse(ref)
	if err != nil {
		return db.Session{}, err
	}
	candidates := make([]db.Session, 0, 1+len(s.sessions))
	if s.session.ID != 0 {
		candidates = append(candidates, s.session)
	}
	candidates = append(candidates, s.sessions...)
	for _, session := range candidates {
		pid := session.PublicID
		if pid == "" {
			pid = fmt.Sprintf("%07x", session.ID)
		}
		if publicID == pid {
			session.PublicID = pid
			return session, nil
		}
	}
	return db.Session{}, fmt.Errorf("session %s: not found", publicID)
}

func (s *fakeSessionExportStore) SessionHandle(_ context.Context, sessionID int64) (string, error) {
	if s.session.ID == sessionID {
		if s.session.PublicID != "" {
			return s.session.PublicID, nil
		}
		return fmt.Sprintf("%07x", sessionID), nil
	}
	for _, session := range s.sessions {
		if session.ID == sessionID {
			if session.PublicID != "" {
				return session.PublicID, nil
			}
			return fmt.Sprintf("%07x", sessionID), nil
		}
	}
	return fmt.Sprintf("%07x", sessionID), nil
}

func (s *fakeSessionExportStore) AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error) {
	if s.leaseErr != nil {
		return nil, s.leaseErr
	}
	s.leases++
	return &db.ExecutionSessionLease{}, nil
}

func (s *fakeSessionExportStore) ListSessions(context.Context, db.ListSessionsParams) ([]db.Session, error) {
	return s.sessions, nil
}
func (s *fakeSessionExportStore) GetSession(context.Context, int64) (db.Session, error) {
	return s.session, nil
}
func (s *fakeSessionExportStore) GetSessionStateForExport(_ context.Context, _ int64, maxBytes int) (string, error) {
	if s.stateErr != nil {
		return "", s.stateErr
	}
	if s.stateRaw == "" {
		return "", db.ErrSessionStateNotFound
	}
	if len(s.stateRaw) > maxBytes {
		return "", db.ErrSessionExportStateTooLarge
	}
	return s.stateRaw, nil
}
func (s *fakeSessionExportStore) ListSessionExecutionEvents(context.Context, int64, string, int) ([]execution.Event, error) {
	return s.events, nil
}
func (s *fakeSessionExportStore) SessionExecutionEventExists(_ context.Context, _ int64, _ string, eventID int64) (bool, error) {
	for _, event := range s.events {
		if event.ID == eventID {
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeSessionExportStore) ListExecutionRecoveryHazards(_ context.Context, _ int64, _ string, afterEventID int64, _ int) ([]execution.State, error) {
	s.cursor = afterEventID
	return s.hazards, nil
}
func (s *fakeSessionExportStore) ListControlStates(context.Context, controlplane.Query) ([]controlplane.State, error) {
	return s.states, nil
}
func (s *fakeSessionExportStore) ListRecentSessionTokenStats(context.Context, int64, int) ([]db.TokenStat, error) {
	return s.tokens, s.tokenErr
}
func (s *fakeSessionExportStore) ListRecentSessionFileChanges(context.Context, int64, int) ([]db.FileChange, error) {
	return s.files, s.fileErr
}
func (s *fakeSessionExportStore) ListRecentSessionCheckpoints(context.Context, int64, int) ([]db.Checkpoint, error) {
	return nil, s.cpErr
}

func exportTestEvent(execID, tool string, id int64, effect execution.EffectClass, eventType execution.EventType) execution.Event {
	return execution.Event{
		ID: id,
		Identity: execution.Identity{
			SessionID: 7, WorkspaceID: "/workspace/repo", ExecutionID: execID,
			TurnID: "turn-1", ToolName: tool, Kind: execution.KindMCP, EffectClass: effect,
		},
		Type: eventType, Approval: execution.ApprovalNotApplicable,
		OccurredAt: time.Date(2026, 7, 14, 9, 52, 0, 0, time.UTC),
	}
}

func exportTestGoalState(t *testing.T, sessionID, cursor int64) string {
	t.Helper()
	runtime, err := goal.New(goal.Spec{
		SessionID: sessionID, Objective: "recover safely",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "safe", Description: "Recovery remains explicit."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"version": 2, "execution_cursor": cursor, "goal": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSessionExportWritesJSONLAndMarkdownWithOpenIssues(t *testing.T) {
	stuck := exportTestEvent("exec_stuck", "mcphub__bob__bob_check", 2, execution.EffectUnknown, execution.EventOutcomeUnknown)
	store := &fakeSessionExportStore{
		session: db.Session{
			ID: 7, Title: "investigate bob", Model: "deepseek-v4-pro:cloud",
			Mode: "AUTO", WorkspaceID: "/workspace/repo",
			CreatedAt: "2026-07-14T09:51:00Z", UpdatedAt: "2026-07-14T09:52:00Z",
		},
		stateRaw: `{"version":2,"messages":[],"execution_cursor":1}`,
		events: []execution.Event{
			exportTestEvent("exec_ok", "mcphub__bob__bob_inspect", 1, execution.EffectReadOnly, execution.EventCompleted),
			stuck,
		},
		// Open issues come from the authoritative hazard projection, not the raw
		// events; the runtime would surface exactly this stuck execution.
		hazards: []execution.State{{Identity: stuck.Identity, Latest: stuck}},
		files:   []db.FileChange{{FilePath: "/workspace/repo/main.go", ToolName: "write", Added: 3, Removed: 1}},
	}
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := handleSessionExport(store, "/workspace/repo", []string{"--out", dir, "0000007"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 open issue(s)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if store.cursor != 1 {
		t.Fatalf("hazard query used cursor %d, want the persisted 1", store.cursor)
	}

	jsonlPath := filepath.Join(dir, "session-7.jsonl")
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	var metadata sessionExportMetadata
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var row struct {
			Kind  string          `json:"kind"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", scanner.Text(), err)
		}
		kinds[row.Kind]++
		if row.Kind == "metadata" {
			if err := json.Unmarshal(row.Value, &metadata); err != nil {
				t.Fatalf("decode metadata: %v", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"metadata", "session", "state_json", "execution_event", "file_change", "open_issue"} {
		if kinds[kind] == 0 {
			t.Fatalf("JSONL missing %q record: %#v", kind, kinds)
		}
	}
	if kinds["execution_event"] != 2 || kinds["open_issue"] != 1 {
		t.Fatalf("JSONL record counts = %#v", kinds)
	}
	if metadata.Schema != sessionExportSchema || metadata.Projection != "bounded_audit_projection" ||
		!metadata.CollectedUnderLease || !metadata.ReviewBeforeSharing || !metadata.StateJSONIncluded || metadata.GoalOwned {
		t.Fatalf("JSONL metadata = %#v", metadata)
	}
	if store.leases != 1 {
		t.Fatalf("session export leases = %d, want 1", store.leases)
	}
	if bound := metadata.Bounds["execution_events"]; bound.Limit != sessionExportLimit ||
		bound.Returned != 2 || bound.AdditionalRecordsMayBeOmitted {
		t.Fatalf("execution event bound = %#v", bound)
	}
	if len(metadata.Disclosure) == 0 || !strings.Contains(strings.Join(metadata.Disclosure, " "), "review every file") {
		t.Fatalf("JSONL disclosure = %#v", metadata.Disclosure)
	}

	md, err := os.ReadFile(filepath.Join(dir, "session-7-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Session 7 audit", "investigate bob", "## Open issues",
		"UNRESOLVED", "exec_stuck", "execution recover 7 --all", "## Execution events (bounded)",
		"## Export bounds", "Review before sharing", "main.go",
	} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown missing %q", want)
		}
	}
	if strings.Contains(string(md), "full machine-readable timeline") ||
		!strings.Contains(stdout.String(), "Review before sharing") {
		t.Fatalf("export retained unsafe completeness/share copy: stdout=%q md=%s", stdout.String(), md)
	}
}

func TestMarkdownSafeEscapesUntrustedMarkup(t *testing.T) {
	value := `![remote](https://attacker.test/x.png) <img src="https://attacker.test/y"> [click](https://attacker.test) | ` + "`code`"
	safe := markdownSafe(value)
	want := `\!\[remote\]\(https\://attacker.test/x.png\) \<img src="https\://attacker.test/y"\> \[click\]\(https\://attacker.test\) \| 'code'`
	if safe != want {
		t.Fatalf("markdownSafe = %q, want %q", safe, want)
	}
}

func TestSessionExportFlagsUnprojectedAnsweredEffect(t *testing.T) {
	// A completed, non-read-only effect newer than the snapshot cursor is the
	// projection-repair wedge class: the runtime blocks on it, so the audit must
	// flag it and point at `session repair`, not `execution recover`.
	answered := exportTestEvent("exec_crash", "write", 9, execution.Effectful, execution.EventCompleted)
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 3, WorkspaceID: "/workspace/repo"},
		stateRaw: `{"version":2,"execution_cursor":4}`,
		events: []execution.Event{
			exportTestEvent("exec_cursor", "read", 4, execution.EffectReadOnly, execution.EventCompleted),
			answered,
		},
		hazards: []execution.State{{Identity: answered.Identity, Latest: answered}},
	}
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := handleSessionExport(store, "/workspace/repo", []string{"--format", "md", "--out", dir, "0000003"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit stderr=%q", stderr.String())
	}
	md, err := os.ReadFile(filepath.Join(dir, "session-3-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"UNPROJECTED", "exec_crash", "session repair 3"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown missing %q: %s", want, md)
		}
	}
	if strings.Contains(string(md), "None — every execution") {
		t.Fatalf("unprojected answered effect reported as no open issues: %s", md)
	}
}

func TestSessionExportGoalOwnedHazardsUseGoalRecoveryInspector(t *testing.T) {
	unknown := exportTestEvent("exec_unknown", "write", 6, execution.EffectUnknown, execution.EventOutcomeUnknown)
	answered := exportTestEvent("exec_answered", "bash", 7, execution.Effectful, execution.EventFailed)
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 11, WorkspaceID: "/workspace/repo"},
		stateRaw: exportTestGoalState(t, 11, 5),
		events: []execution.Event{
			exportTestEvent("exec_cursor", "read", 5, execution.EffectReadOnly, execution.EventCompleted),
			unknown,
			answered,
		},
		hazards: []execution.State{
			{Identity: unknown.Identity, Latest: unknown},
			{Identity: answered.Identity, Latest: answered},
		},
	}
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := handleSessionExport(store, "/workspace/repo", []string{"--format", "md", "--out", dir, "000000b"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	md, err := os.ReadFile(filepath.Join(dir, "session-11-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GOAL-OWNED", "outcome unknown", "answered effect is newer than saved goal state",
		"sonar goal show 11", "sonar goal recover SESSION_ID",
	} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("goal-owned summary missing %q: %s", want, md)
		}
	}
	for _, refused := range []string{"sonar execution recover", "sonar session repair", "`sonar goal recover 11`"} {
		if strings.Contains(string(md), refused) {
			t.Fatalf("goal-owned summary recommends refused command %q: %s", refused, md)
		}
	}
}

func TestSessionExportRejectsCursorBeyondDurableExecutionLedger(t *testing.T) {
	event := exportTestEvent("exec_latest", "write", 9, execution.Effectful, execution.EventCompleted)
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 7, WorkspaceID: "/workspace/repo"},
		stateRaw: `{"version":2,"goal":null,"execution_cursor":10}`,
		events:   []execution.Event{event},
	}
	var stdout, stderr bytes.Buffer
	code := handleSessionExport(store, "/workspace/repo", []string{"--out", t.TempDir(), "0000007"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "cursor 10 exceeds latest durable execution event 9") {
		t.Fatalf("over-cursor exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSessionExportRejectsCursorOwnedByAnotherSession(t *testing.T) {
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 7, WorkspaceID: "/workspace/repo"},
		stateRaw: `{"version":2,"goal":null,"execution_cursor":5}`,
		events: []execution.Event{
			exportTestEvent("exec_effect", "write", 1, execution.Effectful, execution.EventCompleted),
			exportTestEvent("exec_latest", "read", 10, execution.EffectReadOnly, execution.EventCompleted),
		},
	}
	var stdout, stderr bytes.Buffer
	code := handleSessionExport(store, "/workspace/repo", []string{"--out", t.TempDir(), "0000007"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "cursor 5 does not identify a durable execution event in this session/workspace") {
		t.Fatalf("foreign-cursor exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSessionExportMakesReachedBoundsVisible(t *testing.T) {
	events := make([]execution.Event, 0, sessionExportLimit)
	for index := 0; index < sessionExportLimit; index++ {
		events = append(events, exportTestEvent(
			"exec_bound", "read", int64(index+1), execution.EffectReadOnly, execution.EventCompleted,
		))
	}
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 12, WorkspaceID: "/workspace/repo"},
		stateRaw: `{"version":2,"execution_cursor":1000}`,
		events:   events,
	}
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := handleSessionExport(store, "/workspace/repo", []string{"--out", dir, "000000c"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "additional records may be omitted") {
		t.Fatalf("stdout hides reached export bound: %s", stdout.String())
	}

	jsonl, err := os.Open(filepath.Join(dir, "session-12.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jsonl.Close() }()
	var row struct {
		Kind  string                `json:"kind"`
		Value sessionExportMetadata `json:"value"`
	}
	if err := json.NewDecoder(jsonl).Decode(&row); err != nil {
		t.Fatal(err)
	}
	bound := row.Value.Bounds["execution_events"]
	if row.Kind != "metadata" || !bound.BoundReached || !bound.AdditionalRecordsMayBeOmitted || bound.Returned != sessionExportLimit {
		t.Fatalf("JSONL reached-bound metadata = kind %q, bound %#v", row.Kind, bound)
	}

	md, err := os.ReadFile(filepath.Join(dir, "session-12-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1000-event export bound was reached", "additional records may be omitted"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown hides reached bound %q: %s", want, md)
		}
	}
	if strings.Contains(string(md), "every execution is projected and cleanly terminal") ||
		!strings.Contains(string(md), "authoritative recovery projection found no") {
		t.Fatalf("no-issues copy overclaims terminality: %s", md)
	}
}

func TestFailClosedOpenIssueBoundDoesNotClaimOmission(t *testing.T) {
	bound := newFailClosedSessionExportBound(maxSessionExportHazards, maxSessionExportHazards, "fail closed")
	if !bound.BoundReached || bound.AdditionalRecordsMayBeOmitted {
		t.Fatalf("fail-closed bound = %#v", bound)
	}
}

func TestSessionExportRejectsForeignWorkspaceAndBadFormat(t *testing.T) {
	store := &fakeSessionExportStore{session: db.Session{ID: 7, WorkspaceID: "/workspace/other"}}
	var stdout, stderr bytes.Buffer
	if code := handleSessionExport(store, "/workspace/repo", []string{"0000007"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "different workspace") {
		t.Fatalf("foreign workspace exit=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := handleSessionExport(store, "/workspace/repo", []string{"--format", "pdf", "0000007"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "unknown --format") {
		t.Fatalf("bad format exit=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := handleSessionExport(store, "/workspace/repo", []string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad session id exit=%d", code)
	}
}

func TestSessionExportFailsClosedWhenAuditInputsCannotBeRead(t *testing.T) {
	sentinel := errors.New("read failed")
	tests := []struct {
		name string
		set  func(*fakeSessionExportStore)
		want string
	}{
		{name: "malformed state", set: func(store *fakeSessionExportStore) { store.stateRaw = `{"version":` }, want: "session state is not valid UTF-8 JSON"},
		{name: "oversize state", set: func(store *fakeSessionExportStore) { store.stateErr = db.ErrSessionExportStateTooLarge }, want: "state exceeds export byte limit"},
		{name: "missing state with hazards", set: func(store *fakeSessionExportStore) {
			store.stateRaw = ""
			event := exportTestEvent("exec-missing-state", "write", 9, execution.Effectful, execution.EventOutcomeUnknown)
			store.hazards = []execution.State{{Identity: event.Identity, Latest: event}}
		}, want: "durable session state is missing"},
		{name: "token stats", set: func(store *fakeSessionExportStore) { store.tokenErr = sentinel }, want: "list token stats"},
		{name: "file changes", set: func(store *fakeSessionExportStore) { store.fileErr = sentinel }, want: "list file changes"},
		{name: "checkpoints", set: func(store *fakeSessionExportStore) { store.cpErr = sentinel }, want: "list checkpoints"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeSessionExportStore{
				session:  db.Session{ID: 7, WorkspaceID: "/workspace/repo"},
				stateRaw: `{"version":2,"goal":null,"execution_cursor":0}`,
			}
			test.set(store)
			var stdout, stderr bytes.Buffer
			code := handleSessionExport(store, "/workspace/repo", []string{"--out", t.TempDir(), "0000007"}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("exit=%d stderr=%q, want %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestSessionExportPublishesOwnerOnlyFilesWithoutFollowingSymlinks(t *testing.T) {
	store := &fakeSessionExportStore{
		session:  db.Session{ID: 7, WorkspaceID: "/workspace/repo"},
		stateRaw: `{"version":2,"goal":null,"execution_cursor":0}`,
	}
	directory := t.TempDir()
	jsonl := filepath.Join(directory, "session-7.jsonl")
	markdown := filepath.Join(directory, "session-7-summary.md")
	for _, path := range []string{jsonl, markdown} {
		if err := os.WriteFile(path, []byte("old-public-content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := handleSessionExport(store, "/workspace/repo", []string{"--out", directory, "0000007"}, &stdout, &stderr); code != 0 {
		t.Fatalf("replace export exit=%d stderr=%q", code, stderr.String())
	}
	for _, path := range []string{jsonl, markdown} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
			t.Fatalf("export mode/type = %s for %s", info.Mode(), path)
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte("old-public-content")) {
			t.Fatalf("export replacement data=%q err=%v", data, err)
		}
	}

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jsonl); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, jsonl); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := handleSessionExport(store, "/workspace/repo", []string{"--format", "jsonl", "--out", directory, "0000007"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "symbolic links") {
		t.Fatalf("symlink export exit=%d stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "do-not-overwrite" {
		t.Fatalf("symlink victim data=%q err=%v", data, err)
	}
}

func TestPrepareSessionExportDirectoryDoesNotChangeExistingPermissions(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "shared")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessionExportDirectory(directory); err != nil {
		t.Fatalf("prepare existing directory: %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory mode = %v", info.Mode().Perm())
	}
	created := filepath.Join(parent, "private")
	if err := prepareSessionExportDirectory(created); err != nil {
		t.Fatalf("prepare new directory: %v", err)
	}
	createdInfo, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if createdInfo.Mode().Perm() != 0o700 {
		t.Fatalf("new directory mode = %v", createdInfo.Mode().Perm())
	}
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(created, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := prepareSessionExportDirectory(linked); !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("symlink directory error = %v", err)
	}
}

func TestSessionExportAcceptsTrailingValueFlags(t *testing.T) {
	// SESSION_ID before a value-taking flag must keep the flag bound to its value.
	store := &fakeSessionExportStore{session: db.Session{ID: 7, PublicID: "0000007", WorkspaceID: "/workspace/repo"}}
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := handleSessionExport(store, "/workspace/repo", []string{"0000007", "--format", "md", "--out", dir}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("trailing value flags exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "session-7-summary.md")); err != nil {
		t.Fatalf("markdown not written to --out dir: %v", err)
	}
}

func TestSessionListRendersAndEmpties(t *testing.T) {
	store := &fakeSessionExportStore{sessions: []db.Session{
		{ID: 7, PublicID: "0000007", Model: "qwen", Mode: "AUTO", UpdatedAt: "2026-07-14T09:52:00Z", Title: "investigate bob"},
	}}
	var stdout, stderr bytes.Buffer
	if code := handleSessionList(store, "/workspace/repo", nil, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"SESSION", "0000007", "investigate bob", "session export SESSION_ID"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("list output missing %q: %s", want, stdout.String())
		}
	}

	stdout.Reset()
	if code := handleSessionList(store, "/workspace/repo", []string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json list exit=%d stderr=%q", code, stderr.String())
	}
	var listed []db.Session
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != 7 {
		t.Fatalf("json list = %#v, error=%v; want numeric ID 7", listed, err)
	}

	empty := &fakeSessionExportStore{}
	stdout.Reset()
	if code := handleSessionList(empty, "/workspace/repo", []string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty json list exit=%d", code)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("empty json list = %q", stdout.String())
	}
}

func TestRelativizeHomeHidesHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	if got := relativizeHome(home); got != "~" {
		t.Fatalf("relativizeHome(home) = %q", got)
	}
	if got := relativizeHome(filepath.Join(home, "projects", "bob")); !strings.HasPrefix(got, "~"+string(os.PathSeparator)) {
		t.Fatalf("relativizeHome(subdir) = %q", got)
	}
	if got := relativizeHome("/opt/elsewhere"); got != "/opt/elsewhere" {
		t.Fatalf("relativizeHome(outside) = %q", got)
	}
}
