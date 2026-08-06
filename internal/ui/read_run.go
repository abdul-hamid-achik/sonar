package ui

import (
	"fmt"
	"strings"
)

// collapsedReadRunMinimum is how many consecutive read-family receipts it takes
// before they are worth summarizing.
//
// Two stacked receipts are not noise; they are two facts, and replacing them
// with "Read 2 files" costs the reader both filenames to save one row. The
// third is where a stack starts reading as texture rather than as content — the
// audited session showed eight in a row, of which a reader takes in none.
const collapsedReadRunMinimum = 3

// collapsedReadRun is a run of consecutive settled read-family receipts that
// the transcript renders as one line.
type collapsedReadRun struct {
	// Start is the entry index that renders the summary; the remaining
	// Length-1 entries render nothing at all.
	Start  int
	Length int

	Reads    int
	Searches int
	// Target is the last member's subject, shown as a live hint so a long run
	// still says what it is working through rather than only how much.
	Target string
}

// readRunMember reports whether one entry may join a run.
//
// The conditions are all about what a reader would lose. A failure or an
// attention state must stay its own line, because summarizing it away is
// precisely how a reader misses the one receipt that mattered. A running
// receipt is excluded because its line is still changing. And an entry the
// user expanded is an entry they asked to see.
func (m *Model) readRunMember(index int) bool {
	if index < 0 || index >= len(m.entries) {
		return false
	}
	entry := m.entries[index]
	if entry.Kind != "tool_group" || entry.ToolIndex < 0 || entry.ToolIndex >= len(m.toolEntries) {
		return false
	}
	tool := m.toolEntries[entry.ToolIndex]
	if tool.Status != ToolStatusDone || tool.IsError || !tool.Collapsed {
		return false
	}
	if len(tool.DiffLines) > 0 || tool.DiffPending {
		// A receipt carrying a diff has content of its own to show.
		return false
	}
	switch classifyProjectedTool(tool.Name, tool.Projection) {
	case ToolTypeFileRead, ToolTypeSearch:
		return true
	default:
		return false
	}
}

// collapsedReadRunAt returns the run that STARTS at index.
//
// It answers false for an entry in the middle of a run, which is what lets the
// renderer ask one question per entry and get a consistent answer: exactly one
// index in a run is its start, and every other member renders nothing.
func (m *Model) collapsedReadRunAt(index int) (collapsedReadRun, bool) {
	if !m.readRunMember(index) || m.readRunMember(index-1) {
		return collapsedReadRun{}, false
	}
	// A run the reader opened stays open. Summarizing receipts away is only
	// acceptable while there is a way back to them, and this is that way.
	if _, expanded := m.expandedReadRuns[m.entries[index].BlockID]; expanded {
		return collapsedReadRun{}, false
	}
	run := collapsedReadRun{Start: index}
	for cursor := index; m.readRunMember(cursor); cursor++ {
		run.Length++
		tool := m.toolEntries[m.entries[cursor].ToolIndex]
		if classifyProjectedTool(tool.Name, tool.Projection) == ToolTypeSearch {
			run.Searches++
		} else {
			run.Reads++
		}
		if target := readRunTarget(tool); target != "" {
			run.Target = target
		}
	}
	if run.Length < collapsedReadRunMinimum {
		return collapsedReadRun{}, false
	}
	return run, true
}

// collapsedReadRunFollower reports whether an entry is a non-first member of a
// run, and therefore renders nothing.
func (m *Model) collapsedReadRunFollower(index int) bool {
	if !m.readRunMember(index) {
		return false
	}
	start := index
	for m.readRunMember(start - 1) {
		start--
	}
	if start == index {
		return false
	}
	_, collapsed := m.collapsedReadRunAt(start)
	return collapsed
}

// readRunTarget is the one string worth keeping from a member: what it was
// pointed at. Summary and Target are already bounded projections, so nothing
// unbounded reaches the transcript through here.
func readRunTarget(tool ToolEntry) string {
	if summary := strings.TrimSpace(tool.Summary); summary != "" {
		return summary
	}
	if projected := strings.TrimSpace(projectedToolTarget(tool.Projection)); projected != "" {
		return projected
	}
	return ""
}

// Summary renders the run as one sentence.
//
// Both halves are named only when both happened, so a run of pure reads does
// not read as though a search occurred. Plurals are spelled out rather than
// suffixed with (s), because a receipt line is prose a person scans, not a log
// format.
func (run collapsedReadRun) Summary() string {
	switch {
	case run.Reads > 0 && run.Searches > 0:
		return fmt.Sprintf("%s, %s", countedFiles(run.Reads), countedSearches(run.Searches))
	case run.Reads > 0:
		return countedFiles(run.Reads)
	case run.Searches > 0:
		// A run of only searches gets its own verb; "3 searches" alone reads
		// as a fragment where "Read 3 files" reads as a sentence.
		return "Searched " + plural(run.Searches, "time", "times")
	default:
		return ""
	}
}

func countedFiles(count int) string    { return "Read " + plural(count, "file", "files") }
func countedSearches(count int) string { return plural(count, "search", "searches") }

func plural(count int, singular, many string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", count, many)
}

// renderCollapsedReadRun paints one run as a single settled receipt.
//
// It borrows the shape of a collapsed tool card rather than inventing one:
// the same content-grid accent, the same success glyph, the same dimmed
// treatment settled history gets. A reader should not have to learn that this
// line is a different kind of thing — it is the same thing, summarized.
func (m *Model) renderCollapsedReadRun(b *strings.Builder, run collapsedReadRun) {
	summary := run.Summary()
	if summary == "" {
		return
	}
	grid := m.contentGrid()
	glyphs := glyphSet(m.glyphProfile)
	content := m.styles.Dimmed.Render(summary)
	if target := sanitizeTerminalSingleLine(run.Target); target != "" {
		// The hint answers "what is it working through", which a count alone
		// cannot. It is the LAST member's target because that is the one a
		// reader is most likely to still care about.
		content += m.styles.Dimmed.Render(glyphSeparator(m.glyphProfile) + target)
	}
	// A settled run wears the same dimmed treatment a collapsed success receipt
	// does, so the summary reads as history rather than as a new kind of event.
	line := m.styles.Dimmed.Render(glyphs.Success) + " " + content
	b.WriteString(grid.Line(
		m.styles.Dimmed.Render(glyphs.Vertical),
		truncateDisplayWithGlyphProfile(line, max(1, grid.ContentWidth()), m.glyphProfile),
	))
}

// toggleReadRunAtTool opens or re-collapses the run owned by a tool index, and
// reports whether it found one.
//
// The pointer region for a run summary is the ordinary tool header region — a
// summary is one line, so the existing code already registers it — which means
// a click arrives here carrying the first member's tool index rather than an
// entry index. Resolving it costs a scan, and a click is rare enough that the
// alternative (a second region type threaded through the paint cache) would be
// machinery bought for nothing.
func (m *Model) toggleReadRunAtTool(toolIndex int) bool {
	for index, entry := range m.entries {
		if entry.Kind != "tool_group" || entry.ToolIndex != toolIndex {
			continue
		}
		if _, expanded := m.expandedReadRuns[entry.BlockID]; expanded {
			delete(m.expandedReadRuns, entry.BlockID)
			m.afterReadRunToggle()
			return true
		}
		if _, collapsed := m.collapsedReadRunAt(index); !collapsed {
			return false
		}
		if m.expandedReadRuns == nil {
			m.expandedReadRuns = make(map[BlockID]struct{})
		}
		m.expandedReadRuns[entry.BlockID] = struct{}{}
		m.afterReadRunToggle()
		return true
	}
	return false
}

// afterReadRunToggle repaints around the change while holding the reader's
// place. Expanding a run makes the transcript taller by every receipt it was
// hiding, so without the anchor the content under the pointer jumps away from
// the line that was just clicked.
func (m *Model) afterReadRunToggle() {
	anchor := m.captureTranscriptReflowAnchor()
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.restoreTranscriptReflowAnchor(anchor)
}
