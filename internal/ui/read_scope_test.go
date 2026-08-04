package ui

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
)

func TestScopeCommandUpdatesAgentReadRootsAsynchronously(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "mcphub")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	external, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	m.agent.SetWorkDir(workspace)

	parsed := m.cmdRegistry.Execute(m.buildCommandContext(), "scope", []string{"add-read", external})
	if parsed.Error != "" || parsed.Action != command.ActionAddReadRoot {
		t.Fatalf("parsed /scope add-read = %#v", parsed)
	}
	cmd := m.handleCommandAction(parsed)
	if cmd == nil || !m.readScopeOpRunning {
		t.Fatal("scope preview did not start asynchronously")
	}
	preview := awaitCommandMessage[ReadScopePreviewResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ := m.Update(preview)
	m = updated.(*Model)
	if m.readScopeOpRunning || m.readScopePrompt == nil || m.readScopePrompt.Canonical != external {
		t.Fatalf("preview did not open canonical confirmation: running=%v prompt=%#v", m.readScopeOpRunning, m.readScopePrompt)
	}
	updated, cmd = m.Update(charKey('y'))
	m = updated.(*Model)
	if cmd == nil || !m.readScopeOpRunning || m.readScopePrompt != nil {
		t.Fatal("allow did not start tokened read-root commit")
	}
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)
	if m.readScopeOpRunning {
		t.Fatal("matching add receipt did not settle operation")
	}
	if roots := m.agent.ReadRoots(); len(roots) != 1 || roots[0] != external {
		t.Fatalf("agent roots = %#v", roots)
	}
	if roots := m.buildCommandContext().ReadRoots; len(roots) != 1 || roots[0] != external {
		t.Fatalf("command context roots = %#v", roots)
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "Added temporary read-only root") || !strings.Contains(transcript, "Write authority remains confined") || !strings.Contains(transcript, "not saved with sessions") {
		t.Fatalf("missing add receipt: %s", transcript)
	}

	remove := m.cmdRegistry.Execute(m.buildCommandContext(), "scope", []string{"remove-read", external})
	cmd = m.handleCommandAction(remove)
	receipt = awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)
	if roots := m.agent.ReadRoots(); len(roots) != 0 {
		t.Fatalf("roots after remove = %#v", roots)
	}
}

func TestScopeCommandSurfacesAuthorityErrorsAndIgnoresStaleReceipts(t *testing.T) {
	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	workspace := t.TempDir()
	m.agent.SetWorkDir(workspace)

	cmd := m.handleCommandAction(command.Result{Action: command.ActionAddReadRoot, Data: workspace})
	result := awaitCommandMessage[ReadScopePreviewResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ := m.Update(result)
	m = updated.(*Model)
	if transcript := entryText(m.entries); !strings.Contains(transcript, "/scope add-read preview failed") || !strings.Contains(transcript, "overlaps") {
		t.Fatalf("authority error not surfaced: %s", transcript)
	}

	m.readScopeOpRunning = true
	m.readScopeOpToken = 9
	updated, _ = m.Update(ReadScopeResultMsg{Token: 8, Operation: "clear-read", Count: 99})
	m = updated.(*Model)
	if !m.readScopeOpRunning {
		t.Fatal("stale scope receipt settled the active operation")
	}
}

func TestReadScopePromptIsExplicitReadOnlyAndRestoresDraftOnDenyAndCancel(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{name: "deny", key: charKey('n'), want: "denied"},
		{name: "cancel", key: escKey(), want: "cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			t.Cleanup(m.agent.Close)
			m.agent.SetWorkDir(workspace)
			draft := "/scope add-read \"" + external + "\""
			m.input.SetValue(draft)

			cmd := m.submitInput()
			preview := awaitCommandMessage[ReadScopePreviewResultMsg](t, commandMessages(cmd), 2*time.Second)
			updated, _ := m.Update(preview)
			m = updated.(*Model)
			plain := ansi.Strip(m.renderReadScopePrompt())
			for _, want := range []string{"Allow external read-only directory", "Read-only", "not saved", "Writes", "never", "allow read-only", "deny", "cancel"} {
				if !strings.Contains(strings.ToLower(plain), strings.ToLower(want)) {
					t.Fatalf("scope confirmation missing %q:\n%s", want, plain)
				}
			}

			updated, _ = m.Update(test.key)
			m = updated.(*Model)
			if m.readScopePrompt != nil || m.input.Value() != draft {
				t.Fatalf("%s did not restore exact draft: prompt=%#v draft=%q", test.name, m.readScopePrompt, m.input.Value())
			}
			if transcript := entryText(m.entries); !strings.Contains(transcript, test.want) || !strings.Contains(transcript, "draft restored") {
				t.Fatalf("%s outcome not explicit: %s", test.name, transcript)
			}
			if len(m.agent.ReadRoots()) != 0 {
				t.Fatal("denied/cancelled preview mutated read authority")
			}
		})
	}
}

func TestReadScopeOperationBlocksSubmissionAndShowsAccessibleActivity(t *testing.T) {
	m := newTestModel(t)
	m.readScopeOpRunning = true
	m.readScopeOpLabel = "Checking external read root"
	m.input.SetValue("do not send")
	before := append([]string(nil), m.promptHistory...)

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("read-scope ownership dispatched input")
	}
	if m.input.Value() != "do not send" || len(m.promptHistory) != len(before) {
		t.Fatal("blocked submission consumed the draft or history")
	}
	line := ansi.Strip(m.renderWorkingLine())
	for _, want := range []string{"Checking external read root", "writes remain workspace-only"} {
		if !strings.Contains(line, want) {
			t.Fatalf("scope activity missing %q: %q", want, line)
		}
	}
}

func TestPromptReadGrantBatchRollsBackOnlyNewAuthority(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	preexisting := filepath.Join(external, "preexisting.txt")
	newFile := filepath.Join(base, "new.txt")
	for path, content := range map[string]string{preexisting: "preexisting", newFile: "new"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	canonicalPreexisting, err := filepath.EvalSymlinks(preexisting)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		prepare  func(*agent.Agent) []agent.ReadGrant
		wantKind agent.ReadGrantKind
		wantPath string
	}{
		{
			name: "new exact file is revoked",
			prepare: func(ag *agent.Agent) []agent.ReadGrant {
				inspection, err := ag.InspectReadPath(newFile)
				if err != nil {
					t.Fatal(err)
				}
				return []agent.ReadGrant{
					inspection.Grant(),
					{Path: string(filepath.Separator), Kind: agent.ReadGrantDirectory},
				}
			},
		},
		{
			name: "preexisting exact file is restored after broader directory",
			prepare: func(ag *agent.Agent) []agent.ReadGrant {
				if _, err := ag.AddReadFile(preexisting); err != nil {
					t.Fatal(err)
				}
				inspection, err := ag.InspectReadPath(external)
				if err != nil {
					t.Fatal(err)
				}
				return []agent.ReadGrant{
					inspection.Grant(),
					{Path: newFile, Kind: agent.ReadGrantKind("unsupported")},
				}
			},
			wantKind: agent.ReadGrantExactFile,
			wantPath: canonicalPreexisting,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ag := agent.New(nil, nil, 0)
			ag.SetWorkDir(workspace)
			t.Cleanup(ag.Close)
			approved := test.prepare(ag)
			before := ag.ReadGrants()
			applied, rolledBack, rollbackErr, err := applyPromptReadGrantsTransactional(ag, approved)
			if err == nil || rollbackErr != nil || rolledBack == 0 || len(applied) != 0 {
				t.Fatalf("transaction = applied %#v, rolledBack %d, rollbackErr %v, err %v", applied, rolledBack, rollbackErr, err)
			}
			after := ag.ReadGrants()
			if len(after) != len(before) {
				t.Fatalf("authority leaked after rollback: before=%#v after=%#v", before, after)
			}
			for index := range before {
				if readGrantKey(before[index]) != readGrantKey(after[index]) {
					t.Fatalf("rollback changed preexisting authority: before=%#v after=%#v", before, after)
				}
			}
			if test.wantKind != "" && (len(after) != 1 || after[0].Kind != test.wantKind || after[0].Path != test.wantPath) {
				t.Fatalf("restored grant = %#v", after)
			}
		})
	}
}

func TestRestoreReadGrantSnapshotRejectsReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "evidence.txt")
	if err := os.WriteFile(target, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	if _, err := ag.AddReadFile(target); err != nil {
		t.Fatal(err)
	}
	before, err := ag.SnapshotReadGrants()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseReadGrants(before) })
	if _, err := ag.RemoveReadPath(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}

	rolledBack, rollbackErr := restoreReadGrantSnapshot(ag, before)
	if rollbackErr == nil || rolledBack != 0 {
		t.Fatalf("restore replacement = rolledBack %d, err %v", rolledBack, rollbackErr)
	}
	if grants := ag.ReadGrants(); len(grants) != 0 {
		t.Fatalf("replacement inherited authority: %#v", grants)
	}
}

func TestReadScopeBatchFailureReportsSuccessfulRollback(t *testing.T) {
	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	draft := "inspect /external/requested.txt"
	m.readScopeOpRunning = true
	m.readScopeOpToken = 7
	m.readScopeOpDraft = draft
	updated, _ := m.Update(ReadScopeResultMsg{
		Token: 7, Operation: "add-intents", RolledBack: 1,
		Err: errors.New("second grant failed"),
	})
	m = updated.(*Model)
	transcript := entryText(m.entries)
	for _, want := range []string{"failed", "rolled back 1", "no partial approval remains"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("rollback receipt missing %q: %s", want, transcript)
		}
	}
	if m.input.Value() != draft {
		t.Fatalf("rollback did not restore draft: %q", m.input.Value())
	}
}

func TestCanonicalReadScopePreviewExpandsTilde(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tilde fixture uses Unix path syntax")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	external := filepath.Join(home, "shared docs")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, _, err := canonicalReadScopePreview("~/shared docs", workspace, nil)
	canonicalExternal, canonicalErr := filepath.EvalSymlinks(external)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	if err != nil || canonical != canonicalExternal {
		t.Fatalf("tilde preview = %q, %v, want %q", canonical, err, canonicalExternal)
	}
}

func TestRuntimeStatusExposesTemporaryExactFileAuthority(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "requested-report.pdf")
	if err := os.WriteFile(target, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	if _, err := m.agent.AddReadFile(target); err != nil {
		t.Fatal(err)
	}
	runtimeView := ansi.Strip(m.buildRuntimeStatusContent(56))
	searchable := strings.Join(strings.Fields(runtimeView), " ")
	for _, want := range []string{
		"Read scope", "1 temporary external grant", "External read access", "Exact file",
		"/scope clear-read revokes all", "shell writes remain workspace-only", "typed-write grants expire with the turn",
	} {
		if !strings.Contains(searchable, want) {
			t.Fatalf("Runtime omitted %q:\n%s", want, runtimeView)
		}
	}
	grants := m.agent.ReadGrants()
	if len(grants) != 1 || grants[0].Kind != agent.ReadGrantExactFile {
		t.Fatalf("exact-file authority = %#v", grants)
	}
	// Long Linux test paths wrap inside the filename at the normal Runtime
	// width. Remove only layout whitespace to verify that the complete escaped
	// authority remains visible across continuation lines.
	compactView := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(runtimeView)
	compactPath := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(displayWorkspacePath(grants[0].Path))
	if !strings.Contains(compactView, compactPath) {
		t.Fatalf("Runtime omitted complete exact-file path %q:\n%s", grants[0].Path, runtimeView)
	}
}

func TestReadScopePromptResponsiveNoColorAndReducedMotion(t *testing.T) {
	t.Setenv("SONAR_REDUCED_MOTION", "1")
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.readScopePrompt = &ReadScopePrompt{
		Canonical: "/a/very/long/external/path/with spaces/project",
		Workspace: "/a/very/long/workspace/path/project",
		Draft:     "/scope add-read '/a/very/long/external/path/with spaces/project'",
	}
	rendered := m.renderReadScopePrompt()
	if hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR scope prompt emitted ANSI color: %q", rendered)
	}
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	if got := lipgloss.Height(rendered); got > minTerminalHeight-2 {
		t.Fatalf("scope prompt height=%d exceeds minimum terminal budget:\n%s", got, rendered)
	}

	m.readScopePrompt = nil
	m.readScopeOpRunning = true
	m.readScopeOpLabel = "Checking external read root"
	if m.needsSpinner() || m.needsScramble() {
		t.Fatal("reduced-motion read-scope activity started a decorative animation clock")
	}
	if m.startActivityCmd() == nil || !m.activityHeartbeatPending {
		t.Fatal("reduced-motion read-scope activity omitted its informational heartbeat")
	}
	if line := m.renderWorkingLine(); !strings.Contains(line, "…") {
		t.Fatalf("reduced-motion activity lost static unfinished marker: %q", line)
	}
}

func TestReadScopePromptOwnsPointerInput(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent(strings.Repeat("transcript line\n", 80))
	m.transcriptGotoTop()
	m.toolEntries = []ToolEntry{{Collapsed: true}}
	m.toolHitRegions = []toolHitRegion{{ToolIndex: 0, Row: 0, EndCol: 12}}
	m.readScopePrompt = &ReadScopePrompt{Canonical: "/external", Workspace: "/workspace"}

	updated, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	m = updated.(*Model)
	if got := m.transcriptYOffset(); got != 0 {
		t.Fatalf("read-scope wheel moved transcript to %d", got)
	}
	updated, _ = m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if !m.toolEntries[0].Collapsed {
		t.Fatal("read-scope click toggled a ToolCard behind the authority prompt")
	}
}

func TestUndersizedTerminalPausesReadScopeDecisionAndDraftUntilResize(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "evidence.txt")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("review me"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	m.agent.SetWorkDir(workspace)
	inspection, err := m.agent.InspectReadPath(external)
	if err != nil {
		t.Fatal(err)
	}
	draft := "Analyze the external evidence."
	m.input.SetValue(draft)
	m.readScopePrompt = &ReadScopePrompt{
		Requested: external,
		Canonical: inspection.Path,
		Workspace: workspace,
		Draft:     draft,
		Kind:      agent.ReadGrantExactFile,
		Grants:    []agent.ReadGrant{inspection.Grant()},
		Operation: "add-intents",
	}
	m.recalcViewportHeight()

	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth - 1, Height: minTerminalHeight - 1})
	m = updated.(*Model)
	if visible := ansi.Strip(m.View().Content); !strings.Contains(visible, "Input paused") || strings.Contains(visible, "evidence.txt") {
		t.Fatalf("undersized frame exposed the hidden read decision instead of pause state:\n%s", visible)
	}

	for _, input := range []tea.Msg{charKey('y'), charKey('n'), enterKey(), escKey(), tea.PasteMsg{Content: "hidden paste"}} {
		updated, cmd := m.Update(input)
		m = updated.(*Model)
		if cmd != nil || m.readScopePrompt == nil || m.readScopeOpRunning {
			t.Fatalf("hidden input resolved read scope: input=%T cmd=%v prompt=%v running=%v", input, cmd != nil, m.readScopePrompt != nil, m.readScopeOpRunning)
		}
		if m.input.Value() != draft {
			t.Fatalf("hidden input changed draft to %q", m.input.Value())
		}
		if grants := m.agent.ReadGrants(); len(grants) != 0 {
			t.Fatalf("hidden input granted read authority: %#v", grants)
		}
	}

	updated, resumeCmd := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	firstResumeToken := m.terminalInputResumeToken
	firstResumeAt := m.terminalInputResumeAt
	if resumeCmd == nil || !m.terminalInputResumeActive() {
		t.Fatalf("supported resize did not quarantine read-scope input: cmd=%v active=%v", resumeCmd != nil, m.terminalInputResumeActive())
	}
	if visible := ansi.Strip(m.View().Content); !strings.Contains(visible, "INPUT PAUSED") || strings.Contains(visible, "evidence.txt") {
		t.Fatalf("resize quarantine exposed read authority early:\n%s", visible)
	}

	// A key typed against the undersized frame may be delivered after SIGWINCH.
	// It must extend the quiet handshake, never grant authority.
	updated, delayedKeyCmd := m.Update(charKey('y'))
	m = updated.(*Model)
	latestResumeAt := m.terminalInputResumeAt
	if delayedKeyCmd != nil || m.terminalInputResumeToken != firstResumeToken || !latestResumeAt.After(firstResumeAt) || m.readScopePrompt == nil || m.readScopeOpRunning {
		t.Fatalf("delayed read key escaped quarantine: cmd=%v token=%d deadline advanced=%v prompt=%v running=%v", delayedKeyCmd != nil, m.terminalInputResumeToken, latestResumeAt.After(firstResumeAt), m.readScopePrompt != nil, m.readScopeOpRunning)
	}
	updated, remainingCmd := m.Update(terminalInputResumeMsg{Token: firstResumeToken, At: firstResumeAt})
	m = updated.(*Model)
	if remainingCmd == nil || !m.terminalInputResumeActive() || m.readScopePrompt == nil || len(m.agent.ReadGrants()) != 0 {
		t.Fatal("early resume receipt granted or exposed read authority")
	}
	updated, _ = m.Update(terminalInputResumeMsg{Token: firstResumeToken, At: latestResumeAt})
	m = updated.(*Model)
	if m.terminalInputResumePhase != terminalInputResumeAwaitGesture || !strings.Contains(ansi.Strip(m.View().Content), "press enter") {
		t.Fatalf("read-scope drain did not request explicit resume:\n%s", ansi.Strip(m.View().Content))
	}
	updated, confirmationCmd := m.Update(enterKey())
	m = updated.(*Model)
	confirmationToken := m.terminalInputResumeToken
	confirmationAt := m.terminalInputResumeAt
	if confirmationCmd == nil || m.terminalInputResumePhase != terminalInputResumeConfirmationQuiet || m.readScopePrompt == nil {
		t.Fatal("resume gesture leaked into read-scope decision")
	}
	updated, _ = m.Update(terminalInputResumeMsg{Token: confirmationToken, At: confirmationAt})
	m = updated.(*Model)
	visible := ansi.Strip(m.View().Content)
	for _, want := range []string{"exact file", "evidence.txt", "y allow"} {
		if !strings.Contains(strings.ToLower(visible), strings.ToLower(want)) {
			t.Fatalf("read-scope surface did not restore %q after resize:\n%s", want, visible)
		}
	}
	if m.terminalInputResumeActive() || len(m.agent.ReadGrants()) != 0 || m.input.Value() != draft {
		t.Fatalf("quiet handshake changed read state: active=%v grants=%#v draft=%q", m.terminalInputResumeActive(), m.agent.ReadGrants(), m.input.Value())
	}
	updated, _ = m.Update(charKey('n'))
	m = updated.(*Model)
	if m.readScopePrompt != nil || m.input.Value() != draft {
		t.Fatalf("visible denial did not restore exact draft: prompt=%#v draft=%q", m.readScopePrompt, m.input.Value())
	}
}

func TestReadScopePromptFourGrantsFitsAccessibleMinimum(t *testing.T) {
	t.Setenv("SONAR_REDUCED_MOTION", "1")
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	m := newTestModel(t)
	m.width = 30
	m.height = 12
	m.readScopePrompt = &ReadScopePrompt{
		Canonical: "/external/one.txt",
		Workspace: "/workspace/project",
		Grants: []agent.ReadGrant{
			{Path: "/external/one.txt", Kind: agent.ReadGrantExactFile},
			{Path: "/external/two.txt", Kind: agent.ReadGrantExactFile},
			{Path: "/external/three", Kind: agent.ReadGrantDirectory},
			{Path: "/external/four", Kind: agent.ReadGrantDirectory},
		},
	}
	rendered := m.renderReadScopePrompt()
	plain := ansi.Strip(rendered)
	if hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR multi-grant prompt emitted ANSI color: %q", rendered)
	}
	for _, want := range []string{"4 explicit", "exact file", "directory", "y allow", "n deny", "esc cancel"} {
		if !strings.Contains(strings.ToLower(plain), want) {
			t.Fatalf("multi-grant prompt omitted %q:\n%s", want, plain)
		}
	}
	assertRenderedLinesFit(t, rendered, 30)
	if got := lipgloss.Height(rendered); got > 10 {
		t.Fatalf("multi-grant prompt height=%d exceeds 30x12 content budget:\n%s", got, rendered)
	}
}

func TestReadScopePromptBlocksCollidingCompactPathsUntilTheyAreDistinct(t *testing.T) {
	home := t.TempDir()
	var err error
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	targets := []string{
		filepath.Join(home, "a", "shared.txt"),
		filepath.Join(home, "b", "shared.txt"),
	}
	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	grants := make([]agent.ReadGrant, 0, len(targets))
	for _, target := range targets {
		inspection, err := m.agent.InspectReadPath(target)
		if err != nil {
			t.Fatal(err)
		}
		grants = append(grants, inspection.Grant())
	}
	m.width, m.height = 30, 12
	m.readScopePrompt = &ReadScopePrompt{Workspace: workspace, Grants: grants, Operation: "add-intents"}
	rendered := ansi.Strip(m.renderReadScopePrompt())
	if !strings.Contains(rendered, "y disabled") || strings.Contains(rendered, "y allow") {
		t.Fatalf("colliding paths did not disable blind approval:\n%s", rendered)
	}
	updated, cmd := m.Update(charKey('y'))
	m = updated.(*Model)
	if cmd != nil || m.readScopePrompt == nil || len(m.agent.ReadGrants()) != 0 {
		t.Fatalf("narrow collision was confirmable: cmd=%v prompt=%#v grants=%#v", cmd != nil, m.readScopePrompt, m.agent.ReadGrants())
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	if !m.readScopePromptPathsDistinct() {
		t.Fatal("40-column terminal did not expose distinct authority paths")
	}
	updated, cmd = m.Update(charKey('y'))
	m = updated.(*Model)
	if cmd == nil || m.readScopePrompt != nil {
		t.Fatal("distinct paths did not become confirmable after resize")
	}
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)
	if got := m.agent.ReadGrants(); len(got) != 2 {
		t.Fatalf("confirmed distinct grants = %#v", got)
	}

	// Runtime is a scrollable inspection surface and therefore keeps both full
	// escaped identities instead of repeating their compact shared tail.
	runtimeWidth := pickerListWidth(30)
	runtimeView := ansi.Strip(m.buildRuntimeStatusContent(runtimeWidth))
	assertRenderedLinesFit(t, runtimeView, runtimeWidth)
	for _, want := range []string{"~/a/shared.txt", "~/b/shared.txt"} {
		if !strings.Contains(runtimeView, want) {
			t.Fatalf("Runtime lost distinct authority %q:\n%s", want, runtimeView)
		}
	}
}

func TestReadScopeAndRuntimeEscapeTerminalControlsInAuthorityPaths(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	home := t.TempDir()
	var err error
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	external := filepath.Join(home, "external")
	for _, path := range []string{workspace, external} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unsafeName := "x\nz\x1b]52;c;a\a\u202e"
	target := filepath.Join(external, unsafeName)
	if err := os.WriteFile(target, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	inspection, err := m.agent.InspectReadPath(target)
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 80, 24
	promptGrant := inspection.Grant()
	t.Cleanup(promptGrant.Release)
	m.readScopePrompt = &ReadScopePrompt{Workspace: workspace, Grants: []agent.ReadGrant{promptGrant}}
	prompt := m.renderReadScopePrompt()
	commitInspection, err := m.agent.InspectReadPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.agent.AddInspectedReadGrant(commitInspection.Grant()); err != nil {
		t.Fatal(err)
	}
	runtimeView := m.buildRuntimeStatusContent(58)
	for name, rendered := range map[string]string{"prompt": prompt, "runtime": runtimeView} {
		for _, forbidden := range []string{"\x1b]52", "\a", "\u202e"} {
			if strings.Contains(rendered, forbidden) {
				t.Fatalf("%s rendered raw terminal control %q: %q", name, forbidden, rendered)
			}
		}
		for _, escaped := range []string{`\x1b`, `\a`, `\u202e`, `\n`} {
			if !strings.Contains(rendered, escaped) {
				t.Fatalf("%s omitted visible escape %q: %q", name, escaped, rendered)
			}
		}
	}
	grants := m.agent.ReadGrants()
	if len(grants) != 1 || grants[0].Path != inspection.Path {
		t.Fatalf("display escaping changed authority: grants=%#v inspection=%#v", grants, inspection)
	}
}

func TestReadScopeReceiptEscapesAuthorityPathControls(t *testing.T) {
	m := newTestModel(t)
	unsafePath := "/external/report\nApproval skipped\x1b]52;c;payload\a\u202e.txt"
	m.readScopeOpRunning = true
	m.readScopeOpToken = 7

	updated, _ := m.Update(ReadScopeResultMsg{
		Token: 7, Operation: "add-file", Path: unsafePath, Kind: string(agent.ReadGrantExactFile), Count: 1,
	})
	m = updated.(*Model)
	if len(m.entries) == 0 {
		t.Fatal("read-scope receipt was not recorded")
	}
	receipt := m.entries[len(m.entries)-1].Content
	if strings.Contains(receipt, unsafePath) || strings.Contains(receipt, "\nApproval skipped\x1b") {
		t.Fatalf("receipt retained raw authority path controls: %q", receipt)
	}
	for _, escaped := range []string{`\nApproval skipped`, `\x1b`, `\a`, `\u202e`} {
		if !strings.Contains(receipt, escaped) {
			t.Fatalf("receipt omitted visible path escape %q: %q", escaped, receipt)
		}
	}
}

func TestGracefulShutdownWaitsForReadScopeReceipt(t *testing.T) {
	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	m.readScopeOpRunning = true
	m.readScopeOpToken = 4
	if cmd := m.beginShutdown(); cmd == nil {
		t.Fatal("shutdown did not wait for read-scope operation")
	}

	updated, cmd := m.Update(ReadScopeResultMsg{Token: 3, Operation: "clear-read"})
	m = updated.(*Model)
	if !m.readScopeOpRunning || cmd != nil {
		t.Fatal("stale read-scope receipt released shutdown")
	}
	updated, cmd = m.Update(ReadScopeResultMsg{Token: 4, Operation: "clear-read"})
	m = updated.(*Model)
	if m.readScopeOpRunning || cmd == nil {
		t.Fatal("matching read-scope receipt did not release shutdown")
	}
}

func entryText(entries []ChatEntry) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, entry.Content)
	}
	return strings.Join(parts, "\n")
}
