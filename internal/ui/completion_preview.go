package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	completionPreviewByteLimit = 8 * 1024
	completionPreviewTimeout   = 2 * time.Second
)

type completionPreviewState uint8

const (
	completionPreviewNone completionPreviewState = iota
	completionPreviewLoading
	completionPreviewReady
	completionPreviewBinary
	completionPreviewError
	completionPreviewFolder
	completionPreviewAgent
)

type completionPreview struct {
	State     completionPreviewState
	Path      string
	Content   string
	Message   string
	Size      int64
	Truncated bool
}

type completionPreviewResultMsg struct {
	Generation uint64
	Token      uint64
	Preview    completionPreview
}

type completionPreviewLoader func(context.Context, string, string) completionPreview

// completionPreviewReader bounds blocking filesystem work to one worker. A
// cancelled or timed-out caller returns immediately, while the occupied slot
// remains held until the underlying syscall returns. That prevents navigation
// across a slow FUSE or network mount from accumulating blocked goroutines and
// descriptors.
type completionPreviewReader struct {
	slot chan struct{}
	load completionPreviewLoader
}

func newCompletionPreviewReader(load completionPreviewLoader) *completionPreviewReader {
	if load == nil {
		load = loadCompletionPreview
	}
	return &completionPreviewReader{slot: make(chan struct{}, 1), load: load}
}

var defaultCompletionPreviewReader = newCompletionPreviewReader(loadCompletionPreview)

func (reader *completionPreviewReader) read(ctx context.Context, workDir, relative string) completionPreview {
	if reader == nil || reader.slot == nil || reader.load == nil {
		return completionPreview{
			State:   completionPreviewError,
			Path:    filepath.ToSlash(relative),
			Message: "Preview unavailable",
		}
	}
	select {
	case reader.slot <- struct{}{}:
	case <-ctx.Done():
		return completionPreviewContextError(relative, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		<-reader.slot
		return completionPreviewContextError(relative, err)
	}

	result := make(chan completionPreview, 1)
	go func() {
		preview := reader.load(ctx, workDir, relative)
		<-reader.slot
		result <- preview
	}()

	select {
	case preview := <-result:
		return preview
	case <-ctx.Done():
		return completionPreviewContextError(relative, ctx.Err())
	}
}

func completionPreviewContextError(relative string, err error) completionPreview {
	message := "Preview cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		message = "Preview timed out"
	}
	return completionPreview{
		State:   completionPreviewError,
		Path:    filepath.ToSlash(relative),
		Message: message,
	}
}

func (m *Model) refreshCompletionPreview() tea.Cmd {
	cs := m.completionState
	if cs == nil || cs.Kind != "attachments" {
		return nil
	}
	if cs.PreviewCancel != nil {
		cs.PreviewCancel()
		cs.PreviewCancel = nil
	}
	cs.PreviewToken++
	token := cs.PreviewToken
	generation := cs.Generation

	item, ok := selectedCompletion(cs)
	if !ok {
		cs.Preview = completionPreview{State: completionPreviewNone, Message: "No file selected"}
		return nil
	}
	switch item.Category {
	case "folder":
		cs.Preview = completionPreview{State: completionPreviewFolder, Path: completionItemPath(item), Message: "Enter to open"}
		return nil
	case "agent":
		cs.Preview = completionPreview{State: completionPreviewAgent, Path: strings.TrimSpace(item.Label), Message: "Agent profile"}
		return nil
	case "file", "search_result":
		// Continue below; file content is always loaded off the Update/View path.
	default:
		cs.Preview = completionPreview{State: completionPreviewNone, Path: strings.TrimSpace(item.Label), Message: "Preview unavailable"}
		return nil
	}

	path := completionItemPath(item)
	cs.Preview = completionPreview{State: completionPreviewLoading, Path: path, Message: "Loading…"}
	if m.completer == nil {
		cs.Preview = completionPreview{State: completionPreviewError, Path: path, Message: "Workspace is unavailable"}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionPreviewTimeout)
	cs.PreviewCancel = cancel
	workDir := m.completer.workDir
	return func() tea.Msg {
		defer cancel()
		preview := defaultCompletionPreviewReader.read(ctx, workDir, path)
		return completionPreviewResultMsg{Generation: generation, Token: token, Preview: preview}
	}
}

func selectedCompletion(cs *CompletionState) (Completion, bool) {
	if cs == nil || cs.Index < 0 || cs.Index >= len(cs.FilteredItems) {
		return Completion{}, false
	}
	return cs.FilteredItems[cs.Index], true
}

func completionItemPath(item Completion) string {
	path := strings.TrimSpace(item.Insert)
	path = strings.TrimPrefix(path, "@")
	if path == "" {
		path = strings.TrimPrefix(strings.TrimSpace(item.Label), "@")
	}
	return filepath.ToSlash(strings.TrimSpace(path))
}

func loadCompletionPreview(ctx context.Context, workDir, relative string) completionPreview {
	preview := completionPreview{Path: filepath.ToSlash(relative)}
	if err := ctx.Err(); err != nil {
		return completionPreviewContextError(relative, err)
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		preview.State = completionPreviewError
		preview.Message = "Workspace path unavailable"
		return preview
	}
	absolute := relative
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(root, filepath.FromSlash(relative))
	}
	absolute, err = filepath.Abs(absolute)
	if err != nil {
		preview.State = completionPreviewError
		preview.Message = "Invalid file path"
		return preview
	}
	within, err := filepath.Rel(root, absolute)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		preview.State = completionPreviewError
		preview.Message = "Outside the workspace"
		return preview
	}
	preview.Path = filepath.ToSlash(within)

	file, err := safeio.OpenWithinNoFollow(root, within)
	if err != nil {
		preview.State = completionPreviewError
		if errors.Is(err, safeio.ErrSymlink) {
			preview.Message = "Symlink preview disabled"
		} else {
			preview.Message = completionPreviewErrorText(err)
		}
		return preview
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		preview.State = completionPreviewError
		preview.Message = "Preview unavailable"
		return preview
	}
	preview.Size = openedInfo.Size()
	switch {
	case openedInfo.IsDir():
		preview.State = completionPreviewFolder
		preview.Message = "Enter to open"
		return preview
	case !openedInfo.Mode().IsRegular():
		preview.State = completionPreviewBinary
		preview.Message = "Non-regular file"
		return preview
	}
	payload, err := io.ReadAll(io.LimitReader(file, completionPreviewByteLimit+utf8.UTFMax))
	if err != nil {
		preview.State = completionPreviewError
		preview.Message = "Read failed"
		return preview
	}
	if err := ctx.Err(); err != nil {
		return completionPreviewContextError(preview.Path, err)
	}
	if len(payload) > completionPreviewByteLimit {
		payload = trimCompletionPreviewUTF8Boundary(payload, completionPreviewByteLimit)
		preview.Truncated = true
	}
	if preview.Size > completionPreviewByteLimit {
		preview.Truncated = true
	}
	if completionPreviewIsBinary(payload) {
		preview.State = completionPreviewBinary
		preview.Message = "Binary file"
		return preview
	}
	preview.State = completionPreviewReady
	preview.Content = sanitizeCompletionPreview(string(payload))
	if strings.TrimSpace(preview.Content) == "" {
		preview.Message = "Empty file"
	}
	return preview
}

// trimCompletionPreviewUTF8Boundary removes only a valid multibyte rune that
// crosses limit. Invalid bytes wholly inside the visible prefix remain present
// so binary detection still reports them honestly.
func trimCompletionPreviewUTF8Boundary(payload []byte, limit int) []byte {
	if limit < 0 {
		limit = 0
	}
	if len(payload) <= limit {
		return payload
	}
	cut := limit
	if cut > 0 && cut < len(payload) && !utf8.RuneStart(payload[cut]) {
		start := cut - 1
		for start > 0 && !utf8.RuneStart(payload[start]) && cut-start < utf8.UTFMax {
			start--
		}
		if utf8.RuneStart(payload[start]) {
			_, size := utf8.DecodeRune(payload[start:])
			if size > 1 && start+size > cut {
				cut = start
			}
		}
	}
	return payload[:cut]
}

func completionPreviewErrorText(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "File no longer exists"
	case errors.Is(err, os.ErrPermission):
		return "Permission denied"
	case errors.Is(err, safeio.ErrNoFollowUnsupported):
		return "Secure preview unavailable on this platform"
	default:
		return "Preview unavailable"
	}
}

func completionPreviewIsBinary(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if bytes.IndexByte(payload, 0) >= 0 || !utf8.Valid(payload) {
		return true
	}
	controls := 0
	for _, value := range string(payload) {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			controls++
		}
	}
	return controls*20 > utf8.RuneCount(payload)
}

func sanitizeCompletionPreview(content string) string {
	return sanitizeTerminalMultiline(content)
}

func sanitizeCompletionPreviewPath(path string) string {
	return sanitizeTerminalSingleLine(path)
}

func (m *Model) renderCompletionPreview(width, rows int) string {
	cs := m.completionState
	if cs == nil || cs.Kind != "attachments" || rows < 1 {
		return ""
	}
	preview := cs.Preview
	meta := completionPreviewMeta(preview, m.glyphProfile)
	if rows == 1 {
		return m.styles.CompletionCategory.Render(
			truncateDisplayWithGlyphProfile(meta, width, m.glyphProfile),
		)
	}
	glyphs := glyphSet(m.glyphProfile)
	lines := []string{m.styles.Divider.Render(
		strings.Repeat(glyphs.Horizontal, max(1, width)),
	)}
	lines = append(lines, m.styles.CompletionCategory.Render(
		truncateDisplayWithGlyphProfile(meta, width, m.glyphProfile),
	))
	if preview.State == completionPreviewReady && rows > 2 {
		contentLines := strings.Split(preview.Content, "\n")
		if len(contentLines) == 0 || strings.TrimSpace(preview.Content) == "" {
			contentLines = []string{"(empty)"}
		}
		for _, contentLine := range contentLines {
			if len(lines) >= rows {
				break
			}
			contentLine = strings.ReplaceAll(contentLine, "\t", "    ")
			lines = append(lines, m.styles.StatusText.Render(
				truncateDisplayWithGlyphProfile(
					glyphs.Vertical+" "+contentLine,
					width,
					m.glyphProfile,
				),
			))
		}
	}
	return strings.Join(lines[:min(rows, len(lines))], "\n")
}

func completionPreviewMeta(preview completionPreview, profiles ...GlyphProfile) string {
	profile := resolveGlyphProfile(profiles...)
	separator := glyphSeparator(profile)
	path := sanitizeCompletionPreviewPath(preview.Path)
	if path == "" {
		path = "selection"
	}
	var state string
	switch preview.State {
	case completionPreviewLoading:
		state = "loading" + glyphEllipsis(profile)
	case completionPreviewReady:
		state = formatCompletionPreviewBytes(preview.Size)
		if preview.Truncated {
			state += separator + "first 8 KiB"
		} else if preview.Message != "" {
			state += separator + preview.Message
		}
	case completionPreviewBinary:
		state = "binary" + separator + formatCompletionPreviewBytes(preview.Size)
		if preview.Message != "" && preview.Message != "Binary file" {
			state = preview.Message + separator + formatCompletionPreviewBytes(preview.Size)
		}
	case completionPreviewError:
		state = preview.Message
	case completionPreviewFolder:
		state = "folder" + separator + preview.Message
	case completionPreviewAgent:
		state = preview.Message
	default:
		state = preview.Message
	}
	if strings.TrimSpace(state) == "" {
		state = "preview unavailable"
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, "Preview", separator, path, separator, state)
}

func formatCompletionPreviewBytes(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
	}
}
