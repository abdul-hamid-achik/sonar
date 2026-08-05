package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type imageAttachmentTestClient struct{}

func (*imageAttachmentTestClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return nil
}
func (*imageAttachmentTestClient) Ping() error   { return nil }
func (*imageAttachmentTestClient) Model() string { return "vision-model" }
func (*imageAttachmentTestClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func newImageTestModel(t *testing.T) (*Model, *imageasset.Store) {
	t.Helper()
	m := newTestModel(t)
	store, err := imageasset.NewStore(filepath.Join(t.TempDir(), "images"), imageasset.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	m.SetImageStore(store)
	m.model = "vision-model"
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "vision-model", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true,
		Capabilities: []string{"completion", "tools", "vision"}, Current: true,
	}}
	return m, store
}

func writeImageAttachmentFixture(t *testing.T, path string) []byte {
	return writeImageAttachmentFixtureVariant(t, path, 0)
}

func writeImageAttachmentFixtureVariant(t *testing.T, path string, variant uint8) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 18, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 18; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x*9) + variant, G: uint8(y*13) + variant, B: 90 + variant, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func attachImageFixture(t *testing.T, m *Model, path, fallback string) {
	t.Helper()
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(m.beginImageFileAttachment(path, fallback)), 2*time.Second)
	updated, _ := m.Update(receipt)
	if updated.(*Model) != m {
		t.Fatal("image receipt replaced the model pointer")
	}
}

func settleImageAttachmentCommands(t *testing.T, m *Model, cmd tea.Cmd) *Model {
	t.Helper()
	for cmd != nil {
		receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
		updated, next := m.Update(receipt)
		m = updated.(*Model)
		cmd = next
	}
	return m
}

func TestPastedImagePathAttachesWithoutLeakingPathIntoComposer(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "design review.png")
	writeImageAttachmentFixture(t, path)

	updated, cmd := m.Update(tea.PasteMsg{Content: `"` + path + `"`})
	m = updated.(*Model)
	if cmd == nil || !m.imageAttachRunning || m.input.Focused() {
		t.Fatalf("image paste did not start an owned admission: running=%v focused=%v", m.imageAttachRunning, m.input.Focused())
	}
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)

	if len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Name != "design review.png" {
		t.Fatalf("pending images = %#v", m.pendingImages)
	}
	if m.input.Value() != "" || !m.input.Focused() {
		t.Fatalf("path paste leaked into composer or lost focus: %q focused=%v", m.input.Value(), m.input.Focused())
	}
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"Images ready", "design review.png", "/image clear"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("pending image footer omitted %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, filepath.Dir(path)) {
		t.Fatalf("pending image UI leaked source directory:\n%s", plain)
	}
}

func TestPastedImageListAdmitsQuotedNewlinePathsInOrder(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := t.TempDir()
	first := filepath.Join(directory, "first screen.png")
	second := filepath.Join(directory, "second screen.png")
	writeImageAttachmentFixtureVariant(t, first, 1)
	writeImageAttachmentFixtureVariant(t, second, 2)

	content := fmt.Sprintf("%q\n%q", first, second)
	updated, cmd := m.Update(tea.PasteMsg{Content: content})
	m = settleImageAttachmentCommands(t, updated.(*Model), cmd)

	if len(m.pendingImages) != 2 || m.pendingImages[0].Ref.Name != "first screen.png" || m.pendingImages[1].Ref.Name != "second screen.png" {
		t.Fatalf("ordered image queue = %#v", m.pendingImages)
	}
	if m.input.Value() != "" || m.imageAttachRunning || len(m.imageAttachQueue) != 0 {
		t.Fatalf("settled queue state: draft=%q running=%v queued=%d", m.input.Value(), m.imageAttachRunning, len(m.imageAttachQueue))
	}
	encoded, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, directory) {
		t.Fatalf("session projection leaked source directory: %s", encoded)
	}
}

func TestPastedImageListDeduplicatesByContent(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := t.TempDir()
	first := filepath.Join(directory, "original.png")
	duplicate := filepath.Join(directory, "duplicate.png")
	writeImageAttachmentFixtureVariant(t, first, 3)
	writeImageAttachmentFixtureVariant(t, duplicate, 3)

	updated, cmd := m.Update(tea.PasteMsg{Content: fmt.Sprintf("%q\n%q", first, duplicate)})
	m = settleImageAttachmentCommands(t, updated.(*Model), cmd)
	if len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Name != "original.png" {
		t.Fatalf("content-addressed deduplication = %#v", m.pendingImages)
	}
}

func TestMixedImagePathAndTextPasteRemainsText(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "capture.png")
	writeImageAttachmentFixture(t, path)
	content := path + "\nplease compare this with the previous screen"

	updated, cmd := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if cmd != nil || len(m.pendingImages) != 0 || m.imageAttachRunning {
		t.Fatalf("mixed text was treated as images: cmd=%v pending=%d running=%v", cmd != nil, len(m.pendingImages), m.imageAttachRunning)
	}
	if m.input.Value() != content {
		t.Fatalf("mixed text paste = %q, want %q", m.input.Value(), content)
	}
}

func TestPastedImageListContinuesAfterPerFileValidationFailure(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := t.TempDir()
	first := filepath.Join(directory, "first.png")
	missing := filepath.Join(directory, "missing private.png")
	third := filepath.Join(directory, "third.png")
	writeImageAttachmentFixtureVariant(t, first, 4)
	writeImageAttachmentFixtureVariant(t, third, 5)

	content := fmt.Sprintf("%q\n%q\n%q", first, missing, third)
	updated, cmd := m.Update(tea.PasteMsg{Content: content})
	m = settleImageAttachmentCommands(t, updated.(*Model), cmd)

	if len(m.pendingImages) != 2 || m.pendingImages[0].Ref.Name != "first.png" || m.pendingImages[1].Ref.Name != "third.png" {
		t.Fatalf("partial image queue result = %#v", m.pendingImages)
	}
	if m.input.Value() != "" {
		t.Fatalf("failed list member leaked source paste into draft: %q", m.input.Value())
	}
	foundSafeError := false
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "missing private.png") {
			foundSafeError = true
		}
		if strings.Contains(entry.Content, directory) {
			t.Fatalf("per-file receipt leaked source directory: %#v", entry)
		}
	}
	if !foundSafeError {
		t.Fatalf("missing per-file validation receipt: %#v", m.entries)
	}
}

func TestPastedImageQueueIgnoresBusyAndStaleReceipts(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := t.TempDir()
	first := filepath.Join(directory, "first.png")
	second := filepath.Join(directory, "second.png")
	third := filepath.Join(directory, "third.png")
	writeImageAttachmentFixtureVariant(t, first, 6)
	writeImageAttachmentFixtureVariant(t, second, 7)
	writeImageAttachmentFixtureVariant(t, third, 8)

	updated, firstCmd := m.Update(tea.PasteMsg{Content: fmt.Sprintf("%q\n%q", first, second)})
	m = updated.(*Model)
	busy := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(m.beginImageFileAttachment(third, "")), time.Second)
	updated, _ = m.Update(busy)
	m = updated.(*Model)
	if !m.imageAttachRunning || len(m.imageAttachQueue) != 1 || len(m.pendingImages) != 0 {
		t.Fatalf("busy receipt changed active queue: running=%v queued=%d pending=%d", m.imageAttachRunning, len(m.imageAttachQueue), len(m.pendingImages))
	}

	firstReceipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(firstCmd), 2*time.Second)
	updated, secondCmd := m.Update(firstReceipt)
	m = updated.(*Model)
	if secondCmd == nil || !m.imageAttachRunning || len(m.pendingImages) != 1 {
		t.Fatalf("first receipt did not advance queue: cmd=%v running=%v pending=%d", secondCmd != nil, m.imageAttachRunning, len(m.pendingImages))
	}
	updated, staleCmd := m.Update(firstReceipt)
	m = updated.(*Model)
	if staleCmd != nil || !m.imageAttachRunning || len(m.pendingImages) != 1 {
		t.Fatalf("stale receipt changed second admission: cmd=%v running=%v pending=%d", staleCmd != nil, m.imageAttachRunning, len(m.pendingImages))
	}
	m = settleImageAttachmentCommands(t, m, secondCmd)
	if len(m.pendingImages) != 2 || m.pendingImages[1].Ref.Name != "second.png" {
		t.Fatalf("final image queue = %#v", m.pendingImages)
	}
}

func TestPastedImageQueueHonorsPendingLimit(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := t.TempDir()
	paths := make([]string, 5)
	quoted := make([]string, len(paths))
	for index := range paths {
		paths[index] = filepath.Join(directory, fmt.Sprintf("screen-%d.png", index+1))
		writeImageAttachmentFixtureVariant(t, paths[index], uint8(index+10))
		quoted[index] = fmt.Sprintf("%q", paths[index])
	}

	updated, cmd := m.Update(tea.PasteMsg{Content: strings.Join(quoted, "\n")})
	m = settleImageAttachmentCommands(t, updated.(*Model), cmd)
	if len(m.pendingImages) != maxPendingImages {
		t.Fatalf("pending image count = %d, want %d", len(m.pendingImages), maxPendingImages)
	}
	for index, attachment := range m.pendingImages {
		if attachment.Ref.Name != filepath.Base(paths[index]) {
			t.Fatalf("pending image %d = %q, want %q", index, attachment.Ref.Name, filepath.Base(paths[index]))
		}
	}
	foundLimit := false
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "1 file not queued") && strings.Contains(entry.Content, "at most 4") {
			foundLimit = true
		}
		if strings.Contains(entry.Content, directory) {
			t.Fatalf("limit receipt leaked source directory: %#v", entry)
		}
	}
	if !foundLimit || m.input.Value() != "" {
		t.Fatalf("limit result: found=%v draft=%q entries=%#v", foundLimit, m.input.Value(), m.entries)
	}
}

func TestPendingImageChromeStripsBidirectionalControls(t *testing.T) {
	m, _ := newImageTestModel(t)
	m.pendingImages = []pendingImageAttachment{{Ref: imageasset.Ref{
		Digest: strings.Repeat("a", 64), MIMEType: "image/png", Name: "screen\u202egnp.png",
		SizeBytes: 1, Width: 1, Height: 1,
	}}}

	for _, rendered := range []string{m.renderPendingImagesStatus(80), m.renderPlainImageList()} {
		plain := ansi.Strip(rendered)
		if strings.ContainsRune(plain, '\u202e') || !strings.Contains(plain, "screengnp.png") {
			t.Fatalf("image chrome retained visual-order control: %q", plain)
		}
	}
}

func TestCtrlVPastesClipboardImageIntoPrivateAttachmentStore(t *testing.T) {
	m, _ := newImageTestModel(t)
	rawPath := filepath.Join(t.TempDir(), "clipboard.png")
	raw := writeImageAttachmentFixture(t, rawPath)
	m.clipboardRead = func() (string, error) { return "", nil }
	m.clipboardImageRead = func(context.Context) (string, []byte, error) {
		return "clipboard.png", append([]byte(nil), raw...), nil
	}

	updated, cmd := m.Update(ctrlKey('v'))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("Ctrl+V did not inspect the clipboard")
	}
	message, ok := cmd().(ClipboardImagePasteMsg)
	if !ok || message.Err != nil || !bytes.Equal(message.Data, raw) {
		t.Fatalf("clipboard image receipt = %#v", message)
	}
	updated, cmd = m.Update(message)
	m = updated.(*Model)
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)

	if len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Name != "clipboard.png" || m.input.Value() != "" {
		t.Fatalf("clipboard image admission = pending=%#v draft=%q", m.pendingImages, m.input.Value())
	}
	if len(m.pendingImages[0].Image.Data) != 0 {
		t.Fatal("raw clipboard bytes survived in pending UI state")
	}
}

func TestInvalidImagePathPasteReportsWithoutLeakingSourcePath(t *testing.T) {
	m, _ := newImageTestModel(t)
	original := filepath.Join(t.TempDir(), "missing image.png")
	updated, cmd := m.Update(tea.PasteMsg{Content: `"` + original + `"`})
	m = updated.(*Model)
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)

	if len(m.pendingImages) != 0 || m.input.Value() != "" {
		t.Fatalf("failed image paste leaked into draft: pending=%d draft=%q", len(m.pendingImages), m.input.Value())
	}
	if len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "error" || !strings.Contains(m.entries[len(m.entries)-1].Content, "Attach image") {
		t.Fatalf("failed image admission has no receipt: %#v", m.entries)
	}
	if strings.Contains(m.entries[len(m.entries)-1].Content, filepath.Dir(original)) {
		t.Fatalf("failed image receipt leaked source path: %#v", m.entries[len(m.entries)-1])
	}
}

func TestImageTurnUsesTypedPayloadAndSafeTranscriptMetadata(t *testing.T) {
	m, store := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "checkout.png")
	raw := writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("Find the layout problem")

	cmd := m.submitInput()
	if cmd == nil || m.state != StateWaiting {
		t.Fatalf("image prompt did not start: cmd=%v state=%v", cmd != nil, m.state)
	}
	messages := m.agent.Messages()
	last := messages[len(messages)-1]
	if last.Role != "user" || last.Content != "Find the layout problem" || len(last.Images) != 1 || len(last.Images[0].Data) != 0 {
		t.Fatalf("provider message = %#v", last)
	}
	loaded, err := store.Load(context.Background(), m.turnImages[0].Ref)
	if err != nil || !bytes.Equal(loaded, raw) {
		t.Fatalf("private attachment payload was not recoverable: bytes=%d err=%v", len(loaded), err)
	}
	if len(m.pendingImages) != 0 || len(m.turnImages) != 1 {
		t.Fatalf("attachment ownership pending=%d turn=%d", len(m.pendingImages), len(m.turnImages))
	}
	entry := m.entries[len(m.entries)-1]
	if len(entry.Attachments) != 1 || strings.Contains(entry.Content, filepath.Dir(path)) {
		t.Fatalf("visible entry = %#v", entry)
	}
	plain := ansi.Strip(m.renderEntries())
	for _, want := range []string{"checkout.png", "18x12", entry.Attachments[0].Handle()} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered attachment omitted %q:\n%s", want, plain)
		}
	}
}

func TestQueuedFollowUpOwnsAndDisplaysItsImagesAtomically(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "queued.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	wantDigest := m.pendingImages[0].Ref.Digest
	m.state = StateStreaming
	m.input.SetValue("inspect after the current turn")

	m.queueComposerFollowUp()
	if m.queuedFollowUp == nil || len(m.queuedFollowUp.Images) != 1 || len(m.pendingImages) != 0 {
		t.Fatalf("queued image ownership = queue %#v pending %d", m.queuedFollowUp, len(m.pendingImages))
	}
	if got := m.queuedFollowUp.Images[0].Ref.Digest; got != wantDigest {
		t.Fatalf("queued digest = %q, want %q", got, wantDigest)
	}
	if row := ansi.Strip(m.renderQueuedFollowUp()); !strings.Contains(row, "+ 1 image") {
		t.Fatalf("queued receipt hid attachment count: %q", row)
	}

	if !m.editQueuedFollowUp() || len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Digest != wantDigest {
		t.Fatalf("editing queue did not restore exact attachment: pending=%#v", m.pendingImages)
	}
}

func TestQueuedImagesRestoreAfterFailureAndDispatchAfterSuccess(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		m, _ := newImageTestModel(t)
		path := filepath.Join(t.TempDir(), "failure.png")
		writeImageAttachmentFixture(t, path)
		attachImageFixture(t, m, path, "")
		wantDigest := m.pendingImages[0].Ref.Digest
		m.state = StateStreaming
		m.input.SetValue("retry with the image")
		m.queueComposerFollowUp()

		updated, _ := m.Update(AgentDoneMsg{Err: errors.New("provider failed")})
		m = updated.(*Model)
		if m.queuedFollowUp != nil || m.input.Value() != "retry with the image" || len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Digest != wantDigest {
			t.Fatalf("failed queue restore = queue %#v draft %q pending %#v", m.queuedFollowUp, m.input.Value(), m.pendingImages)
		}
	})

	t.Run("dispatch", func(t *testing.T) {
		m, _ := newImageTestModel(t)
		path := filepath.Join(t.TempDir(), "dispatch.png")
		writeImageAttachmentFixture(t, path)
		attachImageFixture(t, m, path, "")
		wantDigest := m.pendingImages[0].Ref.Digest
		m.state = StateStreaming
		m.input.SetValue("send with image")
		m.queueComposerFollowUp()
		m.state = StateIdle

		if cmd := m.dispatchQueuedFollowUp(); cmd == nil {
			t.Fatal("queued image turn did not dispatch")
		}
		if len(m.turnImages) != 1 || m.turnImages[0].Ref.Digest != wantDigest || len(m.pendingImages) != 0 {
			t.Fatalf("dispatched attachment ownership = turn %#v pending %#v", m.turnImages, m.pendingImages)
		}
	})
}

func TestImageAdmissionFinishingAfterQueueBindsToQueuedTurn(t *testing.T) {
	m, _ := newImageTestModel(t)
	firstPath := filepath.Join(t.TempDir(), "first.png")
	secondPath := filepath.Join(t.TempDir(), "second.png")
	writeImageAttachmentFixtureVariant(t, firstPath, 1)
	writeImageAttachmentFixtureVariant(t, secondPath, 2)
	attachImageFixture(t, m, firstPath, "")
	wantFirst := m.pendingImages[0].Ref.Digest

	m.state = StateStreaming
	lateCmd := m.beginImageFileAttachment(secondPath, "")
	m.input.SetValue("compare both images")
	m.queueComposerFollowUp()
	lateReceipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(lateCmd), 2*time.Second)
	wantSecond := lateReceipt.Ref.Digest
	updated, _ := m.Update(lateReceipt)
	m = updated.(*Model)

	if len(m.pendingImages) != 0 || m.queuedFollowUp == nil || len(m.queuedFollowUp.Images) != 2 {
		t.Fatalf("late attachment ownership = pending %d queue %#v", len(m.pendingImages), m.queuedFollowUp)
	}
	if got := []string{m.queuedFollowUp.Images[0].Ref.Digest, m.queuedFollowUp.Images[1].Ref.Digest}; got[0] != wantFirst || got[1] != wantSecond {
		t.Fatalf("queued digest order = %#v, want %#v", got, []string{wantFirst, wantSecond})
	}
	if row := ansi.Strip(m.renderQueuedFollowUp()); !strings.Contains(row, "+ 2 images") {
		t.Fatalf("late queued image count is hidden: %q", row)
	}
}

func TestImageAdmissionStartedBeforeRecoveryHoldStaysWithQueuedOwner(t *testing.T) {
	m, _ := newImageTestModel(t)
	firstPath := filepath.Join(t.TempDir(), "first.png")
	secondPath := filepath.Join(t.TempDir(), "second.png")
	writeImageAttachmentFixtureVariant(t, firstPath, 34)
	writeImageAttachmentFixtureVariant(t, secondPath, 35)
	attachImageFixture(t, m, firstPath, "")

	m.state = StateStreaming
	lateCmd := m.beginImageFileAttachment(secondPath, "")
	m.input.SetValue("queued owner")
	m.queueComposerFollowUp()
	m.queuedFollowUp.RecoveryHeld = true
	lateReceipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(lateCmd), 2*time.Second)
	updated, _ := m.Update(lateReceipt)
	m = updated.(*Model)

	if len(m.pendingImages) != 0 || len(m.queuedFollowUp.Images) != 2 {
		t.Fatalf("pre-hold admission changed owners: pending=%#v queue=%#v", m.pendingImages, m.queuedFollowUp)
	}
}

func TestPinnedTextModelRejectsImageBeforeDispatchAndPreservesDraft(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "capture.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.model = "text-model"
	m.modelPinned = true
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "text-model", Source: OllamaModelLocal, Selectable: true, Fit: true,
		Capabilities: []string{"completion", "tools"}, Current: true,
	}}
	m.input.SetValue("inspect it")

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("pinned non-vision model scheduled provider work")
	}
	if m.state != StateIdle || m.input.Value() != "inspect it" || len(m.pendingImages) != 1 || len(m.agent.Messages()) != 0 {
		t.Fatalf("rejected image turn state=%v draft=%q pending=%d messages=%d", m.state, m.input.Value(), len(m.pendingImages), len(m.agent.Messages()))
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "does not advertise vision") {
		t.Fatalf("missing capability receipt: %#v", m.entries)
	}
}

func TestUnpinnedImageTurnSelectsAdmittedVisionModel(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "capture.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.model = "text-model"
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "text-model", Source: OllamaModelLocal, Selectable: true, Fit: true, Capabilities: []string{"completion", "tools"}, Current: true},
		{Name: "vision-local", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion", "tools", "vision"}},
	}
	m.input.SetValue("inspect it")

	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("admitted vision model did not start image turn")
	}
	if m.model != "vision-local" || len(m.agent.Messages()) != 1 || len(m.agent.Messages()[0].Images) != 1 {
		t.Fatalf("vision routing model=%q messages=%#v", m.model, m.agent.Messages())
	}
}

func TestImageMetadataPersistsWithoutRawBytesAndRestoresReference(t *testing.T) {
	source, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "persist.png")
	raw := writeImageAttachmentFixture(t, path)
	attachImageFixture(t, source, path, "")
	attachment := source.pendingImages[0]
	if err := source.agent.AddUserMessageWithImages("inspect", []llm.ImageData{attachment.Image}); err != nil {
		t.Fatal(err)
	}
	source.entries = []ChatEntry{{Kind: "user", Content: "inspect", Attachments: []imageasset.Ref{attachment.Ref}}}

	encoded, err := encodeSessionState(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, string(raw)) || strings.Contains(encoded, base64.StdEncoding.EncodeToString(raw)) || strings.Contains(encoded, filepath.Dir(path)) {
		t.Fatalf("session leaked raw bytes or source path: %s", encoded)
	}
	if !strings.Contains(encoded, attachment.Ref.Digest) || !strings.Contains(encoded, `"images"`) {
		t.Fatalf("session omitted durable image reference: %s", encoded)
	}
	var state persistedSessionState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		t.Fatal(err)
	}
	target, _ := newImageTestModel(t)
	// Use the original object store so the Agent resolver can rehydrate this ref.
	target.SetImageStore(source.imageStore)
	if err := target.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	restored := target.agent.Messages()[0]
	if len(restored.Images) != 1 || restored.Images[0].SHA256 != attachment.Ref.Digest || len(restored.Images[0].Data) != 0 {
		t.Fatalf("restored image metadata = %#v", restored.Images)
	}
}

func TestImageResolutionPreflightFailureRestoresDraftAndAttachment(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "retry.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("inspect this layout")

	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("image prompt did not start")
	}
	if len(m.agent.Messages()) != 1 || len(m.turnImages) != 1 || len(m.pendingImages) != 0 {
		t.Fatalf("precondition messages=%d turn=%d pending=%d", len(m.agent.Messages()), len(m.turnImages), len(m.pendingImages))
	}

	updated, _ := m.Update(AgentDoneMsg{Err: fmt.Errorf("asset missing: %w", llm.ErrInferenceNotStarted)})
	m = updated.(*Model)
	if got := m.input.Value(); got != "inspect this layout" {
		t.Fatalf("restored draft = %q", got)
	}
	if len(m.pendingImages) != 1 || len(m.turnImages) != 0 || len(m.agent.Messages()) != 0 {
		t.Fatalf("rolled-back state messages=%d turn=%d pending=%d", len(m.agent.Messages()), len(m.turnImages), len(m.pendingImages))
	}
	for _, entry := range m.entries {
		if entry.Kind == "user" {
			t.Fatalf("pre-dispatch user entry remained visible: %#v", entry)
		}
	}
}

func TestImageResolutionPreflightErrorReceiptDoesNotBlockRollback(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "retry.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("inspect this layout")

	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("image prompt did not start")
	}
	updated, _ := m.Update(ErrorMsg{Msg: "LLM request not started: image asset missing"})
	m = updated.(*Model)
	updated, _ = m.Update(AgentDoneMsg{Err: fmt.Errorf("asset missing: %w", llm.ErrInferenceNotStarted)})
	m = updated.(*Model)

	if got := m.input.Value(); got != "inspect this layout" {
		t.Fatalf("restored draft = %q", got)
	}
	if len(m.pendingImages) != 1 || len(m.agent.Messages()) != 0 {
		t.Fatalf("rolled-back state messages=%d pending=%d", len(m.agent.Messages()), len(m.pendingImages))
	}
	for _, entry := range m.entries {
		if entry.Kind == "user" {
			t.Fatalf("pre-dispatch user entry remained after error receipt: %#v", m.entries)
		}
	}
}

func TestPreflightRejectedTurnKeepsRetryAndQueuedImageOwnersSeparate(t *testing.T) {
	m, _ := newImageTestModel(t)
	activePath := filepath.Join(t.TempDir(), "active.png")
	queuedPath := filepath.Join(t.TempDir(), "queued.png")
	writeImageAttachmentFixtureVariant(t, activePath, 31)
	writeImageAttachmentFixtureVariant(t, queuedPath, 32)
	attachImageFixture(t, m, activePath, "")
	activeDigest := m.pendingImages[0].Ref.Digest
	m.input.SetValue("retry the rejected turn")
	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("active image prompt did not start")
	}

	attachImageFixture(t, m, queuedPath, "")
	queuedDigest := m.pendingImages[0].Ref.Digest
	m.input.SetValue("then inspect the queued image")
	m.queueComposerFollowUp()

	updated, _ := m.Update(AgentDoneMsg{Err: fmt.Errorf("asset missing: %w", llm.ErrInferenceNotStarted)})
	m = updated.(*Model)
	if !m.queuedFollowUpHeld() {
		t.Fatalf("rejected turn queue was not held: %#v", m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "retry the rejected turn" {
		t.Fatalf("retry draft = %q", got)
	}
	if len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Digest != activeDigest {
		t.Fatalf("retry image owner = %#v", m.pendingImages)
	}
	if m.queuedFollowUp.Prompt != "then inspect the queued image" || len(m.queuedFollowUp.Images) != 1 || m.queuedFollowUp.Images[0].Ref.Digest != queuedDigest {
		t.Fatalf("queued image owner = %#v", m.queuedFollowUp)
	}
	if len(m.pendingImages) > maxPendingImages || len(m.queuedFollowUp.Images) > maxPendingImages {
		t.Fatalf("owner image caps exceeded: retry=%d queue=%d", len(m.pendingImages), len(m.queuedFollowUp.Images))
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"retry the rejected turn", "then inspect the queued image", "↑ swap"} {
		if !strings.Contains(view, want) {
			t.Fatalf("dual-owner footer omitted %q:\n%s", want, view)
		}
	}
	if cmd := m.dispatchQueuedFollowUp(); cmd != nil {
		t.Fatal("held recovery queue auto-dispatched")
	}

	updated, _ = m.Update(upKey())
	m = updated.(*Model)
	if m.input.Value() != "then inspect the queued image" || len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Digest != queuedDigest {
		t.Fatalf("first swap live owner = draft %q images %#v", m.input.Value(), m.pendingImages)
	}
	if !m.queuedFollowUpHeld() || m.queuedFollowUp.Prompt != "retry the rejected turn" || len(m.queuedFollowUp.Images) != 1 || m.queuedFollowUp.Images[0].Ref.Digest != activeDigest {
		t.Fatalf("first swap held owner = %#v", m.queuedFollowUp)
	}

	updated, _ = m.Update(upKey())
	m = updated.(*Model)
	if m.input.Value() != "retry the rejected turn" || len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Digest != activeDigest ||
		!m.queuedFollowUpHeld() || m.queuedFollowUp.Prompt != "then inspect the queued image" || len(m.queuedFollowUp.Images) != 1 || m.queuedFollowUp.Images[0].Ref.Digest != queuedDigest {
		t.Fatalf("second swap did not restore exact owners: live=%q pending=%#v queue=%#v", m.input.Value(), m.pendingImages, m.queuedFollowUp)
	}
}

func TestHeldQueueRoutesNewImageAdmissionToEditableDraft(t *testing.T) {
	m, _ := newImageTestModel(t)
	m.queuedFollowUp = &queuedFollowUp{
		Prompt:       "queued owner",
		Images:       []pendingImageAttachment{{Ref: imageasset.Ref{Digest: strings.Repeat("a", 64)}}},
		RecoveryHeld: true,
	}
	m.input.SetValue("editable retry")
	path := filepath.Join(t.TempDir(), "new-active.png")
	writeImageAttachmentFixtureVariant(t, path, 33)

	attachImageFixture(t, m, path, "")
	if len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Name != "new-active.png" {
		t.Fatalf("new admission did not follow editable owner: %#v", m.pendingImages)
	}
	if len(m.queuedFollowUp.Images) != 1 || m.queuedFollowUp.Images[0].Ref.Digest != strings.Repeat("a", 64) {
		t.Fatalf("new admission mutated held owner: %#v", m.queuedFollowUp)
	}
}

func TestQueueRestoreNeverMergesMoreThanFourImages(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < maxPendingImages; i++ {
		m.pendingImages = append(m.pendingImages, pendingImageAttachment{Ref: imageasset.Ref{Digest: fmt.Sprintf("active-%d", i)}})
	}
	m.queuedFollowUp = &queuedFollowUp{
		Prompt: "separate owner",
		Images: []pendingImageAttachment{{Ref: imageasset.Ref{Digest: "queued"}}},
	}

	m.restoreQueuedFollowUp()
	if len(m.pendingImages) != maxPendingImages {
		t.Fatalf("active owner image count = %d, want %d", len(m.pendingImages), maxPendingImages)
	}
	if !m.queuedFollowUpHeld() || len(m.queuedFollowUp.Images) != 1 {
		t.Fatalf("overflowing queue was merged or lost: %#v", m.queuedFollowUp)
	}
}

func TestContextCompactionReconcilesVisibleImageProjection(t *testing.T) {
	m, _ := newImageTestModel(t)
	first := imageasset.Ref{Digest: strings.Repeat("1", 64), MIMEType: "image/png", Name: "old.png", SizeBytes: 10, Width: 2, Height: 2}
	second := imageasset.Ref{Digest: strings.Repeat("2", 64), MIMEType: "image/png", Name: "recent.png", SizeBytes: 10, Width: 2, Height: 2}
	m.entries = []ChatEntry{
		{Kind: "user", Content: "old", Attachments: []imageasset.Ref{first}},
		{Kind: "assistant", Content: "old answer"},
		{Kind: "user", Content: "recent", Attachments: []imageasset.Ref{second}},
	}
	m.agent.ReplaceMessages([]llm.Message{
		{Role: "system", Content: "Conversation summary:\nold turn summarized"},
		{Role: "user", Content: "recent", Images: []llm.ImageData{{SHA256: second.Digest, Name: second.Name, MediaType: second.MIMEType, Size: second.SizeBytes, Width: second.Width, Height: second.Height}}},
	})

	updated, _ := m.Update(ContextCompactedMsg{})
	m = updated.(*Model)
	if len(m.entries[0].Attachments) != 0 || !reflect.DeepEqual(m.entries[2].Attachments, []imageasset.Ref{second}) {
		t.Fatalf("compacted image projection = %#v", m.entries)
	}
	if _, err := encodeSessionState(m); err != nil {
		t.Fatalf("compacted session no longer persists: %v", err)
	}
}

func TestPastedImagePathDetectionIsNarrow(t *testing.T) {
	tests := []struct {
		input string
		path  string
		ok    bool
	}{
		{input: `"/tmp/design review.png"`, path: "/tmp/design review.png", ok: true},
		{input: `/tmp/design\ review.jpg`, path: "/tmp/design review.jpg", ok: true},
		{input: `file:///tmp/capture%20one.gif`, path: "/tmp/capture one.gif", ok: true},
		{input: "first.png\nsecond.png"},
		{input: "/tmp/readme.md"},
		{input: "please inspect capture.png"},
		{input: "https://example.com/private.png"},
		{input: "s3://private-bucket/capture.jpg"},
	}
	for _, test := range tests {
		path, ok := pastedImagePath(test.input)
		if path != test.path || ok != test.ok {
			t.Errorf("pastedImagePath(%q) = %q, %v; want %q, %v", test.input, path, ok, test.path, test.ok)
		}
	}
}

func TestPastedImageFieldPathPreservesWindowsDrivePaths(t *testing.T) {
	path := `C:\screenshots\design.png`
	if got, ok := pastedImageFieldPath(path); !ok || got != path {
		t.Fatalf("Windows drive path = %q, %v; want %q, true", got, ok, path)
	}
}

func TestPastedImagePathsRecognizesOnlyCompleteImageLists(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
		ok    bool
	}{
		{
			name:  "newline and quoted spaces",
			input: `"/tmp/design one.png"` + "\n" + `'/tmp/design two.jpg'`,
			want:  []string{"/tmp/design one.png", "/tmp/design two.jpg"},
			ok:    true,
		},
		{
			name:  "file URLs",
			input: "file:///tmp/capture%20one.gif\nfile:///tmp/capture-two.jpeg",
			want:  []string{"/tmp/capture one.gif", "/tmp/capture-two.jpeg"},
			ok:    true,
		},
		{
			name:  "exact duplicate",
			input: "/tmp/one.png\n/tmp/one.png",
			want:  []string{"/tmp/one.png"},
			ok:    true,
		},
		{name: "mixed prose", input: "/tmp/one.png\nplease inspect this"},
		{name: "mixed extension", input: "/tmp/one.png\n/tmp/readme.txt"},
		{name: "remote file URL", input: "file://example.com/tmp/one.png"},
		{name: "https URL", input: "https://example.com/one.png"},
		{name: "too many fields", input: strings.Repeat("/tmp/one.png ", maxImagePathPasteFields+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths, ok := pastedImagePaths(test.input)
			if ok != test.ok || !reflect.DeepEqual(paths, test.want) {
				t.Fatalf("pastedImagePaths(%q) = %#v, %v; want %#v, %v", test.input, paths, ok, test.want, test.ok)
			}
		})
	}
}

func TestOversizedImageLookingPasteUsesBoundedTextReview(t *testing.T) {
	m, _ := newImageTestModel(t)
	content := strings.Repeat("/tmp/private-image.png ", maxImagePathPasteBytes/8)
	if len(content) <= maxImagePathPasteBytes {
		t.Fatal("oversized paste fixture is not oversized")
	}
	updated, cmd := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if cmd != nil || m.imageAttachRunning || len(m.imageAttachQueue) != 0 || len(m.pendingImages) != 0 {
		t.Fatalf("oversized text entered image admission: cmd=%v running=%v queued=%d pending=%d", cmd != nil, m.imageAttachRunning, len(m.imageAttachQueue), len(m.pendingImages))
	}
	if m.pendingPaste == nil || m.pendingPaste.PlainFits {
		t.Fatalf("oversized text did not reach the bounded paste review: %#v", m.pendingPaste)
	}
}

func TestImageBudgetRejectionStopsQueueBeforePublishing(t *testing.T) {
	m, store := newImageTestModel(t)
	history := make([]llm.ImageData, maxConversationImages)
	for index := range history {
		history[index] = llm.ImageData{
			SHA256: fmt.Sprintf("%064x", index+1), Name: fmt.Sprintf("history-%d.png", index+1),
			MediaType: "image/png", Size: 1, Width: 1, Height: 1,
		}
	}
	m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "history", Images: history}})

	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first rejected.png")
	secondPath := filepath.Join(directory, "second never read.png")
	firstData := writeImageAttachmentFixtureVariant(t, firstPath, 21)
	secondData := writeImageAttachmentFixtureVariant(t, secondPath, 22)

	updated, cmd := m.Update(tea.PasteMsg{Content: fmt.Sprintf("%q\n%q", firstPath, secondPath)})
	m = updated.(*Model)
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, next := m.Update(receipt)
	m = updated.(*Model)
	if next != nil || m.imageAttachRunning || len(m.imageAttachQueue) != 0 || len(m.pendingImages) != 0 {
		t.Fatalf("budget rejection did not stop queue: next=%v running=%v queued=%d pending=%d", next != nil, m.imageAttachRunning, len(m.imageAttachQueue), len(m.pendingImages))
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "conversation image budget") {
		t.Fatalf("budget rejection receipt = %#v", m.entries)
	}

	for name, data := range map[string][]byte{"first rejected.png": firstData, "second never read.png": secondData} {
		digest := sha256.Sum256(data)
		ref := imageasset.Ref{
			Digest: fmt.Sprintf("%x", digest), MIMEType: "image/png", Name: name,
			SizeBytes: int64(len(data)), Width: 18, Height: 12,
		}
		if _, err := store.Load(context.Background(), ref); err == nil {
			t.Fatalf("budget-rejected queue published %s", name)
		}
	}
}

func TestClipboardImageBudgetRejectionDoesNotPublish(t *testing.T) {
	m, store := newImageTestModel(t)
	history := make([]llm.ImageData, maxConversationImages)
	for index := range history {
		history[index] = llm.ImageData{
			SHA256: fmt.Sprintf("%064x", index+1), Name: fmt.Sprintf("history-%d.png", index+1),
			MediaType: "image/png", Size: 1, Width: 1, Height: 1,
		}
	}
	m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "history", Images: history}})

	path := filepath.Join(t.TempDir(), "clipboard.png")
	data := writeImageAttachmentFixtureVariant(t, path, 23)
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(m.beginImageBytesAttachment("clipboard.png", data)), 2*time.Second)
	updated, next := m.Update(receipt)
	m = updated.(*Model)
	if next != nil || m.imageAttachRunning || len(m.pendingImages) != 0 {
		t.Fatalf("clipboard budget rejection changed pending state: next=%v running=%v pending=%d", next != nil, m.imageAttachRunning, len(m.pendingImages))
	}
	digest := sha256.Sum256(data)
	ref := imageasset.Ref{
		Digest: fmt.Sprintf("%x", digest), MIMEType: "image/png", Name: "clipboard.png",
		SizeBytes: int64(len(data)), Width: 18, Height: 12,
	}
	if _, err := store.Load(context.Background(), ref); err == nil {
		t.Fatal("budget-rejected clipboard image was published")
	}
}

func TestManualOnlyCloudVisionModelIsNeverAutoSelected(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "cloud-boundary.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.model = "text-model"
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "text-model", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion"}, Current: true},
		{Name: "vision-cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, AutoRoutable: false, Capabilities: []string{"completion", "vision"}},
	}
	m.input.SetValue("inspect it")

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("manual-only cloud model was auto-selected")
	}
	if m.model != "text-model" || m.input.Value() != "inspect it" || len(m.pendingImages) != 1 {
		t.Fatalf("privacy rejection changed state: model=%q draft=%q pending=%d", m.model, m.input.Value(), len(m.pendingImages))
	}
	if got := m.entries[len(m.entries)-1].Content; !strings.Contains(got, "no admitted Ollama model advertises vision") {
		t.Fatalf("privacy rejection receipt = %q", got)
	}
}

func TestModelAutoDoesNotKeepManualCloudVisionModel(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "cloud-current.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.model = "vision-cloud"
	m.modelPinned = false
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "vision-cloud", Source: OllamaModelCloud, Selectable: true, Fit: true,
		AutoRoutable: false, Capabilities: []string{"completion", "vision"}, Current: true,
	}}
	m.input.SetValue("inspect it")

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("automatic routing retained a manual-only cloud model")
	}
	if m.model != "vision-cloud" || len(m.pendingImages) != 1 || m.input.Value() != "inspect it" {
		t.Fatalf("manual cloud rejection changed state: model=%q pending=%d draft=%q", m.model, len(m.pendingImages), m.input.Value())
	}
}

func TestVisionAutoRoutingRejectsNonLocalAutoDescriptor(t *testing.T) {
	m, _ := newImageTestModel(t)
	m.model = "text-model"
	m.modelPinned = false
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "text-model", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion"}, Current: true},
		{Name: "stale-cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion", "vision"}},
	}
	if err := m.ensureVisionModel(); err == nil || m.model != "text-model" {
		t.Fatalf("non-local AUTO vision descriptor was accepted: model=%q err=%v", m.model, err)
	}
}

func TestHistoricalImagesKeepLaterTurnsOnVisionModel(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "history.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("inspect the first view")
	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("first image turn did not start")
	}
	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)

	m.model = "text-model"
	m.modelPinned = false
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "text-model", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion"}, Current: true},
		{Name: "vision-local", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Capabilities: []string{"completion", "vision"}},
	}
	m.input.SetValue("now compare the spacing")
	if cmd := m.submitInput(); cmd == nil {
		t.Fatal("follow-up with historical image did not start")
	}
	if m.model != "vision-local" {
		t.Fatalf("historical image follow-up selected model %q", m.model)
	}
	if messages := m.agent.Messages(); len(messages) != 2 || len(messages[0].Images) != 1 || len(messages[1].Images) != 0 {
		t.Fatalf("historical image messages = %#v", messages)
	}
}

func TestStalePreflightReceiptCannotCancelNewImageAdmission(t *testing.T) {
	m, store := newImageTestModel(t)
	m.SetImageStore(nil)
	staleCmd := m.beginImageFileAttachment("missing.png", "missing.png")
	m.SetImageStore(store)
	path := filepath.Join(t.TempDir(), "current.png")
	writeImageAttachmentFixture(t, path)
	currentCmd := m.beginImageFileAttachment(path, "")
	if !m.imageAttachRunning {
		t.Fatal("new admission did not start")
	}

	stale := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(staleCmd), 2*time.Second)
	updated, _ := m.Update(stale)
	m = updated.(*Model)
	if !m.imageAttachRunning || len(m.pendingImages) != 0 {
		t.Fatalf("stale preflight changed current operation: running=%v pending=%d", m.imageAttachRunning, len(m.pendingImages))
	}

	current := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(currentCmd), 2*time.Second)
	updated, _ = m.Update(current)
	m = updated.(*Model)
	if m.imageAttachRunning || len(m.pendingImages) != 1 || m.pendingImages[0].Ref.Name != "current.png" {
		t.Fatalf("current admission did not settle: running=%v pending=%#v", m.imageAttachRunning, m.pendingImages)
	}
}

func TestEscapeCancelsPastedImagePathWithoutRestoringSourcePath(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "cancel me.png")
	writeImageAttachmentFixture(t, path)
	original := `"` + path + `"`
	updated, cmd := m.Update(tea.PasteMsg{Content: original})
	m = updated.(*Model)
	if !m.imageAttachRunning || cmd == nil {
		t.Fatal("pasted path did not start image admission")
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.imageAttachRunning || m.input.Value() != "" || len(m.imageAttachQueue) != 0 {
		t.Fatalf("cancelled image paste leaked into draft: running=%v queued=%d draft=%q", m.imageAttachRunning, len(m.imageAttachQueue), m.input.Value())
	}

	late := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(late)
	m = updated.(*Model)
	if len(m.pendingImages) != 0 || m.input.Value() != "" {
		t.Fatalf("late admission changed cancelled state: pending=%d draft=%q", len(m.pendingImages), m.input.Value())
	}
}

func TestImageAdmissionErrorDoesNotPersistSourceDirectory(t *testing.T) {
	m, _ := newImageTestModel(t)
	directory := filepath.Join(t.TempDir(), "private", "nested")
	path := filepath.Join(directory, "missing.png")
	updated, cmd := m.Update(tea.PasteMsg{Content: path})
	m = updated.(*Model)
	receipt := awaitCommandMessage[ImageAttachmentResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ = m.Update(receipt)
	m = updated.(*Model)
	if len(m.entries) == 0 || strings.Contains(m.entries[len(m.entries)-1].Content, directory) {
		t.Fatalf("attachment receipt leaked source directory: %#v", m.entries)
	}
	encoded, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, directory) {
		t.Fatalf("session state leaked source directory: %s", encoded)
	}
}

func TestImageConversationAggregateBudget(t *testing.T) {
	ref := func(index int, size int64, width, height int) imageasset.Ref {
		return imageasset.Ref{
			Digest: fmt.Sprintf("%064x", index+1), MIMEType: "image/png", Name: fmt.Sprintf("image-%d.png", index+1),
			SizeBytes: size, Width: width, Height: height,
		}
	}
	t.Run("bytes", func(t *testing.T) {
		refs := []imageasset.Ref{ref(0, 15<<20, 100, 100), ref(1, 15<<20, 100, 100), ref(2, 15<<20, 100, 100)}
		if err := validateImageConversationBudget(nil, refs); err == nil || !strings.Contains(err.Error(), "aggregate") {
			t.Fatalf("byte budget error = %v", err)
		}
	})
	t.Run("pixels", func(t *testing.T) {
		refs := []imageasset.Ref{ref(0, 1, 4_000, 4_000), ref(1, 1, 4_000, 4_000), ref(2, 1, 4_000, 4_000), ref(3, 1, 1_000, 1_000)}
		if err := validateImageConversationBudget(nil, refs); err == nil || !strings.Contains(err.Error(), "megapixel") {
			t.Fatalf("pixel budget error = %v", err)
		}
	})
}

func TestForgetHistoricalImagesPersistsRecovery(t *testing.T) {
	m, _ := newImageTestModel(t)
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.ReleaseExecutionSessionLease()
		_ = store.Close()
	})
	m.agent.SetWorkDir(t.TempDir())
	m.SetSessionStore(store)
	path := filepath.Join(t.TempDir(), "recover.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("inspect")
	if cmd := m.submitInput(); cmd == nil || m.sessionID <= 0 {
		t.Fatalf("durable image turn did not start: cmd=%v session=%d", cmd != nil, m.sessionID)
	}
	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	sessionID := m.sessionID

	m.forgetHistoricalImages()
	if len(m.agent.Messages()) != 1 || len(m.agent.Messages()[0].Images) != 0 || len(m.entries[0].Attachments) != 0 {
		t.Fatalf("historical images were not forgotten: messages=%#v entries=%#v", m.agent.Messages(), m.entries)
	}
	record, err := store.GetSessionStateRecord(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(record.StateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 1 || len(state.Messages[0].Images) != 0 || len(state.Entries[0].Attachments) != 0 {
		t.Fatalf("durable image recovery did not persist: %#v", state)
	}
}

func TestForgetHistoricalImagesRollsBackWhenSessionSaveFails(t *testing.T) {
	m, imageStore := newImageTestModel(t)
	client := &imageAttachmentTestClient{}
	m.agent = agent.New(client, nil, 16_384)
	m.SetImageStore(imageStore)
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.ReleaseExecutionSessionLease()
		_ = store.Close()
	})
	m.agent.SetWorkDir(t.TempDir())
	m.SetSessionStore(store)
	path := filepath.Join(t.TempDir(), "recover.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	m.input.SetValue("inspect")
	if cmd := m.submitInput(); cmd == nil || m.sessionID <= 0 {
		t.Fatalf("durable image turn did not start: cmd=%v session=%d", cmd != nil, m.sessionID)
	}
	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	wantPromptFloor := agent.ContextPromptFloor{
		Tokens: 12_000, HostTokens: 1_000, MessageTokens: 200, Model: client.Model(),
	}
	if err := m.agent.RestoreContextPromptFloor(wantPromptFloor); err != nil {
		t.Fatal(err)
	}
	beforeMessages := m.agent.Messages()
	beforeAttachments := append([]imageasset.Ref(nil), m.entries[0].Attachments...)
	recordBefore, err := store.GetSessionStateRecord(context.Background(), m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	m.resetSessionStateRevision()

	m.forgetHistoricalImages()
	if !reflect.DeepEqual(m.agent.Messages(), beforeMessages) || !reflect.DeepEqual(m.entries[0].Attachments, beforeAttachments) {
		t.Fatalf("failed forget did not restore live state: messages=%#v entries=%#v", m.agent.Messages(), m.entries)
	}
	if got := m.agent.ContextPromptFloor(); got != wantPromptFloor {
		t.Fatalf("failed forget rollback lost context prompt floor: got %#v, want %#v", got, wantPromptFloor)
	}
	recordAfter, err := store.GetSessionStateRecord(context.Background(), m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if recordAfter.Revision != recordBefore.Revision || recordAfter.StateJSON != recordBefore.StateJSON {
		t.Fatal("failed forget changed durable session state")
	}
}

func TestForgetHistoricalImagesRepairsOrphanedVisibleBadge(t *testing.T) {
	m, _ := newImageTestModel(t)
	ref := imageasset.Ref{Digest: strings.Repeat("3", 64), MIMEType: "image/png", Name: "old.png", SizeBytes: 10, Width: 2, Height: 2}
	m.entries = []ChatEntry{{Kind: "user", Content: "old", Attachments: []imageasset.Ref{ref}}}
	m.agent.ReplaceMessages([]llm.Message{{Role: "system", Content: "Conversation summary:\nold image turn"}})

	m.forgetHistoricalImages()
	if len(m.entries[0].Attachments) != 0 {
		t.Fatalf("orphaned badge was not cleared: %#v", m.entries)
	}
}

func TestSessionRejectsInvisibleImageProjection(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "hidden.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	image := m.pendingImages[0].Image
	_, err := marshalPersistedSessionState(persistedSessionState{
		Version:  currentPersistedSessionVersion,
		Messages: []llm.Message{{Role: "user", Content: "inspect", Images: []llm.ImageData{image}}},
		Entries:  []persistedChatEntry{{Kind: "user", Content: "inspect"}},
		Mode:     ModeNormal,
	})
	if err == nil || !strings.Contains(err.Error(), "projection is inconsistent") {
		t.Fatalf("invisible image projection error = %v", err)
	}
}

func TestSessionRejectsImageProjectionMovedToDifferentPrompt(t *testing.T) {
	m, _ := newImageTestModel(t)
	path := filepath.Join(t.TempDir(), "hidden.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	image := m.pendingImages[0].Image
	ref := m.pendingImages[0].Ref
	_, err := marshalPersistedSessionState(persistedSessionState{
		Version: currentPersistedSessionVersion,
		Messages: []llm.Message{
			{Role: "user", Content: "first prompt", Images: []llm.ImageData{image}},
			{Role: "user", Content: "second prompt"},
		},
		Entries: []persistedChatEntry{
			{Kind: "user", Content: "first prompt"},
			{Kind: "user", Content: "second prompt", Attachments: []imageasset.Ref{ref}},
		},
		Mode: ModeNormal,
	})
	if err == nil || !strings.Contains(err.Error(), "projection is inconsistent") {
		t.Fatalf("moved image projection error = %v", err)
	}
}

func TestImageOnlySessionTitleUsesAttachmentName(t *testing.T) {
	images := []pendingImageAttachment{{Ref: imageasset.Ref{Name: "architecture.png"}}, {Ref: imageasset.Ref{Name: "details.png"}}}
	if got, want := imageOnlySessionTitle(images), "Images · architecture.png +1"; got != want {
		t.Fatalf("image-only session title = %q, want %q", got, want)
	}
}

func TestImageOnlyTurnCreatesDescriptiveDurableSessionTitle(t *testing.T) {
	m, _ := newImageTestModel(t)
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.ReleaseExecutionSessionLease()
		_ = store.Close()
	})
	m.agent.SetWorkDir(t.TempDir())
	m.SetSessionStore(store)
	path := filepath.Join(t.TempDir(), "architecture.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")

	if cmd := m.submitInput(); cmd == nil || m.sessionID <= 0 {
		t.Fatalf("image-only turn did not create a session: cmd=%v session=%d", cmd != nil, m.sessionID)
	}
	if got, want := m.activeSessionTitle, "Image · architecture.png"; got != want {
		t.Fatalf("active image-only title = %q, want %q", got, want)
	}
	session, err := store.GetSession(context.Background(), m.sessionID)
	if err != nil || session.Title != m.activeSessionTitle {
		t.Fatalf("durable image-only session = %#v, error %v", session, err)
	}
}
