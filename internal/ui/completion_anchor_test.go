package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func runeOffsetAfter(t *testing.T, value, marker string) int {
	t.Helper()
	byteOffset := strings.Index(value, marker)
	if byteOffset < 0 {
		t.Fatalf("%q does not contain %q", value, marker)
	}
	return utf8.RuneCountInString(value[:byteOffset+len(marker)])
}

func TestCompletionTokenAtCursorUsesRuneBoundariesAndCommandPosition(t *testing.T) {
	tests := []struct {
		name    string
		draft   string
		marker  string
		want    bool
		kind    string
		query   string
		endText string
	}{
		{name: "mid_sentence_at", draft: "send @café after", marker: "@café", want: true, kind: "attachments", query: "café", endText: "@café"},
		{name: "unicode_whitespace_hash", draft: "α\u2003#sk 后", marker: "#sk", want: true, kind: "skills", query: "sk", endText: "#sk"},
		{name: "multiline_hash", draft: "first line\n#skill tail", marker: "#skill", want: true, kind: "skills", query: "skill", endText: "#skill"},
		{name: "leading_whitespace_command", draft: " \n\t/he tail", marker: "/he", want: true, kind: "command", query: "he", endText: "/he"},
		{name: "goal_action_prefix", draft: "/goal re", marker: "/goal re", want: true, kind: "command", query: "re", endText: "/goal re"},
		{name: "goal_action_preserves_suffix", draft: "/goal re later", marker: "/goal re", want: true, kind: "command", query: "re", endText: "/goal re"},
		{name: "email_is_not_mention", draft: "mail person@example.com", marker: "person@example", want: false},
		{name: "embedded_hash", draft: "value foo#bar", marker: "foo#bar", want: false},
		{name: "slash_after_text", draft: "please /help", marker: "/help", want: false},
		{name: "slash_after_prior_line", draft: "prior\n/help", marker: "/help", want: false},
		{name: "second_argument_is_not_action", draft: "/goal resume later", marker: "later", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cursor := runeOffsetAfter(t, test.draft, test.marker)
			token, ok := completionTokenAtCursor(test.draft, cursor)
			if ok != test.want {
				t.Fatalf("completionTokenAtCursor(%q) ok = %v, want %v: %#v", test.draft, ok, test.want, token)
			}
			if !test.want {
				return
			}
			if token.Kind != test.kind || token.Query != test.query {
				t.Fatalf("token = kind %q query %q, want %q %q", token.Kind, token.Query, test.kind, test.query)
			}
			if token.Anchor.Draft != test.draft || token.Anchor.CursorRune != cursor {
				t.Fatalf("anchor lost full draft/cursor: %#v", token.Anchor)
			}
			runes := []rune(test.draft)
			if got := string(runes[token.Anchor.StartRune:token.Anchor.EndRune]); got != test.endText {
				t.Fatalf("anchored span = %q, want %q", got, test.endText)
			}
		})
	}
}

func TestGoalActionCompletionCarriesRegistrySourceAndReplacementPrefix(t *testing.T) {
	const draft = "  /g stat"
	token, ok := completionTokenAtCursor(draft, utf8.RuneCountInString(draft))
	if !ok {
		t.Fatal("goal action token was not recognized")
	}
	if token.Source != "/g stat" || token.CommandPrefix != "/g " || token.Query != "stat" {
		t.Fatalf("goal action token = %#v", token)
	}
}

func TestAcceptCompletionReplacesOnlyAnchoredUnicodeSpan(t *testing.T) {
	tests := []struct {
		name   string
		draft  string
		marker string
		want   string
	}{
		{
			name:   "mid_sentence_suffix",
			draft:  "añade #sk después",
			marker: "#sk",
			want:   "añade #skill-a después",
		},
		{
			name:   "multiline_token_suffix_after_cursor",
			draft:  "λ first\nuse #skOLD tomorrow",
			marker: "#sk",
			want:   "λ first\nuse #skill-a tomorrow",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			cursor := runeOffsetAfter(t, test.draft, test.marker)
			m.setComposerDraftAtRune(test.draft, cursor)
			m.triggerCompletion(m.input.Value())
			if !m.isCompletionActive() {
				t.Fatal("completion did not open")
			}
			m.acceptCompletion()
			if got := m.input.Value(); got != test.want {
				t.Fatalf("accepted draft = %q, want %q", got, test.want)
			}
			wantCursor := runeOffsetAfter(t, test.want, "#skill-a")
			if gotCursor := textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()); gotCursor != wantCursor {
				t.Fatalf("cursor rune = %d, want %d", gotCursor, wantCursor)
			}
		})
	}
}

func TestAcceptCompletionKeepsCappedDraftTailVisibleWhileComposerWasBlurred(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	draft := strings.Repeat("long context ", 70) + "use #sk"
	m.input.SetValue(draft)
	m.input.CursorEnd()
	_ = m.reflowInputViewport()
	if m.input.ScrollYOffset() <= 0 {
		t.Fatal("fixture did not scroll the capped composer")
	}

	m.triggerCompletion(m.input.Value())
	if !m.isCompletionActive() || m.input.Focused() {
		t.Fatal("completion did not own focus before acceptance")
	}
	m.acceptCompletion()
	if !m.input.Focused() || m.input.ScrollYOffset() <= 0 {
		t.Fatalf("accepted composer focus/offset = %v/%d", m.input.Focused(), m.input.ScrollYOffset())
	}
	if got := m.input.Value(); !strings.HasSuffix(got, "use #skill-a ") {
		t.Fatalf("accepted completion changed tail: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.input.View()), "#skill-a") {
		t.Fatalf("accepted completion cursor tail is offscreen:\n%s", m.input.View())
	}
}

func TestComposerUpdateAutoTriggersAtMidSentenceCursor(t *testing.T) {
	m := newTestModel(t)
	draft := "ask  later"
	cursor := runeOffsetAfter(t, draft, "ask ")
	m.setComposerDraftAtRune(draft, cursor)
	updated, _ := m.Update(charKey('#'))
	m = updated.(*Model)
	if !m.isCompletionActive() || m.completionState.Kind != "skills" {
		t.Fatalf("mid-sentence # did not open completion: %#v", m.completionState)
	}
	anchor := m.completionState.Anchor
	if got := string([]rune(anchor.Draft)[anchor.StartRune:anchor.EndRune]); got != "#" {
		t.Fatalf("automatic trigger anchored %q, want #", got)
	}
	if got := m.input.Value(); got != "ask # later" {
		t.Fatalf("automatic trigger changed surrounding draft to %q", got)
	}
}

func TestDismissCompletionRestoresEditedQueryInsideAnchor(t *testing.T) {
	m := newTestModel(t)
	draft := "α prefix @old suffix"
	m.setComposerDraftAtRune(draft, runeOffsetAfter(t, draft, "@old"))
	m.triggerCompletion(m.input.Value())
	if !m.isCompletionActive() {
		t.Fatal("attachment completion did not open")
	}
	m.completionState.Filter.SetValue("nuevo")
	m.completionState.Filter.SetCursor(2)
	m.dismissCompletion()

	want := "α prefix @nuevo suffix"
	if got := m.input.Value(); got != want {
		t.Fatalf("dismissed draft = %q, want %q", got, want)
	}
	wantCursor := runeOffsetAfter(t, want, "@nu")
	if got := textareaCursorRuneOffset(want, m.input.Line(), m.input.Column()); got != wantCursor {
		t.Fatalf("dismiss cursor = %d, want %d", got, wantCursor)
	}
	if m.completionSuppressedDraft != want {
		t.Fatalf("suppressed draft = %q, want %q", m.completionSuppressedDraft, want)
	}
}

func TestMultiSelectCompletionInsertionUsesAllItemsOrder(t *testing.T) {
	m := newTestModel(t)
	draft := "before @x after"
	m.setComposerDraftAtRune(draft, runeOffsetAfter(t, draft, "@x"))
	m.triggerCompletion(m.input.Value())
	items := []Completion{
		{Label: "@a", Insert: "@a ", Category: "file"},
		{Label: "@b", Insert: "@b ", Category: "file"},
		{Label: "@c", Insert: "@c ", Category: "file"},
	}
	m.completionState.BaseItems = items
	m.completionState.AllItems = items
	m.completionState.FilteredItems = items
	m.completionState.Selected = map[int]bool{2: true, 0: true}
	m.acceptCompletion()

	if got, want := m.input.Value(), "before @a @c after"; got != want {
		t.Fatalf("deterministic multi-select = %q, want %q", got, want)
	}
}

func TestCompletionSearchGenerationRejectsCloseReopenResult(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("@")
	m.input.CursorEnd()
	m.triggerCompletion(m.input.Value())
	oldGeneration := m.completionState.Generation
	oldTag := m.completionState.DebounceTag
	m.closeCompletion()
	m.triggerCompletion(m.input.Value())
	if m.completionState.Generation == oldGeneration || m.completionState.DebounceTag != oldTag {
		t.Fatalf("close/reopen guard = generation %d/%d tag %d/%d", oldGeneration, m.completionState.Generation, oldTag, m.completionState.DebounceTag)
	}

	updated, _ := m.Update(CompletionSearchResultMsg{
		Generation: oldGeneration,
		Tag:        oldTag,
		Results:    []Completion{{Label: "@stale.txt", Insert: "@stale.txt ", Category: "file"}},
	})
	m = updated.(*Model)
	for _, item := range m.completionState.AllItems {
		if item.Label == "@stale.txt" {
			t.Fatal("stale close/reopen search result entered the new completion")
		}
	}
}

func TestSupersededCompletionSearchCancelsRunningContext(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("@")
	m.input.CursorEnd()
	m.triggerCompletion(m.input.Value())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	m.completionSearch = func(ctx context.Context, _, _ string) []Completion {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil
	}
	cmd := m.scheduleCompletionSearch("first", "", false)
	if cmd == nil {
		t.Fatal("first search command was not scheduled")
		return
	}
	done := make(chan struct{})
	go func() {
		_ = cmd()
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("search did not start")
	}
	_ = m.scheduleCompletionSearch("second", "", true)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("superseded search context was not cancelled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled search command did not return")
	}
}

func TestStaleCompletionDebounceTickDoesNotStartSearch(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("@")
	m.input.CursorEnd()
	m.triggerCompletion(m.input.Value())
	if first := m.scheduleCompletionSearch("first", "", true); first == nil {
		t.Fatal("first debounce tick was not scheduled")
	}
	firstMsg := CompletionDebounceTickMsg{
		Generation: m.completionState.Generation,
		Tag:        m.completionState.DebounceTag,
		Query:      "first",
	}
	_ = m.scheduleCompletionSearch("second", "", true)
	if m.completionState.SearchCancel != nil {
		t.Fatal("debounced search unexpectedly owns a running context")
	}
	updated, _ := m.Update(firstMsg)
	m = updated.(*Model)
	if m.completionState.SearchCancel != nil {
		t.Fatal("stale debounce tick started a workspace search")
	}
}

func TestCompletionPreviewGenerationRejectsCloseReopenResult(t *testing.T) {
	m := newTestModel(t)
	m.completionGeneration = 10
	m.completionState = newCompletionState("attachments", []Completion{completionFileItem("old.txt")}, true, m.themeID, m.isDark)
	m.completionState.Generation = m.completionGeneration
	m.overlay = OverlayCompletion
	old := m.refreshCompletionPreview()
	if old == nil {
		t.Fatal("old preview was not scheduled")
		return
	}
	m.closeCompletion()
	m.completionGeneration++
	m.completionState = newCompletionState("attachments", []Completion{completionFileItem("new.txt")}, true, m.themeID, m.isDark)
	m.completionState.Generation = m.completionGeneration
	m.overlay = OverlayCompletion
	m.completionState.Preview = completionPreview{State: completionPreviewLoading, Path: "new.txt"}
	m.completionState.PreviewToken = 1

	updated, _ := m.Update(old())
	m = updated.(*Model)
	if m.completionState.Preview.Path != "new.txt" || m.completionState.Preview.State != completionPreviewLoading {
		t.Fatalf("stale preview replaced reopened state: %#v", m.completionState.Preview)
	}
}

func TestCompletionFilesystemWorkIsDeferredOutOfUpdateAndView(t *testing.T) {
	m := newTestModel(t)
	m.completionSearch = func(context.Context, string, string) []Completion {
		panic("filesystem search ran synchronously")
	}
	m.input.SetValue("ask @file later")
	m.setComposerDraftAtRune(m.input.Value(), runeOffsetAfter(t, m.input.Value(), "@file"))
	cmd := m.triggerCompletion(m.input.Value())
	if cmd == nil || !m.isCompletionActive() {
		t.Fatal("attachment completion did not defer a search command")
	}
	_ = m.View()
}

func TestWorkspaceCompletionsListTypedAndBrowsedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.completer.workDir = root

	typed := m.completer.WorkspaceCompletions(context.Background(), "src/ma", "")
	if len(typed) != 1 || typed[0].Label != "@src/main.go" || typed[0].Insert != "@src/main.go " {
		t.Fatalf("typed path completions = %#v", typed)
	}
	browsed := m.completer.WorkspaceCompletions(context.Background(), "", "src")
	if len(browsed) != 1 || browsed[0].Label != "main.go" || browsed[0].Insert != "@src/main.go " {
		t.Fatalf("browsed path completions = %#v", browsed)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := m.completer.WorkspaceCompletions(cancelled, "", "src"); len(got) != 0 {
		t.Fatalf("cancelled workspace listing returned %#v", got)
	}
}

func TestWorkspaceCompletionsRejectSymlinkedDirectoryEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.completer.workDir = root

	if got := m.completer.WorkspaceCompletions(context.Background(), "linked/", ""); len(got) != 0 {
		t.Fatalf("typed symlink escape listed outside entries: %#v", got)
	}
	if got := m.completer.WorkspaceCompletions(context.Background(), "", "linked"); len(got) != 0 {
		t.Fatalf("browsed symlink escape listed outside entries: %#v", got)
	}
}

func TestCompletionPopupIsInlineAboveVisibleComposerAtSupportedSizes(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{{width: 30, height: 12}, {width: 120, height: 36}} {
		t.Run(strings.Repeat("w", size.width/10), func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)
			m.setTestTranscriptContent("transcript anchor")
			m.input.SetValue("ask @ag")
			m.input.CursorEnd()
			m.triggerCompletion(m.input.Value())

			view := m.View()
			plain := ansi.Strip(view.Content)
			popupAt := strings.Index(plain, "Attach Files")
			composerAt := strings.Index(plain, "ask @ag")
			if popupAt < 0 || composerAt < 0 || popupAt >= composerAt {
				t.Fatalf("popup/composer order is not inline: popup=%d composer=%d\n%s", popupAt, composerAt, plain)
			}
			if !strings.Contains(plain, "transcript anchor") {
				t.Fatalf("completion covered the transcript:\n%s", plain)
			}
			if got := m.input.Value(); got != "ask @ag" {
				t.Fatalf("visible composer draft mutated to %q", got)
			}
			assertViewCursorAfter(t, view, completionFilterPrompt+"ag")
			assertRenderedLinesFit(t, view.Content, size.width)
			assertRenderedHeightFits(t, view.Content, size.height)
		})
	}
}

func TestCompletionPopupFitsFiveLineComposerAtMinimumSize(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.setTestTranscriptContent("transcript anchor")
	m.input.SetValue("one\ntwo\nthree\nfour\n@ag")
	m.input.CursorEnd()
	m.syncInputHeight()
	m.triggerCompletion(m.input.Value())

	view := m.View()
	plain := ansi.Strip(view.Content)
	for _, marker := range []string{
		"transcript anchor",
		completionFilterPrompt + "ag",
		"@agent-x",
		"one",
		"@ag",
		"esc",
		"enter",
	} {
		if !strings.Contains(plain, marker) {
			t.Fatalf("minimum multiline completion lost %q:\n%s", marker, plain)
		}
	}
	if m.viewport.Height() < 1 {
		t.Fatalf("minimum completion left %d transcript rows", m.viewport.Height())
	}
	assertViewCursorAfter(t, view, completionFilterPrompt+"ag")
	assertRenderedLinesFit(t, view.Content, 30)
	assertRenderedHeightFits(t, view.Content, 12)
}

func TestCompletionPopupFitsCappedWrappedComposer(t *testing.T) {
	for _, size := range []struct {
		width, height int
	}{{30, 12}, {54, 18}} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		m = updated.(*Model)
		m.setTestTranscriptContent("transcript anchor")
		draft := strings.Repeat("wrapped context ", 30) + "@ag"
		m.input.SetValue(draft)
		m.input.CursorEnd()
		_ = m.reflowInputViewport()
		if m.inputLines != composerVisibleRowLimit(size.height) || m.input.ScrollYOffset() <= 0 {
			t.Fatalf("%dx%d fixture was not capped at the tail: rows=%d offset=%d", size.width, size.height, m.inputLines, m.input.ScrollYOffset())
		}
		m.triggerCompletion(m.input.Value())

		view := m.View()
		plain := ansi.Strip(view.Content)
		for _, marker := range []string{"transcript anchor", completionFilterPrompt + "ag", "@agent-x", "@ag"} {
			if !strings.Contains(plain, marker) {
				t.Fatalf("%dx%d wrapped completion lost %q:\n%s", size.width, size.height, marker, plain)
			}
		}
		if m.input.Value() != draft {
			t.Fatalf("%dx%d completion changed wrapped draft", size.width, size.height)
		}
		assertRenderedLinesFit(t, view.Content, size.width)
		assertRenderedHeightFits(t, view.Content, size.height)
	}
}

func TestCompletionOpenAndAcceptPreservePausedTranscriptAnchor(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent(strings.Repeat("transcript\n", 100))
	m.setTranscriptYOffset(7)
	m.pauseFollow()
	m.input.SetValue("#sk")
	m.input.CursorEnd()
	m.triggerCompletion(m.input.Value())
	if got := m.transcriptYOffset(); got != 7 || !m.followPaused() {
		t.Fatalf("open moved paused anchor to %d paused=%v", got, m.followPaused())
	}
	m.acceptCompletion()
	if got := m.transcriptYOffset(); got != 7 || !m.followPaused() {
		t.Fatalf("accept moved paused anchor to %d paused=%v", got, m.followPaused())
	}
}
