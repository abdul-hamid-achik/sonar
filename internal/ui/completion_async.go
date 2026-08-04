package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

const completionSearchDebounce = 300 * time.Millisecond

type completionWorkspaceSearch func(context.Context, string, string) []Completion

// completionWorkspaceReader admits at most one filesystem search worker. A
// cancelled command returns immediately, but the slot stays occupied until an
// uninterruptible ReadDir/WalkDir call actually returns. Rapid filter edits on
// a stalled FUSE or network mount therefore cannot accumulate goroutines.
type completionWorkspaceReader struct {
	slot chan struct{}
}

func newCompletionWorkspaceReader() *completionWorkspaceReader {
	return &completionWorkspaceReader{slot: make(chan struct{}, 1)}
}

func (reader *completionWorkspaceReader) read(
	ctx context.Context,
	search completionWorkspaceSearch,
	query, path string,
) []Completion {
	if reader == nil || reader.slot == nil || search == nil || ctx == nil {
		return nil
	}
	select {
	case reader.slot <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	if ctx.Err() != nil {
		<-reader.slot
		return nil
	}

	result := make(chan []Completion, 1)
	go func() {
		defer func() { <-reader.slot }()
		result <- search(ctx, query, path)
	}()
	select {
	case items := <-result:
		return items
	case <-ctx.Done():
		return nil
	}
}

type completionTranscriptAnchor struct {
	valid   bool
	paused  bool
	yOffset int
}

func (m *Model) captureCompletionTranscriptAnchor() completionTranscriptAnchor {
	if m == nil || !m.ready {
		return completionTranscriptAnchor{}
	}
	return completionTranscriptAnchor{valid: true, paused: m.followPaused(), yOffset: m.transcriptYOffset()}
}

func (m *Model) restoreCompletionTranscriptAnchor(anchor completionTranscriptAnchor) {
	if m == nil || !anchor.valid {
		return
	}
	m.restoreFollowPosition(anchor.paused, anchor.yOffset)
}

func (m *Model) scheduleCompletionSearch(query, path string, debounce bool) tea.Cmd {
	cs := m.completionState
	if cs == nil || cs.Kind != "attachments" || m.completer == nil {
		return nil
	}
	if cs.SearchCancel != nil {
		cs.SearchCancel()
		cs.SearchCancel = nil
	}
	cs.DebounceTag++
	cs.Searching = true
	generation := cs.Generation
	tag := cs.DebounceTag
	if debounce {
		return tea.Tick(completionSearchDebounce, func(time.Time) tea.Msg {
			return CompletionDebounceTickMsg{
				Generation: generation,
				Tag:        tag,
				Query:      query,
				Path:       path,
			}
		})
	}
	return m.beginCompletionSearch(generation, tag, query, path)
}

func (m *Model) beginCompletionSearch(generation uint64, tag int, query, path string) tea.Cmd {
	cs := m.completionState
	if cs == nil || cs.Kind != "attachments" || cs.Generation != generation || cs.DebounceTag != tag || m.completer == nil {
		return nil
	}
	if cs.SearchCancel != nil {
		cs.SearchCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cs.SearchCancel = cancel
	cs.Searching = true
	completer := m.completer
	search := completer.WorkspaceCompletions
	if m.completionSearch != nil {
		search = m.completionSearch
	}
	reader := m.completionReader
	if reader == nil {
		reader = newCompletionWorkspaceReader()
		m.completionReader = reader
	}
	return func() tea.Msg {
		defer cancel()
		return CompletionSearchResultMsg{
			Generation: generation,
			Tag:        tag,
			Results:    reader.read(ctx, search, query, path),
		}
	}
}

func mergeCompletionItems(base, workspace []Completion) []Completion {
	merged := make([]Completion, 0, len(base)+len(workspace))
	existing := make(map[string]bool, len(base)+len(workspace))
	for _, group := range [][]Completion{base, workspace} {
		for _, item := range group {
			key := item.Insert + "\x00" + item.Label
			if existing[key] {
				continue
			}
			existing[key] = true
			merged = append(merged, item)
		}
	}
	return merged
}

func completionSelectionKey(item Completion) string {
	return item.Insert + "\x00" + item.Label
}

func replaceCompletionItems(cs *CompletionState, items []Completion) {
	if cs == nil {
		return
	}
	selected := make(map[string]bool, len(cs.Selected))
	for index, isSelected := range cs.Selected {
		if isSelected && index >= 0 && index < len(cs.AllItems) {
			selected[completionSelectionKey(cs.AllItems[index])] = true
		}
	}
	cs.AllItems = items
	if cs.Selected != nil {
		cs.Selected = make(map[int]bool)
		for index, item := range items {
			if selected[completionSelectionKey(item)] {
				cs.Selected[index] = true
			}
		}
	}
	cs.FilteredItems = FilterCompletions(items, cs.Filter.Value())
	cs.Index = max(0, min(cs.Index, len(cs.FilteredItems)-1))
}

// handleCompletionDebounceTick begins the debounced completion search when
// the tick still matches the active completion generation.
func (m *Model) handleCompletionDebounceTick(msg CompletionDebounceTickMsg) (tea.Cmd, bool) {
	if m.isCompletionActive() &&
		m.completionState.Generation == msg.Generation &&
		m.completionState.DebounceTag == msg.Tag {
		return m.beginCompletionSearch(msg.Generation, msg.Tag, msg.Query, msg.Path), true
	}
	return nil, false
}

// handleCompletionSearchResult merges asynchronous completion search results
// into the active completion state.
func (m *Model) handleCompletionSearchResult(msg CompletionSearchResultMsg, cmds []tea.Cmd) []tea.Cmd {
	if m.isCompletionActive() &&
		m.completionState.Generation == msg.Generation &&
		m.completionState.DebounceTag == msg.Tag {
		anchor := m.captureCompletionTranscriptAnchor()
		cs := m.completionState
		cs.Searching = false
		cs.SearchCancel = nil
		replaceCompletionItems(cs, mergeCompletionItems(cs.BaseItems, msg.Results))
		cmds = append(cmds, m.refreshCompletionPreview())
		m.recalcViewportHeight()
		m.restoreCompletionTranscriptAnchor(anchor)
	}
	return cmds
}

// handleCompletionPreviewResult applies a tokened attachment preview to the
// active completion state.
func (m *Model) handleCompletionPreviewResult(msg completionPreviewResultMsg) {
	if m.isCompletionActive() &&
		m.completionState.Kind == "attachments" &&
		m.completionState.Generation == msg.Generation &&
		m.completionState.PreviewToken == msg.Token {
		anchor := m.captureCompletionTranscriptAnchor()
		m.completionState.PreviewCancel = nil
		m.completionState.Preview = msg.Preview
		m.recalcViewportHeight()
		m.restoreCompletionTranscriptAnchor(anchor)
	}
}
