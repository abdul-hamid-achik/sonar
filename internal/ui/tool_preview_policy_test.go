package ui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestToolRenderAdapterProjectsExplicitPreviewModes(t *testing.T) {
	tests := []struct {
		name       string
		projection ecosystem.ToolProjection
		want       ToolPreviewMode
	}{
		{name: "read", want: ToolPreviewRead},
		{name: "bash", want: ToolPreviewExec},
		{name: "grep", want: ToolPreviewSearch},
		{name: "write", want: ToolPreviewEdit},
		{name: "diff", want: ToolPreviewEdit},
		{name: "readiness_probe", want: ToolPreviewGeneric},
		{
			name: "files__read_file",
			projection: ecosystem.ProjectToolCall(
				"files__read_file",
				nil,
			),
			want: ToolPreviewRead,
		},
		{
			name:       "files__readiness_probe",
			projection: ecosystem.ProjectToolCall("files__readiness_probe", nil),
			want:       ToolPreviewGeneric,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := test.projection
			if projection == (ecosystem.ToolProjection{}) {
				projection = ecosystem.ProjectToolResult(
					ecosystem.ProjectToolCall(test.name, nil),
					"ok",
					false,
				)
			} else {
				projection = ecosystem.ProjectToolResult(projection, "ok", false)
			}
			model, err := ToolRenderModelFromEntry(
				ChatEntry{
					BlockID:  BlockID(fmt.Sprintf("block-preview-%d", index)),
					Revision: 1,
				},
				ToolEntry{
					ID:         fmt.Sprintf("call-preview-%d", index),
					Name:       test.name,
					Status:     ToolStatusDone,
					Projection: projection,
				},
			)
			if err != nil {
				t.Fatalf("project render model: %v", err)
			}
			if model.Preview.Mode != test.want {
				t.Fatalf("preview mode = %d, want %d", model.Preview.Mode, test.want)
			}
		})
	}
}

func TestToolPreviewPoliciesSelectExpectedSourceRows(t *testing.T) {
	source := numberedPreviewSource(20)
	tests := []struct {
		name         string
		mode         ToolPreviewMode
		wantIndexes  []int
		wantOmission int
	}{
		{
			name:         "read head and tail",
			mode:         ToolPreviewRead,
			wantIndexes:  []int{0, 1, 2, 3, 4, 17, 18, 19},
			wantOmission: 5,
		},
		{
			name:         "exec head and tail",
			mode:         ToolPreviewExec,
			wantIndexes:  []int{0, 1, 17, 18, 19},
			wantOmission: 2,
		},
		{
			name:         "search bounded prefix",
			mode:         ToolPreviewSearch,
			wantIndexes:  []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			wantOmission: 12,
		},
		{
			name:         "edit bounded prefix",
			mode:         ToolPreviewEdit,
			wantIndexes:  []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			wantOmission: 12,
		},
		{
			name:         "generic bounded prefix",
			mode:         ToolPreviewGeneric,
			wantIndexes:  []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			wantOmission: 12,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			card := NewToolCard("tool", ToolCardGeneric, true, defaultThemeID)
			card.PreviewMode = test.mode
			plan := card.resultPreviewPlan(source, 80)
			if !reflect.DeepEqual(plan.sourceIndexes, test.wantIndexes) {
				t.Fatalf("source indexes = %v, want %v", plan.sourceIndexes, test.wantIndexes)
			}
			if plan.omissionIndex != test.wantOmission {
				t.Fatalf("omission index = %d, want %d", plan.omissionIndex, test.wantOmission)
			}
			if plan.hiddenRows != 20-len(test.wantIndexes) {
				t.Fatalf("hidden rows = %d, want %d", plan.hiddenRows, 20-len(test.wantIndexes))
			}
		})
	}
}

func TestReadAndExecPreviewNeverPresentRetainedPrefixAsSourceTail(t *testing.T) {
	source := numberedPreviewSource(600)
	store := NewOutputDetailStore()
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatal(err)
	}
	result := boundedToolCardResult(source)

	for _, test := range []struct {
		name string
		mode ToolPreviewMode
		head int
	}{
		{name: "read", mode: ToolPreviewRead, head: readPreviewHeadRows},
		{name: "exec", mode: ToolPreviewExec, head: execPreviewHeadRows},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := NewToolCard(test.name, ToolCardGeneric, true, defaultThemeID)
			card.PreviewMode = test.mode
			card.OutputDigest = receipt.Digest
			plan := card.resultPreviewPlan(result, 80)
			want := make([]int, test.head)
			for index := range want {
				want[index] = index
			}
			if !reflect.DeepEqual(plan.sourceIndexes, want) {
				t.Fatalf("incomplete source indexes = %v, want prefix only %v", plan.sourceIndexes, want)
			}
			if plan.omissionIndex != len(want) {
				t.Fatalf("incomplete-source omission index = %d, want %d", plan.omissionIndex, len(want))
			}
		})
	}
}

func TestHeadTailOmissionIsRenderedBetweenHeadAndTail(t *testing.T) {
	card := NewToolCard("read", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Lifecycle = ToolLifecycleSucceeded
	card.Expanded = true
	card.PreviewMode = ToolPreviewRead
	card.Result = numberedPreviewSource(12)

	view := ansi.Strip(card.View(80))
	head := strings.Index(view, "row-05")
	omission := strings.Index(view, "4 more lines")
	tail := strings.Index(view, "row-10")
	if head < 0 || omission < 0 || tail < 0 || head >= omission || omission >= tail {
		t.Fatalf("head/tail omission order is not honest:\n%s", view)
	}
	for _, hidden := range []string{"row-06", "row-07", "row-08", "row-09"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("center row %q escaped the preview budget:\n%s", hidden, view)
		}
	}
}

func TestReadPreviewRendersAbsoluteSourceRowsInsideCellBudget(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	sourceLines := make([]string, 20)
	for index := range sourceLines {
		sourceLines[index] = fmt.Sprintf("var row%02d = %d", index+1, index+1)
	}
	card := NewToolCard("read", ToolCardFile, true, defaultThemeID)
	card.PreviewMode = ToolPreviewRead
	card.ResultLanguage = "go"
	rendered := card.renderSemanticResultLines(strings.Join(sourceLines, "\n"), 24)
	plain := ansi.Strip(strings.Join(rendered, "\n"))

	for _, want := range []string{
		" 1 │ var row01",
		" 2 │ var row02",
		" 5 │ var row05",
		"18 │ var row18",
		"19 │ var row19",
		"20 │ var row20",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("numbered read preview omitted %q:\n%s", want, plain)
		}
	}
	for _, hidden := range []string{" 6 │", "17 │"} {
		if strings.Contains(plain, hidden) {
			t.Fatalf("numbered read preview exposed hidden row %q:\n%s", hidden, plain)
		}
	}
	for index, line := range rendered {
		if width := lipgloss.Width(line); width > 24 {
			t.Fatalf("rendered row %d width = %d, want <= 24: %q", index, width, ansi.Strip(line))
		}
	}

	const usefulNarrowRow = "func answer() int { return 42 }"
	narrowPlan := card.resultPreviewPlan(usefulNarrowRow, lipgloss.Width(usefulNarrowRow))
	if narrowPlan.lineNumCells != 0 || len(narrowPlan.lines) != 1 ||
		narrowPlan.lines[0] != usefulNarrowRow {
		t.Fatalf("narrow read gutter displaced useful content: %#v", narrowPlan)
	}

	ascii := NewToolCard("read", ToolCardFile, true, defaultThemeID, GlyphASCII)
	ascii.PreviewMode = ToolPreviewRead
	asciiLines := ascii.renderSemanticResultLines("one\ntwo", 12)
	asciiPlain := ansi.Strip(strings.Join(asciiLines, "\n"))
	if !strings.Contains(asciiPlain, "1 | one") || strings.Contains(asciiPlain, "│") {
		t.Fatalf("ASCII read gutter is not profile-safe:\n%s", asciiPlain)
	}
}

func TestSearchPreviewGroupsAdjacentMatchesWithoutHidingFileAfterGap(t *testing.T) {
	card := NewToolCard("grep", ToolCardSearch, true, defaultThemeID)
	card.PreviewMode = ToolPreviewSearch
	lines := []string{
		"internal/ui/model.go:42:7:first match",
		"internal/ui/model.go:43:7:second match",
		"internal/ui/view.go:10:third match",
	}
	rendered := card.renderSearchPreviewLines(lines, 80, len(lines))
	plain := ansi.Strip(strings.Join(rendered, "\n"))
	if count := strings.Count(plain, "internal/ui/model.go"); count != 1 {
		t.Fatalf("adjacent search path rendered %d times, want one grouped heading:\n%s", count, plain)
	}
	for _, want := range []string{"42:7:first match", "43:7:second match", "internal/ui/view.go:10:third match"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("grouped search preview omitted %q:\n%s", want, plain)
		}
	}

	rendered = card.renderSearchPreviewLines(lines[:2], 80, 1)
	plain = ansi.Strip(strings.Join(rendered, "\n"))
	if count := strings.Count(plain, "internal/ui/model.go"); count != 2 {
		t.Fatalf("search path crossed an omission gap, count=%d:\n%s", count, plain)
	}
}

func TestGenericPreviewStylesBoundedKeyValueRowsWithoutParsingJSON(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("remote_status", ToolCardGeneric, true, defaultThemeID)
	card.PreviewMode = ToolPreviewGeneric
	keyValue := card.renderGenericResultLine("state: ready", 40)
	if plain := ansi.Strip(keyValue); plain != "state: ready" {
		t.Fatalf("generic key/value text changed: %q", plain)
	}
	if keyValue == card.Styles.Result.Render("state: ready") {
		t.Fatal("generic key/value row retained one undifferentiated style")
	}

	jsonLine := `{"state":"ready"}`
	if got := card.renderGenericResultLine(jsonLine, 40); got != card.Styles.Result.Render(jsonLine) {
		t.Fatalf("generic renderer guessed structure inside JSON: %q", ansi.Strip(got))
	}
}

func numberedPreviewSource(rows int) string {
	lines := make([]string, rows)
	for index := range rows {
		lines[index] = fmt.Sprintf("row-%02d", index+1)
	}
	return strings.Join(lines, "\n")
}
