package ui

import (
	"strings"
	"testing"
)

func TestStartupAcceptsDraftBeforeTurnAdmission(t *testing.T) {
	m := newTestModel(t)
	m.initializing = true
	m.turnReady = false

	for _, char := range "hello" {
		updated, _ := m.Update(charKey(char))
		m = updated.(*Model)
	}
	if got := m.input.Value(); got != "hello" {
		t.Fatalf("startup draft = %q, want hello (editable=%v focused=%v state=%v overlay=%v)", got, m.composerEditable(), m.input.Focused(), m.state, m.overlay)
	}

	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("startup send did not surface a bounded draft-kept notice")
	}
	if got := m.input.Value(); got != "hello" {
		t.Fatalf("startup send consumed draft = %q", got)
	}
	if m.turnReady {
		t.Fatal("draft input opened turn admission")
	}
}

func TestCoreReadyOpensTurnsWithoutSettlingOptionalStartup(t *testing.T) {
	m := newTestModel(t)
	m.initializing = true
	m.turnReady = false
	m.startupItems = []startupItem{{ID: "mcp:slow", Status: "connecting"}}

	updated, _ := m.Update(CoreReadyMsg{
		Model: "model-a", ModelList: []string{"model-a"}, AgentProfile: "coder", NumCtx: 8192,
	})
	m = updated.(*Model)

	if !m.turnReady || !m.initializing {
		t.Fatalf("core readiness = turnReady:%v initializing:%v", m.turnReady, m.initializing)
	}
	if len(m.startupItems) != 1 {
		t.Fatalf("core readiness discarded optional startup progress: %#v", m.startupItems)
	}
	if m.model != "model-a" || m.agentProfile != "coder" || m.numCtx != 8192 {
		t.Fatalf("core projection = model:%q profile:%q numCtx:%d", m.model, m.agentProfile, m.numCtx)
	}
}

func TestAdmittedTurnOwnsActivityWhileOptionalStartupContinues(t *testing.T) {
	m := newTestModel(t)
	m.initializing = true
	m.turnReady = true
	m.state = StateWaiting

	activity, ok := m.currentWorkingActivity()
	if !ok || activity.label != "Running" {
		t.Fatalf("active turn lost activity ownership to startup: %#v, active=%v", activity, ok)
	}
}

func TestStartupKeepsStableWelcomeShellAndOneFooterProgressLine(t *testing.T) {
	m := newTestModel(t)
	m.initializing = true
	updated, _ := m.Update(StartupStatusMsg{
		ID: "ollama", Label: "Ollama", Status: "connecting", Detail: "line one\nline two",
	})
	m = updated.(*Model)
	updated, _ = m.Update(StartupStatusMsg{
		ID: "mcp:local", Label: "MCP", Status: "connected", Detail: `{"secret":"hidden"}`,
	})
	m = updated.(*Model)

	content := m.renderEntries()
	// Roomy empty canvas is quiet; startup progress lives only in the footer.
	for _, noise := range []string{"SONAR", "API-first", "Ask, @mention files, or type /help"} {
		if strings.Contains(content, noise) {
			t.Errorf("startup still paints mid-canvas welcome %q:\n%s", noise, content)
		}
	}
	for _, hidden := range []string{"line one line two", "details available in logs"} {
		if strings.Contains(content, hidden) {
			t.Errorf("startup shell exposed per-service detail %q:\n%s", hidden, content)
		}
	}
	status := m.renderStatusLine()
	for _, want := range []string{"Starting", "ollama", "1/2"} {
		if !strings.Contains(status, want) {
			t.Fatalf("startup footer omitted %q: %q", want, status)
		}
	}
	if strings.Contains(strings.ToLower(status), "ready") {
		t.Fatalf("initializing status claimed readiness: %q", status)
	}
	assertRenderedLinesFit(t, content, m.chatPaneWidth())

	m.initializing = false
	m.startupItems = nil
	if settled := m.renderEntries(); settled != content {
		t.Fatalf("welcome shell jumped when startup settled:\nduring:\n%s\nafter:\n%s", content, settled)
	}
}

func TestPreWindowStartupUsesProductShellInsteadOfDebugPlaceholder(t *testing.T) {
	m := newTestModel(t)
	m.ready = false
	view := m.View()
	for _, want := range []string{"SONAR", "Starting"} {
		if !strings.Contains(view.Content, want) {
			t.Fatalf("pre-window startup omitted %q: %q", want, view.Content)
		}
	}
	if strings.Contains(strings.ToLower(view.Content), "initializing") {
		t.Fatalf("pre-window startup leaked implementation placeholder: %q", view.Content)
	}
}

func TestStartupStatusUpdatesByID(t *testing.T) {
	m := newTestModel(t)
	m.initializing = true
	updated, _ := m.Update(StartupStatusMsg{ID: "ollama", Label: "Ollama", Status: "connecting"})
	m = updated.(*Model)
	updated, _ = m.Update(StartupStatusMsg{ID: "ollama", Label: "Ollama", Status: "connected"})
	m = updated.(*Model)
	if len(m.startupItems) != 1 || m.startupItems[0].Status != "connected" {
		t.Fatalf("startup update was duplicated: %#v", m.startupItems)
	}
}

func TestSanitizeStartupDetail(t *testing.T) {
	if got := sanitizeStartupDetail(" a\n\tb   c "); got != "a b c" {
		t.Fatalf("sanitized detail = %q", got)
	}
	if got := sanitizeStartupDetail(`[1,2,3]`); got != "details available in logs" {
		t.Fatalf("JSON detail = %q", got)
	}
}
