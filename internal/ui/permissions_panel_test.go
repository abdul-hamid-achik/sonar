package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestPermissionsPanelOpensFromSettings(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	m.openSettingsChild(m.openPermissionsPanel)
	if m.overlay != OverlayPermissions || m.permissionsPanelState == nil {
		t.Fatalf("permissions panel not open: overlay=%d state=%v", m.overlay, m.permissionsPanelState)
	}
	if m.overlayParent != OverlaySettings {
		t.Fatalf("overlayParent = %d, want Settings", m.overlayParent)
	}
	rendered := ansi.Strip(m.renderPermissionsPanel())
	for _, want := range []string{"Permissions", "Accept workspace edits", "This workspace", "Export"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("panel missing %q:\n%s", want, rendered)
		}
	}
	// Esc returns to settings.
	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlaySettings || m.permissionsPanelState != nil {
		t.Fatalf("esc did not return to settings: overlay=%d panel=%v", m.overlay, m.permissionsPanelState)
	}
}

func TestPermissionsPanelExportWritesPortableFile(t *testing.T) {
	m := newTestModel(t)
	workDir := t.TempDir()
	m.agent.SetWorkDir(workDir)
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.agent.SetWorkspaceRulesStore(store)
	if _, err := m.agent.AddWorkspaceBashPrefix("go test *"); err != nil {
		t.Fatal(err)
	}
	m.openPermissionsPanel()
	for _, item := range m.permissionsPanelItems() {
		if item.action == permissionsExport {
			m.activatePermissionsItem(item)
			break
		}
	}
	out := filepath.Join(workDir, permission.DefaultExportFileName)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("export file missing: %v", err)
	}
	if !strings.Contains(string(data), "go test *") {
		t.Fatalf("export content = %s", data)
	}
	if _, err := m.agent.ClearWorkspaceRules(); err != nil {
		t.Fatal(err)
	}
	m.importWorkspaceRules(out)
	rules := m.agent.WorkspaceRulesSnapshot()
	if !rules.AllowsBash("go test ./pkg") {
		t.Fatalf("import lost bash rule: %#v", rules)
	}
}

func TestPermissionsPanelToggleAcceptEdits(t *testing.T) {
	m := newTestModel(t)
	m.openPermissionsPanel()
	item := permissionsItem{action: permissionsToggleAcceptEdits}
	m.activatePermissionsItem(item)
	if !m.acceptWorkspaceEditsEnabled() {
		t.Fatal("accept edits should be on")
	}
	m.activatePermissionsItem(item)
	if m.acceptWorkspaceEditsEnabled() {
		t.Fatal("accept edits should toggle off")
	}
}

func TestPermissionsPanelCommandAction(t *testing.T) {
	m := newTestModel(t)
	result := m.cmdRegistry.Execute(m.buildCommandContext(), "permissions", []string{"panel"})
	if result.Action != command.ActionPermissionsPanel {
		t.Fatalf("action = %v", result.Action)
	}
	// Mimic command dispatch.
	m.overlayParent = OverlayNone
	m.openPermissionsPanel()
	if m.overlay != OverlayPermissions {
		t.Fatal("panel not open")
	}
}

// The Codex-shaped posture section: a handful of presets described by what
// runs without asking, the current one marked, selection committing mode and
// approval posture together.
func TestPermissionsPanelPostureRowsLeadThePanel(t *testing.T) {
	m := newTestModel(t)
	items := m.permissionsPanelItems()
	if len(items) < 5 {
		t.Fatalf("expected posture presets before management rows, got %d items", len(items))
	}
	wantOrder := []permissionsAction{
		permissionsPostureReadOnly, permissionsPostureAsk,
		permissionsPostureAcceptEdits, permissionsPostureAuto, permissionsPostureSkipInfo,
	}
	for i, action := range wantOrder {
		if items[i].action != action {
			t.Fatalf("row %d = %v, want %v", i, items[i].action, action)
		}
	}
	// NORMAL + prompted is the default posture and must be the marked row.
	if got := items[1].value; got != "current" {
		t.Fatalf("Ask row should be current, value = %q", got)
	}
}

func TestPermissionsPanelPostureSelectionCommitsModeAndPosture(t *testing.T) {
	m := newTestModel(t)
	m.openPermissionsPanel()
	m.activatePermissionsItem(permissionsItem{action: permissionsPostureAuto})
	if m.mode != ModeAuto {
		t.Fatalf("mode = %v, want AUTO", m.mode)
	}
	if m.overlay == OverlayPermissions {
		t.Fatal("panel should close after committing a posture")
	}

	m.openPermissionsPanel()
	m.activatePermissionsItem(permissionsItem{action: permissionsPostureAcceptEdits})
	if m.mode != ModeNormal || !m.acceptWorkspaceEditsEnabled() {
		t.Fatalf("accept-edits posture: mode=%v acceptEdits=%t", m.mode, m.acceptWorkspaceEditsEnabled())
	}

	m.openPermissionsPanel()
	m.activatePermissionsItem(permissionsItem{action: permissionsPostureReadOnly})
	if m.mode != ModePlan || m.acceptWorkspaceEditsEnabled() {
		t.Fatalf("read-only posture: mode=%v acceptEdits=%t", m.mode, m.acceptWorkspaceEditsEnabled())
	}
}

func TestPermissionsPanelReSelectingCurrentPostureIsANoOp(t *testing.T) {
	m := newTestModel(t)
	m.openPermissionsPanel()
	entriesBefore := len(m.entries)
	m.activatePermissionsItem(permissionsItem{action: permissionsPostureAsk})
	if m.mode != ModeNormal {
		t.Fatalf("mode changed on re-selecting current posture: %v", m.mode)
	}
	if len(m.entries) != entriesBefore {
		t.Fatal("re-selecting the current posture must not post a notice")
	}
}

func TestPermissionsPanelSkipApprovalsRowIsInformational(t *testing.T) {
	m := newTestModel(t)
	m.openPermissionsPanel()
	modeBefore := m.mode
	m.activatePermissionsItem(permissionsItem{action: permissionsPostureSkipInfo})
	if m.mode != modeBefore {
		t.Fatal("skip-approvals row must not change mode")
	}
	if m.skipApprovalsEnabled() {
		t.Fatal("skip-approvals must never be enabled from the panel")
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "--skip-approvals") {
		t.Fatal("the row should explain the launch flag")
	}
}

func TestPermissionsPanelSectionedLayout(t *testing.T) {
	m := newTestModel(t)
	workDir := t.TempDir()
	m.agent.SetWorkDir(workDir)
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.agent.SetWorkspaceRulesStore(store)
	
	// Add some workspace rules so the workspace section appears
	if _, err := m.agent.AddWorkspaceBashPrefix("npm run *"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.agent.AddWorkspaceWritePath("src/test.go"); err != nil {
		t.Fatal(err)
	}
	
	items := m.permissionsPanelItems()
	
	// Check that workspace section header exists
	foundWorkspaceSection := false
	for _, item := range items {
		if item.action == permissionsSectionHeader {
			if strings.Contains(item.title, "This workspace") {
				foundWorkspaceSection = true
			}
		}
	}
	
	if !foundWorkspaceSection {
		t.Fatal("should have 'This workspace' section header")
	}
}

func TestPermissionsPanelObjectFirstCopy(t *testing.T) {
	m := newTestModel(t)
	workDir := t.TempDir()
	m.agent.SetWorkDir(workDir)
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.agent.SetWorkspaceRulesStore(store)
	
	// Add a bash rule
	if _, err := m.agent.AddWorkspaceBashPrefix("git status *"); err != nil {
		t.Fatal(err)
	}
	
	items := m.permissionsPanelItems()
	
	// Find the bash rule item
	var bashItem *permissionsItem
	for _, item := range items {
		if item.action == permissionsForgetBash {
			bashItem = &item
			break
		}
	}
	
	if bashItem == nil {
		t.Fatal("bash rule item not found")
	}
	
	// Check that the title starts with the kind, not the action
	if !strings.HasPrefix(bashItem.title, "Bash · ") {
		t.Fatalf("bash item title should be object-first, got %q", bashItem.title)
	}
	
	// Check that the action is on the right (in value)
	if bashItem.value != "forget" {
		t.Fatalf("bash item value should be 'forget', got %q", bashItem.value)
	}
}
