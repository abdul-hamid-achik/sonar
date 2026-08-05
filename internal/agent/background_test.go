package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func newBackgroundAgent(t *testing.T) *Agent {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("background shell execution requires a POSIX shell")
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(t.TempDir())
	// Every background test must reap what it started, including on failure.
	t.Cleanup(ag.Close)
	return ag
}

// startBackground runs the bash tool with background=true and returns the id
// the receipt handed back, exactly as a model would read it.
func startBackground(t *testing.T, ag *Agent, command string) (string, *backgroundProcess) {
	t.Helper()
	result, isErr := ag.handleBash(context.Background(), map[string]any{
		"command": command, "background": true,
	})
	if isErr {
		t.Fatalf("background start failed: %s", result)
	}
	id := backgroundIDFromReceipt(t, result)
	proc, ok := ag.backgroundShells().lookup(id)
	if !ok {
		t.Fatalf("receipt named %q but the registry does not hold it: %s", id, result)
	}
	return id, proc
}

func backgroundIDFromReceipt(t *testing.T, receipt string) string {
	t.Helper()
	for _, field := range strings.Fields(receipt) {
		if strings.HasPrefix(field, "bg_") {
			return strings.Trim(field, `"`)
		}
	}
	t.Fatalf("receipt carries no background id: %s", receipt)
	return ""
}

func waitForExit(t *testing.T, proc *backgroundProcess) {
	t.Helper()
	select {
	case <-proc.done:
	case <-time.After(30 * time.Second):
		t.Fatal("background process did not exit within 30s")
	}
}

func readBack(t *testing.T, ag *Agent, id string) string {
	t.Helper()
	result, isErr := ag.handleBashOutput(map[string]any{"id": id})
	if isErr {
		t.Fatalf("bash_output(%q) failed: %s", id, result)
	}
	return result
}

// --- 1. output is bounded -------------------------------------------------

func TestBoundedStreamRetainsAtMostTheCapAndReportsOmittedBytes(t *testing.T) {
	stream := &boundedStream{limit: 64}
	for i := 0; i < 10; i++ {
		if _, err := stream.Write([]byte(strings.Repeat("x", 32))); err != nil {
			t.Fatal(err)
		}
	}
	if got := stream.totalWritten(); got != 320 {
		t.Fatalf("totalWritten = %d, want 320", got)
	}
	if len(stream.buf) != 64 {
		t.Fatalf("retained %d bytes, want the 64-byte cap", len(stream.buf))
	}
	text, next, omitted := stream.readSince(0)
	if len(text) != 64 {
		t.Fatalf("delivered %d bytes, want 64", len(text))
	}
	if next != 320 {
		t.Fatalf("next cursor = %d, want 320", next)
	}
	if omitted != 256 {
		t.Fatalf("omitted = %d, want 256 (320 written - 64 retained)", omitted)
	}
	// A second read after no further writes reports nothing new.
	text, _, omitted = stream.readSince(next)
	if text != "" || omitted != 0 {
		t.Fatalf("second read = %q omitted=%d, want empty", text, omitted)
	}
}

func TestBoundedStreamKeepsTheNewestBytesOfAnOversizedWrite(t *testing.T) {
	stream := &boundedStream{limit: 8}
	if _, err := stream.Write([]byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	text, _, omitted := stream.readSince(0)
	if text != "ijklmnop" {
		t.Fatalf("retained %q, want the newest 8 bytes", text)
	}
	if omitted != 8 {
		t.Fatalf("omitted = %d, want 8", omitted)
	}
}

func TestBackgroundReadBackReportsRetentionTruncation(t *testing.T) {
	ag := newBackgroundAgent(t)
	const emitted = 200_000
	id, proc := startBackground(t, ag, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'", emitted))
	waitForExit(t, proc)

	result := readBack(t, ag, id)
	if !strings.Contains(result, fmt.Sprintf("%d byte(s) produced in total", emitted)) {
		t.Fatalf("read-back hid the true byte count:\n%s", firstLines(result, 6))
	}
	if !strings.Contains(result, fmt.Sprintf("%d new byte(s)", maxBackgroundReadBytes)) {
		t.Fatalf("read-back delivered more than one read's worth:\n%s", firstLines(result, 6))
	}
	omittedNote := fmt.Sprintf("(%d earlier stdout byte(s) omitted", emitted-maxBackgroundReadBytes)
	if !strings.Contains(result, omittedNote) {
		t.Fatalf("read-back did not say output was truncated (want %q):\n%s", omittedNote, firstLines(result, 8))
	}
	if proc.stdout.totalWritten() != emitted {
		t.Fatalf("stream accounting = %d, want %d", proc.stdout.totalWritten(), emitted)
	}
	if len(proc.stdout.buf) > maxBackgroundStreamBytes {
		t.Fatalf("retained %d bytes, above the %d cap", len(proc.stdout.buf), maxBackgroundStreamBytes)
	}
}

// --- 5. read-back is usable ----------------------------------------------

func TestBackgroundReadBackAfterProcessExited(t *testing.T) {
	ag := newBackgroundAgent(t)
	id, proc := startBackground(t, ag, "echo done-out; echo done-err 1>&2; exit 3")
	waitForExit(t, proc)

	result := readBack(t, ag, id)
	for _, want := range []string{id, "exited with status 3", "done-out", "done-err"} {
		if !strings.Contains(result, want) {
			t.Fatalf("read-back missing %q:\n%s", want, result)
		}
	}
	if strings.Contains(result, "Still running") {
		t.Fatalf("exited process reported as running:\n%s", result)
	}
	// A second read has nothing new but still reports the terminal status.
	second := readBack(t, ag, id)
	if strings.Contains(streamSection(t, second, "stdout"), "done-out") {
		t.Fatalf("second read replayed already-delivered output:\n%s", second)
	}
	if strings.Contains(streamSection(t, second, "stderr"), "done-err") {
		t.Fatalf("second read replayed already-delivered stderr:\n%s", second)
	}
	if !strings.Contains(second, "exited with status 3") {
		t.Fatalf("second read lost the exit status:\n%s", second)
	}
}

func TestBackgroundReadBackWhileStillRunningReturnsOnlyNewOutput(t *testing.T) {
	ag := newBackgroundAgent(t)
	marker := filepath.Join(t.TempDir(), "second")
	id, proc := startBackground(t, ag,
		fmt.Sprintf("echo first; while [ ! -f %q ]; do sleep 0.02; done; echo second; sleep 30", marker))

	first := waitForStreamOutput(t, ag, id, "stdout", "first")
	if !strings.Contains(first, "running") || !strings.Contains(first, "Still running") {
		t.Fatalf("running process was not reported as running:\n%s", first)
	}
	if !strings.Contains(first, fmt.Sprintf("pid %d", proc.pid)) {
		t.Fatalf("read-back did not identify the process:\n%s", first)
	}

	if err := os.WriteFile(marker, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := waitForStreamOutput(t, ag, id, "stdout", "second")
	if strings.Contains(streamSection(t, second, "stdout"), "first") {
		t.Fatalf("read-back replayed output already delivered:\n%s", second)
	}
	if !proc.running() {
		t.Fatal("process should still be running")
	}
}

func TestBackgroundListingNamesEveryProcess(t *testing.T) {
	ag := newBackgroundAgent(t)
	first, firstProc := startBackground(t, ag, "echo one")
	second, secondProc := startBackground(t, ag, "echo two")
	waitForExit(t, firstProc)
	waitForExit(t, secondProc)

	listing, isErr := ag.handleBashOutput(map[string]any{})
	if isErr {
		t.Fatalf("listing failed: %s", listing)
	}
	for _, want := range []string{first, second, "echo one", "echo two"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing missing %q:\n%s", want, listing)
		}
	}
}

func TestBackgroundReadBackRejectsUnknownID(t *testing.T) {
	ag := newBackgroundAgent(t)
	result, isErr := ag.handleBashOutput(map[string]any{"id": "bg_nope"})
	if !isErr || !strings.Contains(result, "bg_nope") {
		t.Fatalf("unknown id was not refused: %q (isErr=%v)", result, isErr)
	}
}

func TestBackgroundStartRefusesBeyondTheProcessCapWithoutDiscardingOutput(t *testing.T) {
	ag := newBackgroundAgent(t)
	for i := 0; i < maxBackgroundProcesses; i++ {
		startBackground(t, ag, "sleep 30")
	}
	result, isErr := ag.handleBash(context.Background(), map[string]any{
		"command": "sleep 30", "background": true,
	})
	if !isErr {
		t.Fatalf("start beyond the cap was allowed: %s", result)
	}
	if !strings.Contains(result, "bash_output") {
		t.Fatalf("refusal does not tell the model how to recover: %s", result)
	}
}

func TestBackgroundStartReusesSlotsOfDrainedProcesses(t *testing.T) {
	ag := newBackgroundAgent(t)
	for i := 0; i < maxBackgroundProcesses; i++ {
		id, proc := startBackground(t, ag, "echo drained")
		waitForExit(t, proc)
		readBack(t, ag, id)
	}
	// startBackground fails the test if the start was refused, so reaching the
	// end proves drained slots were reclaimed.
	_, proc := startBackground(t, ag, "echo more")
	waitForExit(t, proc)
}

// --- 2. processes must not outlive the harness ----------------------------

func TestAgentCloseTerminatesBackgroundProcesses(t *testing.T) {
	ag := newBackgroundAgent(t)
	_, proc := startBackground(t, ag, "sleep 300")
	if !proc.running() {
		t.Fatal("process exited before the shutdown test could run")
	}
	ag.Close()
	if proc.running() {
		t.Fatal("Close returned while a background process was still running")
	}
	// Close is idempotent: the TUI and headless paths both call it.
	ag.Close()

	result, isErr := ag.handleBash(context.Background(), map[string]any{
		"command": "sleep 300", "background": true,
	})
	if !isErr {
		t.Fatalf("a closed session still started a background process: %s", result)
	}
}

// --- 4. authorization is unchanged ----------------------------------------

// TestBackgroundFlagNeverChangesShellAuthorization pins the property that makes
// backgrounding safe: every authorization input for `bash` is derived from the
// command string, so adding background=true can neither widen the durable
// effect class nor turn a prompted command into an automatic one.
func TestBackgroundFlagNeverChangesShellAuthorization(t *testing.T) {
	commands := []string{
		"ls",
		"go build ./...",
		"npm run dev",
		"curl https://example.com",
		"rm -rf /",
		"cd /tmp; rm -rf x",
	}
	modes := []AuthorityMode{AuthorityNormal, AuthorityPlan, AuthorityAutoScoped}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(t.TempDir())
	t.Cleanup(ag.Close)

	autoApprovals := 0
	for _, command := range commands {
		foreground := llm.ToolCall{ID: "fg", Name: "bash", Arguments: map[string]any{"command": command}}
		background := llm.ToolCall{ID: "bg", Name: "bash", Arguments: map[string]any{"command": command, "background": true}}

		fgKind, fgEffect := ag.executionKindForCall(foreground)
		bgKind, bgEffect := ag.executionKindForCall(background)
		if fgKind != bgKind || fgEffect != bgEffect {
			t.Errorf("%q: durable classification changed with background: %s/%s vs %s/%s",
				command, fgKind, fgEffect, bgKind, bgEffect)
		}
		if !builtinToolRequiresApproval(background.Name) {
			t.Errorf("%q: background start left the approval-gated tool family", command)
		}
		for _, mode := range modes {
			fgAuto := ag.authorityAutoApproves(mode, foreground, fgKind)
			bgAuto := ag.authorityAutoApproves(mode, background, bgKind)
			if fgAuto != bgAuto {
				t.Errorf("%q in mode %d: auto-approval changed with background (%v vs %v)",
					command, mode, fgAuto, bgAuto)
			}
			if bgAuto {
				autoApprovals++
			}
		}
		// The scoped-shell classifier is the gate AUTO consults, and it only
		// ever sees the command string.
		if ag.assessAutoScopedCommand(command) != ag.assessAutoScopedCommand(command) {
			t.Errorf("%q: scoped-command assessment is not deterministic", command)
		}
	}
	if autoApprovals == 0 {
		t.Fatal("no command was auto-approved, so equality proves nothing")
	}
	// A denied shell policy still denies the background spelling.
	denied := New(nil, nil, 0)
	denied.SetWorkDir(t.TempDir())
	t.Cleanup(denied.Close)
	checker := permission.NewChecker(nil, false)
	if err := checker.SetPolicy("bash", permission.PolicyDeny); err != nil {
		t.Fatal(err)
	}
	denied.SetPermissionChecker(checker)
	backgroundLS := llm.ToolCall{Name: "bash", Arguments: map[string]any{"command": "ls", "background": true}}
	if !denied.authorityPermissionDeniedForCall(backgroundLS) {
		t.Fatal("a bash deny policy did not cover the background spelling")
	}
	if denied.authorityAutoApproves(AuthorityAutoScoped, backgroundLS, executionpkg.KindBuiltin) {
		t.Fatal("a denied command was auto-approved when backgrounded")
	}
}

// TestBackgroundBashPromptsExactlyLikeForegroundBash drives the real dispatch
// path so the equality above is observed end to end, at the approval surface
// the operator actually sees.
func TestBackgroundBashPromptsExactlyLikeForegroundBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("background shell execution requires a POSIX shell")
	}
	run := func(t *testing.T, args map[string]any) permission.ApprovalRequest {
		t.Helper()
		client := &scriptedClient{responses: [][]llm.StreamChunk{
			{{ToolCalls: []llm.ToolCall{{ID: "bash-1", Name: "bash", Arguments: args}}, Done: true}},
			{{Text: "done", Done: true}},
		}}
		ag := New(client, nil, 4096)
		ag.SetWorkDir(t.TempDir())
		ag.SetModeContext("test", BuildToolPolicy())
		ag.SetPermissionChecker(permission.NewChecker(nil, false))
		t.Cleanup(ag.Close)
		var seen permission.ApprovalRequest
		prompts := 0
		ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
			prompts++
			seen = request
			request.Response <- permission.ApprovalResponse{Allowed: false}
		})
		ag.AddUserMessage("run it")
		if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
			t.Fatal(err)
		}
		if prompts != 1 {
			t.Fatalf("approval prompts = %d, want exactly 1", prompts)
		}
		return seen
	}

	foreground := run(t, map[string]any{"command": "curl https://example.com"})
	background := run(t, map[string]any{"command": "curl https://example.com", "background": true})

	if foreground.ToolName != background.ToolName {
		t.Fatalf("tool name differs: %q vs %q", foreground.ToolName, background.ToolName)
	}
	if foreground.Preview.Kind != background.Preview.Kind {
		t.Fatalf("preview kind differs: %q vs %q", foreground.Preview.Kind, background.Preview.Kind)
	}
	if foreground.Preview.Command != background.Preview.Command {
		t.Fatalf("previewed command differs: %q vs %q", foreground.Preview.Command, background.Preview.Command)
	}
}

// --- 3. the durable ledger stays honest -----------------------------------

// TestBackgroundStartAndReadBackAreSeparateLedgerExecutions records the ledger
// decision: a verified launch is a completed effect (the host holds the pid),
// not an outcome_unknown hazard, and the later read is its own read-only
// execution carrying the observation.
func TestBackgroundStartAndReadBackAreSeparateLedgerExecutions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("background shell execution requires a POSIX shell")
	}
	// A command the scoped-shell classifier does not admit, so the launch keeps
	// the same non-read-only effect class the identical foreground call has.
	const command = "sleep 0.05; echo ledger-output"
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "start", Name: "bash", Arguments: map[string]any{
			"command": command, "background": true,
		}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "read", Name: "bash_output", Arguments: map[string]any{
			"id": "bg_1",
		}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	t.Cleanup(ag.Close)
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	byExecution := map[string][]executionpkg.Event{}
	order := []string{}
	for _, event := range ledger.snapshot() {
		id := event.Identity.ExecutionID
		if _, seen := byExecution[id]; !seen {
			order = append(order, id)
		}
		byExecution[id] = append(byExecution[id], event)
		if event.Type == executionpkg.EventOutcomeUnknown {
			t.Fatalf("a deliberate background launch minted an outcome_unknown hazard: %+v", event)
		}
	}
	if len(order) != 2 {
		t.Fatalf("executions recorded = %d, want 2 (start and read-back)", len(order))
	}

	start := byExecution[order[0]]
	last := start[len(start)-1]
	if last.Identity.ToolName != "bash" {
		t.Fatalf("first execution tool = %q, want bash", last.Identity.ToolName)
	}
	if last.Type != executionpkg.EventCompleted {
		t.Fatalf("background start terminal event = %q, want completed", last.Type)
	}
	_, foregroundEffect := ag.executionKindForCall(llm.ToolCall{
		Name: "bash", Arguments: map[string]any{"command": command},
	})
	if last.Identity.EffectClass != foregroundEffect {
		t.Fatalf("background launch effect class = %q, want the foreground class %q",
			last.Identity.EffectClass, foregroundEffect)
	}
	if last.Identity.EffectClass == executionpkg.EffectReadOnly {
		t.Fatal("this fixture should exercise a non-read-only launch")
	}
	if !strings.Contains(last.ResultReceipt, "Started background process") {
		t.Fatalf("start receipt does not describe the launch: %q", last.ResultReceipt)
	}

	read := byExecution[order[1]]
	terminal := read[len(read)-1]
	if terminal.Identity.ToolName != "bash_output" {
		t.Fatalf("second execution tool = %q, want bash_output", terminal.Identity.ToolName)
	}
	if terminal.Identity.EffectClass != executionpkg.EffectReadOnly {
		t.Fatalf("read-back effect class = %q, want read_only", terminal.Identity.EffectClass)
	}
	if terminal.Type != executionpkg.EventCompleted {
		t.Fatalf("read-back terminal event = %q, want completed", terminal.Type)
	}
	if terminal.Identity.ExecutionID == last.Identity.ExecutionID {
		t.Fatal("start and read-back shared one execution identity")
	}
}

func TestBashOutputPreflightRejectsNonStringID(t *testing.T) {
	ag := New(nil, nil, 0)
	t.Cleanup(ag.Close)
	if err := ag.preflightToolCall(executionpkg.KindBuiltin, llm.ToolCall{
		Name: "bash_output", Arguments: map[string]any{"id": 7},
	}); err == nil {
		t.Fatal("non-string id passed preflight")
	}
	if err := ag.preflightToolCall(executionpkg.KindBuiltin, llm.ToolCall{
		Name: "bash_output", Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("listing form rejected by preflight: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

// waitForStreamOutput polls the read-back tool until the named stream section
// carries want, then returns that whole read-back receipt.
func waitForStreamOutput(t *testing.T, ag *Agent, id, stream, want string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var collected strings.Builder
	for time.Now().Before(deadline) {
		result := readBack(t, ag, id)
		collected.WriteString(result)
		if strings.Contains(streamSection(t, result, stream), want) {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background %s never contained %q:\n%s", stream, want, collected.String())
	return ""
}

// streamSection isolates one stream's block of a read-back receipt so an
// assertion cannot accidentally match the echoed command line in the header.
func streamSection(t *testing.T, result, name string) string {
	t.Helper()
	marker := "--- " + name + ":"
	start := strings.Index(result, marker)
	if start < 0 {
		t.Fatalf("read-back has no %s section:\n%s", name, result)
	}
	section := result[start+len(marker):]
	if end := strings.Index(section, "\n--- "); end >= 0 {
		section = section[:end]
	}
	return section
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
