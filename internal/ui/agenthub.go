package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const agentHubASCIIListTitleReserve = 3

type agentHubMode uint8

const (
	agentHubListMode agentHubMode = iota
	agentHubViewerMode
)

type agentHubItem struct {
	displayID    string
	group        AgentGroupProjection
	glyphProfile GlyphProfile
}

func (item agentHubItem) Title() string {
	return sanitizeTerminalSingleLine(fmt.Sprintf(
		"Consultation #%s%s%s",
		item.displayID,
		glyphSeparator(item.glyphProfile),
		agentGroupStatusLabel(item.group),
	))
}

func (item agentHubItem) Description() string {
	return sanitizeTerminalSingleLine(agentGroupSummary(item.group, item.glyphProfile))
}

func (item agentHubItem) FilterValue() string {
	parts := []string{
		item.Title(),
		item.Description(),
		string(item.group.Strategy),
	}
	for _, node := range item.group.Nodes {
		parts = append(
			parts,
			node.Label,
			node.Model,
			node.FailureCode,
			string(node.Location),
		)
	}
	return sanitizeTerminalSingleLine(strings.Join(parts, " "))
}

// agentHubDelegate keeps Bubbles list behavior while replacing its fixed
// Unicode truncation mark in ASCII mode. The wrapper supplies already-bounded
// item text; Bubbles still owns selection, filtering, pagination, and styling.
type agentHubDelegate struct {
	list.DefaultDelegate
	glyphProfile GlyphProfile
}

type agentHubRenderItem struct {
	item        list.Item
	title       string
	description string
}

func (item agentHubRenderItem) Title() string       { return item.title }
func (item agentHubRenderItem) Description() string { return item.description }
func (item agentHubRenderItem) FilterValue() string { return item.item.FilterValue() }

func newAgentHubDelegate(isDark, compact bool, themeID string, profile GlyphProfile) agentHubDelegate {
	return agentHubDelegate{
		DefaultDelegate: newPickerDelegate(isDark, compact, themeID, profile),
		glyphProfile:    resolveGlyphProfile(profile),
	}
}

func (delegate agentHubDelegate) Render(
	writer io.Writer,
	model list.Model,
	index int,
	item list.Item,
) {
	if delegate.glyphProfile != GlyphASCII {
		delegate.DefaultDelegate.Render(writer, model, index, item)
		return
	}
	defaultItem, ok := item.(list.DefaultItem)
	if !ok {
		return
	}
	textWidth := model.Width() -
		delegate.Styles.NormalTitle.GetPaddingLeft() -
		delegate.Styles.NormalTitle.GetPaddingRight()
	descriptionLines := strings.Split(defaultItem.Description(), "\n")
	for lineIndex := range descriptionLines {
		descriptionLines[lineIndex] = truncateDisplayWithGlyphProfile(
			descriptionLines[lineIndex],
			textWidth,
			delegate.glyphProfile,
		)
	}
	delegate.DefaultDelegate.Render(writer, model, index, agentHubRenderItem{
		item: item,
		title: truncateDisplayWithGlyphProfile(
			defaultItem.Title(),
			textWidth,
			delegate.glyphProfile,
		),
		description: strings.Join(descriptionLines, "\n"),
	})
}

// AgentHubState is a presentation-only Bubbles surface. The parent Model owns
// every message, projection refresh, focus transition, and transcript jump.
type AgentHubState struct {
	List             list.Model
	Viewer           viewport.Model
	Mode             agentHubMode
	Surface          AgentSurfaceProjection
	ViewerGroupID    BlockID
	Unavailable      bool
	ItemHeight       int
	ItemSpacing      int
	width            int
	height           int
	isDark           bool
	themeID          string
	reducedMotion    bool
	glyphProfile     GlyphProfile
	compact          bool
	viewerContentKey string
	viewerRows       []agentViewerRowAnchor
	seenNodeRevision map[string]uint64
}

// agentViewerRowAnchor keeps scroll ownership attached to semantic work rather
// than to a physical row number. A node can gain a metadata row while running,
// or collapse from two rows to one at a wider terminal width.
type agentViewerRowAnchor struct {
	key    string
	nodeID string
	subrow int
}

type agentViewerLayout struct {
	content string
	rows    []agentViewerRowAnchor
}

func newAgentHubState(
	surface AgentSurfaceProjection,
	unavailable bool,
	terminalWidth int,
	terminalHeight int,
	isDark bool, themeID string,
	reducedMotion bool,
	profiles ...GlyphProfile,
) *AgentHubState {
	if !agentSurfaceProjectionInputValid(surface) {
		surface = AgentSurfaceProjection{}
		unavailable = true
	}
	surface = cloneAgentSurfaceProjection(surface)
	profile := resolveGlyphProfile(profiles...)
	items := agentHubItems(surface, profile)
	compact := compactAgentHub(terminalWidth, terminalHeight)
	delegate := newAgentHubDelegate(isDark, compact, themeID, profile)
	l := list.New(items, delegate, 1, 1)
	configurePickerList(&l, isDark, themeID, reducedMotion)
	configurePickerListGlyphProfile(&l, profile)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(len(items) > 0)
	l.DisableQuitKeybindings()
	l.SetStatusBarItemName("agent consultation", "agent consultations")

	state := &AgentHubState{
		List:             l,
		Viewer:           viewport.New(viewport.WithWidth(1), viewport.WithHeight(1)),
		Mode:             agentHubListMode,
		Surface:          surface,
		Unavailable:      unavailable,
		ItemHeight:       delegate.Height(),
		ItemSpacing:      delegate.Spacing(),
		width:            terminalWidth,
		height:           terminalHeight,
		isDark:           isDark,
		reducedMotion:    reducedMotion,
		glyphProfile:     profile,
		compact:          compact,
		seenNodeRevision: make(map[string]uint64),
	}
	state.admitInitialNodeRevisions()
	state.configureListTitle()
	state.SetSize(terminalWidth, terminalHeight)
	state.selectDefaultGroup()
	return state
}

func agentSurfaceProjectionInputValid(surface AgentSurfaceProjection) bool {
	if !surface.valid() {
		return false
	}
	for _, group := range surface.Groups {
		for _, node := range group.Nodes {
			if node.Unread != 0 {
				return false
			}
		}
	}
	return true
}

func (state *AgentHubState) admitInitialNodeRevisions() {
	if state == nil {
		return
	}
	if state.seenNodeRevision == nil {
		state.seenNodeRevision = make(map[string]uint64)
	}
	for _, group := range state.Surface.Groups {
		for _, node := range group.Nodes {
			state.seenNodeRevision[node.ID] = node.Revision
		}
	}
}

func cloneAgentSurfaceProjection(surface AgentSurfaceProjection) AgentSurfaceProjection {
	cloned := AgentSurfaceProjection{
		Groups:        make([]AgentGroupProjection, len(surface.Groups)),
		OmittedGroups: surface.OmittedGroups,
	}
	for index, group := range surface.Groups {
		cloned.Groups[index] = group
		cloned.Groups[index].Nodes = append([]WorkNode(nil), group.Nodes...)
		for nodeIndex := range cloned.Groups[index].Nodes {
			node := &cloned.Groups[index].Nodes[nodeIndex]
			if node.ReportRef != nil {
				reportRef := *node.ReportRef
				node.ReportRef = &reportRef
			}
		}
	}
	return cloned
}

func agentHubItems(surface AgentSurfaceProjection, profiles ...GlyphProfile) []list.Item {
	profile := resolveGlyphProfile(profiles...)
	items := make([]list.Item, len(surface.Groups))
	for index, group := range surface.Groups {
		items[index] = agentHubItem{
			displayID:    agentGroupDisplayID(group.ID),
			group:        group,
			glyphProfile: profile,
		}
	}
	return items
}

// agentGroupDisplayID is a stable, presentation-only handle for an opaque
// durable group identity. A list ordinal would change whenever the bounded
// Agent Hub omits old history, while this handle survives truncation, filtering,
// lifecycle refresh, and restore without adding another persisted field.
func agentGroupDisplayID(id BlockID) string {
	digest := sha256.Sum256([]byte("sonar.agent-group-label.v1\x00" + string(id)))
	return hex.EncodeToString(digest[:4])
}

func compactAgentHub(width, height int) bool {
	return width <= 40 || height <= 16
}

func (state *AgentHubState) configureListTitle() {
	if state == nil {
		return
	}
	title := "Agents"
	if count := len(state.Surface.Groups); count > 0 {
		title = fmt.Sprintf(
			"Agents%s%d %s",
			glyphSeparator(state.glyphProfile),
			count,
			pluralizeNoun(count, "consultation", "consultations"),
		)
	}
	if state.Surface.OmittedGroups > 0 {
		title += fmt.Sprintf("%s+%d older", glyphSeparator(state.glyphProfile), state.Surface.OmittedGroups)
	}
	titleWidth := pickerListWidth(max(1, state.width))
	if state.glyphProfile == GlyphASCII {
		// Bubbles reserves one spinner cell plus a two-cell status gap in the
		// title row even while both are visually empty.
		titleWidth = max(1, titleWidth-agentHubASCIIListTitleReserve)
	}
	state.List.Title = truncateDisplayWithGlyphProfile(
		title,
		titleWidth,
		state.glyphProfile,
	)
	setSettingsTitleDensity(&state.List, state.compact)
}

func (state *AgentHubState) SetProjection(surface AgentSurfaceProjection, unavailable bool) tea.Cmd {
	if state == nil {
		return nil
	}
	if !agentSurfaceProjectionInputValid(surface) {
		surface = AgentSurfaceProjection{}
		unavailable = true
	}
	surface = cloneAgentSurfaceProjection(surface)
	selectedID := state.selectedGroupID()
	viewerID := state.ViewerGroupID
	filterState := state.List.FilterState()
	filterText := state.List.FilterInput.Value()
	filterCursor := state.List.FilterInput.Position()

	if !unavailable && !state.decorateNodeUnread(&surface, viewerID) {
		surface = AgentSurfaceProjection{}
		unavailable = true
	}
	state.Surface = surface
	state.Unavailable = unavailable
	items := agentHubItems(surface, state.glyphProfile)
	state.List.SetFilteringEnabled(len(items) > 0)
	_ = state.List.SetItems(items)
	if len(items) == 0 {
		state.List.ResetFilter()
	} else {
		state.applyFilterSynchronously(filterState, filterText)
		if filterState == list.Filtering {
			state.List.FilterInput.SetCursor(min(
				max(0, filterCursor),
				utf8.RuneCountInString(filterText),
			))
		}
	}
	state.configureListTitle()
	state.SetSize(state.width, state.height)

	if state.Mode == agentHubViewerMode {
		if _, ok := state.groupByID(viewerID); !ok {
			state.Mode = agentHubListMode
			state.ViewerGroupID = ""
			state.Viewer.GotoTop()
		}
	}
	if selectedID != "" {
		if index := state.visibleGroupIndex(selectedID); index >= 0 {
			state.List.Select(index)
			return nil
		}
	}
	if selectedID == "" || state.groupIndex(selectedID) < 0 {
		state.selectDefaultGroup()
	}
	return nil
}

// decorateNodeUnread derives process-local unread counts from monotonic node
// revisions. New nodes and semantic transitions count as unread while the Hub
// list is open; a group currently visible in the Viewer is admitted as read.
// A revision regression fails closed instead of turning a replay into fresh
// activity.
func (state *AgentHubState) decorateNodeUnread(
	surface *AgentSurfaceProjection,
	viewerID BlockID,
) bool {
	if state == nil || surface == nil {
		return false
	}
	if state.seenNodeRevision == nil {
		state.seenNodeRevision = make(map[string]uint64)
	}
	retained := make(map[string]struct{})
	for groupIndex := range surface.Groups {
		group := &surface.Groups[groupIndex]
		for nodeIndex := range group.Nodes {
			node := &group.Nodes[nodeIndex]
			retained[node.ID] = struct{}{}
			seen, known := state.seenNodeRevision[node.ID]
			if known && node.Revision < seen {
				return false
			}
			if group.ID == viewerID {
				state.seenNodeRevision[node.ID] = node.Revision
				node.Unread = 0
				continue
			}
			if known {
				node.Unread = int(node.Revision - seen)
			} else {
				node.Unread = int(node.Revision)
			}
		}
	}
	for id := range state.seenNodeRevision {
		if _, ok := retained[id]; !ok {
			delete(state.seenNodeRevision, id)
		}
	}
	return surface.valid()
}

func (state *AgentHubState) SetSize(terminalWidth, terminalHeight int) {
	if state == nil {
		return
	}
	state.width = max(1, terminalWidth)
	state.height = max(1, terminalHeight)
	compact := compactAgentHub(state.width, state.height)
	if compact != state.compact {
		state.compact = compact
		delegate := newAgentHubDelegate(state.isDark, compact, state.themeID, state.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		state.ItemSpacing = delegate.Spacing()
	}
	state.configureListTitle()

	listWidth := pickerListWidth(state.width)
	desiredRows := max(5, len(state.List.Items())*(state.ItemHeight+max(0, state.ItemSpacing))+2)
	listHeight := pickerListHeight(state.height, desiredRows, 4)
	state.List.SetSize(listWidth, listHeight)
	state.List.SetShowPagination(!state.compact && desiredRows > listHeight)

	state.Viewer.SetWidth(listWidth)
	state.Viewer.SetHeight(max(1, state.height-6))
	state.rebuildViewerContent(true)
}

// fitViewerToContent shrinks the viewer to its document when the document is
// shorter than the space reserved for it.
//
// The height was a fixed terminal-derived reservation, so a viewer with four
// rows of content still painted a panel with ten blank rows inside its border
// — the panel read as unfinished rather than as a short answer. The reservation
// stays the ceiling; scrolling is unaffected because a document that fits has
// nothing to scroll.
func (state *AgentHubState) fitViewerToContent() {
	if state == nil || state.Mode != agentHubViewerMode {
		return
	}
	ceiling := max(1, state.height-6)
	rows := state.Viewer.TotalLineCount()
	if rows <= 0 || rows >= ceiling {
		return
	}
	state.Viewer.SetHeight(rows)
}

func (state *AgentHubState) SetTheme(isDark bool, reducedMotion bool, themeID string) {
	if state == nil {
		return
	}
	state.isDark = isDark
	state.themeID = resolveThemeID(themeID)
	state.reducedMotion = reducedMotion
	delegate := newAgentHubDelegate(isDark, state.compact, state.themeID, state.glyphProfile)
	state.List.SetDelegate(delegate)
	state.ItemHeight = delegate.Height()
	state.ItemSpacing = delegate.Spacing()
	configurePickerList(&state.List, isDark, state.themeID, reducedMotion)
	configurePickerListGlyphProfile(&state.List, state.glyphProfile)
	state.configureListTitle()
	state.rebuildViewerContent(true)
}

func (state *AgentHubState) selectedGroupID() BlockID {
	if state == nil {
		return ""
	}
	if state.Mode == agentHubViewerMode && state.ViewerGroupID != "" {
		return state.ViewerGroupID
	}
	item, ok := state.List.SelectedItem().(agentHubItem)
	if !ok {
		return ""
	}
	return item.group.ID
}

func (state *AgentHubState) selectedGroup() (AgentGroupProjection, bool) {
	if state == nil {
		return AgentGroupProjection{}, false
	}
	if state.Mode == agentHubViewerMode {
		return state.groupByID(state.ViewerGroupID)
	}
	item, ok := state.List.SelectedItem().(agentHubItem)
	if !ok {
		return AgentGroupProjection{}, false
	}
	return item.group, true
}

func (state *AgentHubState) groupByID(id BlockID) (AgentGroupProjection, bool) {
	if state == nil || id == "" {
		return AgentGroupProjection{}, false
	}
	for _, group := range state.Surface.Groups {
		if group.ID == id {
			return group, true
		}
	}
	return AgentGroupProjection{}, false
}

func (state *AgentHubState) groupIndex(id BlockID) int {
	if state == nil || id == "" {
		return -1
	}
	for index, group := range state.Surface.Groups {
		if group.ID == id {
			return index
		}
	}
	return -1
}

func (state *AgentHubState) visibleGroupIndex(id BlockID) int {
	if state == nil || id == "" {
		return -1
	}
	for index, raw := range state.List.VisibleItems() {
		item, ok := raw.(agentHubItem)
		if ok && item.group.ID == id {
			return index
		}
	}
	return -1
}

func (state *AgentHubState) selectDefaultGroup() {
	if state == nil {
		return
	}
	visible := state.List.VisibleItems()
	if len(visible) == 0 {
		return
	}
	selected := len(visible) - 1
	for index := len(visible) - 1; index >= 0; index-- {
		item, ok := visible[index].(agentHubItem)
		if ok && item.group.Lifecycle == BlockLive {
			selected = index
			break
		}
	}
	state.List.Select(selected)
}

func (state *AgentHubState) openSelectedViewer() bool {
	group, ok := state.selectedGroup()
	if !ok {
		return false
	}
	state.markAgentGroupRead(group.ID)
	group, ok = state.groupByID(group.ID)
	if !ok {
		return false
	}
	state.Mode = agentHubViewerMode
	state.ViewerGroupID = group.ID
	state.Viewer.GotoTop()
	state.rebuildViewerContent(false)
	return true
}

func (state *AgentHubState) markAgentGroupRead(id BlockID) {
	if state == nil || id == "" {
		return
	}
	groupIndex := state.groupIndex(id)
	if groupIndex < 0 {
		return
	}
	group := &state.Surface.Groups[groupIndex]
	for nodeIndex := range group.Nodes {
		node := &group.Nodes[nodeIndex]
		state.seenNodeRevision[node.ID] = node.Revision
		node.Unread = 0
	}
	items := state.List.Items()
	if groupIndex < len(items) {
		items[groupIndex] = agentHubItem{
			displayID:    agentGroupDisplayID(group.ID),
			group:        *group,
			glyphProfile: state.glyphProfile,
		}
		if state.List.FilterState() != list.Unfiltered {
			state.applyCurrentFilterSynchronously()
		}
	}
}

// Back consumes Escape when it first needs to clear a filter or return from
// the Viewer. False means the parent should close the modal.
func (state *AgentHubState) Back() bool {
	if state == nil {
		return false
	}
	if state.Mode == agentHubViewerMode {
		state.Mode = agentHubListMode
		state.ViewerGroupID = ""
		state.Viewer.GotoTop()
		return true
	}
	if state.List.FilterState() != list.Unfiltered {
		state.List.ResetFilter()
		return true
	}
	return false
}

func (state *AgentHubState) UpdateKey(msg tea.KeyPressMsg, keys KeyMap) tea.Cmd {
	if state == nil {
		return nil
	}
	if state.Mode == agentHubViewerMode {
		navigateReadOnlyViewport(&state.Viewer, msg.String())
		return nil
	}
	if state.List.FilterState() == list.Filtering {
		state.List, _ = state.List.Update(msg)
		state.applyCurrentFilterSynchronously()
		return nil
	}
	if key.Matches(msg, keys.CompleteSelect) {
		state.openSelectedViewer()
		return nil
	}
	var cmd tea.Cmd
	state.List, cmd = state.List.Update(msg)
	if state.List.FilterState() != list.Unfiltered {
		state.applyCurrentFilterSynchronously()
		return nil
	}
	return cmd
}

func (state *AgentHubState) Update(msg tea.Msg) tea.Cmd {
	if state == nil {
		return nil
	}
	if _, staleFilterResult := msg.(list.FilterMatchesMsg); staleFilterResult {
		// Agent Hub filtering is bounded to maxAgentSurfaceGroups and applied
		// synchronously. Ignore generation-less Bubbles filter receipts so a
		// delayed result cannot replace a newer live projection.
		return nil
	}
	if state.Mode == agentHubViewerMode {
		var cmd tea.Cmd
		state.Viewer, cmd = state.Viewer.Update(msg)
		return cmd
	}
	filterState := state.List.FilterState()
	filterText := state.List.FilterInput.Value()
	var cmd tea.Cmd
	state.List, cmd = state.List.Update(msg)
	if state.List.FilterState() != list.Unfiltered &&
		(filterState != state.List.FilterState() || filterText != state.List.FilterInput.Value()) {
		// Paste and other non-key input can mutate FilterInput while returning
		// Bubbles' generation-less asynchronous matcher. Apply the bounded
		// filter now so a later receipt can never install stale matches.
		state.applyCurrentFilterSynchronously()
		return nil
	}
	return cmd
}

func (state *AgentHubState) applyCurrentFilterSynchronously() {
	if state == nil {
		return
	}
	cursor := state.List.FilterInput.Position()
	state.applyFilterSynchronously(state.List.FilterState(), state.List.FilterInput.Value())
	if state.List.FilterState() == list.Filtering {
		state.List.FilterInput.SetCursor(min(
			max(0, cursor),
			utf8.RuneCountInString(state.List.FilterInput.Value()),
		))
	}
}

func (state *AgentHubState) applyFilterSynchronously(filterState list.FilterState, value string) {
	if state == nil || filterState == list.Unfiltered {
		return
	}
	state.List.SetFilterText(value)
	if filterState == list.Filtering {
		state.List.SetFilterState(list.Filtering)
	}
}

func (state *AgentHubState) rebuildViewerContent(preserveOffset bool) {
	if state == nil || state.Mode != agentHubViewerMode {
		return
	}
	offset := 0
	var anchor agentViewerRowAnchor
	hasAnchor := false
	if preserveOffset {
		offset = state.Viewer.YOffset()
		if offset >= 0 && offset < len(state.viewerRows) {
			anchor = state.viewerRows[offset]
			hasAnchor = true
		}
	}
	group, ok := state.groupByID(state.ViewerGroupID)
	if !ok {
		state.Viewer.SetContent("")
		state.Viewer.GotoTop()
		state.viewerContentKey = ""
		state.viewerRows = nil
		return
	}
	key := fmt.Sprintf(
		"%s:%d:%d:%t:%t:%d",
		group.ID,
		group.Revision,
		state.Viewer.Width(),
		state.isDark,
		noColor,
		state.glyphProfile,
	)
	if key == state.viewerContentKey && len(state.viewerRows) > 0 {
		state.Viewer.SetYOffset(offset)
		return
	}
	layout := renderAgentViewerLayout(group, state.Viewer.Width(), state.isDark, state.themeID, state.glyphProfile)
	state.Viewer.SetContent(layout.content)
	state.viewerContentKey = key
	state.viewerRows = layout.rows
	if hasAnchor {
		offset = resolveAgentViewerRowOffset(anchor, layout.rows, offset)
	}
	state.Viewer.SetYOffset(offset)
	state.fitViewerToContent()
}

func renderAgentViewerBody(group AgentGroupProjection, width int, isDark bool, themeID string, profiles ...GlyphProfile) string {
	return renderAgentViewerLayout(group, width, isDark, themeID, profiles...).content
}

func renderAgentViewerLayout(
	group AgentGroupProjection,
	width int,
	isDark bool,
	themeID string,
	profiles ...GlyphProfile,
) agentViewerLayout {
	width = max(1, width)
	profile := resolveGlyphProfile(profiles...)
	separator := glyphSeparator(profile)
	truncate := func(value string) string {
		return truncateDisplayWithGlyphProfile(value, width, profile)
	}
	styles := NewStyles(isDark, themeID)
	nodeStyles := NewToolCardStyles(isDark, themeID)
	lines := make([]string, 0, 10+len(group.Nodes)*2)
	rows := make([]agentViewerRowAnchor, 0, cap(lines))
	appendRow := func(line string, row agentViewerRowAnchor) {
		lines = append(lines, line)
		rows = append(rows, row)
	}
	appendRow(
		styles.OverlayAccent.Render(truncate(agentViewerStatusLine(group, profile))),
		agentViewerRowAnchor{key: "status"},
	)
	appendRow(
		styles.OverlayDim.Render(truncate(agentGroupSummary(group, profile))),
		agentViewerRowAnchor{key: "summary"},
	)
	if group.Interrupted {
		appendRow(styles.StatusWarning.Render(truncate(
			"Restored after interruption; child outcomes are unknown.",
		)), agentViewerRowAnchor{key: "interrupted"})
	}
	appendRow("", agentViewerRowAnchor{key: "before-subagents"})
	appendRow(
		styles.OverlayAccent.Render(truncate("Subagents")),
		agentViewerRowAnchor{key: "subagents"},
	)

	if !group.ProgressAvailable {
		appendRow(styles.OverlayDim.Render(truncate(
			"No public subagent progress is available.",
		)), agentViewerRowAnchor{key: "no-progress"})
	} else {
		for _, node := range group.Nodes {
			glyph, status, style := agentNodePresentation(node, nodeStyles, profile)
			label := node.Label
			if node.Status == WorkNodeQueued {
				label = fmt.Sprintf("Agent %d", node.Index+1)
			}
			role := fmt.Sprintf("%s %s%s%s", glyph, label, separator, status)
			if node.EvalTokens > 0 {
				role += fmt.Sprintf("%s%d tok", separator, node.EvalTokens)
			}
			activity := workNodeActivitySummary(node)
			meta := ""
			if node.Status != WorkNodeQueued {
				meta = node.Model + separator + string(node.Location)
			}
			if width >= 72 && meta != "" {
				line := role
				if activity != "" {
					line += separator + activity
				}
				line += separator + meta
				appendRow(
					style.Render(truncate(line)),
					agentViewerRowAnchor{nodeID: node.ID},
				)
			} else {
				appendRow(
					style.Render(truncate(role)),
					agentViewerRowAnchor{nodeID: node.ID},
				)
				if width >= 12 && (activity != "" || meta != "") {
					detail := activity
					if detail != "" && meta != "" {
						detail += separator
					}
					detail += meta
					appendRow(
						styles.OverlayDim.Render(truncate("  "+detail)),
						agentViewerRowAnchor{nodeID: node.ID, subrow: 1},
					)
				}
			}
			if node.ReportRef != nil && validWorkReportRef(*node.ReportRef) {
				appendRow(
					styles.OverlayDim.Render(truncate(
						"  report artifact"+separator+node.ReportRef.URI,
					)),
					agentViewerRowAnchor{nodeID: node.ID, subrow: 2},
				)
			}
		}
	}

	appendRow("", agentViewerRowAnchor{key: "before-activity"})
	appendRow(
		styles.OverlayAccent.Render(truncate("Activity")),
		agentViewerRowAnchor{key: "activity"},
	)
	appendRow(
		// "No public child events are available for this runtime" described the
		// implementation, not the situation: nothing in it tells a reader
		// whether something is wrong or what to do about it.
		styles.OverlayDim.Render(truncate(
			"This agent does not report step-by-step activity.",
		)),
		agentViewerRowAnchor{key: "no-events"},
	)
	return agentViewerLayout{content: strings.Join(lines, "\n"), rows: rows}
}

func agentViewerStatusLine(group AgentGroupProjection, profiles ...GlyphProfile) string {
	parts := []string{
		"Status",
		agentGroupStatusLabel(group),
	}
	if group.Elapsed > 0 {
		parts = append(parts, formatWorkingElapsed(group.Elapsed))
	}
	parts = append(parts, fmt.Sprintf("revision %d", group.Revision))
	return strings.Join(parts, glyphSeparator(resolveGlyphProfile(profiles...)))
}

func resolveAgentViewerRowOffset(
	anchor agentViewerRowAnchor,
	rows []agentViewerRowAnchor,
	fallback int,
) int {
	if anchor.nodeID != "" {
		firstNodeRow := -1
		for index, candidate := range rows {
			if candidate.nodeID != anchor.nodeID {
				continue
			}
			if firstNodeRow < 0 {
				firstNodeRow = index
			}
			if candidate.subrow == anchor.subrow {
				return index
			}
		}
		if firstNodeRow >= 0 {
			return firstNodeRow
		}
	} else if anchor.key != "" {
		for index, candidate := range rows {
			if candidate.key == anchor.key {
				return index
			}
		}
	}
	return min(max(0, fallback), max(0, len(rows)-1))
}

func agentNodePresentation(
	node WorkNode,
	styles ToolCardStyles,
	profiles ...GlyphProfile,
) (string, string, lipgloss.Style) {
	profile := resolveGlyphProfile(profiles...)
	glyphs := glyphSet(profile)
	switch node.Status {
	case WorkNodeQueued:
		return glyphs.Queued, "queued", styles.Dimmed
	case WorkNodeRunning:
		if profile == GlyphASCII {
			return glyphs.Running, "running", styles.TitleRunning
		}
		return "…", "running", styles.TitleRunning
	case WorkNodeWaiting:
		return glyphs.Waiting, "waiting", styles.TitleAttention
	case WorkNodeCompleted:
		return glyphs.Success, "completed", styles.TitleSuccess
	case WorkNodeAttention:
		return "!", expertFailureLabel(node.FailureCode), styles.TitleAttention
	case WorkNodeFailed:
		return glyphs.Error, expertFailureLabel(node.FailureCode), styles.TitleError
	case WorkNodeCancelled:
		return glyphs.Cancelled, "cancelled", styles.Dimmed
	default:
		return "?", "unknown", styles.Dimmed
	}
}

func agentGroupStatusLabel(group AgentGroupProjection) string {
	if group.Interrupted {
		return "interrupted"
	}
	switch group.Lifecycle {
	case BlockLive:
		if !group.ProgressAvailable {
			return "awaiting plan"
		}
		return "active"
	case BlockSettled:
		return "settled"
	case BlockFailed:
		return "failed"
	case BlockCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

func agentGroupSummary(group AgentGroupProjection, profiles ...GlyphProfile) string {
	separator := glyphSeparator(resolveGlyphProfile(profiles...))
	if !group.ProgressAvailable {
		parts := []string{"No public subagent progress yet"}
		if group.Elapsed > 0 {
			parts = append(parts, formatWorkingElapsed(group.Elapsed))
		}
		return strings.Join(parts, separator)
	}
	parts := []string{
		string(group.Strategy),
		fmt.Sprintf("%d %s", group.Total, pluralizeNoun(group.Total, "agent", "agents")),
	}
	for _, count := range []struct {
		value int
		label string
	}{
		{group.Running, "active"},
		{group.Waiting, "waiting"},
		{group.Queued, "queued"},
		{group.Completed, "completed"},
		{group.Attention, "attention"},
		{group.Failed, "failed"},
		{group.Cancelled, "cancelled"},
	} {
		if count.value > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count.value, count.label))
		}
	}
	if unread := agentGroupUnread(group); unread > 0 {
		parts = append(parts, fmt.Sprintf("%d unread", unread))
	}
	if group.Elapsed > 0 {
		parts = append(parts, formatWorkingElapsed(group.Elapsed))
	}
	return strings.Join(parts, separator)
}

func agentGroupUnread(group AgentGroupProjection) int {
	unread := 0
	for _, node := range group.Nodes {
		unread += node.Unread
	}
	return unread
}

func (state *AgentHubState) hubContent(styles Styles) string {
	if state == nil {
		return ""
	}
	if state.Unavailable {
		width := state.List.Width()
		content := styles.OverlayTitle.Render("Agents") + "\n\n" +
			renderAgentHubWrapped(styles.ErrorText, "Agent activity is unavailable.", width) + "\n" +
			renderAgentHubWrapped(styles.OverlayDim, "The safe runtime projection was rejected.", width)
		return lipgloss.NewStyle().Width(width).Height(state.List.Height()).Render(content)
	}
	if len(state.Surface.Groups) == 0 {
		width := state.List.Width()
		content := styles.OverlayTitle.Render("Agents") + "\n\n" +
			renderAgentHubWrapped(styles.OverlayDim, "No agent consultations yet.", width) + "\n" +
			renderAgentHubWrapped(styles.OverlayDim, "Agents created during a run will appear here.", width)
		return lipgloss.NewStyle().Width(width).Height(state.List.Height()).Render(content)
	}
	return state.List.View()
}

func renderAgentHubWrapped(style lipgloss.Style, value string, width int) string {
	lines := strings.Split(wrapText(value, max(1, width)), "\n")
	for index := range lines {
		lines[index] = style.Render(lines[index])
	}
	return strings.Join(lines, "\n")
}

func (state *AgentHubState) viewerContent(styles Styles) string {
	if state == nil {
		return ""
	}
	group, ok := state.groupByID(state.ViewerGroupID)
	if !ok {
		return styles.ErrorText.Render("Agent activity is unavailable.")
	}
	width := state.Viewer.Width()
	title := styles.OverlayTitle.Render(truncateDisplayWithGlyphProfile(
		"Agent viewer",
		width,
		state.glyphProfile,
	))
	subtitle := styles.OverlayDim.Render(truncateDisplayWithGlyphProfile(
		fmt.Sprintf(
			"Consultation #%s%s%s",
			agentGroupDisplayID(group.ID),
			glyphSeparator(state.glyphProfile),
			agentGroupStatusLabel(group),
		),
		width,
		state.glyphProfile,
	))
	return title + "\n" + subtitle + "\n" + state.Viewer.View()
}

func (m *Model) openAgentHub() {
	if m == nil {
		return
	}
	anchor := m.captureTranscriptReflowAnchor()
	surface, err := m.agentSurfaceProjection()
	m.agentHubState = newAgentHubState(
		surface,
		err != nil,
		m.width,
		m.height,
		m.isDark,
		m.themeID,
		m.reducedMotion,
		m.glyphProfile,
	)
	m.overlay = OverlayAgents
	m.input.Blur()
	m.recalcViewportHeight()
	m.restoreTranscriptReflowAnchor(anchor)
}

func (m *Model) closeAgentHub() {
	if m == nil {
		return
	}
	anchor := m.captureTranscriptReflowAnchor()
	m.agentHubState = nil
	m.closeOverlayToParent()
	if !m.composerEditable() {
		m.input.Blur()
	}
	m.recalcViewportHeight()
	m.restoreTranscriptReflowAnchor(anchor)
}

func (m *Model) agentSurfaceProjection() (AgentSurfaceProjection, error) {
	if err := m.reconcileTranscriptEntries(); err != nil {
		return AgentSurfaceProjection{}, err
	}
	return projectAgentSurfaceAt(m.entries, m.toolEntries, m.nowTime())
}

// refreshedAgentSurfaceProjection is used only after transcript mutation paths
// have invalidated and repainted the semantic transcript. It reuses that
// renderer-owned admission result instead of reconciling the full transcript a
// second time for the visible Hub. Opening the Hub still uses the direct
// projection above and never depends on a paint cache.
func (m *Model) refreshedAgentSurfaceProjection() (AgentSurfaceProjection, error) {
	if _, err := m.reconcileTranscriptEntriesForRender(); err != nil {
		return AgentSurfaceProjection{}, err
	}
	return projectAgentSurfaceAt(m.entries, m.toolEntries, m.nowTime())
}

func (m *Model) refreshAgentHub() tea.Cmd {
	if m == nil || m.overlay != OverlayAgents || m.agentHubState == nil {
		return nil
	}
	surface, err := m.refreshedAgentSurfaceProjection()
	return m.agentHubState.SetProjection(surface, err != nil)
}

func (m *Model) resizeAgentHub() {
	if m != nil && m.overlay == OverlayAgents && m.agentHubState != nil {
		m.agentHubState.SetSize(m.width, m.height)
	}
}

func (m *Model) restyleAgentHub() {
	if m != nil && m.overlay == OverlayAgents && m.agentHubState != nil {
		m.agentHubState.SetTheme(m.isDark, m.reducedMotion, m.themeID)
	}
}

func (m *Model) renderAgentHub() string {
	if m == nil || m.agentHubState == nil {
		return ""
	}
	width := pickerListWidth(m.width)
	var content string
	var hints []keyHint
	if m.agentHubState.Mode == agentHubViewerMode {
		content = m.agentHubState.viewerContent(m.styles)
		hints = []keyHint{
			{Key: "esc", Action: "back"},
			{Key: "enter", Action: "jump"},
			{Key: "j/k", Action: "scroll"},
			{Key: "pgup/pgdn", Action: "page"},
		}
	} else {
		content = m.agentHubState.hubContent(m.styles)
		switch {
		case m.agentHubState.Unavailable || len(m.agentHubState.Surface.Groups) == 0:
			hints = []keyHint{{Key: "esc", Action: "close"}}
		case m.agentHubState.List.FilterState() == list.Filtering:
			hints = []keyHint{{Key: "esc", Action: "cancel"}}
			if strings.TrimSpace(m.agentHubState.List.FilterInput.Value()) != "" {
				hints = append(hints, keyHint{Key: "enter", Action: "apply"})
			}
		case m.agentHubState.List.FilterState() == list.FilterApplied &&
			len(m.agentHubState.List.VisibleItems()) == 0:
			hints = []keyHint{{Key: "esc", Action: "clear"}}
		case m.agentHubState.List.FilterState() == list.FilterApplied:
			hints = []keyHint{
				{Key: "esc", Action: "clear"},
				{Key: "enter", Action: "view"},
				{Key: pickerMoveKey(m.glyphProfile), Action: "move"},
			}
		default:
			hints = []keyHint{
				{Key: "esc", Action: "close"},
				{Key: "enter", Action: "view"},
				{Key: pickerMoveKey(m.glyphProfile), Action: "move"},
				{Key: "/", Action: "filter"},
			}
		}
	}
	return m.renderPickerFrame(content, m.renderAgentHubHints(width, hints))
}

func (m *Model) renderAgentHubHints(width int, hints []keyHint) string {
	for keep := len(hints); keep > 0; keep-- {
		rendered := m.renderKeyHintSet(hints[:keep], -1)
		if lipgloss.Width(rendered) <= width {
			return rendered
		}
	}
	return m.renderKeyHints(width, hints...)
}

func (m *Model) agentHubBack() bool {
	return m != nil && m.agentHubState != nil && m.agentHubState.Back()
}

func (m *Model) handleAgentHubKey(msg tea.KeyPressMsg) tea.Cmd {
	if m == nil {
		return nil
	}
	if m.agentHubState == nil {
		m.closeAgentHub()
		return nil
	}
	if key.Matches(msg, m.keys.AgentHub) {
		m.closeAgentHub()
		return tea.ClearScreen
	}
	if m.agentHubState.Mode == agentHubViewerMode && key.Matches(msg, m.keys.CompleteSelect) {
		if group, ok := m.agentHubState.selectedGroup(); ok && m.jumpToAgentGroup(group) {
			m.closeAgentHub()
			return tea.ClearScreen
		}
		return nil
	}
	return m.agentHubState.UpdateKey(msg, m.keys)
}

func (m *Model) updateAgentHubMessage(msg tea.Msg) tea.Cmd {
	if m == nil || m.agentHubState == nil {
		return nil
	}
	switch msg.(type) {
	case tea.WindowSizeMsg, tea.BackgroundColorMsg,
		ToolCallStartMsg, ExpertProgressMsg, ToolCallResultMsg:
		// The smart parent already resized, restyled, or reprojected the Hub
		// while handling these messages. Delivering them to the child again
		// would duplicate list/filter work without adding state.
		return nil
	}
	return m.agentHubState.Update(msg)
}

func (m *Model) jumpToAgentGroup(group AgentGroupProjection) bool {
	if m == nil || !group.ID.Valid() || len(m.transcriptLayout.Records) == 0 {
		return false
	}
	viewportHeight := max(1, m.viewport.Height())
	screenRow := min(2, max(0, viewportHeight/3))
	resolution, err := ResolveTranscriptAnchor(
		ManualTranscriptAnchor(SemanticAnchor{
			SessionID: m.transcriptLayout.SessionID,
			BlockID:   group.ID,
			TurnID:    group.TurnID,
			ScreenRow: screenRow,
			Bias:      AnchorBiasNext,
		}),
		m.transcriptLayout,
		m.transcriptLayout,
		viewportHeight,
	)
	if err != nil || resolution.BlockID != group.ID {
		return false
	}
	m.setTranscriptYOffset(resolution.ViewportTop)
	m.pauseFollow()
	return true
}

type agentHubPointerProjection struct {
	lines    []string
	startY   int
	baseRows int
	width    int
}

func (m *Model) projectAgentHubPointer() (agentHubPointerProjection, bool) {
	if m == nil || m.agentHubState == nil || m.overlay != OverlayAgents || m.width <= 0 {
		return agentHubPointerProjection{}, false
	}
	overlay := m.renderAgentHub()
	if overlay == "" {
		return agentHubPointerProjection{}, false
	}
	frame := m.projectFrame()
	base := m.viewport.View() + "\n" + frame.Footer.Content
	return agentHubPointerProjection{
		lines:    strings.Split(overlay, "\n"),
		startY:   centeredOverlayStartY(base, overlay),
		baseRows: len(strings.Split(base, "\n")),
		width:    m.width,
	}, true
}

func (projection agentHubPointerProjection) rowRect(localY int) CellRect {
	if localY < 0 || localY >= len(projection.lines) || projection.startY+localY >= projection.baseRows {
		return CellRect{}
	}
	lineWidth := lipgloss.Width(projection.lines[localY])
	if lineWidth <= 0 {
		return CellRect{}
	}
	startX := centeredOverlayLineX(projection.width, projection.lines[localY])
	return NewCellRect(
		startX,
		projection.startY+localY,
		startX+lineWidth,
		projection.startY+localY+1,
	)
}

func (projection agentHubPointerProjection) contains(x, y int) bool {
	return projection.rowRect(y-projection.startY).Contains(x, y)
}

func agentHubTitleRows(state *AgentHubState) int {
	if state == nil {
		return 0
	}
	title := state.List.Styles.Title.Render(state.List.Title)
	if state.List.FilterState() == list.Filtering {
		title = state.List.FilterInput.View()
	}
	return lipgloss.Height(state.List.Styles.TitleBar.Render(title))
}

func (m *Model) agentHubItemAt(x, y int) (int, bool) {
	state := m.agentHubState
	projection, ok := m.projectAgentHubPointer()
	if !ok || state.Mode != agentHubListMode || state.ItemHeight <= 0 {
		return 0, false
	}
	localY := y - projection.startY
	itemStartY := 1 + agentHubTitleRows(state)
	itemY := localY - itemStartY
	stride := state.ItemHeight + max(0, state.ItemSpacing)
	if itemY < 0 || stride <= 0 || itemY%stride >= state.ItemHeight {
		return 0, false
	}
	itemRow := Inset(projection.rowRect(localY), Insets{
		Left:  pickerFrameCursorX,
		Right: pickerFrameCursorX,
	})
	if !itemRow.Contains(x, y) {
		return 0, false
	}
	rowOnPage := itemY / stride
	if rowOnPage < 0 || rowOnPage >= state.List.Paginator.PerPage {
		return 0, false
	}
	index := state.List.Paginator.Page*state.List.Paginator.PerPage + rowOnPage
	if index < 0 || index >= len(state.List.VisibleItems()) {
		return 0, false
	}
	if _, ok := state.List.VisibleItems()[index].(agentHubItem); !ok {
		return 0, false
	}
	return index, true
}

func (m *Model) updateAgentHubWheel(msg tea.MouseWheelMsg) tea.Cmd {
	state := m.agentHubState
	projection, ok := m.projectAgentHubPointer()
	if !ok || !projection.contains(msg.X, msg.Y) {
		return nil
	}
	if state.Mode == agentHubViewerMode {
		var cmd tea.Cmd
		state.Viewer, cmd = state.Viewer.Update(msg)
		return cmd
	}
	if len(state.List.VisibleItems()) == 0 {
		return nil
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		state.List.CursorUp()
	case tea.MouseWheelDown:
		state.List.CursorDown()
	}
	return nil
}

func (m *Model) selectAgentHubPointer(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft || m.agentHubState == nil {
		return nil
	}
	index, ok := m.agentHubItemAt(msg.X, msg.Y)
	if ok {
		m.agentHubState.List.Select(index)
	}
	return nil
}
