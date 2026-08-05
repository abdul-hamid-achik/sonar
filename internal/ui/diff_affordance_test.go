package ui

import (
	"testing"
)

// A collapsed write shows an inline diff preview cut to a few rows. Nothing
// told the reader the rest existed: the "alt+d open diff viewer" hint rendered
// only once the card was already expanded, so you had to know the card
// expanded in order to learn it expanded further.
//
// Every harness that collapses tool output by default pairs the collapsed
// state with a visible way out of it — Docker's agent TUI shows a short
// preview plus the expand affordance, pi-tool-display shows N preview lines
// with an explicit expanded ceiling. An affordance nobody can see is not one.
func TestTruncatedDiffAdvertisesTheViewerWhileCollapsed(t *testing.T) {
	m := newTestModel(t)
	rows := m.inlineDiffPreviewRows()
	if rows <= 0 {
		t.Fatalf("inline preview rows = %d", rows)
	}

	long := make([]DiffLine, 0, rows*3)
	for i := 0; i < rows*3; i++ {
		long = append(long, DiffLine{Kind: DiffAdded, Content: "added line"})
	}
	short := []DiffLine{{Kind: DiffAdded, Content: "one added line"}}

	for _, test := range []struct {
		name     string
		lines    []DiffLine
		pending  bool
		expanded bool
		wantHint bool
	}{
		{name: "collapsed and truncated", lines: long, wantHint: true},
		{name: "collapsed and fully shown", lines: short, wantHint: false},
		{name: "expanded always hints", lines: short, expanded: true, wantHint: true},
		// A diff still building has nothing to open yet; promising the viewer
		// would be an affordance that fails when used.
		{name: "still loading stays quiet", lines: long, pending: true, wantHint: false},
		{name: "no diff at all", lines: nil, wantHint: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			preview := ToolPreview{
				DiffLines:   test.lines,
				DiffPending: test.pending,
				Expanded:    test.expanded,
			}
			got := shouldShowToolViewerHint(m, preview)
			if got != test.wantHint {
				t.Errorf("hint shown = %v, want %v", got, test.wantHint)
			}
		})
	}
}

// The threshold is exactly "more lines than the preview shows". A diff that
// fits needs no invitation to see the rest, because there is no rest.
func TestHintThresholdIsTheVisibleRowCount(t *testing.T) {
	m := newTestModel(t)
	rows := m.inlineDiffPreviewRows()

	exact := make([]DiffLine, rows)
	for i := range exact {
		exact[i] = DiffLine{Kind: DiffAdded, Content: "line"}
	}
	if shouldShowToolViewerHint(m, ToolPreview{DiffLines: exact}) {
		t.Error("a diff that exactly fits advertised a viewer for nothing")
	}

	oneMore := append(append([]DiffLine(nil), exact...), DiffLine{Kind: DiffAdded, Content: "line"})
	if !shouldShowToolViewerHint(m, ToolPreview{DiffLines: oneMore}) {
		t.Error("a diff one line past the preview did not advertise the viewer")
	}
}
