package ui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

const (
	toolToggleActionID     command.ActionID = "tool.toggle"
	toolOpenOutputActionID command.ActionID = "tool.open-output"
	toolOpenDiffActionID   command.ActionID = "tool.open-diff"
)

type outputViewerPageResultMsg struct {
	OverlayID OverlayID
	Token     OutputViewerPageToken
	Page      OutputDetailPage
	Err       error
}

type viewerClipboardResultMsg struct {
	OverlayID OverlayID
	Kind      ModalKind
	Err       error
}

func (m *Model) viewerModalActive() bool {
	return m != nil && !m.modalStack.Empty()
}

// toolActionTargetForChat resolves the typed identity already owned by one
// rendered ToolCard. It deliberately does not walk the transcript: render
// callers already have the exact ChatEntry whose action hints they are
// projecting.
func (m *Model) toolActionTargetForChat(chat ChatEntry) (EntityRef, bool) {
	if m == nil || chat.Kind != "tool_group" ||
		chat.ToolIndex < 0 || chat.ToolIndex >= len(m.toolEntries) ||
		!chat.BlockID.Valid() {
		return EntityRef{}, false
	}
	invocationID := m.toolEntries[chat.ToolIndex].ID
	if invocationID == "" {
		invocationID = "invocation-" + string(chat.BlockID)
	}
	target := EntityRef{
		Kind: EntityKindTranscriptBlock, BlockID: chat.BlockID,
		InvocationID: invocationID,
	}
	if !target.Valid() {
		return EntityRef{}, false
	}
	return target, true
}

func (m *Model) toolActionTarget(toolIndex int) (EntityRef, bool) {
	if m == nil || toolIndex < 0 || toolIndex >= len(m.toolEntries) {
		return EntityRef{}, false
	}
	for _, chat := range m.entries {
		if chat.Kind != "tool_group" || chat.ToolIndex != toolIndex {
			continue
		}
		return m.toolActionTargetForChat(chat)
	}
	return EntityRef{}, false
}

func (m *Model) resolveToolActionTarget(target EntityRef) (int, ChatEntry, bool) {
	if m == nil || target.Kind != EntityKindTranscriptBlock || !target.Valid() {
		return -1, ChatEntry{}, false
	}
	for _, chat := range m.entries {
		if chat.Kind != "tool_group" || chat.BlockID != target.BlockID ||
			chat.ToolIndex < 0 || chat.ToolIndex >= len(m.toolEntries) {
			continue
		}
		current, ok := m.toolActionTargetForChat(chat)
		if !ok || current != target {
			return -1, ChatEntry{}, false
		}
		return chat.ToolIndex, chat, true
	}
	return -1, ChatEntry{}, false
}

func (m *Model) toolActionRegistry(target EntityRef) (*UIActionRegistry, int, ChatEntry, bool) {
	toolIndex, chat, ok := m.resolveToolActionTarget(target)
	if !ok {
		return NewUIActionRegistry(), -1, ChatEntry{}, false
	}
	registry, resolvedIndex, ok := m.toolActionRegistryForResolvedChat(target, chat)
	if !ok || resolvedIndex != toolIndex {
		return NewUIActionRegistry(), -1, ChatEntry{}, false
	}
	return registry, toolIndex, chat, true
}

// toolActionRegistryForResolvedChat projects actions from an exact ToolCard
// identity in constant time. Mutating dispatch still resolves the target
// against m.entries first; render-only hints can safely use the ChatEntry they
// are already rendering without a second global lookup.
func (m *Model) toolActionRegistryForResolvedChat(
	target EntityRef,
	chat ChatEntry,
) (*UIActionRegistry, int, bool) {
	current, ok := m.toolActionTargetForChat(chat)
	if !ok || current != target {
		return NewUIActionRegistry(), -1, false
	}
	toolIndex := chat.ToolIndex
	entry := m.toolEntries[toolIndex]
	terminal := entry.Status != ToolStatusRunning

	toggleLabel := "Expand receipt"
	if !entry.Collapsed {
		toggleLabel = "Collapse receipt"
	}
	toggle := UIActionSpec{
		ID: toolToggleActionID, Label: toggleLabel,
		Shortcut: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "expand/collapse receipt"),
		),
	}.Resolve(target, true, "")

	outputReason := ""
	outputEnabled := terminal && !isExpertConsultTool(entry.Name) &&
		entry.OutputDetail.Digest != (OutputDetailDigest{}) &&
		entry.OutputDetail.Digest.Valid() &&
		m.outputDetails.Available(entry.OutputDetail.Ref)
	switch {
	case !terminal:
		outputReason = "Output is still running."
	case isExpertConsultTool(entry.Name):
		outputReason = "Expert reports are private to the agent boundary."
	case entry.OutputDetail.Digest == (OutputDetailDigest{}):
		outputReason = "No full output was retained."
	case !entry.OutputDetail.Digest.Valid():
		outputReason = "Output metadata is invalid."
	case !m.outputDetails.Available(entry.OutputDetail.Ref):
		outputReason = "Output expired or was evicted."
	}
	openOutput := UIActionSpec{
		ID: toolOpenOutputActionID, Label: "Open output",
		Shortcut: key.NewBinding(
			key.WithKeys("alt+o"),
			key.WithHelp("alt+o", "open output viewer"),
		),
	}.Resolve(target, outputEnabled, outputReason)

	diffReason := ""
	diffEnabled := terminal && !entry.DiffPending && len(entry.DiffLines) > 0
	switch {
	case !terminal:
		diffReason = "Diff is still running."
	case entry.DiffPending:
		diffReason = "Diff is still loading."
	case len(entry.DiffLines) == 0:
		diffReason = "No diff is available."
	}
	openDiff := UIActionSpec{
		ID: toolOpenDiffActionID, Label: "Open diff",
		Shortcut: key.NewBinding(
			key.WithKeys("alt+d"),
			key.WithHelp("alt+d", "open diff viewer"),
		),
	}.Resolve(target, diffEnabled, diffReason)

	return NewUIActionRegistry(toggle, openOutput, openDiff), toolIndex, true
}

func (m *Model) dispatchUIAction(request UIActionRequest) tea.Cmd {
	registry, toolIndex, chat, ok := m.toolActionRegistry(request.Target)
	if !ok {
		return m.setFooterNotice(noticeWarning, uiActionTargetReason, 2*time.Second)
	}
	action, reason, accepted := registry.ResolveRequest(request)
	if !accepted {
		return m.setFooterNotice(noticeWarning, reason, 2*time.Second)
	}
	switch action.ID {
	case toolToggleActionID:
		// Keyboard and pointer share the same anchor/focus semantics.
		m.toggleToolReceipt(toolIndex, true)
		return nil
	case toolOpenOutputActionID:
		return m.openOutputViewer(toolIndex, chat, action.Target)
	case toolOpenDiffActionID:
		return m.openDiffViewer(toolIndex, chat, action.Target)
	default:
		return m.setFooterNotice(noticeWarning, uiActionMissingReason, 2*time.Second)
	}
}

func (m *Model) currentInspectedToolTarget() (EntityRef, bool) {
	if m == nil || strings.TrimSpace(m.input.Value()) != "" {
		return EntityRef{}, false
	}
	if m.receiptInspectActive {
		return m.toolActionTarget(m.receiptInspectToolIndex)
	}
	target := m.lastTurnToolIndex
	if target < 0 && len(m.toolEntries) > 0 {
		target = len(m.toolEntries) - 1
	}
	if target < 0 || target >= len(m.toolEntries) || m.toolEntries[target].Collapsed {
		return EntityRef{}, false
	}
	return m.toolActionTarget(target)
}

func (m *Model) dispatchInspectedToolAction(id command.ActionID) tea.Cmd {
	target, ok := m.currentInspectedToolTarget()
	if !ok {
		return nil
	}
	return m.dispatchUIAction(UIActionRequest{
		ActionID: id, Target: target, Source: UIActionSourceKeyboard,
	})
}

func (m *Model) toolViewerActionHint(chat ChatEntry) string {
	target, ok := m.toolActionTargetForChat(chat)
	if !ok {
		return ""
	}
	registry, _, ok := m.toolActionRegistryForResolvedChat(target, chat)
	if !ok {
		return ""
	}
	hints := make([]string, 0, 2)
	for _, id := range []command.ActionID{toolOpenOutputActionID, toolOpenDiffActionID} {
		action, exists := registry.Action(id)
		if !exists || !action.Enabled {
			continue
		}
		help := action.Shortcut.Help()
		if help.Key != "" {
			hints = append(hints, help.Key+" "+action.Label)
		}
	}
	separator := " · "
	if m.glyphProfile == GlyphASCII {
		separator = " | "
	}
	return strings.Join(hints, separator)
}

func (m *Model) openOutputViewer(toolIndex int, chat ChatEntry, target EntityRef) tea.Cmd {
	if m == nil {
		return nil
	}
	if m.state != StateIdle || m.overlay != OverlayNone ||
		m.pendingApproval != nil || m.readScopePrompt != nil ||
		toolIndex < 0 || toolIndex >= len(m.toolEntries) {
		return m.setFooterNotice(noticeWarning, "Output viewer is unavailable right now.", 2*time.Second)
	}
	entry := m.toolEntries[toolIndex]
	if !m.outputDetails.Available(entry.OutputDetail.Ref) {
		m.invalidateEntryCache()
		m.refreshTranscript()
		return m.setFooterNotice(noticeWarning, "Output expired or was evicted.", 2*time.Second)
	}
	id, err := newTranscriptID("overlay_")
	if err != nil {
		return m.setFooterNotice(noticeError, "Output viewer could not be opened.", 2*time.Second)
	}
	modal := ModalInstance{
		ID: OverlayID(id), Kind: ModalKindOutputViewer, Origin: target,
	}
	if err := m.modalStack.Push(modal, m.currentViewerFocus()); err != nil {
		return m.setFooterNotice(noticeWarning, "Output viewer could not be stacked.", 2*time.Second)
	}
	viewer := NewOutputViewer(
		target, entry.OutputDetail, m.width, m.height, m.isDark, m.glyphProfile,
	)
	viewer.SetReducedMotion(m.reducedMotion)
	if m.outputViewers == nil {
		m.outputViewers = make(map[OverlayID]*OutputViewer)
	}
	m.outputViewers[modal.ID] = viewer
	m.input.Blur()
	return m.handleOutputViewerEvent(modal.ID, viewer.InitialPageRequest())
}

func (m *Model) openDiffViewer(toolIndex int, chat ChatEntry, target EntityRef) tea.Cmd {
	if m == nil {
		return nil
	}
	if m.state != StateIdle || m.overlay != OverlayNone ||
		m.pendingApproval != nil || m.readScopePrompt != nil ||
		toolIndex < 0 || toolIndex >= len(m.toolEntries) {
		return m.setFooterNotice(noticeWarning, "Diff viewer is unavailable right now.", 2*time.Second)
	}
	entry := m.toolEntries[toolIndex]
	if entry.DiffPending || len(entry.DiffLines) == 0 {
		return m.setFooterNotice(noticeWarning, "Diff is not available.", 2*time.Second)
	}
	id, err := newTranscriptID("overlay_")
	if err != nil {
		return m.setFooterNotice(noticeError, "Diff viewer could not be opened.", 2*time.Second)
	}
	modal := ModalInstance{ID: OverlayID(id), Kind: ModalKindDiffViewer, Origin: target}
	if err := m.modalStack.Push(modal, m.currentViewerFocus()); err != nil {
		return m.setFooterNotice(noticeWarning, "Diff viewer could not be stacked.", 2*time.Second)
	}
	truncated := false
	for _, line := range entry.DiffLines {
		if line.Kind == DiffOmitted {
			truncated = true
			break
		}
	}
	file := DiffFileProjection{
		ID: entry.ID, DisplayPath: entry.Summary,
		Lines:     append([]DiffLine(nil), entry.DiffLines...),
		Truncated: truncated, Revision: max(uint64(1), chat.Revision),
	}
	viewer := NewDiffViewer(chat.BlockID, []DiffFileProjection{file}, DiffViewerOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark, GlyphProfile: m.glyphProfile,
	})
	viewer.SetReducedMotion(m.reducedMotion)
	if m.diffViewers == nil {
		m.diffViewers = make(map[OverlayID]*DiffViewer)
	}
	m.diffViewers[modal.ID] = viewer
	m.input.Blur()
	return nil
}

func (m *Model) currentViewerFocus() FocusToken {
	if top, ok := m.modalStack.Top(); ok {
		return top.FocusToken()
	}
	if m.input.Focused() {
		return FocusToken{Owner: FocusOwnerComposer}
	}
	return FocusToken{Owner: FocusOwnerTranscript}
}

func (m *Model) handleViewerKey(message tea.KeyPressMsg) tea.Cmd {
	top, ok := m.modalStack.Top()
	if !ok {
		return nil
	}
	switch top.Kind {
	case ModalKindOutputViewer:
		viewer := m.outputViewers[top.ID]
		if viewer == nil {
			return m.closeTopViewer()
		}
		event, child := viewer.Update(message)
		return tea.Batch(child, m.handleOutputViewerEvent(top.ID, event))
	case ModalKindDiffViewer:
		viewer := m.diffViewers[top.ID]
		if viewer == nil {
			return m.closeTopViewer()
		}
		if message.Code == tea.KeyEscape {
			if viewer.Back() {
				return nil
			}
			return m.closeTopViewer()
		}
		event, child := viewer.Update(message)
		return tea.Batch(child, m.handleDiffViewerEvent(top.ID, event))
	default:
		return m.closeTopViewer()
	}
}

func (m *Model) updateViewerMessage(message tea.Msg) tea.Cmd {
	top, ok := m.modalStack.Top()
	if !ok {
		return nil
	}
	switch top.Kind {
	case ModalKindOutputViewer:
		if viewer := m.outputViewers[top.ID]; viewer != nil {
			event, child := viewer.Update(message)
			return tea.Batch(child, m.handleOutputViewerEvent(top.ID, event))
		}
	case ModalKindDiffViewer:
		if viewer := m.diffViewers[top.ID]; viewer != nil {
			event, child := viewer.Update(message)
			return tea.Batch(child, m.handleDiffViewerEvent(top.ID, event))
		}
	}
	return nil
}

func (m *Model) handleViewerWheel(message tea.MouseWheelMsg) tea.Cmd {
	return m.updateViewerMessage(message)
}

func (m *Model) handleViewerClick(message tea.MouseClickMsg) tea.Cmd {
	return m.updateViewerMessage(message)
}

func (m *Model) handleOutputViewerEvent(id OverlayID, event OutputViewerEvent) tea.Cmd {
	if event.Empty() {
		return nil
	}
	top, ok := m.modalStack.Top()
	viewer := m.outputViewers[id]
	if !ok || top.ID != id || top.Kind != ModalKindOutputViewer ||
		viewer == nil || event.Origin != viewer.Origin {
		return nil
	}
	switch event.Kind {
	case OutputViewerEventRequestPage:
		return outputViewerPageCmd(id, event.Token, event.Request, m.outputDetails)
	case OutputViewerEventCopyVisible:
		return m.viewerClipboardCmd(id, ModalKindOutputViewer, event.CopyText)
	case OutputViewerEventClose:
		return m.closeTopViewer()
	default:
		return nil
	}
}

func outputViewerPageCmd(
	id OverlayID,
	token OutputViewerPageToken,
	request OutputDetailPageRequest,
	store *OutputDetailStore,
) tea.Cmd {
	if !id.Valid() || !token.Valid() {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		page, err := store.Page(ctx, request)
		return outputViewerPageResultMsg{OverlayID: id, Token: token, Page: page, Err: err}
	}
}

func (m *Model) handleOutputViewerPageResult(message outputViewerPageResultMsg) {
	top, ok := m.modalStack.Top()
	if !ok || top.ID != message.OverlayID || top.Kind != ModalKindOutputViewer {
		return
	}
	if viewer := m.outputViewers[top.ID]; viewer != nil {
		viewer.ApplyPageResult(message.Token, message.Page, message.Err)
	}
}

func (m *Model) handleDiffViewerEvent(id OverlayID, event DiffViewerEvent) tea.Cmd {
	if event.Kind == DiffViewerEventNone {
		return nil
	}
	top, ok := m.modalStack.Top()
	viewer := m.diffViewers[id]
	if !ok || top.ID != id || top.Kind != ModalKindDiffViewer || viewer == nil ||
		event.BlockID != top.Origin.BlockID {
		return nil
	}
	switch event.Kind {
	case DiffViewerEventCopyLine, DiffViewerEventCopyHunk, DiffViewerEventCopyPath:
		return m.viewerClipboardCmd(id, ModalKindDiffViewer, event.Text)
	case DiffViewerEventUnavailable:
		return nil
	default:
		return nil
	}
}

func (m *Model) viewerClipboardCmd(id OverlayID, kind ModalKind, text string) tea.Cmd {
	text = sanitizeTerminalMultiline(text)
	if text == "" || !id.Valid() || !kind.Valid() {
		return nil
	}
	write := m.clipboardWrite
	return func() tea.Msg {
		var err error
		if write == nil {
			err = context.Canceled
		} else {
			err = write(text)
		}
		return viewerClipboardResultMsg{OverlayID: id, Kind: kind, Err: err}
	}
}

func (m *Model) handleViewerClipboardResult(message viewerClipboardResultMsg) tea.Cmd {
	top, ok := m.modalStack.Top()
	if !ok || top.ID != message.OverlayID || top.Kind != message.Kind {
		return nil
	}
	text := "Copied visible content."
	severity := noticeSuccess
	if message.Err != nil {
		text = "Clipboard is unavailable."
		severity = noticeWarning
	}
	switch message.Kind {
	case ModalKindOutputViewer:
		if viewer := m.outputViewers[top.ID]; viewer != nil {
			viewer.notice = text
		}
	case ModalKindDiffViewer:
		if viewer := m.diffViewers[top.ID]; viewer != nil {
			viewer.notice = text
		}
	}
	return m.setFooterNotice(severity, text, 2*time.Second)
}

func (m *Model) closeTopViewer() tea.Cmd {
	removed, focus, ok := m.modalStack.Pop(DefaultFocusToken())
	if !ok {
		return nil
	}
	delete(m.outputViewers, removed.ID)
	delete(m.diffViewers, removed.ID)
	if m.modalStack.Empty() && focus.Owner == FocusOwnerComposer && m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	return nil
}

func (m *Model) clearViewerModals(restoreFocus bool) {
	removed, focus := m.modalStack.Clear(DefaultFocusToken())
	for _, modal := range removed {
		delete(m.outputViewers, modal.ID)
		delete(m.diffViewers, modal.ID)
	}
	if restoreFocus && focus.Owner == FocusOwnerComposer && m.composerEditable() {
		m.input.Focus()
	} else if len(removed) > 0 {
		m.input.Blur()
	}
}

func (m *Model) resizeViewerModals() {
	for _, viewer := range m.outputViewers {
		viewer.SetSize(m.width, m.height)
	}
	for _, viewer := range m.diffViewers {
		viewer.SetSize(m.width, m.height)
	}
}

func (m *Model) restyleViewerModals() {
	for _, viewer := range m.outputViewers {
		viewer.SetTheme(m.isDark, m.themeID)
		viewer.SetReducedMotion(m.reducedMotion)
	}
	for _, viewer := range m.diffViewers {
		viewer.SetTheme(m.isDark, m.themeID)
		viewer.SetReducedMotion(m.reducedMotion)
	}
}

func (m *Model) viewerModalFrame() (string, *tea.Cursor, CellRect, bool) {
	top, ok := m.modalStack.Top()
	if !ok {
		return "", nil, CellRect{}, false
	}
	switch top.Kind {
	case ModalKindOutputViewer:
		viewer := m.outputViewers[top.ID]
		if viewer == nil {
			return "", nil, CellRect{}, false
		}
		frame, cursor := viewer.ViewWithCursor()
		return frame, cursor, viewer.Layout().OuterRect, frame != ""
	case ModalKindDiffViewer:
		viewer := m.diffViewers[top.ID]
		if viewer == nil {
			return "", nil, CellRect{}, false
		}
		frame, cursor := viewer.ViewWithCursor()
		return frame, cursor, viewer.Layout().OuterRect, frame != ""
	default:
		return "", nil, CellRect{}, false
	}
}

func (m *Model) composeViewerModal(base string) (string, *tea.Cursor, bool) {
	frame, localCursor, rect, ok := m.viewerModalFrame()
	if !ok {
		return base, nil, false
	}
	screen := NewCellRect(0, 0, max(0, m.width), max(0, m.height))
	rect = NewCellRect(
		max(rect.MinX, screen.MinX),
		max(rect.MinY, screen.MinY),
		min(rect.MaxX, screen.MaxX),
		min(rect.MaxY, screen.MaxY),
	)
	if rect.Empty() {
		return base, nil, true
	}
	rows := exactModalScreenRows(base, screen.Width(), screen.Height())
	for index := range rows {
		plain := ansi.Strip(rows[index])
		rows[index] = m.styles.OverlayDim.Render(fitModalScreenRow(plain, screen.Width()))
	}
	frameRows := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	for row := 0; row < rect.Height() && row < len(frameRows); row++ {
		screenRow := rect.MinY + row
		if screenRow < 0 || screenRow >= len(rows) {
			continue
		}
		rows[screenRow] = replaceDiffViewerCells(
			rows[screenRow], rect.MinX,
			fitModalScreenRow(frameRows[row], rect.Width()),
			rect.Width(),
		)
	}
	var cursor *tea.Cursor
	if localCursor != nil {
		copy := *localCursor
		copy.X += rect.MinX
		copy.Y += rect.MinY
		if screen.Contains(copy.X, copy.Y) {
			cursor = &copy
		}
	}
	return strings.Join(rows, "\n"), cursor, true
}

func exactModalScreenRows(content string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	source := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	rows := make([]string, height)
	for index := range rows {
		if index < len(source) {
			rows[index] = fitModalScreenRow(source[index], width)
		} else {
			rows[index] = strings.Repeat(" ", max(0, width))
		}
	}
	return rows
}

func fitModalScreenRow(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Truncate(value, width, "")
	missing := width - lipgloss.Width(value)
	if missing > 0 {
		value += strings.Repeat(" ", missing)
	}
	return value
}
