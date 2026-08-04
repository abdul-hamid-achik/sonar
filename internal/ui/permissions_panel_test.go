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
	for _, want := range []string{"Permissions", "Accept workspace edits", "Export workspace rules"} {
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
