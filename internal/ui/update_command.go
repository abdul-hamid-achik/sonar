package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

// buildCommandContext creates a Context for slash command execution.
func (m *Model) buildCommandContext() *command.Context {
	artifacts, artifactsTruncated := commandArtifactInfos(m.toolEntries)
	ctx := &command.Context{
		Model:              m.model,
		ModelList:          m.modelList,
		Theme:              m.ThemeID(),
		ThemeList:          themeIDs(),
		Provider:           m.activeProviderName(),
		ProviderList:       m.providerNames(),
		AgentProfile:       m.agentProfile,
		AgentList:          m.agentList,
		ToolCount:          m.toolCount,
		ServerCount:        m.serverCount,
		LoadedFile:         m.loadedFile,
		ICEEnabled:         m.iceEnabled,
		ICEConversations:   m.iceConversations,
		ICESessionID:       m.iceSessionID,
		SessionEvalTotal:   m.sessionEvalTotal,
		SessionPromptTotal: m.sessionPromptTotal,
		LatestPromptTokens: m.promptTokens,
		SessionTurnCount:   m.sessionTurnCount,
		NumCtx:             m.numCtx,
		CurrentModel:       m.model,
		Artifacts:          artifacts,
		ArtifactsTruncated: artifactsTruncated,
		FileChanges:        m.fileChanges,
	}
	if m.agent != nil {
		if sessionID := m.agent.ICESessionID(); sessionID != "" {
			ctx.ICESessionID = sessionID
		}
		ctx.Servers = m.commandMCPServers()
		_, _, ctx.MCPToolCount = m.mcpStatusCounts()
		if len(ctx.Servers) == 0 {
			ctx.ServerNames = m.agent.ServerNames()
		}
		ctx.ReadRoots = m.agent.ReadRoots()
		for _, grant := range m.agent.ReadGrants() {
			ctx.ReadGrants = append(ctx.ReadGrants, command.ReadGrantInfo{Path: grant.Path, Kind: string(grant.Kind)})
		}
		ctx.SessionApprovals = m.agent.ListSessionApprovalSummary()
		rules := m.agent.WorkspaceRulesSnapshot()
		ctx.WorkspaceBashPrefixes = append([]string(nil), rules.BashPrefixes...)
		ctx.WorkspaceMCPTools = append([]string(nil), rules.MCPTools...)
		ctx.WorkspaceWritePaths = append([]string(nil), rules.WritePaths...)
	}
	switch {
	case m.skipApprovalsEnabled():
		ctx.ApprovalPosture = string(ApprovalPostureSkipApprovals)
	case m.acceptWorkspaceEditsEnabled():
		ctx.ApprovalPosture = string(ApprovalPostureAcceptWorkspaceEdits)
	default:
		ctx.ApprovalPosture = string(ApprovalPosturePrompted)
	}
	if m.goalRuntime != nil {
		if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err == nil {
			ctx.GoalConfigured = true
			ctx.GoalObjective = snapshot.Objective
			ctx.GoalStatus = string(snapshot.State)
			ctx.GoalPending = snapshot.PendingContinuation != nil
			ctx.GoalExhausted = len(snapshot.ExhaustedBy) > 0
			if snapshot.Blocker != nil {
				ctx.GoalBlocker = string(snapshot.Blocker.Kind)
			}
		}
	}
	ctx.GoalPersistenceDirty = m.goalPersistenceDirty
	ctx.GoalBusy = m.goalOperationRunning || m.goalOperation != ""

	if m.skillMgr != nil {
		for _, s := range m.skillMgr.All() {
			ctx.Skills = append(ctx.Skills, command.SkillInfo{
				Name:        s.Name,
				Description: s.Description,
				Active:      s.Active,
			})
		}
	}

	if m.agent != nil {
		if store := m.agent.MemoryStore(); store != nil {
			ctx.MemoryCount = store.Count()
			recent, _ := store.Recent(20)
			for _, mem := range recent {
				auto := false
				for _, tag := range mem.Tags {
					if tag == "auto" {
						auto = true
						break
					}
				}
				ctx.Memories = append(ctx.Memories, command.MemoryInfo{
					ID:      mem.ID,
					Content: mem.Content,
					Tags:    mem.Tags,
					Auto:    auto,
				})
			}
		}
		for _, ts := range m.agent.MCPToolSummaries() {
			ctx.MCPTools = append(ctx.MCPTools, command.ToolSummary{
				Name:        ts.Name,
				Description: ts.Description,
				Server:      ts.Server,
			})
		}
	}

	return ctx
}

// handleCommandAction processes a command result's action.
func (m *Model) handleCommandAction(result command.Result) tea.Cmd {
	return m.handleCommandActionWithDraft(result, "")
}

func (m *Model) handleCommandActionWithDraft(result command.Result, draft string) tea.Cmd {
	switch result.Action {
	case command.ActionShowHelp:
		m.overlayParent = OverlayNone
		m.overlay = OverlayHelp
		m.initHelpViewport()
		return nil

	case command.ActionClear:
		if m.queuedFollowUpHeld() {
			// submitPreparedInput has already consumed the slash command. Restore
			// it so resolving the old-session owner does not also lose the user's
			// requested reset.
			if draft != "" && strings.TrimSpace(m.input.Value()) == "" {
				m.setComposerDraftAtRune(draft, utf8.RuneCountInString(draft))
			}
			m.blockSessionReplacementForHeldFollowUp("starting a new conversation")
			return nil
		}
		m.agent.ClearHistory()
		m.entries = nil
		m.toolEntries = nil
		m.resetConversationSession()
		m.invalidateEntryCache()
		if result.Text != "" {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: result.Text,
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionQuit:
		return m.beginShutdown()

	case command.ActionAddReadRoot, command.ActionRemoveReadRoot, command.ActionClearReadRoots:
		return m.beginReadScopeAction(result, draft)

	case command.ActionAttachImage:
		if m.goalRuntime != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Images cannot be attached to a host-owned goal continuation. Finish or drop the goal, then attach the image to an ordinary prompt."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		return m.beginImageFileAttachment(result.Data, "")

	case command.ActionListImages:
		if len(m.pendingImages) == 0 {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No images are attached to the pending prompt. Paste or drag an image file path, or run /image <path>."})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: sanitizeTerminalMultiline(m.renderPlainImageList())})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionClearImages:
		count := m.clearPendingImages()
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Cleared %d pending image attachment%s.", count, pluralSuffix(count))})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionForgetImageHistory:
		if m.goalRuntime != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Image history cannot be changed while a durable goal is attached. Finish or drop the goal first."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		return m.forgetHistoricalImages()

	case command.ActionLoadContext:
		path := strings.TrimSpace(result.Data)
		if path == "" {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "load: no path specified"})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		m.fileOpToken++
		token := m.fileOpToken
		m.fileLoading = true
		m.input.Blur()
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Loading context from: %s (Esc cancels)", path)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.resumeFollow()
		load := func() tea.Msg {
			data, err := safeio.ReadRegularFileNoFollow(path, maxLoadedContextBytes, safeio.StartupReadTimeout)
			return ContextLoadResultMsg{Token: token, Path: path, Data: string(data), Err: err}
		}
		return tea.Batch(m.startActivityCmd(), load)

	case command.ActionUnloadContext:
		m.loadedFile = ""
		m.manualLoadedContext = ""
		m.syncLoadedContext()
		if result.Text != "" {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: result.Text,
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionActivateSkill:
		if m.skillMgr != nil {
			if err := m.setManualSkill(result.Data, true); err != nil {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "error",
					Content: err.Error(),
				})
			} else {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "system",
					Content: result.Text,
				})
			}
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionDeactivateSkill:
		if m.skillMgr != nil {
			if err := m.setManualSkill(result.Data, false); err != nil {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "error",
					Content: err.Error(),
				})
			} else {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "system",
					Content: result.Text,
				})
			}
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionSwitchModel:
		// Find last user query for learning
		query := ""
		currentInput := strings.TrimSpace(m.input.Value())
		if currentInput != "" && !strings.HasPrefix(currentInput, "/") {
			query = currentInput
		} else {
			// Find last user message in conversation
			for i := len(m.entries) - 1; i >= 0; i-- {
				if m.entries[i].Kind == "user" {
					query = m.entries[i].Content
					break
				}
			}
		}
		// Record the override for learning
		if m.router != nil && query != "" {
			m.router.RecordOverride(query, result.Data)
		}
		m.selectModel(result.Data)
		return nil

	case command.ActionSwitchProvider:
		return m.beginProviderSwitch(result.Data, result.Text)

	case command.ActionShowProviderPicker:
		m.overlayParent = OverlayNone
		m.openProviderPicker()
		return nil

	case command.ActionShowAgents:
		m.overlayParent = OverlayNone
		m.openAgentHub()
		return nil

	case command.ActionDeleteMemory:
		if m.agent != nil {
			if store := m.agent.MemoryStore(); store != nil {
				if id, err := strconv.Atoi(result.Text); err == nil {
					if deleted, delErr := store.Delete(id); delErr != nil {
						m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("memory delete: %v", delErr)})
					} else if deleted {
						m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Memory #%d deleted.", id)})
					} else {
						m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Memory #%d not found.", id)})
					}
				}
			}
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionSetNumCtx:
		text, apply, err := m.handleContextWindowCommand(result.Data)
		switch {
		case err != nil:
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		case apply != nil:
			// The receipt arrives with the result; say something now so the
			// wait is not silent when a background stream holds the lock.
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "Applying context window…",
			})
		case text != "":
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: text})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return apply

	case command.ActionSaveNumCtx:
		text, err := m.saveConfiguredNumCtx()
		if err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: text})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionMCPReconnect:
		if m.agent != nil && result.Data != "" {
			name := result.Data
			agent := m.agent
			p := m.program
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				tools, err := agent.ReconnectMCPServer(ctx, name)
				if err != nil {
					sendMsg(p, systemNoticeMsg{Text: fmt.Sprintf("MCP reconnect %s: %v", name, err), IsError: true})
				} else {
					sendMsg(p, systemNoticeMsg{Text: fmt.Sprintf("MCP server %s reconnected · %d tools", name, tools)})
				}
			}()
		}
		return nil

	case command.ActionEnableAutoModel:
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: "automatic model routing was removed with the local model inventory; pick a model explicitly",
		})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionShowModelPicker:
		m.overlayParent = OverlayNone
		m.openModelPicker()
		return nil

	case command.ActionShowThemePicker:
		m.overlayParent = OverlayNone
		m.openThemePicker()
		return nil

	case command.ActionShowContextDoctor:
		m.overlayParent = OverlayNone
		return m.openContextDoctor()

	case command.ActionSwitchTheme:
		return m.applyTheme(result.Data)

	case command.ActionSendPrompt:
		if m.goalRuntime != nil {
			return m.rejectPromptWhileGoalAttached(result.Data, true)
		}
		if result.Text != "" {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: result.Text})
		}
		return m.sendToAgent(result.Data)

	case command.ActionCommit:
		if m.commitRunning {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: "A commit is already in progress. Wait for it to finish before starting another.",
			})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: "Generating commit message from staged changes. Automated /commit disables Git hooks, signing, fsmonitor, and background maintenance.",
		})
		m.refreshTranscript()
		m.resumeFollow()
		m.commitToken++
		ctx, cancel := context.WithCancel(context.Background())
		m.commitCancel = cancel
		m.commitRunning = true
		m.input.Blur()
		runner := m.commitRunner
		if runner == nil {
			runner = runCommit
		}
		return tea.Batch(
			m.startActivityCmd(),
			runner(ctx, m.agent.LLMClient(), m.model, result.Data, m.agent.WorkDir(), m.commitToken),
		)

	case command.ActionShowSessions:
		m.overlayParent = OverlayNone
		m.openSessionsPicker()
		return m.requestSessions()

	case command.ActionSwitchAgent:
		if err := m.applyAgentProfile(result.Data); err != nil {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: err.Error(),
			})
		} else {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: result.Text,
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionExport:
		path := result.Data
		if path == "" {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: "export: no path specified",
			})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		if m.exportRunning {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "An export is already in progress. Wait for its receipt before starting another."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		content := []byte(m.formatConversationForExport())
		workDir := m.agent.WorkDir()
		force := result.Force
		m.exportToken++
		token := m.exportToken
		m.exportRunning = true
		m.input.Blur()
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Exporting conversation to: %s", path)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.resumeFollow()
		return tea.Batch(m.startActivityCmd(), exportConversationCmd(workDir, path, content, force, token))

	case command.ActionImport:
		if m.queuedFollowUpHeld() {
			// Import replaces both the visible and model transcripts. Keep the
			// consumed slash command recoverable until the old-session follow-up
			// owner is explicitly swapped or cleared.
			if draft != "" && strings.TrimSpace(m.input.Value()) == "" {
				m.setComposerDraftAtRune(draft, utf8.RuneCountInString(draft))
			}
			m.blockSessionReplacementForHeldFollowUp("importing another conversation")
			return nil
		}
		path := result.Data
		if path == "" {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: "import: no path specified",
			})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		m.fileOpToken++
		token := m.fileOpToken
		m.fileLoading = true
		m.input.Blur()
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Importing conversation from: %s (Esc cancels)", path)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.resumeFollow()
		load := func() tea.Msg {
			data, err := safeio.ReadRegularFile(path, maxImportBytes, safeio.StartupReadTimeout)
			if err != nil {
				return ImportResultMsg{Token: token, Path: path, Err: err}
			}
			entries, err := parseImportedConversationData(string(data))
			if err != nil {
				return ImportResultMsg{Token: token, Path: path, Err: fmt.Errorf("parse transcript: %w", err)}
			}
			messages, uiOnlySections, err := importedConversationMessages(entries)
			if err != nil {
				return ImportResultMsg{Token: token, Path: path, Err: fmt.Errorf("reject transcript: %w", err)}
			}
			toolSections := 0
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "## Tool:") {
					toolSections++
				}
			}
			return ImportResultMsg{
				Token: token, Path: path, Entries: entries, Messages: messages,
				UIOnlySections: uiOnlySections, ToolSections: toolSections,
			}
		}
		return tea.Batch(m.startActivityCmd(), load)

	case command.ActionCheckpoint:
		id, err := m.agent.CreateCheckpoint(context.Background(), result.Data, "manual")
		var note string
		if err != nil {
			note = fmt.Sprintf("checkpoint failed: %v", err)
		} else if id == 0 {
			note = "checkpoints are unavailable (database not open)"
		} else {
			label := result.Data
			if label != "" {
				label = " \"" + label + "\""
			}
			note = fmt.Sprintf("saved checkpoint #%d%s — restore with /restore %d", id, label, id)
		}
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: note})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionListCheckpoints:
		cps, err := m.agent.ListCheckpoints(context.Background())
		var b strings.Builder
		if err != nil {
			fmt.Fprintf(&b, "could not list checkpoints: %v", err)
		} else if len(cps) == 0 {
			b.WriteString("No checkpoints yet. Save one with /checkpoint [label].")
		} else {
			fmt.Fprintf(&b, "Checkpoints (%d) — restore with /restore <id>:\n", len(cps))
			for _, c := range cps {
				label := c.Label
				if label == "" {
					label = "(no label)"
				}
				fmt.Fprintf(&b, "  #%d  %s  ·  %s  ·  %d msgs  ·  %s\n", c.ID, label, c.Kind, c.MsgCount, c.CreatedAt)
			}
		}
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: strings.TrimRight(b.String(), "\n")})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionRestoreCheckpoint:
		id, perr := strconv.ParseInt(strings.TrimSpace(result.Data), 10, 64)
		if perr != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("restore: %q is not a valid checkpoint id", result.Data)})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		n, err := m.agent.RestoreCheckpoint(context.Background(), id)
		if err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("restore failed: %v", err)})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		// Rebuild the visible transcript from bounded checkpoint facts. Tool
		// receipts retain causal placement and an inspectable attention state,
		// while raw result/argument content stays inside the agent boundary.
		m.entries, m.toolEntries = checkpointTranscriptFromMessages(m.agent.Messages())
		m.toolsPending = 0
		m.resetEntryMemo()
		m.invalidateEntryCache()
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("restored checkpoint #%d — conversation rewound to %d messages", id, n),
		})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionOpenPlan:
		if m.goalRuntime != nil {
			return m.rejectPromptWhileGoalAttached(result.Data, true)
		}
		m.setMode(ModePlan)
		m.openPlanForm(result.Data)
		return nil

	case command.ActionOpenGoal:
		var err error
		var goalCmd tea.Cmd
		if result.Goal != nil {
			goalCmd, err = m.openGoalRequestForm(*result.Goal)
		} else {
			err = m.openGoalForm(result.Data, false)
		}
		if err != nil {
			m.appendGoalError(err.Error())
		}
		return goalCmd

	case command.ActionEditGoalBudget:
		if err := m.openGoalForm("", true); err != nil {
			m.appendGoalError(err.Error())
		}
		return nil

	case command.ActionShowGoal:
		// Opening a centered modal over an asynchronously changing empty-state
		// header can otherwise leave terminal cells from the previous frame in
		// the modal gutter. Request one full Charm repaint at the ownership
		// transition; the inspector remains a dumb child and any recovery load
		// still runs beside the presentation command.
		return tea.Batch(m.showGoal(), tea.ClearScreen)

	case command.ActionPauseGoal:
		m.pauseGoal()
		return nil

	case command.ActionResumeGoal:
		return m.resumeGoal()

	case command.ActionDropGoal:
		m.dropGoal()
		return nil

	case command.ActionRecoverExecution:
		return m.openStandaloneRecovery()

	case command.ActionPermissionsAcceptEdits:
		switch strings.ToLower(strings.TrimSpace(result.Data)) {
		case "on":
			m.SetApprovalPosture(ApprovalPostureAcceptWorkspaceEdits)
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "Accept workspace edits enabled. write/edit/mkdir inside the workspace (or an explicit write grant) no longer prompt. bash, remove, MCP, and memory still require approval. Process-local; lost on restart.",
			})
		case "off":
			m.SetApprovalPosture(ApprovalPosturePrompted)
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "Accept workspace edits disabled. Approval-gated tools prompt again.",
			})
		default:
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: "usage: /permissions accept-edits on|off",
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsClear:
		count := 0
		if m.agent != nil {
			count = m.agent.RevokeSessionApprovals("")
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Cleared %d process-local session approval grant%s.", count, pluralSuffix(count)),
		})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsRevoke:
		tool := strings.TrimSpace(result.Data)
		count := 0
		if m.agent != nil {
			count = m.agent.RevokeSessionApprovals(tool)
		}
		var note string
		switch {
		case tool == "" && count == 0:
			note = "No process-local session approval grants to revoke."
		case tool == "":
			note = fmt.Sprintf("Revoked %d process-local session approval grant%s.", count, pluralSuffix(count))
		case count == 0:
			note = fmt.Sprintf("No process-local session approval grants for tool %q.", tool)
		default:
			note = fmt.Sprintf("Revoked %d process-local session approval grant%s for tool %q.", count, pluralSuffix(count), tool)
		}
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: note})
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsAllowBash:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		rules, err := m.agent.AddWorkspaceBashPrefix(result.Data)
		if err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: fmt.Sprintf("Saved durable bash prefix for this workspace (%d total).", len(rules.BashPrefixes)),
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsAllowMCP:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		rules, err := m.agent.AddWorkspaceMCPTool(result.Data)
		if err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: fmt.Sprintf("Saved durable MCP tool allow for this workspace (%d total).", len(rules.MCPTools)),
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsForgetBash:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		_, removed, err := m.agent.RemoveWorkspaceBashPrefix(result.Data)
		switch {
		case err != nil:
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		case !removed:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching durable bash prefix."})
		default:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable bash prefix."})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsForgetMCP:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		_, removed, err := m.agent.RemoveWorkspaceMCPTool(result.Data)
		switch {
		case err != nil:
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		case !removed:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching durable MCP tool allow."})
		default:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable MCP tool allow."})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsAllowPath:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		rules, err := m.agent.AddWorkspaceWritePath(result.Data)
		if err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: fmt.Sprintf("Saved durable write path for this workspace (%d total). Covers write/edit/mkdir only.", len(rules.WritePaths)),
			})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsForgetPath:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		_, removed, err := m.agent.RemoveWorkspaceWritePath(result.Data)
		switch {
		case err != nil:
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		case !removed:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching durable write path."})
		default:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable write path."})
		}
		m.refreshTranscript()
		m.resumeFollow()
		return nil

	case command.ActionPermissionsPanel:
		m.preemptTranscriptSearch()
		m.clearViewerModals(false)
		m.overlayParent = OverlayNone
		m.openPermissionsPanel()
		return nil

	case command.ActionPermissionsExport:
		return m.exportWorkspaceRules(result.Data)

	case command.ActionPermissionsImport:
		return m.importWorkspaceRules(result.Data)

	case command.ActionPermissionsClearRules:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		if _, err := m.agent.ClearWorkspaceRules(); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Cleared all durable workspace rules."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
		m.resumeFollow()
		return nil

	default:
		if result.Action != command.ActionNone {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: fmt.Sprintf("unsupported command action: %d", result.Action),
			})
			m.refreshTranscript()
			m.resumeFollow()
			return nil
		}
		// ActionNone — just show text.
		if result.Text != "" {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: result.Text,
			})
			m.refreshTranscript()
			m.resumeFollow()
		}
		return nil
	}
}

// handleCommandResult renders an asynchronous slash-command receipt.
func (m *Model) handleCommandResult(msg CommandResultMsg) {
	if msg.Text != "" {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: msg.Text,
		})
		m.refreshTranscript()
		m.resumeFollow()
	}
}
