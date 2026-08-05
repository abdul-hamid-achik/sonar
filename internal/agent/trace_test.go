package agent

import (
	"bytes"
	"strings"
	"testing"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/charmbracelet/log"
)

func traceLoggerFor(t *testing.T) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	return traceLoggerAt(t, log.DebugLevel)
}

// traceLoggerAt asserts a level by filtering rather than by matching the
// library's rendered abbreviation ("ERRO", not "ERROR"), which is a rendering
// detail that would make these tests fail on a cosmetic upgrade.
func traceLoggerAt(t *testing.T, level log.Level) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	buffer := &bytes.Buffer{}
	return log.NewWithOptions(buffer, log.Options{Level: level}), buffer
}

func traceEvent(eventType executionpkg.EventType) executionpkg.Event {
	return executionpkg.Event{
		Identity: executionpkg.Identity{
			SessionID: 7, WorkspaceID: "ws", RunID: "run_1",
			TurnID: "turn_abc", ExecutionID: "exec_xyz", ToolName: "bash",
			Kind: executionpkg.KindBuiltin, EffectClass: executionpkg.Effectful,
			Iteration: 3, Ordinal: 1,
		},
		Type:            eventType,
		Approval:        executionpkg.ApprovalNotApplicable,
		ArgumentsSHA256: strings.Repeat("a", 64),
	}
}

// The reason this trace exists: a session log recorded `tool call name=ls
// duration=264µs error=true` and nothing else, so a failure could be seen but
// never attributed. Every lifecycle line must carry the keys that let a reader
// join it to the rest of the session without opening the SQLite ledger.
func TestExecutionTraceCarriesTheCorrelationKeys(t *testing.T) {
	logger, buffer := traceLoggerFor(t)
	traceExecutionEvent(logger, traceEvent(executionpkg.EventRequested), nil)

	line := buffer.String()
	for _, want := range []string{
		"turn=turn_abc", "execution=exec_xyz", "tool=bash",
		"kind=builtin", "iter=3", "ordinal=1", "args=aaaaaaaaaaaa",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("trace is missing %q: %s", want, line)
		}
	}
	// The full digest belongs in the ledger; the line carries only the prefix
	// needed to join two events about the same arguments.
	if strings.Contains(line, strings.Repeat("a", 64)) {
		t.Errorf("trace printed the full arguments digest: %s", line)
	}
}

// The whole complaint behind "it asks permission every time it runs a program"
// was an unactionable prompt. The trace has to answer it in the log too, or the
// reason is only ever visible to whoever was watching the screen at the time.
func TestApprovalRequestTraceCarriesTheRuleThatTripped(t *testing.T) {
	logger, buffer := traceLoggerFor(t)
	event := traceEvent(executionpkg.EventApprovalRequested)
	event.Approval = executionpkg.ApprovalRequested
	event.Detail = "interactive approval requested: dynamic shell syntax ($?)"
	traceExecutionEvent(logger, event, nil)

	line := buffer.String()
	if !strings.Contains(line, "dynamic shell syntax") {
		t.Errorf("approval trace does not say what tripped: %s", line)
	}
	if !strings.Contains(line, "approval=requested") {
		t.Errorf("approval trace does not carry the disposition: %s", line)
	}
	// An approval prompt stops the run and waits for a human. It is never
	// something to scroll past at debug level.
	raised, atWarn := traceLoggerAt(t, log.WarnLevel)
	traceExecutionEvent(raised, event, nil)
	if atWarn.Len() == 0 {
		t.Error("approval request was not raised above debug")
	}
}

// A failure whose reason is absent is the line that sent me to the database.
func TestFailureTraceCarriesTheReceipt(t *testing.T) {
	logger, buffer := traceLoggerFor(t)
	event := traceEvent(executionpkg.EventFailed)
	event.ResultReceipt = "edit: old_string matched 3 locations; widen the target or pass replace_all"
	traceExecutionEvent(logger, event, nil)

	line := buffer.String()
	if !strings.Contains(line, "matched 3 locations") {
		t.Errorf("failure trace does not say why: %s", line)
	}
	raised, atError := traceLoggerAt(t, log.ErrorLevel)
	traceExecutionEvent(raised, event, nil)
	if atError.Len() == 0 {
		t.Error("failure was not raised to error level")
	}
}

// A completed read-only call is the most common event in a session and the
// least interesting one. It stays at debug so the levels above it mean
// something.
func TestRoutineCompletionStaysQuiet(t *testing.T) {
	for eventType, wantLevel := range map[executionpkg.EventType]log.Level{
		executionpkg.EventRequested:         log.DebugLevel,
		executionpkg.EventApproved:          log.DebugLevel,
		executionpkg.EventStarted:           log.DebugLevel,
		executionpkg.EventCompleted:         log.InfoLevel,
		executionpkg.EventApprovalRequested: log.WarnLevel,
		executionpkg.EventDenied:            log.WarnLevel,
		executionpkg.EventFailed:            log.ErrorLevel,
		executionpkg.EventOutcomeUnknown:    log.ErrorLevel,
	} {
		if got := traceLevel(eventType); got != wantLevel {
			t.Errorf("%s logs at %v, want %v", eventType, got, wantLevel)
		}
	}
}

// A ledger write that fails leaves the durable record and the log disagreeing,
// which is exactly the state that latches an unresolved execution. The trace
// must say so rather than reporting the event as if it had been recorded.
func TestUnrecordedEventIsMarkedAsSuch(t *testing.T) {
	logger, buffer := traceLoggerFor(t)
	traceExecutionEvent(logger, traceEvent(executionpkg.EventCompleted), errUnrecordedProbe)

	line := buffer.String()
	if !strings.Contains(line, "not durably recorded") {
		t.Errorf("a failed ledger write reads as a normal event: %s", line)
	}
	raised, atError := traceLoggerAt(t, log.ErrorLevel)
	traceExecutionEvent(raised, traceEvent(executionpkg.EventCompleted), errUnrecordedProbe)
	if atError.Len() == 0 {
		t.Error("a failed ledger write was not raised to error level")
	}
}

// A tool result is arbitrary text: multi-line output must not break the log's
// one-record-per-line shape, and an unbounded receipt must not fill the file.
func TestTraceSnippetStaysOnOneBoundedLine(t *testing.T) {
	snippet := traceSnippet("first line\nsecond\rthird\tfourth")
	if strings.ContainsAny(snippet, "\n\r\t") {
		t.Errorf("snippet still spans lines: %q", snippet)
	}
	if snippet != "first line second third fourth" {
		t.Errorf("snippet = %q", snippet)
	}

	long := traceSnippet(strings.Repeat("x", maxTraceSnippetBytes*3))
	if len(long) > maxTraceSnippetBytes+4 {
		t.Errorf("snippet is %d bytes, want it bounded near %d", len(long), maxTraceSnippetBytes)
	}

	// Truncation must not split a multi-byte rune into invalid UTF-8; a log
	// line is read by a human and by grep, and neither copes with a half rune.
	multibyte := traceSnippet(strings.Repeat("é", maxTraceSnippetBytes))
	if !utf8ValidString(multibyte) {
		t.Errorf("snippet is not valid UTF-8: %q", multibyte)
	}
}

// A nil logger is the ordinary embedding case and must not panic.
func TestTraceWithoutALoggerIsANoop(t *testing.T) {
	traceExecutionEvent(nil, traceEvent(executionpkg.EventFailed), nil)
}

var errUnrecordedProbe = errProbe("ledger unavailable")

type errProbe string

func (e errProbe) Error() string { return string(e) }

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
