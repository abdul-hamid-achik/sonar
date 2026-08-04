package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func completionFileItem(path string) Completion {
	return Completion{Label: "@" + path, Insert: "@" + path + " ", Category: "file"}
}

func openCompletionPreviewFixture(t *testing.T, files ...string) *Model {
	t.Helper()
	m := newTestModel(t)
	items := make([]Completion, 0, len(files))
	for _, file := range files {
		items = append(items, completionFileItem(file))
	}
	m.completionState = newCompletionState("attachments", items, true, m.themeID, m.isDark)
	m.overlay = OverlayCompletion
	return m
}

func TestCompletionPreviewLoadsOnlyThroughAsyncCommand(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := openCompletionPreviewFixture(t, "main.go")
	m.completer.workDir = workspace

	cmd := m.refreshCompletionPreview()
	if cmd == nil || m.completionState.Preview.State != completionPreviewLoading {
		t.Fatalf("preview did not enter async loading state: %#v", m.completionState.Preview)
	}
	before := ansi.Strip(m.renderCompletionModal())
	if strings.Contains(before, "func main") || !strings.Contains(before, "loading") {
		t.Fatalf("View performed or obscured the pending read:\n%s", before)
	}

	updated, _ := m.Update(cmd())
	m = updated.(*Model)
	if m.completionState.Preview.State != completionPreviewReady || !strings.Contains(m.completionState.Preview.Content, "func main") {
		t.Fatalf("loaded preview = %#v", m.completionState.Preview)
	}
	if rendered := ansi.Strip(m.renderCompletionModal()); !strings.Contains(rendered, "func main") {
		t.Fatalf("ready preview was not rendered:\n%s", rendered)
	}
}

func TestCompletionPreviewIgnoresCancelledStaleResult(t *testing.T) {
	workspace := t.TempDir()
	for name, content := range map[string]string{"a.txt": "first", "b.txt": "second"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := openCompletionPreviewFixture(t, "a.txt", "b.txt")
	m.completer.workDir = workspace
	first := m.refreshCompletionPreview()
	m.completionState.Index = 1
	second := m.refreshCompletionPreview()
	if first == nil || second == nil {
		t.Fatal("preview commands were not scheduled")
	}

	stale := first().(completionPreviewResultMsg)
	if stale.Preview.Message != "Preview cancelled" {
		t.Fatalf("cancelled preview result = %#v", stale.Preview)
	}
	updated, _ := m.Update(stale)
	m = updated.(*Model)
	if m.completionState.Preview.Path != "b.txt" || m.completionState.Preview.State != completionPreviewLoading {
		t.Fatalf("stale result replaced current selection: %#v", m.completionState.Preview)
	}

	updated, _ = m.Update(second())
	m = updated.(*Model)
	if m.completionState.Preview.Path != "b.txt" || m.completionState.Preview.Content != "second" {
		t.Fatalf("current preview = %#v", m.completionState.Preview)
	}
}

func TestClosingCompletionCancelsAndIgnoresPreview(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := openCompletionPreviewFixture(t, "note.txt")
	m.completer.workDir = workspace
	cmd := m.refreshCompletionPreview()
	m.closeCompletion()
	if m.completionState != nil || m.overlay != OverlayNone {
		t.Fatal("completion did not close")
	}
	result := cmd().(completionPreviewResultMsg)
	if result.Preview.Message != "Preview cancelled" {
		t.Fatalf("closed preview result = %#v", result.Preview)
	}
	updated, _ := m.Update(result)
	m = updated.(*Model)
	if m.completionState != nil {
		t.Fatal("late preview reopened completion")
	}
}

func TestCompletionPreviewReaderBoundsCancelledBlockingWork(t *testing.T) {
	workspace := t.TempDir()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	reader := newCompletionPreviewReader(func(_ context.Context, _, relative string) completionPreview {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return completionPreview{State: completionPreviewReady, Path: relative, Content: "ready"}
	})

	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan completionPreview, 1)
	go func() {
		firstResult <- reader.read(firstContext, workspace, "first.txt")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking preview worker did not start")
	}
	cancelFirst()
	if preview := <-firstResult; preview.Message != "Preview cancelled" {
		t.Fatalf("cancelled blocking preview = %#v", preview)
	}

	secondContext, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	second := reader.read(secondContext, workspace, "second.txt")
	if second.Message != "Preview timed out" {
		t.Fatalf("queued preview = %#v", second)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("blocked reader started %d workers, want exactly one", got)
	}

	close(release)
	thirdContext, cancelThird := context.WithTimeout(context.Background(), time.Second)
	defer cancelThird()
	third := reader.read(thirdContext, workspace, "third.txt")
	if third.State != completionPreviewReady || third.Content != "ready" {
		t.Fatalf("reader did not recover after worker release: %#v", third)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("recovered reader worker calls = %d, want 2", got)
	}
}

func TestCompletionWorkspaceReaderBoundsCancelledBlockingWork(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	reader := newCompletionWorkspaceReader()
	search := func(_ context.Context, query, _ string) []Completion {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return []Completion{{Label: query, Insert: "@" + query + " "}}
	}

	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan []Completion, 1)
	go func() {
		firstResult <- reader.read(firstContext, search, "first.txt", "")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking workspace worker did not start")
	}
	cancelFirst()
	if result := <-firstResult; len(result) != 0 {
		t.Fatalf("cancelled workspace search returned %#v", result)
	}

	secondContext, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	if result := reader.read(secondContext, search, "second.txt", ""); len(result) != 0 {
		t.Fatalf("queued workspace search returned %#v", result)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("blocked workspace reader started %d workers, want exactly one", got)
	}

	close(release)
	thirdContext, cancelThird := context.WithTimeout(context.Background(), time.Second)
	defer cancelThird()
	third := reader.read(thirdContext, search, "third.txt", "")
	if len(third) != 1 || third[0].Label != "third.txt" {
		t.Fatalf("workspace reader did not recover after release: %#v", third)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("recovered workspace worker calls = %d, want 2", got)
	}
}

func TestCompletionPreviewReportsBinaryErrorAndTruncationHonestly(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "binary.dat"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("x", completionPreviewByteLimit+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("large.txt", filepath.Join(workspace, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked-dir")); err != nil {
		t.Fatal(err)
	}

	binary := loadCompletionPreview(context.Background(), workspace, "binary.dat")
	if binary.State != completionPreviewBinary || binary.Message != "Binary file" {
		t.Fatalf("binary preview = %#v", binary)
	}
	missing := loadCompletionPreview(context.Background(), workspace, "missing.txt")
	if missing.State != completionPreviewError || missing.Message != "File no longer exists" {
		t.Fatalf("missing preview = %#v", missing)
	}
	large := loadCompletionPreview(context.Background(), workspace, "large.txt")
	if large.State != completionPreviewReady || !large.Truncated || len(large.Content) != completionPreviewByteLimit {
		t.Fatalf("large preview = state %v truncated=%v bytes=%d", large.State, large.Truncated, len(large.Content))
	}
	linked := loadCompletionPreview(context.Background(), workspace, "linked.txt")
	if linked.State != completionPreviewError || linked.Message != "Symlink preview disabled" {
		t.Fatalf("symlink preview = %#v", linked)
	}
	linkedDirectory := loadCompletionPreview(context.Background(), workspace, "linked-dir/secret.txt")
	if linkedDirectory.State != completionPreviewError || strings.Contains(linkedDirectory.Content, "outside secret") {
		t.Fatalf("intermediate symlink preview = %#v", linkedDirectory)
	}
	escape := loadCompletionPreview(context.Background(), workspace, "../outside.txt")
	if escape.State != completionPreviewError || escape.Message != "Outside the workspace" {
		t.Fatalf("outside preview = %#v", escape)
	}
}

func TestCompletionPreviewTruncationPreservesUTF8Boundary(t *testing.T) {
	workspace := t.TempDir()
	content := strings.Repeat("a", completionPreviewByteLimit-1) + "€" + "tail"
	if err := os.WriteFile(filepath.Join(workspace, "unicode.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	preview := loadCompletionPreview(context.Background(), workspace, "unicode.txt")
	if preview.State != completionPreviewReady || !preview.Truncated {
		t.Fatalf("UTF-8 boundary preview = %#v", preview)
	}
	if !utf8.ValidString(preview.Content) {
		t.Fatalf("truncated preview is invalid UTF-8: %q", preview.Content[len(preview.Content)-8:])
	}
	if got, want := len(preview.Content), completionPreviewByteLimit-1; got != want {
		t.Fatalf("truncated preview length = %d, want %d", got, want)
	}
}

func TestCompletionPreviewMetaSanitizesUntrustedPath(t *testing.T) {
	unsafePath := "safe/\x1b]52;c;c2VjcmV0\x07\n\t\u009b31m\u202eevil\u2066.txt"
	meta := completionPreviewMeta(completionPreview{
		State: completionPreviewReady,
		Path:  unsafePath,
		Size:  12,
	})
	if strings.Contains(meta, "c2VjcmV0") {
		t.Fatalf("OSC payload survived path sanitization: %q", meta)
	}
	for _, value := range meta {
		if unicode.IsControl(value) || isBidiControl(value) {
			t.Fatalf("unsafe path rune %U survived sanitization: %q", value, meta)
		}
	}
	if !strings.Contains(meta, "safe/") || !strings.Contains(meta, "evil") {
		t.Fatalf("visible filename content was lost: %q", meta)
	}
}

func TestCompletionPreviewContentSanitizesTerminalAndBidiControls(t *testing.T) {
	unsafe := "safe\n\x1b]52;c;PREVIEW_SECRET\x07visible\x1b[2J\ttext\u202e\u2066"
	sanitized := sanitizeCompletionPreview(unsafe)
	if strings.Contains(sanitized, "PREVIEW_SECRET") || !strings.Contains(sanitized, "safe\nvisible\ttext") {
		t.Fatalf("sanitized preview content = %q", sanitized)
	}
	for _, character := range sanitized {
		if character == '\n' || character == '\t' {
			continue
		}
		if unicode.IsControl(character) || isBidiControl(character) {
			t.Fatalf("unsafe rune %U survived preview content: %q", character, sanitized)
		}
	}
}

func TestCompletionModalSanitizesUntrustedChromeWithoutChangingInsertion(t *testing.T) {
	unsafeLabel := "@safe\x1b]52;c;COMPLETION_SECRET\x07\nspoof\u202e.txt"
	unsafeInsert := unsafeLabel + " "
	unsafe := Completion{
		Label:       unsafeLabel,
		Insert:      unsafeInsert,
		Category:    "file\nCATEGORY_SPOOF",
		Description: "open\x1b[2J\nDESCRIPTION_SPOOF",
	}
	m := newTestModel(t)
	m.completionState = newCompletionState("attachments", []Completion{unsafe}, true, m.themeID, m.isDark)
	m.completionState.CurrentPath = "folder\x1b]0;PATH_SECRET\x07\nBREADCRUMB_SPOOF"
	m.overlay = OverlayCompletion

	rendered := m.renderCompletionModal()
	plain := ansi.Strip(rendered)
	for _, secret := range []string{"COMPLETION_SECRET", "PATH_SECRET"} {
		if strings.Contains(rendered, secret) || strings.Contains(plain, secret) {
			t.Fatalf("terminal payload %q survived completion rendering:\n%s", secret, plain)
		}
	}
	for _, character := range plain {
		if character == '\n' || character == '\t' {
			continue
		}
		if unicode.IsControl(character) || isBidiControl(character) {
			t.Fatalf("unsafe rune %U survived completion chrome: %q", character, plain)
		}
	}
	if got := m.completionState.AllItems[0].Insert; got != unsafeInsert {
		t.Fatalf("display sanitization changed raw insertion: got %q want %q", got, unsafeInsert)
	}
	assertRenderedLinesFit(t, rendered, m.width)
}

func TestCompletionPreviewReportsUnsupportedSecureTraversal(t *testing.T) {
	if got := completionPreviewErrorText(safeio.ErrNoFollowUnsupported); got != "Secure preview unavailable on this platform" {
		t.Fatalf("unsupported traversal preview message = %q", got)
	}
}

func TestCompletionPreviewFitsMinimumTerminalInEveryState(t *testing.T) {
	states := []struct {
		preview completionPreview
		want    string
	}{
		{
			preview: completionPreview{State: completionPreviewLoading, Path: "internal/ui/completion_preview.go", Message: "Loading…"},
			want:    "loading",
		},
		{
			preview: completionPreview{State: completionPreviewReady, Path: "README.md", Size: 42, Content: "# sonar\nUseful local harness"},
			want:    "42 B",
		},
		{
			preview: completionPreview{State: completionPreviewBinary, Path: "image.png", Size: 4096, Message: "Binary file"},
			want:    "binary",
		},
		{
			preview: completionPreview{State: completionPreviewError, Path: "gone.txt", Message: "File no longer exists"},
			want:    "error",
		},
	}
	for _, state := range states {
		m := openCompletionPreviewFixture(t, "README.md")
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
		m = updated.(*Model)
		m.completionState.Preview = state.preview
		rendered := m.renderCompletionModal()
		assertRenderedLinesFit(t, rendered, 30)
		assertRenderedHeightFits(t, rendered, 12)
		plain := ansi.Strip(rendered)
		for _, want := range []string{"Preview", state.want} {
			if !strings.Contains(plain, want) {
				t.Fatalf("minimum modal hid preview state %q:\n%s", want, plain)
			}
		}
	}
}
