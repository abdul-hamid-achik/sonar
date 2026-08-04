package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// ReadScopePrompt is transient presentation authority for canonicalized
// external paths. It is never serialized into session state.
type ReadScopePrompt struct {
	Requested   string
	Canonical   string
	Workspace   string
	Draft       string
	Kind        agent.ReadGrantKind
	Grants      []agent.ReadGrant
	WriteGrants []agent.WriteGrant
	Operation   string
	AutoResume  bool
}

func (m *Model) beginReadScopeAction(result command.Result, draft string) tea.Cmd {
	if m.agent == nil {
		m.appendReadScopeError("/scope is unavailable before the agent is initialized.")
		return nil
	}
	if m.readScopeOpRunning || m.readScopePrompt != nil {
		m.appendReadScopeError("A /scope change is already in progress.")
		return nil
	}

	operation := "add-read"
	path := strings.TrimSpace(result.Data)
	switch result.Action {
	case command.ActionRemoveReadRoot:
		operation = "remove-read"
	case command.ActionClearReadRoots:
		operation = "clear-read"
	}
	if operation != "clear-read" && path == "" {
		m.appendReadScopeError(fmt.Sprintf("/scope %s: no directory specified", operation))
		return nil
	}
	if strings.TrimSpace(draft) == "" {
		draft = "/scope " + operation
		if path != "" {
			draft += " " + path
		}
	}
	if operation == "add-read" {
		return m.beginReadScopePreview(path, draft)
	}
	return m.beginReadScopeMutation(operation, path, draft)
}

func (m *Model) beginReadScopePreview(path, draft string) tea.Cmd {
	m.readScopeOpToken++
	token := m.readScopeOpToken
	m.readScopeOpRunning = true
	m.readScopeOpLabel = "Checking external read root"
	m.readScopeOpDraft = draft
	m.input.Blur()
	m.recalcViewportHeight()

	workspace := m.agent.WorkDir()
	agentInstance := m.agent
	preview := func() tea.Msg {
		inspection, err := agentInstance.InspectReadPath(path)
		if err == nil && inspection.Kind != agent.ReadGrantDirectory {
			err = fmt.Errorf("read-only root is not a directory: %s", inspection.Path)
		}
		if err == nil && !inspection.External {
			err = fmt.Errorf("read-only root %q overlaps writable workspace %q", inspection.Path, canonicalReadScopeWorkspace(workspace))
		}
		if err == nil && inspection.AlreadyReadable {
			err = fmt.Errorf("read-only root is already inside active read authority: %s", inspection.Path)
		}
		canonicalWorkspace := canonicalReadScopeWorkspace(workspace)
		return ReadScopePreviewResultMsg{
			Token: token, Requested: path, Canonical: inspection.Path,
			Workspace: canonicalWorkspace, Draft: draft, Grant: inspection.Grant(), Err: err,
		}
	}
	return tea.Batch(m.startActivityCmd(), preview)
}

func (m *Model) handleReadScopePreviewResult(msg ReadScopePreviewResultMsg) {
	if !m.readScopeOpRunning || msg.Token != m.readScopeOpToken {
		msg.Grant.Release()
		return
	}
	m.readScopeOpRunning = false
	m.readScopeOpLabel = ""
	if m.shuttingDown {
		msg.Grant.Release()
		m.readScopeOpDraft = ""
		return
	}
	// Preview completion either installs the host-owned scope decision or
	// restores the held draft after an error. Do not let either settle behind a
	// search footer that opened while the read inspection was running.
	m.preemptTranscriptSearch()
	if msg.Err != nil {
		msg.Grant.Release()
		m.restoreReadScopeDraft(msg.Draft)
		m.readScopeOpDraft = ""
		m.appendReadScopeError(fmt.Sprintf("/scope add-read preview failed: %v", msg.Err))
		return
	}
	m.readScopePrompt = &ReadScopePrompt{
		Requested: msg.Requested, Canonical: msg.Canonical,
		Workspace: msg.Workspace, Draft: msg.Draft, Kind: agent.ReadGrantDirectory,
		Grants: []agent.ReadGrant{msg.Grant}, Operation: "add-read",
	}
	m.readScopeOpDraft = ""
	m.input.Blur()
	m.recalcViewportHeight()
}

func (m *Model) confirmReadScopePrompt() tea.Cmd {
	if m.readScopePrompt == nil {
		return nil
	}
	if !m.readScopePromptPathsDistinct() {
		// Resizing recomputes the projection. Keep the exact prompt and draft in
		// place until every authority is visually distinguishable.
		return nil
	}
	prompt := *m.readScopePrompt
	m.readScopePrompt = nil
	if len(prompt.Grants) > 0 || len(prompt.WriteGrants) > 0 {
		operation := prompt.Operation
		if operation == "" {
			operation = "add-intents"
		}
		return m.beginPathGrantMutation(prompt.Grants, prompt.WriteGrants, prompt.Draft, prompt.AutoResume, operation)
	}
	m.restoreReadScopeDraft(prompt.Draft)
	m.appendReadScopeError("External read-path preview expired; inspect the path again.")
	return nil
}

func (m *Model) resolveReadScopePrompt(outcome string) {
	if m.readScopePrompt == nil {
		return
	}
	prompt := *m.readScopePrompt
	m.readScopePrompt = nil
	releaseReadGrants(prompt.Grants)
	releaseWriteGrants(prompt.WriteGrants)
	m.restoreReadScopeDraft(prompt.Draft)
	message := "External read path denied; draft restored."
	if outcome == "cancelled" {
		message = "External read path request cancelled; draft restored."
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: message})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	m.recalcViewportHeight()
}

func (m *Model) beginPathGrantMutation(readGrants []agent.ReadGrant, writeGrants []agent.WriteGrant, draft string, autoResume bool, operation string) tea.Cmd {
	m.readScopeOpToken++
	token := m.readScopeOpToken
	m.readScopeOpRunning = true
	m.readScopeOpDraft = draft
	m.readScopeOpLabel = "Granting approved path scopes"
	m.input.Blur()
	m.recalcViewportHeight()

	agentInstance := m.agent
	approvedReads := append([]agent.ReadGrant(nil), readGrants...)
	approvedWrites := append([]agent.WriteGrant(nil), writeGrants...)
	mutate := func() tea.Msg {
		msg := ReadScopeResultMsg{
			Token: token, Operation: operation, AutoResume: autoResume,
		}
		msg.Grants, msg.WriteGrants, msg.Rollback, msg.Finalize, msg.RolledBack, msg.RollbackErr, msg.Err =
			applyPromptPathGrantsTransactional(agentInstance, approvedReads, approvedWrites)
		msg.Count = len(msg.Grants) + len(msg.WriteGrants)
		if len(msg.Grants) == 1 && len(msg.WriteGrants) == 0 {
			msg.Path = msg.Grants[0].Path
			msg.Kind = string(msg.Grants[0].Kind)
		}
		return msg
	}
	return tea.Batch(m.startActivityCmd(), mutate)
}

// applyPromptReadGrantsTransactional gives a multi-path approval all-or-none
// behavior from the host's perspective. The Agent owns canonicalization and
// authority validation; this layer snapshots only its typed public grants so
// compensation never revokes authority that existed before this approval.
func applyPromptReadGrantsTransactional(agentInstance *agent.Agent, approved []agent.ReadGrant) ([]agent.ReadGrant, int, error, error) {
	if agentInstance == nil {
		releaseReadGrants(approved)
		return nil, 0, nil, errors.New("agent is unavailable")
	}
	defer releaseReadGrants(approved)
	before, snapshotErr := agentInstance.SnapshotReadGrants()
	if snapshotErr != nil {
		return nil, 0, nil, fmt.Errorf("snapshot existing read grants: %w", snapshotErr)
	}
	defer releaseReadGrants(before)
	applied := make([]agent.ReadGrant, 0, len(approved))
	for _, grant := range approved {
		canonical, err := agentInstance.AddInspectedReadGrant(grant)
		if err != nil {
			rolledBack, rollbackErr := restoreReadGrantSnapshot(agentInstance, before)
			return nil, rolledBack, rollbackErr, fmt.Errorf("grant %s read access: %w", grant.Kind, err)
		}
		applied = append(applied, agent.ReadGrant{Path: canonical, Kind: grant.Kind})
	}
	return applied, 0, nil, nil
}

func applyPromptPathGrantsTransactional(agentInstance *agent.Agent, approvedReads []agent.ReadGrant, approvedWrites []agent.WriteGrant) ([]agent.ReadGrant, []agent.WriteGrant, func() (int, error), func(), int, error, error) {
	if agentInstance == nil {
		releaseReadGrants(approvedReads)
		releaseWriteGrants(approvedWrites)
		return nil, nil, nil, nil, 0, nil, errors.New("agent is unavailable")
	}
	defer releaseReadGrants(approvedReads)
	defer releaseWriteGrants(approvedWrites)
	beforeReads, snapshotErr := agentInstance.SnapshotReadGrants()
	if snapshotErr != nil {
		return nil, nil, nil, nil, 0, nil, fmt.Errorf("snapshot existing read grants: %w", snapshotErr)
	}
	beforeWrites := agentInstance.WriteGrants()

	appliedReads := make([]agent.ReadGrant, 0, len(approvedReads))
	for _, grant := range approvedReads {
		canonical, err := agentInstance.AddInspectedReadGrant(grant)
		if err != nil {
			rolledBack, rollbackErr := rollbackPromptPathGrants(agentInstance, beforeReads, beforeWrites)
			releaseReadGrants(beforeReads)
			return nil, nil, nil, nil, rolledBack, rollbackErr, fmt.Errorf("grant %s read access: %w", grant.Kind, err)
		}
		appliedReads = append(appliedReads, agent.ReadGrant{Path: canonical, Kind: grant.Kind})
	}

	appliedWrites := make([]agent.WriteGrant, 0, len(approvedWrites))
	for _, grant := range approvedWrites {
		canonical, err := agentInstance.AddInspectedWriteGrant(grant)
		if err != nil {
			rolledBack, rollbackErr := rollbackPromptPathGrants(agentInstance, beforeReads, beforeWrites)
			releaseReadGrants(beforeReads)
			return nil, nil, nil, nil, rolledBack, rollbackErr, fmt.Errorf("grant %s typed-write access: %w", grant.Kind, err)
		}
		appliedWrites = append(appliedWrites, agent.WriteGrant{Path: canonical, Kind: grant.Kind})
	}
	var once sync.Once
	var rollbackCount int
	var rollbackErr error
	rollback := func() (int, error) {
		once.Do(func() {
			rollbackCount, rollbackErr = rollbackPromptPathGrants(agentInstance, beforeReads, beforeWrites)
			releaseReadGrants(beforeReads)
		})
		return rollbackCount, rollbackErr
	}
	finalize := func() {
		once.Do(func() { releaseReadGrants(beforeReads) })
	}
	return appliedReads, appliedWrites, rollback, finalize, 0, nil, nil
}

func rollbackPromptPathGrants(agentInstance *agent.Agent, beforeReads []agent.ReadGrant, beforeWrites []agent.WriteGrant) (int, error) {
	rolledBack, rollbackErr := restoreReadGrantSnapshot(agentInstance, beforeReads)
	beforeWriteKeys := make(map[string]struct{}, len(beforeWrites))
	for _, grant := range beforeWrites {
		beforeWriteKeys[writeGrantKey(grant)] = struct{}{}
	}
	for _, grant := range agentInstance.WriteGrants() {
		if _, existed := beforeWriteKeys[writeGrantKey(grant)]; existed {
			continue
		}
		if _, err := agentInstance.RemoveWritePath(grant.Path); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("revoke new %s %q: %w", grant.Kind, grant.Path, err))
		} else {
			rolledBack++
		}
	}
	return rolledBack, rollbackErr
}

func writeGrantKey(grant agent.WriteGrant) string {
	return string(grant.Kind) + "\x00" + filepath.Clean(grant.Path)
}

func releaseReadGrants(grants []agent.ReadGrant) {
	for _, grant := range grants {
		grant.Release()
	}
}

func restoreReadGrantSnapshot(agentInstance *agent.Agent, before []agent.ReadGrant) (int, error) {
	beforeByKey := make(map[string]agent.ReadGrant, len(before))
	for _, grant := range before {
		beforeByKey[readGrantKey(grant)] = grant
	}
	current := agentInstance.ReadGrants()
	rolledBack := 0
	var rollbackErr error

	// Remove only authorities introduced by this batch. Directories go first
	// because one may have superseded a pre-existing exact-file grant that must
	// be restored below.
	for _, kind := range []agent.ReadGrantKind{agent.ReadGrantDirectory, agent.ReadGrantExactFile} {
		for _, grant := range current {
			if grant.Kind != kind {
				continue
			}
			if _, existed := beforeByKey[readGrantKey(grant)]; existed {
				continue
			}
			if _, err := agentInstance.RemoveReadPath(grant.Path); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("revoke new %s %q: %w", grant.Kind, grant.Path, err))
			} else {
				rolledBack++
			}
		}
	}

	currentByKey := make(map[string]struct{})
	for _, grant := range agentInstance.ReadGrants() {
		currentByKey[readGrantKey(grant)] = struct{}{}
	}
	// Restore any narrower pre-existing grant superseded by a newly added
	// directory before a later grant failed.
	for _, kind := range []agent.ReadGrantKind{agent.ReadGrantDirectory, agent.ReadGrantExactFile} {
		for _, grant := range before {
			if grant.Kind != kind {
				continue
			}
			if _, present := currentByKey[readGrantKey(grant)]; present {
				continue
			}
			_, err := agentInstance.AddInspectedReadGrant(grant)
			if err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore pre-existing %s %q: %w", grant.Kind, grant.Path, err))
			} else {
				rolledBack++
				currentByKey[readGrantKey(grant)] = struct{}{}
			}
		}
	}
	return rolledBack, rollbackErr
}

func readGrantKey(grant agent.ReadGrant) string {
	return string(grant.Kind) + "\x00" + filepath.Clean(grant.Path)
}

func (m *Model) beginReadScopeMutation(operation, path, draft string) tea.Cmd {
	m.readScopeOpToken++
	token := m.readScopeOpToken
	m.readScopeOpRunning = true
	m.readScopeOpDraft = draft
	switch operation {
	case "add-read":
		m.readScopeOpLabel = "Adding external read-only root"
	case "add-file":
		m.readScopeOpLabel = "Adding exact read-only file"
	case "remove-read":
		m.readScopeOpLabel = "Removing read-only root"
	case "clear-read":
		m.readScopeOpLabel = "Clearing read-only roots"
	default:
		m.readScopeOpLabel = "Updating read-only scope"
	}
	m.input.Blur()
	m.recalcViewportHeight()

	agentInstance := m.agent
	mutate := func() tea.Msg {
		msg := ReadScopeResultMsg{Token: token, Operation: operation, Path: path}
		switch operation {
		case "add-read":
			msg.Path, msg.Err = agentInstance.AddReadRoot(path)
			msg.Kind = string(agent.ReadGrantDirectory)
		case "add-file":
			msg.Path, msg.Err = agentInstance.AddReadFile(path)
			msg.Kind = string(agent.ReadGrantExactFile)
		case "remove-read":
			var grant agent.ReadGrant
			grant, msg.Err = agentInstance.RemoveReadPath(path)
			msg.Path, msg.Kind = grant.Path, string(grant.Kind)
		case "clear-read":
			msg.Count, msg.Err = agentInstance.ClearReadRoots()
		}
		return msg
	}
	return tea.Batch(m.startActivityCmd(), mutate)
}

func (m *Model) handleReadScopeResult(msg ReadScopeResultMsg) tea.Cmd {
	if !m.readScopeOpRunning || msg.Token != m.readScopeOpToken {
		if msg.Rollback != nil {
			_, _ = msg.Rollback()
		} else if msg.Finalize != nil {
			msg.Finalize()
		}
		return nil
	}
	draft := m.readScopeOpDraft
	m.readScopeOpRunning = false
	m.readScopeOpLabel = ""
	m.readScopeOpDraft = ""
	if m.shuttingDown {
		if msg.Rollback != nil {
			_, _ = msg.Rollback()
		} else if msg.Finalize != nil {
			msg.Finalize()
		}
		return nil
	}
	// Mutation completion restores/submits the held draft and changes composer
	// focus. Search may have opened while the scoped filesystem mutation ran.
	m.preemptTranscriptSearch()
	if msg.Finalize != nil {
		msg.Finalize()
	}
	m.input.Focus()
	if msg.Err != nil {
		m.restoreReadScopeDraft(draft)
		prefix := fmt.Sprintf("/scope %s failed", msg.Operation)
		if msg.Operation == "add-intents" || msg.Operation == "add-auto-intents" {
			prefix = "External path authorization failed"
		}
		if msg.RollbackErr != nil {
			prefix += fmt.Sprintf("; rollback could not fully restore the previous authority set: %v", msg.RollbackErr)
		} else if msg.RolledBack > 0 {
			changeLabel := "changes"
			if msg.RolledBack == 1 {
				changeLabel = "change"
			}
			prefix += fmt.Sprintf("; rolled back %d authority %s, so no partial approval remains", msg.RolledBack, changeLabel)
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: "error", Content: sanitizeTerminalSingleLine(fmt.Sprintf("%s: %v", prefix, msg.Err)),
		})
	} else {
		var receipt string
		path := terminalSafePathLiteral(msg.Path)
		switch msg.Operation {
		case "add-read":
			receipt = fmt.Sprintf("Added temporary read-only root: %s\nWrite authority remains confined to the working directory. This grant is not saved with sessions.", path)
		case "add-file":
			receipt = fmt.Sprintf("Added temporary exact-file read grant: %s\nSibling files remain unavailable. Write authority remains confined to the working directory. This grant is not saved with sessions.", path)
		case "remove-read":
			kind := "directory"
			if msg.Kind == string(agent.ReadGrantExactFile) {
				kind = "exact-file"
			}
			receipt = fmt.Sprintf("Removed temporary %s read grant: %s", kind, path)
		case "clear-read":
			grantLabel := "grants"
			if msg.Count == 1 {
				grantLabel = "grant"
			}
			receipt = fmt.Sprintf("Cleared %d temporary read-only %s.", msg.Count, grantLabel)
		case "add-intents":
			receipt = promptPathGrantReceipt("Granted", msg.Grants, msg.WriteGrants)
		case "add-auto-intents":
			receipt = promptPathGrantReceipt("AUTO accepted", msg.Grants, msg.WriteGrants)
		default:
			receipt = "Updated temporary read-only roots."
		}
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: receipt})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	m.recalcViewportHeight()
	if msg.Err == nil && msg.AutoResume && strings.TrimSpace(draft) != "" {
		return m.submitPreparedInput(draft)
	}
	return nil
}

func promptPathGrantReceipt(prefix string, reads []agent.ReadGrant, writes []agent.WriteGrant) string {
	pathSet := make(map[string]struct{}, len(reads)+len(writes))
	for _, grant := range reads {
		pathSet[filepath.Clean(grant.Path)] = struct{}{}
	}
	for _, grant := range writes {
		pathSet[filepath.Clean(grant.Path)] = struct{}{}
	}
	pathLabel := "paths"
	if len(pathSet) == 1 {
		pathLabel = "path"
	}
	if len(writes) == 0 {
		return fmt.Sprintf("%s temporary read-only access to %d explicit %s. Exact files never include siblings; grants are not saved with sessions.", prefix, len(pathSet), pathLabel)
	}
	return fmt.Sprintf("%s temporary scoped access to %d explicit %s (%d read, %d typed-write). Write is limited to built-in write/edit/mkdir and exact trusted workspace MCP tools; shell remains confined to the primary workspace. Write expires when this turn settles; read remains process-local until /scope clear-read or exit.", prefix, len(pathSet), pathLabel, len(reads), len(writes))
}

func (m *Model) restoreReadScopeDraft(draft string) {
	if strings.TrimSpace(draft) == "" {
		return
	}
	m.input.SetValue(draft)
	m.input.CursorEnd()
	m.input.Focus()
	_ = m.reflowInputViewport()
}

func (m *Model) appendReadScopeError(message string) {
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: sanitizeTerminalSingleLine(message)})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
}

func canonicalReadScopeWorkspace(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		if current, err := os.Getwd(); err == nil {
			workspace = current
		}
	}
	if absolute, err := filepath.Abs(workspace); err == nil {
		workspace = absolute
	}
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	return filepath.Clean(workspace)
}

func canonicalReadScopePreview(requested, workspace string, existing []string) (string, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", "", errors.New("read-only root path is empty")
	}
	if requested == "~" || strings.HasPrefix(requested, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve home directory: %w", err)
		}
		if requested == "~" {
			requested = home
		} else {
			requested = filepath.Join(home, strings.TrimPrefix(requested, "~"+string(filepath.Separator)))
		}
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", "", fmt.Errorf("resolve read-only root %q: %w", requested, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", fmt.Errorf("resolve read-only root %q: %w", absolute, err)
	}
	canonical = filepath.Clean(canonical)
	if filepath.Dir(canonical) == canonical {
		return "", "", errors.New("refusing to grant a filesystem root as read-only scope")
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", "", fmt.Errorf("inspect read-only root: %w", err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("read-only root is not a directory: %s", canonical)
	}

	canonicalWorkspace := strings.TrimSpace(workspace)
	if canonicalWorkspace == "" {
		canonicalWorkspace, err = os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("resolve workspace: %w", err)
		}
	}
	canonicalWorkspace, err = filepath.Abs(canonicalWorkspace)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(canonicalWorkspace); resolveErr == nil {
		canonicalWorkspace = resolved
	}
	canonicalWorkspace = filepath.Clean(canonicalWorkspace)
	if readScopePathsOverlap(canonicalWorkspace, canonical) {
		return "", "", fmt.Errorf("read-only root %q overlaps writable workspace %q", canonical, canonicalWorkspace)
	}
	for _, root := range existing {
		if readScopePathsOverlap(root, canonical) {
			return "", "", fmt.Errorf("read-only root %q overlaps existing root %q", canonical, root)
		}
	}
	return canonical, canonicalWorkspace, nil
}

func readScopePathsOverlap(first, second string) bool {
	contains := func(parent, candidate string) bool {
		relative, err := filepath.Rel(parent, candidate)
		if err != nil {
			return false
		}
		return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	}
	return contains(first, second) || contains(second, first)
}

func (m *Model) renderReadScopePrompt() string {
	if m.readScopePrompt == nil {
		return ""
	}
	if len(m.readScopePrompt.WriteGrants) > 0 {
		return m.renderPathScopePrompt()
	}
	grid := m.contentGrid()
	// ContentWidth matches label-collision budgets so what is painted is what
	// confirmability decides on. Compact path rows stack when kind+path cannot
	// share one WorkWidth cell row.
	width := grid.ContentWidth()
	prompt := m.readScopePrompt
	workspace := compactWorkspacePath(prompt.Workspace, width)
	compact := m.width <= 40 || m.height <= 16
	grants := readScopePromptGrants(prompt)
	pathLabels, pathsDistinct := m.readScopePromptPathLabels(grants)
	subject := "external read-only directory"
	if len(grants) > 1 {
		subject = fmt.Sprintf("%d explicit external read paths", len(grants))
	} else if grants[0].Kind == agent.ReadGrantExactFile {
		subject = "exact external read-only file"
	}
	lines := []string{m.styles.ApprovalPrompt.Render(truncateDisplay(readScopeAllowSubject(subject, width), width))}
	if compact {
		// Multi-grant compact rows stay single-line so four grants fit the
		// 30×12 height budget. Single-grant rows may stack kind/path so the
		// basename is never ellipsized away on the floor.
		allowStack := len(grants) == 1
		for index, grant := range grants {
			kind := "directory"
			if grant.Kind == agent.ReadGrantExactFile {
				kind = "exact file"
			}
			lines = append(lines, readScopeCompactPathLines(m, kind, pathLabels[index], width, allowStack)...)
		}
		lines = append(lines,
			// Lead with read-only so the identity keyword survives min-width
			// content-grid budgets (WorkWidth can be ~23 at 30 columns).
			m.styles.StatusText.Render(truncateDisplay("Read-only · temporary · not saved", width)),
			m.styles.StatusText.Render(truncateDisplay("Writes stay in the workspace", width)),
		)
	} else {
		for index, grant := range grants {
			label := "Directory"
			if grant.Kind == agent.ReadGrantExactFile {
				label = "Exact file"
			}
			lines = append(lines, m.runtimeStatusRow(label, pathLabels[index], width))
		}
		lines = append(lines,
			m.runtimeStatusRow("Access", "Temporary read-only authority; exact files never include siblings and grants are not saved with sessions", width),
			m.runtimeStatusRow("Writes", "Never granted here; write authority remains confined to "+workspace, width),
		)
	}
	if !pathsDistinct {
		lines = append(lines, m.styles.ApprovalPrompt.Render(truncateDisplay("Widen · y disabled", width)))
	}
	if width < 36 {
		if pathsDistinct {
			lines = append(lines,
				m.renderKeyHints(width, keyHint{Key: "y", Action: "allow"}, keyHint{Key: "n", Action: "deny"}),
				m.renderKeyHints(width, keyHint{Key: "esc", Action: "cancel"}),
			)
		} else {
			lines = append(lines, m.renderKeyHints(width,
				keyHint{Key: "n", Action: "deny"}, keyHint{Key: "esc", Action: "cancel"},
			))
		}
	} else {
		hints := []keyHint{{Key: "n", Action: "deny"}, {Key: "esc", Action: "cancel"}}
		if pathsDistinct {
			hints = append([]keyHint{{Key: "y", Action: "allow read-only"}}, hints...)
		}
		lines = append(lines, m.renderKeyHints(width, hints...))
	}
	return indentApprovalSurface(strings.Join(lines, "\n"), grid.PaneWidth, m.glyphProfile)
}

func (m *Model) renderPathScopePrompt() string {
	prompt := m.readScopePrompt
	if prompt == nil {
		return ""
	}
	grid := m.contentGrid()
	width := grid.ContentWidth()
	compact := m.width <= 40 || m.height <= 16
	grants, access := pathScopePromptDisplayGrants(prompt)
	pathLabels, pathsDistinct := m.readScopePromptPathLabels(grants)
	pathSet := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		pathSet[filepath.Clean(grant.Path)] = struct{}{}
	}
	subject := "temporary external path access"
	if len(pathSet) > 1 {
		subject = fmt.Sprintf("temporary access to %d external paths", len(pathSet))
	}
	lines := []string{m.styles.ApprovalPrompt.Render(truncateDisplay(readScopeAllowSubject(subject, width), width))}
	allowStack := len(grants) == 1
	for index, grant := range grants {
		kind := "directory"
		kindLabel := "Directory"
		if grant.Kind == agent.ReadGrantExactFile {
			kind = "exact file"
			kindLabel = "Exact file"
		}
		value := access[index] + " · " + pathLabels[index]
		if compact {
			lines = append(lines, readScopeCompactPathLines(m, kind, value, width, allowStack)...)
		} else {
			lines = append(lines, m.runtimeStatusRow(kindLabel, value, width))
		}
	}
	if compact {
		lines = append(lines,
			m.styles.StatusText.Render(truncateDisplay("Write: this turn · read: until cleared", width)),
			m.styles.StatusText.Render(truncateDisplay("Shell stays in the primary workspace", width)),
		)
	} else {
		lines = append(lines,
			m.runtimeStatusRow("Lifetime", "Write expires when this turn settles; read remains process-local until /scope clear-read or exit", width),
			m.runtimeStatusRow("Write", "Built-in write/edit/mkdir and exact trusted workspace MCP tools only; exact files exclude siblings", width),
			m.runtimeStatusRow("Shell", "External writes are never granted to bash", width),
		)
	}
	if !pathsDistinct {
		lines = append(lines, m.styles.ApprovalPrompt.Render(truncateDisplay("Widen · y disabled", width)))
	}
	hints := []keyHint{{Key: "n", Action: "deny"}, {Key: "esc", Action: "cancel"}}
	if pathsDistinct {
		hints = append([]keyHint{{Key: "y", Action: "allow exact scope"}}, hints...)
	}
	if width < 36 && len(hints) == 3 {
		lines = append(lines,
			m.renderKeyHints(width, hints[0], hints[1]),
			m.renderKeyHints(width, hints[2]),
		)
	} else {
		lines = append(lines, m.renderKeyHints(width, hints...))
	}
	return indentApprovalSurface(strings.Join(lines, "\n"), grid.PaneWidth, m.glyphProfile)
}

func pathScopePromptDisplayGrants(prompt *ReadScopePrompt) ([]agent.ReadGrant, []string) {
	if prompt == nil {
		return nil, nil
	}
	writeByPath := make(map[string]struct{}, len(prompt.WriteGrants))
	grants := make([]agent.ReadGrant, 0, len(prompt.Grants)+len(prompt.WriteGrants))
	access := make([]string, 0, cap(grants))
	for _, grant := range prompt.WriteGrants {
		path := filepath.Clean(grant.Path)
		writeByPath[path] = struct{}{}
		grants = append(grants, agent.ReadGrant{Path: grant.Path, Kind: agent.ReadGrantKind(grant.Kind)})
		access = append(access, "read + typed write")
	}
	for _, grant := range prompt.Grants {
		if _, writable := writeByPath[filepath.Clean(grant.Path)]; writable {
			continue
		}
		grants = append(grants, grant)
		access = append(access, "read only")
	}
	return grants, access
}

func readScopePromptGrants(prompt *ReadScopePrompt) []agent.ReadGrant {
	if prompt == nil {
		return nil
	}
	grants := append([]agent.ReadGrant(nil), prompt.Grants...)
	if len(grants) == 0 {
		grants = []agent.ReadGrant{{Path: prompt.Canonical, Kind: prompt.Kind}}
	}
	return grants
}

func (m *Model) readScopePromptPathsDistinct() bool {
	if m == nil || m.readScopePrompt == nil {
		return false
	}
	grants := readScopePromptGrants(m.readScopePrompt)
	if len(m.readScopePrompt.WriteGrants) > 0 {
		grants, _ = pathScopePromptDisplayGrants(m.readScopePrompt)
	}
	_, distinct := m.readScopePromptPathLabels(grants)
	return distinct
}

// readScopePromptPathLabels projects every exact authority with the same width
// budget used by the renderer. A compact collision disables confirmation until
// a resize makes the identities distinguishable; it never changes the grants.
func (m *Model) readScopePromptPathLabels(grants []agent.ReadGrant) ([]string, bool) {
	width := m.contentGrid().ContentWidth()
	compact := m.width <= 40 || m.height <= 16
	labels := make([]string, len(grants))
	collisionSeen := make(map[string]string, len(grants))
	distinct := true
	for index, grant := range grants {
		// Display labels use the full content-grid flex column so basenames
		// survive stacked kind/path rows on minimum terminals.
		labels[index] = compactWorkspacePath(grant.Path, width)
		// Distinctness still uses the one-line kind · path packing budget so
		// parent-only differences collide when the combined row would.
		collisionWidth := runtimeStatusValueWidth(width)
		if compact {
			kind := "directory"
			if grant.Kind == agent.ReadGrantExactFile {
				kind = "exact file"
			}
			collisionWidth = max(1, width-lipgloss.Width(kind)-3)
		}
		collisionLabel := compactWorkspacePath(grant.Path, collisionWidth)
		if prior, exists := collisionSeen[collisionLabel]; exists && prior != grant.Path {
			distinct = false
		} else {
			collisionSeen[collisionLabel] = grant.Path
		}
	}
	return labels, distinct
}

// readScopeAllowSubject keeps "Allow" plus the most identity-rich phrase that
// fits the content-grid flex column. Narrow budgets prefer the read-only /
// count keywords over longer geographic wording so floor markers stay visible.
func readScopeAllowSubject(subject string, width int) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return truncateDisplay("Allow access?", width)
	}
	candidates := []string{"Allow " + subject + "?"}
	// Prefer shorter forms that still carry identity markers the floor tests
	// and operators rely on ("read-only", "exact", "N explicit").
	switch {
	case strings.Contains(subject, "explicit"):
		// "4 explicit external read paths" → "4 explicit paths"
		short := strings.Replace(subject, " external read", "", 1)
		candidates = append(candidates, "Allow "+short+"?", "Allow paths?")
	case strings.Contains(subject, "exact") && strings.Contains(subject, "file"):
		candidates = append(candidates,
			"Allow exact read-only file?",
			"Allow exact file?",
		)
	case strings.Contains(subject, "read-only") && strings.Contains(subject, "directory"):
		candidates = append(candidates,
			"Allow read-only directory?",
			"Allow read-only?",
		)
	case strings.Contains(subject, "read-only"):
		candidates = append(candidates, "Allow read-only?")
	case strings.Contains(subject, "path"):
		candidates = append(candidates, "Allow path access?", "Allow paths?")
	}
	for _, candidate := range candidates {
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return truncateDisplay(candidates[0], width)
}

// readScopeCompactPathLines paints grant kind + path under the content-grid
// width. When both fit one row they share it. Single-grant confirmations may
// stack kind and path so basenames survive min-width budgets; multi-grant
// floors keep one row per grant so height stays within the 30×12 content budget.
func readScopeCompactPathLines(m *Model, kind, path string, width int, allowStack bool) []string {
	kind = strings.TrimSpace(kind)
	path = strings.TrimSpace(path)
	if kind == "" && path == "" {
		return nil
	}
	dim := m.styles.OverlayDim
	if kind == "" {
		return []string{dim.Render(truncateDisplay(path, width))}
	}
	if path == "" {
		return []string{dim.Render(truncateDisplay(kind, width))}
	}
	combined := kind + " · " + path
	if lipgloss.Width(combined) <= width {
		return []string{dim.Render(combined)}
	}
	if allowStack {
		return []string{
			dim.Render(truncateDisplay(kind, width)),
			dim.Render(truncateDisplay(path, width)),
		}
	}
	pathBudget := max(1, width-lipgloss.Width(kind)-3)
	return []string{dim.Render(truncateDisplay(kind+" · "+truncateDisplay(path, pathBudget), width))}
}
