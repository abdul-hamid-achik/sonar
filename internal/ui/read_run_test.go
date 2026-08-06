package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// readRunModel builds a transcript from a compact spec so a test reads as the
// shape it is asserting: "rrr" is three reads, "rsr" a read, a search, a read,
// "b" a bash receipt, "E" a failed read.
func readRunModel(t *testing.T, spec string) *Model {
	t.Helper()
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		BlockID: "blk-user", Revision: 1, Lifecycle: BlockSettled,
		Kind: "user", Content: "go",
	}}
	for index, kind := range spec {
		tool := ToolEntry{
			ID: fmt.Sprintf("t%d", index), Status: ToolStatusDone,
			Collapsed: true, Summary: fmt.Sprintf("target%d.go", index),
		}
		switch kind {
		case 'r':
			tool.Name = "read"
		case 's':
			tool.Name = "grep"
		case 'b':
			tool.Name = "bash"
		case 'E':
			tool.Name, tool.IsError, tool.Status = "read", true, ToolStatusError
		case 'R':
			tool.Name, tool.Status = "read", ToolStatusRunning
		case 'X':
			tool.Name, tool.Collapsed = "read", false
		}
		m.toolEntries = append(m.toolEntries, tool)
		m.entries = append(m.entries, ChatEntry{
			BlockID:  BlockID(fmt.Sprintf("blk-%d", index)),
			Revision: 1, Lifecycle: BlockSettled,
			Kind: "tool_group", ToolIndex: len(m.toolEntries) - 1,
		})
	}
	return m
}

func TestCollapsedReadRunGrouping(t *testing.T) {
	cases := []struct {
		spec    string
		starts  map[int]int // entry index -> expected run length
		follows []int
	}{
		// A run has to earn its summary: two receipts are two facts.
		{spec: "rr", starts: map[int]int{}, follows: nil},
		{spec: "rrr", starts: map[int]int{1: 3}, follows: []int{2, 3}},
		{spec: "rsrsr", starts: map[int]int{1: 5}, follows: []int{2, 3, 4, 5}},
		// A non-member splits one run into two, and each half is judged alone.
		{spec: "rrrbrrr", starts: map[int]int{1: 3, 5: 3}, follows: []int{2, 3, 6, 7}},
		{spec: "rrbrr", starts: map[int]int{}, follows: nil},
		// A failure keeps its own line and breaks the run around it.
		{spec: "rrrErrr", starts: map[int]int{1: 3, 5: 3}, follows: []int{2, 3, 6, 7}},
		// So does a receipt still running, and one the reader expanded.
		{spec: "rrrRrrr", starts: map[int]int{1: 3, 5: 3}, follows: []int{2, 3, 6, 7}},
		{spec: "rrrXrrr", starts: map[int]int{1: 3, 5: 3}, follows: []int{2, 3, 6, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			m := readRunModel(t, tc.spec)
			for index := range m.entries {
				run, ok := m.collapsedReadRunAt(index)
				wantLength, wantStart := tc.starts[index]
				if ok != wantStart {
					t.Fatalf("entry %d: run start = %v, want %v", index, ok, wantStart)
				}
				if ok && run.Length != wantLength {
					t.Fatalf("entry %d: run length = %d, want %d", index, run.Length, wantLength)
				}
			}
			followers := map[int]bool{}
			for _, index := range tc.follows {
				followers[index] = true
			}
			for index := range m.entries {
				if got := m.collapsedReadRunFollower(index); got != followers[index] {
					t.Fatalf("entry %d: follower = %v, want %v", index, got, followers[index])
				}
			}
		})
	}
}

// Exactly one entry in a run renders, and it is the start. Both predicates
// disagreeing would either duplicate a run or erase it entirely.
func TestCollapsedReadRunRendersExactlyOneEntry(t *testing.T) {
	for _, spec := range []string{"rrr", "rsrsr", "rrrbrrr", "rrrErrr", "rr", "b", ""} {
		t.Run(spec, func(t *testing.T) {
			m := readRunModel(t, spec)
			for index := range m.entries {
				_, starts := m.collapsedReadRunAt(index)
				if starts && m.collapsedReadRunFollower(index) {
					t.Fatalf("entry %d both starts a run and follows one", index)
				}
			}
		})
	}
}

func TestCollapsedReadRunSummaryReadsAsProse(t *testing.T) {
	for _, tc := range []struct {
		run  collapsedReadRun
		want string
	}{
		{run: collapsedReadRun{Reads: 5}, want: "Read 5 files"},
		{run: collapsedReadRun{Reads: 1}, want: "Read 1 file"},
		{run: collapsedReadRun{Searches: 3}, want: "Searched 3 times"},
		{run: collapsedReadRun{Searches: 1}, want: "Searched 1 time"},
		{run: collapsedReadRun{Reads: 4, Searches: 2}, want: "Read 4 files, 2 searches"},
		{run: collapsedReadRun{Reads: 2, Searches: 1}, want: "Read 2 files, 1 search"},
		{run: collapsedReadRun{}, want: ""},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.run.Summary(); got != tc.want {
				t.Fatalf("Summary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The hint names the last member, so a long run says what it is working
// through rather than only how much of it there was.
func TestCollapsedReadRunKeepsTheLastTarget(t *testing.T) {
	m := readRunModel(t, "rrr")
	run, ok := m.collapsedReadRunAt(1)
	if !ok {
		t.Fatal("three consecutive reads did not form a run")
	}
	if run.Target != "target2.go" {
		t.Fatalf("run target = %q, want the last member's", run.Target)
	}
}

// TestCollapsedReadRunSummaryTracksAGrowingRun is the memo hazard. The summary
// is rendered by the run's FIRST entry, but it describes every member — so a
// memo key derived from that one entry's tool goes stale the moment a fourth
// read lands, and the transcript keeps claiming three.
func TestCollapsedReadRunSummaryTracksAGrowingRun(t *testing.T) {
	m := readRunModel(t, "rrr")
	first := ansi.Strip(m.renderEntries())
	if !strings.Contains(first, "Read 3 files") {
		t.Fatalf("three reads did not collapse:\n%s", first)
	}

	m.toolEntries = append(m.toolEntries, ToolEntry{
		ID: "t3", Name: "read", Status: ToolStatusDone, Collapsed: true, Summary: "target3.go",
	})
	m.entries = append(m.entries, ChatEntry{
		BlockID: "blk-3", Revision: 1, Lifecycle: BlockSettled,
		Kind: "tool_group", ToolIndex: len(m.toolEntries) - 1,
	})
	m.invalidateEntryCache()

	grown := ansi.Strip(m.renderEntries())
	if strings.Contains(grown, "Read 3 files") || !strings.Contains(grown, "Read 4 files") {
		t.Fatalf("the summary did not follow its own run:\n%s", grown)
	}
	if !strings.Contains(grown, "target3.go") {
		t.Fatalf("the hint did not follow the run's last member:\n%s", grown)
	}
}

// A run replaces its members entirely: none of their individual lines survive.
func TestCollapsedReadRunReplacesItsMembers(t *testing.T) {
	m := readRunModel(t, "rrr")
	plain := ansi.Strip(m.renderEntries())
	if strings.Count(plain, "Read 3 files") != 1 {
		t.Fatalf("expected exactly one summary line:\n%s", plain)
	}
	for _, target := range []string{"target0.go", "target1.go"} {
		if strings.Contains(plain, target) {
			t.Fatalf("a collapsed member still rendered its own line (%s):\n%s", target, plain)
		}
	}
}

// A run that does not reach the minimum renders every receipt as it always did.
func TestShortReadRunsAreUnchanged(t *testing.T) {
	m := readRunModel(t, "rr")
	plain := ansi.Strip(m.renderEntries())
	if strings.Contains(plain, "Read 2 files") {
		t.Fatalf("two receipts were collapsed:\n%s", plain)
	}
	for _, target := range []string{"target0.go", "target1.go"} {
		if !strings.Contains(plain, target) {
			t.Fatalf("an uncollapsed receipt lost its line (%s):\n%s", target, plain)
		}
	}
}

// TestCollapsedReadRunCanBeOpenedAndClosed is the affordance half of the
// feature. Summarizing receipts away is only acceptable while there is a way
// back to them — this repository already refuses hidden content with no stated
// route, which is why the hidden-lines cue is clickable.
func TestCollapsedReadRunCanBeOpenedAndClosed(t *testing.T) {
	m := readRunModel(t, "rrr")
	collapsed := ansi.Strip(m.renderEntries())
	if !strings.Contains(collapsed, "Read 3 files") {
		t.Fatalf("run did not collapse:\n%s", collapsed)
	}
	if len(m.toolHitRegions) != 1 {
		t.Fatalf("a run summary has %d pointer regions, want exactly one: %#v",
			len(m.toolHitRegions), m.toolHitRegions)
	}

	region := m.toolHitRegions[0]
	m.handleMouseClick(region.StartCol, region.Row-m.transcriptYOffset())
	opened := ansi.Strip(m.renderEntries())
	if strings.Contains(opened, "Read 3 files") {
		t.Fatalf("clicking the summary did not open the run:\n%s", opened)
	}
	for _, target := range []string{"target0.go", "target1.go", "target2.go"} {
		if !strings.Contains(opened, target) {
			t.Fatalf("an opened run did not restore %s:\n%s", target, opened)
		}
	}

	// Opening is reversible, and the second click has to find the run again
	// through whichever receipt now owns the first region.
	m.handleMouseClick(m.toolHitRegions[0].StartCol, m.toolHitRegions[0].Row-m.transcriptYOffset())
	reclosed := ansi.Strip(m.renderEntries())
	if !strings.Contains(reclosed, "Read 3 files") {
		t.Fatalf("the run did not re-collapse:\n%s", reclosed)
	}
}

// An opened run must not silently re-collapse when it grows. The reader asked
// to see these receipts; a fourth read arriving is not them changing their mind.
func TestOpenedReadRunStaysOpenWhenItGrows(t *testing.T) {
	m := readRunModel(t, "rrr")
	m.renderEntries()
	region := m.toolHitRegions[0]
	m.handleMouseClick(region.StartCol, region.Row-m.transcriptYOffset())

	m.toolEntries = append(m.toolEntries, ToolEntry{
		ID: "t3", Name: "read", Status: ToolStatusDone, Collapsed: true, Summary: "target3.go",
	})
	m.entries = append(m.entries, ChatEntry{
		BlockID: "blk-3", Revision: 1, Lifecycle: BlockSettled,
		Kind: "tool_group", ToolIndex: len(m.toolEntries) - 1,
	})
	m.invalidateEntryCache()

	grown := ansi.Strip(m.renderEntries())
	if strings.Contains(grown, "Read 4 files") {
		t.Fatalf("an opened run re-collapsed itself when it grew:\n%s", grown)
	}
	if !strings.Contains(grown, "target3.go") {
		t.Fatalf("the new receipt did not appear in the opened run:\n%s", grown)
	}
}
