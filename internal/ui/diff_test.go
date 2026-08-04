package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestComputeDiff_Identical(t *testing.T) {
	result := computeDiff("hello\nworld\n", "hello\nworld\n")
	if result != nil {
		t.Errorf("identical texts should return nil, got %d lines", len(result))
	}
}

func TestComputeDiffPreservesFinalNewlineOnlyChanges(t *testing.T) {
	tests := []struct {
		name             string
		before           string
		after            string
		markerPrecededBy DiffLineKind
	}{
		{name: "remove final newline", before: "alpha\n", after: "alpha", markerPrecededBy: DiffAdded},
		{name: "add final newline", before: "alpha", after: "alpha\n", markerPrecededBy: DiffRemoved},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines := computeDiff(test.before, test.after)
			if len(lines) == 0 {
				t.Fatal("final-newline-only edit produced no patch")
			}
			markerIndex := -1
			for index, line := range lines {
				if line.Kind == DiffNoNewline {
					if markerIndex >= 0 {
						t.Fatalf("patch contains multiple no-newline markers: %#v", lines)
					}
					markerIndex = index
					if line.Content != diffNoNewlineContent {
						t.Fatalf("no-newline content = %q", line.Content)
					}
				}
			}
			if markerIndex <= 0 || lines[markerIndex-1].Kind != test.markerPrecededBy {
				t.Fatalf("no-newline marker is not immediately after affected side: %#v", lines)
			}

			plain := ansi.Strip(renderUnifiedDiffAtWidth("alpha.txt", lines, NewStyles(true, defaultThemeID), 0, 80))
			if strings.Count(plain, diffNoNewlineContent) != 1 {
				t.Fatalf("rendered patch does not contain the canonical marker once:\n%s", plain)
			}

			persisted := persistToolEntries([]ToolEntry{{
				ID: "write-newline", Name: "write_file", Status: ToolStatusDone, DiffLines: lines,
			}})
			raw, err := json.Marshal(persisted)
			if err != nil {
				t.Fatal(err)
			}
			var decoded []persistedToolEntry
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			restored := restoreToolEntries(decoded)
			if len(restored) != 1 || len(restored[0].DiffLines) != len(lines) {
				t.Fatalf("session round trip changed patch: %#v", restored)
			}
			if got := restored[0].DiffLines[markerIndex]; got.Kind != DiffNoNewline || got.Content != diffNoNewlineContent {
				t.Fatalf("restored marker = %#v", got)
			}
		})
	}
}

func TestComputeDiffOmitsBinarySnapshotsWithoutRawBytes(t *testing.T) {
	const secret = "RAW_BINARY_SECRET_DO_NOT_PERSIST"
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{name: "invalid UTF-8 before", before: string([]byte{0xff}) + secret, after: "text"},
		{name: "invalid UTF-8 after", before: "text", after: secret + string([]byte{0xfe})},
		{name: "NUL before", before: secret + "\x00tail", after: "text"},
		{name: "NUL after", before: "text", after: "head\x00" + secret},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines := computeDiff(test.before, test.after)
			if len(lines) != 2 {
				t.Fatalf("binary patch lines = %d, want 2: %#v", len(lines), lines)
			}
			for _, line := range lines {
				if line.Kind != DiffOmitted || !strings.Contains(line.Content, "binary diff omitted") {
					t.Fatalf("binary patch exposed a non-omission line: %#v", line)
				}
			}
			combined := lines[0].Content + lines[1].Content
			for _, want := range []string{
				fmt.Sprintf("%d bytes before", len(test.before)),
				fmt.Sprintf("%d bytes after", len(test.after)),
			} {
				if !strings.Contains(combined, want) {
					t.Fatalf("binary omission metadata = %q, missing %q", combined, want)
				}
			}
			persisted := persistToolEntries([]ToolEntry{{
				ID: "write-binary", Name: "write_file", Status: ToolStatusDone, DiffLines: lines,
			}})
			raw, err := json.Marshal(persisted)
			if err != nil {
				t.Fatal(err)
			}
			plain := ansi.Strip(renderUnifiedDiffAtWidth("binary.dat", lines, NewStyles(true, defaultThemeID), 0, 80))
			if strings.Contains(string(raw), secret) || strings.Contains(plain, secret) || strings.ContainsRune(string(raw), '\x00') {
				t.Fatalf("binary snapshot bytes leaked: json=%s render=%s", raw, plain)
			}
		})
	}
}

func TestComputeDiff_EmptyBefore(t *testing.T) {
	result := computeDiff("", "line1\nline2\n")
	if len(result) == 0 {
		t.Fatal("expected diff lines for new file")
	}
	var added int
	for _, line := range result {
		switch line.Kind {
		case DiffAdded:
			added++
		case DiffHunkHeader:
			if line.Hunk == nil || line.Hunk.OldStart != 0 || line.Hunk.OldCount != 0 {
				t.Fatalf("new-file hunk = %#v", line.Hunk)
			}
		default:
			t.Errorf("new file body should have only added lines, got kind %d", line.Kind)
		}
	}
	if added != 2 {
		t.Fatalf("added lines = %d, want 2", added)
	}
}

func TestComputeDiff_EmptyAfter(t *testing.T) {
	result := computeDiff("line1\nline2\n", "")
	if len(result) == 0 {
		t.Fatal("expected diff lines for deleted file")
	}
	var removed int
	for _, line := range result {
		switch line.Kind {
		case DiffRemoved:
			removed++
		case DiffHunkHeader:
			if line.Hunk == nil || line.Hunk.NewStart != 0 || line.Hunk.NewCount != 0 {
				t.Fatalf("deleted-file hunk = %#v", line.Hunk)
			}
		default:
			t.Errorf("deleted file body should have only removed lines, got kind %d", line.Kind)
		}
	}
	if removed != 2 {
		t.Fatalf("removed lines = %d, want 2", removed)
	}
}

func TestComputeDiffBoundsNewlineOnlyInput(t *testing.T) {
	got := computeDiff(strings.Repeat("\n", 60_000), "")
	if len(got) != 2 || !strings.Contains(got[0].Content, "large diff omitted") {
		t.Fatalf("large newline-only diff was expanded: len=%d %#v", len(got), got)
	}
}

func TestComputeDiff_Modification(t *testing.T) {
	before := "line1\nline2\nline3\n"
	after := "line1\nline2-modified\nline3\n"
	result := computeDiff(before, after)

	if len(result) == 0 {
		t.Fatal("expected diff lines for modification")
	}

	// Should contain removed and added lines.
	var hasAdded, hasRemoved, hasContext bool
	for _, line := range result {
		switch line.Kind {
		case DiffAdded:
			hasAdded = true
			if line.Content != "line2-modified" {
				t.Errorf("added line should be 'line2-modified', got %q", line.Content)
			}
		case DiffRemoved:
			hasRemoved = true
			if line.Content != "line2" {
				t.Errorf("removed line should be 'line2', got %q", line.Content)
			}
		case DiffContext:
			hasContext = true
		}
	}
	if !hasAdded || !hasRemoved {
		t.Error("modification should produce both added and removed lines")
	}
	if !hasContext {
		t.Error("modification should have context lines")
	}
}

func TestComputeDiff_ContextLimiting(t *testing.T) {
	// Create a file with many lines and a change in the middle.
	var before, after strings.Builder
	for i := 0; i < 50; i++ {
		before.WriteString("line" + strings.Repeat("x", i) + "\n")
		after.WriteString("line" + strings.Repeat("x", i) + "\n")
	}
	// Change line 25
	beforeStr := strings.Replace(before.String(), "line"+strings.Repeat("x", 25), "CHANGED", 1)
	afterStr := strings.Replace(after.String(), "line"+strings.Repeat("x", 25), "MODIFIED", 1)

	result := computeDiff(beforeStr, afterStr)
	if len(result) == 0 {
		t.Fatal("expected diff lines")
	}

	// Should not include all 50 lines — context filtering should limit output.
	if len(result) > 20 {
		t.Errorf("context limiting should reduce output, got %d lines", len(result))
	}
}

func TestComputeDiffBuildsTypedMultipleHunksAndLineCoordinates(t *testing.T) {
	beforeLines := make([]string, 24)
	afterLines := make([]string, 24)
	for i := range beforeLines {
		beforeLines[i] = fmt.Sprintf("line-%02d", i+1)
		afterLines[i] = beforeLines[i]
	}
	afterLines[3] = "changed-04"
	afterLines[19] = "changed-20"

	lines := computeDiff(strings.Join(beforeLines, "\n")+"\n", strings.Join(afterLines, "\n")+"\n")
	var headers, ellipses int
	for _, line := range lines {
		switch line.Kind {
		case DiffHunkHeader:
			headers++
			if line.Hunk == nil || line.Content != formatDiffHunk(*line.Hunk) {
				t.Fatalf("untyped hunk header: %#v", line)
			}
		case DiffEllipsis:
			ellipses++
		case DiffRemoved:
			if line.Content == "line-04" && (line.OldLine != 4 || line.NewLine != 0) {
				t.Fatalf("removed line coordinates = old %d new %d", line.OldLine, line.NewLine)
			}
		case DiffAdded:
			if line.Content == "changed-04" && (line.OldLine != 0 || line.NewLine != 4) {
				t.Fatalf("added line coordinates = old %d new %d", line.OldLine, line.NewLine)
			}
		}
	}
	if headers != 2 || ellipses == 0 {
		t.Fatalf("patch structure = %d headers, %d ellipses; want 2 and at least 1", headers, ellipses)
	}
	if added, removed, known := diffTotals(lines); !known || added != 2 || removed != 2 {
		t.Fatalf("patch totals = +%d -%d known=%v", added, removed, known)
	}
}

func TestRenderUnifiedDiffFitsNarrowAndUsesWideOldNewGutters(t *testing.T) {
	before := "line-01\nline-02\nline-03\nline-04\nline-05\n"
	after := "line-01\nline-02\nline-03\nchanged-04\nline-05\n"
	lines := computeDiff(before, after)
	styles := NewStyles(true, defaultThemeID)

	for _, width := range []int{24, 80} {
		rendered := renderUnifiedDiffAtWidth("internal/ui/example.go", lines, styles, 0, width)
		assertRenderedLinesFit(t, rendered, width)
		plain := ansi.Strip(rendered)
		for _, want := range []string{"+1 -1", "@@ -", "│ -", "│ +"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%d-column patch missing %q:\n%s", width, want, plain)
			}
		}
	}

	wide := ansi.Strip(renderUnifiedDiffAtWidth("internal/ui/example.go", lines, styles, 0, 80))
	firstLine := strings.SplitN(wide, "\n", 2)[0]
	if !strings.Contains(firstLine, "internal/ui/example.go") || len([]rune(firstLine)) != 80 {
		t.Fatalf("wide file header is not full width: %q", firstLine)
	}
	for _, renderedLine := range strings.Split(wide, "\n") {
		fields := strings.Fields(renderedLine)
		if strings.Contains(renderedLine, "line-01") && (len(fields) < 4 || fields[0] != "1" || fields[1] != "1" || fields[2] != "│") {
			t.Fatalf("context gutters = %q", renderedLine)
		}
		if strings.Contains(renderedLine, "line-04") && (len(fields) < 4 || fields[0] != "4" || fields[1] != "│" || fields[2] != "-") {
			t.Fatalf("removed gutter = %q", renderedLine)
		}
		if strings.Contains(renderedLine, "changed-04") && (len(fields) < 4 || fields[0] != "4" || fields[1] != "│" || fields[2] != "+") {
			t.Fatalf("added gutter = %q", renderedLine)
		}
	}
}

func TestResolveDiffGutterNumbersCountsForwardFromHunkHeaders(t *testing.T) {
	// No typed per-line coordinates, as in sessions persisted before line
	// coordinates existed: numbering must come from the hunk headers alone.
	lines := []DiffLine{
		{Kind: DiffHunkHeader, Content: "@@ -3,3 +3,4 @@"},
		{Kind: DiffContext, Content: "ctx-a"},
		{Kind: DiffRemoved, Content: "gone-1"},
		{Kind: DiffAdded, Content: "new-1"},
		{Kind: DiffAdded, Content: "new-2"},
		{Kind: DiffContext, Content: "ctx-b"},
		{Kind: DiffEllipsis, Content: "… unchanged lines"},
		{Kind: DiffHunkHeader, Content: "@@ -40,3 +41,2 @@"},
		{Kind: DiffContext, Content: "ctx-c"},
		{Kind: DiffRemoved, Content: "gone-2"},
		{Kind: DiffContext, Content: "ctx-d"},
	}

	numbers, ok := resolveDiffGutterNumbers(lines)
	if !ok {
		t.Fatal("header-derived numbering failed")
	}
	want := []diffGutterNumbers{
		{}, {old: 3, new: 3}, {old: 4}, {new: 4}, {new: 5}, {old: 5, new: 6},
		{}, {}, {old: 40, new: 41}, {old: 41}, {old: 42, new: 42},
	}
	for i, expected := range want {
		if numbers[i] != expected {
			t.Fatalf("line %d numbers = %+v, want %+v", i, numbers[i], expected)
		}
	}

	plain := ansi.Strip(renderUnifiedDiffAtWidth("example.go", lines, NewStyles(true, defaultThemeID), 0, 100))
	for _, want := range []string{" 3  3 │   ctx-a", " 4    │ - gone-1", "    4 │ + new-1", "41    │ - gone-2"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered patch missing %q:\n%s", want, plain)
		}
	}
}

func TestRenderUnifiedDiffOmitsNumberColumnsUnderMinWidth(t *testing.T) {
	lines := computeDiff("line-01\nline-02\nline-03\n", "line-01\nchanged-02\nline-03\n")

	narrow := ansi.Strip(renderUnifiedDiffAtWidth("example.go", lines, NewStyles(true, defaultThemeID), 0, diffGutterNumbersMinWidth-1))
	for _, renderedLine := range strings.Split(narrow, "\n") {
		if strings.Contains(renderedLine, "│") && !strings.HasPrefix(renderedLine, "│") {
			t.Fatalf("narrow body line still carries number columns: %q", renderedLine)
		}
	}
	for _, want := range []string{"│ - line-02", "│ + changed-02"} {
		if !strings.Contains(narrow, want) {
			t.Fatalf("narrow rendering missing %q:\n%s", want, narrow)
		}
	}

	wide := ansi.Strip(renderUnifiedDiffAtWidth("example.go", lines, NewStyles(true, defaultThemeID), 0, diffGutterNumbersMinWidth))
	if !strings.Contains(wide, "2   │ - line-02") || !strings.Contains(wide, "  2 │ + changed-02") {
		t.Fatalf("min-width rendering lost number columns:\n%s", wide)
	}
}

func TestRenderUnifiedDiffFallsBackNumberlessOnMalformedOrTruncatedHunks(t *testing.T) {
	cases := map[string][]DiffLine{
		"garbled header": {
			{Kind: DiffHunkHeader, Content: "@@ garbled @@"},
			{Kind: DiffContext, Content: "ctx"},
			{Kind: DiffAdded, Content: "plus"},
		},
		"missing header": {
			{Kind: DiffAdded, Content: "plus"},
			{Kind: DiffRemoved, Content: "minus"},
		},
		"body exceeds header budget": {
			{Kind: DiffHunkHeader, Content: "@@ -1,1 +1,1 @@"},
			{Kind: DiffContext, Content: "ctx-1"},
			{Kind: DiffContext, Content: "ctx-2"},
		},
		"body after omission marker": {
			{Kind: DiffHunkHeader, Content: "@@ -1,5 +1,5 @@"},
			{Kind: DiffOmitted, Content: "[diff truncated for session persistence]"},
			{Kind: DiffContext, Content: "ctx"},
		},
	}
	for name, lines := range cases {
		if _, ok := resolveDiffGutterNumbers(lines); ok {
			t.Fatalf("%s: numbering resolved instead of failing closed", name)
		}
		plain := ansi.Strip(renderUnifiedDiffAtWidth("example.go", lines, NewStyles(true, defaultThemeID), 0, 100))
		for _, renderedLine := range strings.Split(plain, "\n") {
			if strings.Contains(renderedLine, "│") && !strings.HasPrefix(renderedLine, "│") {
				t.Fatalf("%s: body line was numbered: %q", name, renderedLine)
			}
		}
	}
}

func TestParseDiffHunkHeader(t *testing.T) {
	cases := []struct {
		content string
		want    DiffHunk
		ok      bool
	}{
		{"@@ -3,4 +5,6 @@", DiffHunk{OldStart: 3, OldCount: 4, NewStart: 5, NewCount: 6}, true},
		{"@@ -3 +5 @@", DiffHunk{OldStart: 3, OldCount: 1, NewStart: 5, NewCount: 1}, true},
		{"@@ -0,0 +1,3 @@", DiffHunk{OldStart: 0, OldCount: 0, NewStart: 1, NewCount: 3}, true},
		{"@@ -3,4 +5,6 @@ func main() {", DiffHunk{OldStart: 3, OldCount: 4, NewStart: 5, NewCount: 6}, true},
		{"@@ -a,b +c,d @@", DiffHunk{}, false},
		{"@@ -3,4 @@", DiffHunk{}, false},
		{"not a header", DiffHunk{}, false},
	}
	for _, tc := range cases {
		hunk, ok := parseDiffHunkHeader(tc.content)
		if ok != tc.ok || hunk != tc.want {
			t.Fatalf("parseDiffHunkHeader(%q) = %+v, %v; want %+v, %v", tc.content, hunk, ok, tc.want, tc.ok)
		}
	}
}

func TestFilterContext_EmptyInput(t *testing.T) {
	result := filterContext(nil, 3)
	if result != nil {
		t.Errorf("empty input should return nil, got %d lines", len(result))
	}
}

func TestFilterContext_AllChanges(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffAdded, Content: "a"},
		{Kind: DiffAdded, Content: "b"},
		{Kind: DiffRemoved, Content: "c"},
	}
	result := filterContext(lines, 3)
	if len(result) != 3 {
		t.Errorf("all changes should be kept, got %d lines", len(result))
	}
}

func TestSplitLines_Empty(t *testing.T) {
	result := splitLines("")
	if result != nil {
		t.Errorf("empty string should return nil, got %v", result)
	}
}

func TestSplitLines_TrailingNewline(t *testing.T) {
	result := splitLines("a\nb\n")
	if len(result) != 2 {
		t.Errorf("should have 2 lines, got %d: %v", len(result), result)
	}
}

func TestLcsLines_Empty(t *testing.T) {
	result := lcsLines(nil, []string{"a"})
	if result != nil {
		t.Errorf("LCS with empty input should be nil, got %v", result)
	}
}

func TestLcsLines_Basic(t *testing.T) {
	a := []string{"a", "b", "c", "d"}
	b := []string{"a", "c", "d", "e"}
	lcs := lcsLines(a, b)
	expected := []string{"a", "c", "d"}
	if len(lcs) != len(expected) {
		t.Fatalf("LCS length mismatch: got %v, want %v", lcs, expected)
	}
	for i, v := range lcs {
		if v != expected[i] {
			t.Errorf("LCS[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

func TestRenderDiff_Empty(t *testing.T) {
	s := NewStyles(true, defaultThemeID)
	result := renderDiff(nil, s, 10)
	if result != "" {
		t.Errorf("empty diff should render empty, got %q", result)
	}
}

func TestRenderDiff_MaxLines(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffAdded, Content: "a"},
		{Kind: DiffAdded, Content: "b"},
		{Kind: DiffAdded, Content: "c"},
		{Kind: DiffAdded, Content: "d"},
		{Kind: DiffAdded, Content: "e"},
	}
	s := NewStyles(true, defaultThemeID)
	result := renderDiff(lines, s, 3)
	// Should contain "more lines" indicator.
	if !strings.Contains(result, "more lines") {
		t.Error("should show 'more lines' when truncating")
	}
	bodyRows := strings.Split(strings.TrimSuffix(ansi.Strip(result), "\n"), "\n")[1:]
	if len(bodyRows) != 3 {
		t.Fatalf("bounded preview rendered %d body rows, want 3:\n%s", len(bodyRows), result)
	}
}

func TestRenderUnifiedDiffWrapsWithContinuationGutter(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 41, OldCount: 0, NewStart: 42, NewCount: 1}},
		{Kind: DiffAdded, Content: "界🙂" + strings.Repeat("abcdef", 8), NewLine: 42},
	}
	const width = 28
	rendered := ansi.Strip(renderUnifiedDiffAtWidth("wide.go", lines, NewStyles(true, defaultThemeID), 0, width))
	rows := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	foundContinuation := false
	for _, row := range rows {
		if lipgloss.Width(row) > width {
			t.Fatalf("wrapped diff row width = %d, want <= %d: %q", lipgloss.Width(row), width, row)
		}
		if strings.Contains(row, "│ ↳ ") {
			foundContinuation = true
			if strings.Contains(row, "42") {
				t.Fatalf("continuation repeated a line-number gutter: %q", row)
			}
		}
	}
	if !foundContinuation {
		t.Fatalf("long diff line did not expose a continuation gutter:\n%s", rendered)
	}
}

func TestRenderUnifiedDiffRowBudgetCountsWrappedRows(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffAdded, Content: strings.Repeat("long-value-", 12), NewLine: 1},
		{Kind: DiffAdded, Content: "second", NewLine: 2},
	}
	rendered := ansi.Strip(renderUnifiedDiffAtWidth("budget.go", lines, NewStyles(true, defaultThemeID), 4, 24))
	rows := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if bodyRows := len(rows) - 1; bodyRows != 4 {
		t.Fatalf("rendered %d bounded body rows, want 4:\n%s", bodyRows, rendered)
	}
	if !strings.Contains(rendered, "line continues") {
		t.Fatalf("wrapped truncation has no explicit continuation receipt:\n%s", rendered)
	}
}

func TestRenderUnifiedDiffGeometryAcrossSupportedWidths(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 999_998, OldCount: 2, NewStart: 999_998, NewCount: 2}},
		{Kind: DiffContext, Content: "\t界🙂e\u0301 " + strings.Repeat("segment-", 20), OldLine: 999_998, NewLine: 999_998},
		{Kind: DiffRemoved, Content: strings.Repeat("before-", 18), OldLine: 999_999},
		{Kind: DiffAdded, Content: strings.Repeat("after-", 18), NewLine: 999_999},
	}
	styles := NewStyles(true, defaultThemeID)

	for width := 30; width <= 200; width++ {
		rendered := ansi.Strip(renderUnifiedDiffAtWidth("界/very/long/component.go", lines, styles, 0, width))
		for rowIndex, row := range strings.Split(strings.TrimSuffix(rendered, "\n"), "\n") {
			if got := lipgloss.Width(row); got > width {
				t.Fatalf("width=%d row=%d cells=%d: %q", width, rowIndex, got, row)
			}
			if strings.ContainsRune(row, '\t') {
				t.Fatalf("width=%d row=%d retained a geometry-dependent tab: %q", width, rowIndex, row)
			}
		}
	}
}

func TestRenderUnifiedDiffBudgetLabelsOnlyVisibleFacts(t *testing.T) {
	styles := NewStyles(true, defaultThemeID)

	singleRows := []DiffLine{
		{Kind: DiffAdded, Content: "first"},
		{Kind: DiffAdded, Content: "second"},
	}
	rendered := ansi.Strip(renderUnifiedDiffAtWidth("budget.go", singleRows, styles, 1, 30))
	if !strings.Contains(rendered, "… 2 more lines") || strings.Contains(rendered, "line continues") {
		t.Fatalf("wholly omitted source lines were mislabeled:\n%s", rendered)
	}

	wrappedRows := []DiffLine{
		{Kind: DiffAdded, Content: strings.Repeat("wrapped-", 20)},
		{Kind: DiffAdded, Content: "later"},
	}
	rendered = ansi.Strip(renderUnifiedDiffAtWidth("budget.go", wrappedRows, styles, 3, 30))
	if !strings.Contains(rendered, "… line continues · 1 more") {
		t.Fatalf("partially visible wrapped line lacks a truthful continuation:\n%s", rendered)
	}

	unbounded := ansi.Strip(renderUnifiedDiffAtWidth("budget.go", wrappedRows, styles, 0, 30))
	totalBodyRows := len(strings.Split(strings.TrimSuffix(unbounded, "\n"), "\n")) - 1
	for budget := 1; budget <= totalBodyRows+1; budget++ {
		bounded := ansi.Strip(renderUnifiedDiffAtWidth("budget.go", wrappedRows, styles, budget, 30))
		bodyRows := len(strings.Split(strings.TrimSuffix(bounded, "\n"), "\n")) - 1
		if want := min(budget, totalBodyRows); bodyRows != want {
			t.Fatalf("budget=%d rendered body rows=%d, want %d:\n%s", budget, bodyRows, want, bounded)
		}
	}
}

func TestRenderUnifiedDiffASCIIChromeCoversHeaderMetaAndContinuation(t *testing.T) {
	styles := NewStyles(true, defaultThemeID)
	metaLines := []DiffLine{
		{Kind: DiffEllipsis, Content: "… unchanged lines"},
		{Kind: DiffOmitted, Content: "… bounded omission · open viewer"},
	}
	meta := ansi.Strip(renderUnifiedDiffAtWidth(
		"internal/ui/a-very-long-component-name.go",
		metaLines,
		styles,
		0,
		28,
		GlyphASCII,
	))
	for _, forbidden := range []string{"…", "·"} {
		if strings.Contains(meta, forbidden) {
			t.Fatalf("ASCII diff meta retained %q:\n%s", forbidden, meta)
		}
	}
	for _, want := range []string{"~", "... unchanged lines", "... bounded omission |"} {
		if !strings.Contains(meta, want) {
			t.Fatalf("ASCII diff meta omitted %q:\n%s", want, meta)
		}
	}
	assertRenderedLinesFit(t, meta, 28)

	wrappedLines := []DiffLine{
		{Kind: DiffAdded, Content: strings.Repeat("wrapped-value-", 12), NewLine: 1},
		{Kind: DiffAdded, Content: "later", NewLine: 2},
	}
	continuation := ansi.Strip(renderUnifiedDiffAtWidth(
		"internal/ui/a-very-long-component-name.go",
		wrappedLines,
		styles,
		3,
		38,
		GlyphASCII,
	))
	for _, forbidden := range []string{"…", "·"} {
		if strings.Contains(continuation, forbidden) {
			t.Fatalf("ASCII diff continuation retained %q:\n%s", forbidden, continuation)
		}
	}
	if !strings.Contains(continuation, "... line continues | 1 more lines") {
		t.Fatalf("ASCII diff lost its bounded continuation receipt:\n%s", continuation)
	}
	assertRenderedLinesFit(t, continuation, 38)
}

func TestResolveDiffGutterNumbersRejectsInconsistentTypedCoordinates(t *testing.T) {
	tests := map[string][]DiffLine{
		"jump inside hunk": {
			{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 10, OldCount: 1, NewStart: 10, NewCount: 1}},
			{Kind: DiffContext, Content: "ctx", OldLine: 99, NewLine: 10},
		},
		"coordinate exceeds hunk budget": {
			{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 10, OldCount: 1, NewStart: 10, NewCount: 2}},
			{Kind: DiffContext, Content: "ctx", OldLine: 10, NewLine: 10},
			{Kind: DiffRemoved, Content: "extra", OldLine: 11},
		},
		"coordinate on wrong side": {
			{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 10, OldCount: 0, NewStart: 10, NewCount: 1}},
			{Kind: DiffAdded, Content: "plus", OldLine: 10, NewLine: 10},
		},
		"negative coordinate": {
			{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 10, OldCount: 1, NewStart: 10, NewCount: 1}},
			{Kind: DiffContext, Content: "ctx", OldLine: -1, NewLine: 10},
		},
	}

	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			if numbers, ok := resolveDiffGutterNumbers(lines); ok {
				t.Fatalf("inconsistent coordinates produced gutters: %#v", numbers)
			}
			rendered := ansi.Strip(renderUnifiedDiffAtWidth("bad.go", lines, NewStyles(true, defaultThemeID), 0, 100))
			for _, row := range strings.Split(rendered, "\n") {
				if strings.Contains(row, "│") && !strings.HasPrefix(row, "│") {
					t.Fatalf("renderer invented a numbered gutter: %q", row)
				}
			}
		})
	}
}

func TestReadFileForDiff_NoArgs(t *testing.T) {
	result := readFileForDiff(nil)
	if result != "" {
		t.Errorf("nil args should return empty, got %q", result)
	}
}

func TestReadFileForDiff_NonexistentFile(t *testing.T) {
	result := readFileForDiff(map[string]any{"path": "/nonexistent/file/path"})
	if result != "" {
		t.Errorf("nonexistent file should return empty, got %q", result)
	}
}

func TestReadFileForDiffRejectsWorkspaceEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFileForDiffAt(map[string]any{"path": outside}, workspace); got != "" {
		t.Fatalf("diff snapshot read outside workspace: %q", got)
	}
}

func TestReadFileForDiffRejectsEscapingSymlinkPrefix(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	snapshot := readDiffSnapshotForPathAt("escape/secret.txt", workspace)
	if snapshot.Available || snapshot.Content != "" {
		t.Fatalf("escaping symlink snapshot = %#v", snapshot)
	}
}

func TestFileWriteDiffRunsAsyncClearsSnapshotsAndDiscardsStaleResult(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(path, []byte("package sample\n\nconst Value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.toolsCollapsed = false

	startMsg := newToolCallStartMsg("write-1", "write_file", map[string]any{"path": "sample.go"}, workspace)
	updated, _ := m.Update(startMsg)
	m = updated.(*Model)
	if len(m.toolEntries) != 1 || !m.toolEntries[0].BeforeSnapshotAvailable {
		t.Fatalf("pre-write snapshot = %#v", m.toolEntries)
	}
	if err := os.WriteFile(path, []byte("package sample\n\nconst Value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.Update(ToolCallResultMsg{
		ID: "write-1", Name: "write_file", Result: "wrote sample.go",
		Projection: ecosystem.ToolProjection{
			Operation: "write_file", Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded,
		},
	})
	m = updated.(*Model)
	entry := m.toolEntries[0]
	if !entry.DiffPending || entry.DiffGeneration == 0 || entry.RawArgs != nil || entry.BeforeContent != "" || entry.BeforeSnapshotAvailable {
		t.Fatalf("pending diff retained ephemeral state: %#v", entry)
	}
	var loadingView strings.Builder
	m.renderToolGroup(&loadingView, testToolChatEntry(0))
	loading := strings.ToLower(ansi.Strip(loadingView.String()))
	if !strings.Contains(loading, "diff loading") || strings.Contains(loading, "verified") {
		t.Fatalf("pending card is not a static truthful loading receipt:\n%s", loading)
	}

	result := awaitCommandMessage[diffBuildResultMsg](t, commandMessages(cmd), 2*time.Second)
	stale := result
	stale.Generation++
	stale.Lines = []DiffLine{{Kind: DiffAdded, Content: "stale", NewLine: 1}}
	updated, _ = m.Update(stale)
	m = updated.(*Model)
	if !m.toolEntries[0].DiffPending || len(m.toolEntries[0].DiffLines) != 0 {
		t.Fatalf("stale generation changed pending patch: %#v", m.toolEntries[0])
	}

	updated, _ = m.Update(result)
	m = updated.(*Model)
	if m.toolEntries[0].DiffPending || m.toolEntries[0].DiffGeneration != 0 || len(m.toolEntries[0].DiffLines) == 0 {
		t.Fatalf("matching diff result was not installed: %#v", m.toolEntries[0])
	}
	var completedView strings.Builder
	m.renderToolGroup(&completedView, testToolChatEntry(0))
	rendered := ansi.Strip(completedView.String())
	for _, want := range []string{"sample.go", "+1 -1", "@@ -", "const Value = 1", "const Value = 2"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("completed patch missing %q:\n%s", want, rendered)
		}
	}
}

func TestDiffSessionRoundTripPreservesHunksCoordinatesAndBounds(t *testing.T) {
	lines := computeDiff("one\ntwo\nthree\n", "one\nchanged\nthree\n")
	persisted := persistToolEntries([]ToolEntry{{
		ID: "write-1", Name: "write_file", Summary: "sample.txt", Status: ToolStatusDone,
		DiffLines: lines, DiffPending: true, DiffGeneration: 99,
	}})
	restored := restoreToolEntries(persisted)
	if len(restored) != 1 || restored[0].DiffPending || restored[0].DiffGeneration != 0 {
		t.Fatalf("restored async state = %#v", restored)
	}
	if len(restored[0].DiffLines) != len(lines) {
		t.Fatalf("restored patch lines = %d, want %d", len(restored[0].DiffLines), len(lines))
	}
	for index, line := range lines {
		got := restored[0].DiffLines[index]
		if got.Kind != line.Kind || got.Content != line.Content || got.OldLine != line.OldLine || got.NewLine != line.NewLine {
			t.Fatalf("restored line %d = %#v, want %#v", index, got, line)
		}
		if line.Hunk != nil && (got.Hunk == nil || *got.Hunk != *line.Hunk) {
			t.Fatalf("restored hunk %d = %#v, want %#v", index, got.Hunk, line.Hunk)
		}
	}

	oversized := make([]DiffLine, maxPersistedDiffLines*2)
	for index := range oversized {
		oversized[index] = DiffLine{
			Kind: DiffContext, Content: strings.Repeat("x", 64), OldLine: index + 1, NewLine: index + 1,
		}
	}
	bounded := persistDiffLines(oversized)
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) > maxPersistedDiffLines || len(encoded) > maxPersistedDiffBytes {
		t.Fatalf("persisted patch exceeded bounds: lines=%d bytes=%d", len(bounded), len(encoded))
	}
	if bounded[len(bounded)-1].Kind != DiffOmitted {
		t.Fatalf("bounded patch lacks typed omission marker: %#v", bounded[len(bounded)-1])
	}
}

func TestPersistDiffLinesCapsActualJSONExpansion(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "quotes and backslashes", content: strings.Repeat(`"\`, 128)},
		{name: "controls", content: strings.Repeat("\x01\t\r", 128)},
		{name: "multibyte and escaped Unicode", content: strings.Repeat("界\u2028", 128)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines := make([]DiffLine, 1_000)
			for index := range lines {
				lines[index] = DiffLine{
					Kind: DiffAdded, Content: test.content, NewLine: index + 1,
				}
			}

			bounded := persistDiffLines(lines)
			encoded, err := json.Marshal(bounded)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > maxPersistedDiffBytes {
				t.Fatalf("encoded patch = %d bytes, cap = %d", len(encoded), maxPersistedDiffBytes)
			}
			if len(bounded) >= len(lines) || len(bounded) == 0 || bounded[len(bounded)-1].Kind != DiffOmitted {
				t.Fatalf("expanded patch was not visibly bounded: input=%d output=%d tail=%#v", len(lines), len(bounded), bounded[len(bounded)-1])
			}

			stateRaw, err := marshalPersistedSessionState(persistedSessionState{
				Version: currentPersistedSessionVersion,
				Mode:    ModeNormal,
				ToolEntries: []persistedToolEntry{{
					ID: "adversarial-diff", Name: "write_file", DiffLines: lines,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeSessionState(stateRaw)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := json.Marshal(decoded.ToolEntries[0].DiffLines)
			if err != nil {
				t.Fatal(err)
			}
			if len(roundTrip) > maxPersistedDiffBytes {
				t.Fatalf("round-trip encoded patch = %d bytes, cap = %d", len(roundTrip), maxPersistedDiffBytes)
			}
		})
	}
}

func TestPersistDiffLinesCanonicalizesNoNewlineMarker(t *testing.T) {
	const secret = "MARKER_CONTENT_MUST_NOT_PERSIST"
	bounded := persistDiffLines([]DiffLine{{Kind: DiffNoNewline, Content: secret}})
	if len(bounded) != 1 || bounded[0].Kind != DiffNoNewline || bounded[0].Content != diffNoNewlineContent {
		t.Fatalf("canonical persisted marker = %#v", bounded)
	}
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("typed marker persisted caller-controlled content: %s", encoded)
	}
}

func TestLiveDiffInstallationKeepsRowsBeyondThirtyInspectableAndMarksOversize(t *testing.T) {
	var before, after strings.Builder
	for line := 1; line <= 40; line++ {
		fmt.Fprintf(&before, "before-%02d\n", line)
		fmt.Fprintf(&after, "after-%02d\n", line)
	}
	patch := computeDiff(before.String(), after.String())

	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{
		ID: "write-many", Name: "write_file", Summary: "many.txt", Status: ToolStatusDone,
		DiffPending: true, DiffGeneration: 7,
	}}

	updated, _ := m.Update(diffBuildResultMsg{
		Generation: 7, ToolID: "write-many", ToolName: "write_file", Lines: patch, Available: true,
	})
	m = updated.(*Model)
	var expanded strings.Builder
	m.renderToolGroup(&expanded, testToolChatEntry(0))
	if plain := ansi.Strip(expanded.String()); strings.Contains(plain, "after-31") ||
		!strings.Contains(plain, "more lines") {
		t.Fatalf("inline patch did not defer deep rows with an honest receipt:\n%s", plain)
	}
	if !slices.ContainsFunc(m.toolEntries[0].DiffLines, func(line DiffLine) bool {
		return strings.Contains(line.Content, "after-31")
	}) {
		t.Fatal("viewer projection lost a retained diff row beyond the inline cap")
	}
	m.toolEntries[0].Collapsed = true
	var collapsed strings.Builder
	m.renderToolGroup(&collapsed, testToolChatEntry(0))
	if strings.Contains(ansi.Strip(collapsed.String()), "after-31") {
		t.Fatalf("collapsed card leaked patch body:\n%s", ansi.Strip(collapsed.String()))
	}

	oversized := make([]DiffLine, maxPersistedDiffLines*2)
	for index := range oversized {
		oversized[index] = DiffLine{
			Kind: DiffAdded, Content: strings.Repeat("x", 64), NewLine: index + 1,
		}
	}
	m.toolEntries = []ToolEntry{{
		ID: "write-oversized", Name: "write_file", Status: ToolStatusDone,
		DiffPending: true, DiffGeneration: 8,
	}}
	updated, _ = m.Update(diffBuildResultMsg{
		Generation: 8, ToolID: "write-oversized", ToolName: "write_file", Lines: oversized, Available: true,
	})
	m = updated.(*Model)
	installed := m.toolEntries[0].DiffLines
	if len(installed) == 0 {
		t.Fatal("live oversized patch was discarded")
	}
	if len(installed) > maxPersistedDiffLines || installed[len(installed)-1].Kind != DiffOmitted {
		t.Fatalf("live oversized patch lacks bounded typed omission: lines=%d tail=%#v", len(installed), installed[len(installed)-1])
	}
}

func TestWriteToolStartNeverReadsFIFOInsideUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo fixture is Unix-specific")
	}
	workspace := t.TempDir()
	fifo := filepath.Join(workspace, "blocked")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	start := time.Now()
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "denied-write", Name: "write", Args: map[string]any{"path": "blocked"},
	})
	m = updated.(*Model)
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("write start performed filesystem work inside Bubble Tea Update for %s", elapsed)
	}
	if len(m.toolEntries) != 1 || m.toolEntries[0].BeforeContent != "" {
		t.Fatalf("FIFO snapshot retained content: %#v", m.toolEntries)
	}
}

func TestToolStartMessageCapturesPreWriteSnapshotBeforeUpdate(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "before.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := newToolCallStartMsg("write-1", "write_file", map[string]any{"path": "before.txt"}, workspace)
	if !msg.BeforeSnapshotAvailable || msg.BeforeContent != "before\n" {
		t.Fatalf("pre-write message snapshot = available %v content %q", msg.BeforeSnapshotAvailable, msg.BeforeContent)
	}
	if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	updated, _ := m.Update(msg)
	m = updated.(*Model)
	if len(m.toolEntries) != 1 || m.toolEntries[0].BeforeContent != "before\n" {
		t.Fatalf("Update did not install the background snapshot: %#v", m.toolEntries)
	}
}
