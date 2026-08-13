package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// runSlashWhileBusy executes one slash command immediately while an agent turn
// is in flight, without consuming the follow-up queue slot. It consumes the
// draft only when the command is known and its action is safe to run mid-turn;
// otherwise it reports false and the caller keeps the deferred behaviour: the
// draft occupies the single follow-up slot and runs after the turn settles.
func (m *Model) runSlashWhileBusy(text string) (tea.Cmd, bool) {
	name, args, err := parseSlashCommandInput(text)
	if err != nil || m.cmdRegistry == nil {
		return nil, false
	}
	result := m.cmdRegistry.Execute(m.buildCommandContext(), name, args)
	if result.Error != "" || !slashSafeWhileBusy(result.Action) {
		return nil, false
	}
	m.pushHistory(text)
	m.clearCompletionSuppression()
	m.input.Reset()
	m.syncInputHeight()
	return m.handleCommandActionWithDraft(result, text), true
}

// slashSafeWhileBusy reports whether a command action can run while an agent
// turn is in flight. Pure UI, read-only information, and future-turn policy
// effects are safe; anything that replaces conversation/session state, starts
// a second agent turn, or mutates the execution substrate (model, provider,
// context window, MCP servers, checkpoints, goals) is deferred to the next
// iteration.
func slashSafeWhileBusy(action command.Action) bool {
	switch action {
	case command.ActionNone,
		command.ActionShowHelp,
		command.ActionShowThemePicker,
		command.ActionSwitchTheme,
		command.ActionToggleMouseCapture,
		command.ActionShowContextDoctor,
		command.ActionPermissionsPanel,
		command.ActionPermissionsAcceptEdits,
		command.ActionPermissionsClear,
		command.ActionPermissionsRevoke,
		command.ActionPermissionsAllowBash,
		command.ActionPermissionsAllowMCP,
		command.ActionPermissionsAllowPath,
		command.ActionPermissionsForgetBash,
		command.ActionPermissionsForgetMCP,
		command.ActionPermissionsForgetPath,
		command.ActionListImages,
		command.ActionClearImages,
		command.ActionDeleteMemory,
		command.ActionExport:
		return true
	default:
		return false
	}
}
