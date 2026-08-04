package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	maxPendingImages           = 4
	maxConversationImages      = 12
	maxConversationImageBytes  = 40 << 20
	maxConversationImagePixels = 48_000_000
	maxImagePathPasteBytes     = 32 << 10
	maxImagePathPasteFields    = 32
)

var (
	errImageAttachmentBusy      = errors.New("another image attachment is still being validated")
	errImageStoreUnavailable    = errors.New("private image storage is unavailable")
	errPendingImageLimitReached = errors.New("pending image limit reached")
	errImageConversationBudget  = errors.New("active conversation image budget reached")
)

// pendingImageAttachment keeps the provider payload paired with its durable,
// path-free reference. Raw bytes never enter ChatEntry or session JSON.
type pendingImageAttachment struct {
	Ref   imageasset.Ref
	Image llm.ImageData
}

// imageFileAttachmentRequest is intentionally ephemeral. Source paths live
// only long enough to feed the private image store; transcript and session
// projections receive the resulting path-free Ref instead.
type imageFileAttachmentRequest struct {
	Path string
}

// ImageAttachmentResultMsg completes one tokened asynchronous image admission.
// Fallback is reserved for explicit single-file callers; terminal PasteMsg
// image lists deliberately leave it empty so source paths never become prompt
// or session text after an admission failure.
type ImageAttachmentResultMsg struct {
	Token     uint64
	Preflight bool
	Name      string
	Ref       imageasset.Ref
	Image     llm.ImageData
	Fallback  string
	Err       error
}

// SetImageStore installs the private attachment store and the Agent resolver
// used to rehydrate path-free image references after session/checkpoint restore.
// It must be called before the Bubble Tea program starts.
func (m *Model) SetImageStore(store *imageasset.Store) {
	m.imageStore = store
	if m.agent == nil {
		return
	}
	if store == nil {
		m.agent.SetImageResolver(nil)
		return
	}
	m.agent.SetImageResolver(func(ctx context.Context, image llm.ImageData) ([]byte, error) {
		return store.Load(ctx, imageasset.Ref{
			Digest: image.SHA256, MIMEType: image.MediaType, Name: image.Name,
			SizeBytes: image.Size, Width: image.Width, Height: image.Height,
		})
	})
}

func (m *Model) beginImageFileAttachment(path, fallback string) tea.Cmd {
	if m.imageAttachRunning {
		token := m.imageAttachToken
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: llm.SanitizeImageName(path), Err: errImageAttachmentBusy}
		}
	}
	return m.startImageFileAttachment(path, fallback)
}

// beginPastedImageFileAttachments admits one complete path-shaped paste as an
// ordered, bounded queue. Once the parent recognizes the whole payload as an
// image-path list it never re-inserts source paths into prompt text, including
// when an individual file fails validation.
func (m *Model) beginPastedImageFileAttachments(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}
	if m.imageAttachRunning {
		token := m.imageAttachToken
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: llm.SanitizeImageName(paths[0]), Err: errImageAttachmentBusy}
		}
	}

	remaining := maxPendingImages - len(m.pendingImages)
	if remaining < 0 {
		remaining = 0
	}
	queued := min(len(paths), remaining)
	for _, path := range paths[:queued] {
		m.imageAttachQueue = append(m.imageAttachQueue, imageFileAttachmentRequest{Path: path})
	}
	if skipped := len(paths) - queued; skipped > 0 {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: imageAttachmentQueueLimitReceipt(skipped)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		m.recalcViewportHeight()
	}
	return m.startNextImageFileAttachment()
}

func (m *Model) startNextImageFileAttachment() tea.Cmd {
	if m.imageAttachRunning || len(m.imageAttachQueue) == 0 {
		return nil
	}
	request := m.imageAttachQueue[0]
	m.imageAttachQueue[0] = imageFileAttachmentRequest{}
	m.imageAttachQueue = m.imageAttachQueue[1:]
	return m.startImageFileAttachment(request.Path, "")
}

func (m *Model) startImageFileAttachment(path, fallback string) tea.Cmd {
	m.imageAttachToken++
	token := m.imageAttachToken
	displayName := llm.SanitizeImageName(path)
	if len(m.pendingImages) >= maxPendingImages {
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: displayName, Fallback: fallback, Err: errPendingImageLimitReached}
		}
	}
	if m.imageStore == nil {
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: displayName, Fallback: fallback, Err: errImageStoreUnavailable}
		}
	}
	m.imageAttachRunning = true
	m.imageAttachFallback = fallback
	m.input.Blur()
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) && m.agent != nil && strings.TrimSpace(m.agent.WorkDir()) != "" {
		path = filepath.Join(m.agent.WorkDir(), path)
	}
	if m.imageAttachCancel != nil {
		m.imageAttachCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	m.imageAttachCancel = cancel
	store := m.imageStore
	checkBudget := m.imageAdmissionBudgetCheck()
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		ref, err := store.AdmitFileChecked(ctx, path, checkBudget)
		if err != nil {
			return ImageAttachmentResultMsg{Token: token, Name: displayName, Fallback: fallback, Err: err}
		}
		image := llm.ImageData{
			SHA256: ref.Digest, Name: ref.Name, MediaType: ref.MIMEType,
			Size: ref.SizeBytes, Width: ref.Width, Height: ref.Height,
		}
		if err := image.ValidateReference(); err != nil {
			return ImageAttachmentResultMsg{Token: token, Name: ref.Name, Fallback: fallback, Err: err}
		}
		return ImageAttachmentResultMsg{Token: token, Name: ref.Name, Ref: ref, Image: image, Fallback: fallback}
	})
}

func (m *Model) beginImageBytesAttachment(name string, data []byte) tea.Cmd {
	if m.imageAttachRunning {
		token := m.imageAttachToken
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: llm.SanitizeImageName(name), Err: errImageAttachmentBusy}
		}
	}
	m.imageAttachToken++
	token := m.imageAttachToken
	displayName := llm.SanitizeImageName(name)
	if displayName == "" {
		displayName = "clipboard.png"
	}
	if m.imageStore == nil {
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: displayName, Err: errImageStoreUnavailable}
		}
	}
	if len(m.pendingImages) >= maxPendingImages {
		return func() tea.Msg {
			return ImageAttachmentResultMsg{Token: token, Preflight: true, Name: displayName, Err: errPendingImageLimitReached}
		}
	}
	if m.imageAttachCancel != nil {
		m.imageAttachCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	m.imageAttachCancel = cancel
	m.imageAttachRunning = true
	m.imageAttachFallback = ""
	m.input.Blur()
	store := m.imageStore
	payload := append([]byte(nil), data...)
	checkBudget := m.imageAdmissionBudgetCheck()
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		ref, err := store.AdmitBytesChecked(ctx, displayName, payload, checkBudget)
		if err != nil {
			return ImageAttachmentResultMsg{Token: token, Name: displayName, Err: err}
		}
		image := llm.ImageData{
			SHA256: ref.Digest, Name: ref.Name, MediaType: ref.MIMEType,
			Size: ref.SizeBytes, Width: ref.Width, Height: ref.Height,
		}
		if err := image.ValidateReference(); err != nil {
			return ImageAttachmentResultMsg{Token: token, Name: ref.Name, Err: err}
		}
		return ImageAttachmentResultMsg{Token: token, Name: ref.Name, Ref: ref, Image: image}
	})
}

func (m *Model) handleImageAttachmentResult(message ImageAttachmentResultMsg) tea.Cmd {
	if message.Token == 0 || message.Token != m.imageAttachToken || (!message.Preflight && !m.imageAttachRunning) {
		return nil
	}
	if !message.Preflight {
		if m.imageAttachCancel != nil {
			m.imageAttachCancel()
		}
		m.imageAttachCancel = nil
		m.imageAttachRunning = false
		m.imageAttachFallback = ""
	}
	if m.shuttingDown {
		m.clearImageAttachmentQueue()
		return nil
	}
	// Attachment admission can restore a fallback paste and open its review
	// owner. It also changes pending composer state and focus, so settle search
	// first even when the receipt is an ordinary success.
	m.preemptTranscriptSearch()

	stopQueue := false
	if message.Err != nil {
		stopQueue = errors.Is(message.Err, errImageConversationBudget)
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: imageAttachmentErrorReceipt(message.Name, message.Err)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		m.recalcViewportHeight()
	} else {
		target := &m.pendingImages
		if m.queuedFollowUpOwnsImageAdmission(message.Token) {
			target = &m.queuedFollowUp.Images
		}
		duplicate := false
		for _, existing := range *target {
			if existing.Ref.Digest == message.Ref.Digest {
				duplicate = true
				break
			}
		}
		if !duplicate {
			refs := attachmentRefs(*target)
			refs = append(refs, message.Ref)
			if err := validateImageConversationBudget(m.agent.Messages(), refs); err != nil {
				stopQueue = true
				m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Attach image " + sanitizeTerminalSingleLine(message.Ref.Name) + ": " + err.Error()})
				m.invalidateEntryCache()
				m.refreshTranscript()
				m.gotoBottomIfFollowing()
			} else {
				*target = append(*target, pendingImageAttachment{Ref: message.Ref, Image: message.Image})
			}
		}
	}
	m.recalcViewportHeight()
	if stopQueue {
		m.clearImageAttachmentQueue()
	}

	if message.Preflight {
		next := m.startNextImageFileAttachment()
		if next == nil && !m.imageAttachRunning {
			m.releaseQueuedImageAdmissionOwnership()
		}
		if next == nil && !m.imageAttachRunning {
			m.input.Focus()
		}
		if message.Fallback != "" && next == nil && m.composerEditable() {
			return m.insertPasteWithReview(message.Fallback)
		}
		return next
	}
	next := m.startNextImageFileAttachment()
	if next == nil && !m.imageAttachRunning {
		m.releaseQueuedImageAdmissionOwnership()
	}
	if next == nil {
		m.input.Focus()
	}
	if message.Fallback != "" && next == nil && m.composerEditable() {
		return m.insertPasteWithReview(message.Fallback)
	}
	return next
}

func imageAttachmentQueueLimitReceipt(skipped int) string {
	return fmt.Sprintf("Attach images: %d file%s not queued; the pending prompt supports at most %d images.", skipped, pluralSuffix(skipped), maxPendingImages)
}

func imageAttachmentErrorReceipt(name string, err error) string {
	name = llm.SanitizeImageName(name)
	prefix := "Attach image"
	if name != "" {
		prefix += " " + sanitizeTerminalSingleLine(name)
	}
	var reason string
	switch {
	case errors.Is(err, errImageAttachmentBusy):
		reason = "another image is still being validated"
	case errors.Is(err, errImageStoreUnavailable):
		reason = "private image storage is unavailable"
	case errors.Is(err, errPendingImageLimitReached):
		reason = fmt.Sprintf("the pending prompt already has %d images; send it or run /image clear", maxPendingImages)
	case errors.Is(err, errImageConversationBudget):
		reason = "the active conversation image budget was reached; run /image forget-history before attaching more images"
	case errors.Is(err, os.ErrNotExist):
		reason = "file was not found"
	case errors.Is(err, os.ErrPermission):
		reason = "file could not be read"
	case errors.Is(err, safeio.ErrTooLarge):
		reason = "file exceeds the 20 MiB limit"
	case errors.Is(err, safeio.ErrNotRegular):
		reason = "path is not a regular file"
	case errors.Is(err, safeio.ErrReadBusy):
		reason = "image reader is busy; try again"
	case errors.Is(err, safeio.ErrReadTimeout), errors.Is(err, context.DeadlineExceeded):
		reason = "validation timed out; try again"
	case errors.Is(err, context.Canceled):
		reason = "attachment was cancelled"
	case errors.Is(err, imageasset.ErrUnsupportedFormat):
		reason = "file is not a supported PNG, JPEG, or GIF"
	case errors.Is(err, imageasset.ErrInvalidDimensions):
		reason = "image dimensions exceed the supported limits"
	case errors.Is(err, imageasset.ErrIntegrity):
		reason = "private copy failed integrity verification"
	default:
		reason = "file could not be validated"
	}
	return prefix + ": " + reason
}

func (m *Model) insertPasteWithReview(content string) tea.Cmd {
	draft := m.input.Value()
	cursor := pasteCursorAt(draft, m.input.Line(), m.input.Column())
	assessment := assessPaste(content, cursor, m.input.Length(), m.input.LineCount(), m.input.CharLimit)
	if !assessment.PlainFits || assessment.NeedsReview {
		m.pendingPaste = assessment
		m.recalcViewportHeight()
		return nil
	}
	m.clearCompletionSuppression()
	m.input.InsertString(assessment.Content)
	m.syncInputHeight()
	return m.reflowInputViewport()
}

func (m *Model) clearPendingImages() int {
	count := len(m.pendingImages)
	for index := range m.pendingImages {
		m.pendingImages[index].Image = llm.ImageData{}
	}
	m.pendingImages = nil
	m.recalcViewportHeight()
	return count
}

func (m *Model) clearImageAttachmentQueue() {
	for index := range m.imageAttachQueue {
		m.imageAttachQueue[index] = imageFileAttachmentRequest{}
	}
	m.imageAttachQueue = nil
	m.releaseQueuedImageAdmissionOwnership()
}

func (m *Model) forgetHistoricalImages() tea.Cmd {
	if m.agent == nil {
		return nil
	}
	beforeMessages := m.agent.Messages()
	beforePromptFloor := m.agent.ContextPromptFloor()
	beforeEntries := append([]ChatEntry(nil), m.entries...)
	messages := cloneMessagesWithoutImageData(beforeMessages)
	removed := 0
	for index := range messages {
		removed += len(messages[index].Images)
		messages[index].Images = nil
	}
	visibleRemoved := 0
	for index := range m.entries {
		visibleRemoved += len(m.entries[index].Attachments)
	}
	if removed == 0 && visibleRemoved == 0 {
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No historical image references are present. Pending prompt images were left unchanged."})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.resumeFollow()
		return nil
	}

	m.agent.ReplaceMessagesWithinSession(messages)
	for index := range m.entries {
		m.entries[index].Attachments = nil
	}
	reported := removed
	if reported == 0 {
		reported = visibleRemoved
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf(
		"Forgot %d historical image reference%s. Pending prompt images and private cached objects were left unchanged.",
		reported, pluralSuffix(reported),
	)})
	if m.sessionID > 0 && m.sessionStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := m.persistSessionState(ctx)
		cancel()
		if err != nil {
			rollbackErr := m.agent.RestoreMessagesWithinSession(beforeMessages, beforePromptFloor)
			m.entries = beforeEntries
			message := "Forget image history: saved session was not changed: " + sanitizeTerminalSingleLine(err.Error())
			if rollbackErr != nil {
				message += " (rollback: " + sanitizeTerminalSingleLine(rollbackErr.Error()) + ")"
			}
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: message})
		}
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
	m.recalcViewportHeight()
	return nil
}

func cloneMessagesWithoutImageData(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := append([]llm.Message(nil), messages...)
	for index := range cloned {
		cloned[index].Images = append([]llm.ImageData(nil), messages[index].Images...)
		for imageIndex := range cloned[index].Images {
			cloned[index].Images[imageIndex].Data = nil
		}
	}
	return cloned
}

func (m *Model) pendingImageRefs() []imageasset.Ref {
	refs := make([]imageasset.Ref, len(m.pendingImages))
	for index := range m.pendingImages {
		refs[index] = m.pendingImages[index].Ref
	}
	return refs
}

func (m *Model) imageAdmissionBudgetCheck() func(imageasset.Ref) error {
	history := []llm.Message(nil)
	if m.agent != nil {
		history = m.agent.Messages()
	}
	pending := m.pendingImageRefs()
	return func(candidate imageasset.Ref) error {
		for _, existing := range pending {
			if existing.Digest == candidate.Digest {
				return nil
			}
		}
		refs := append([]imageasset.Ref(nil), pending...)
		refs = append(refs, candidate)
		if err := validateImageConversationBudget(history, refs); err != nil {
			return fmt.Errorf("%w: %v", errImageConversationBudget, err)
		}
		return nil
	}
}

func clonePendingImages(images []pendingImageAttachment) []pendingImageAttachment {
	if len(images) == 0 {
		return nil
	}
	result := make([]pendingImageAttachment, len(images))
	for index, attachment := range images {
		result[index] = attachment
		result[index].Image.Data = append([]byte(nil), attachment.Image.Data...)
	}
	return result
}

func (m *Model) restoreTurnImages() {
	if len(m.turnImages) == 0 {
		return
	}
	// A failed pre-dispatch turn returns attachments ahead of anything the user
	// may already have prepared for the next prompt, while deduplicating by the
	// durable digest.
	combined := clonePendingImages(m.turnImages)
	seen := make(map[string]struct{}, len(combined))
	for _, attachment := range combined {
		seen[attachment.Ref.Digest] = struct{}{}
	}
	for _, attachment := range m.pendingImages {
		if _, duplicate := seen[attachment.Ref.Digest]; duplicate {
			continue
		}
		combined = append(combined, attachment)
	}
	m.pendingImages = combined
	m.turnImages = nil
}

func (m *Model) renderImageAttachmentSummary(refs []imageasset.Ref, width int) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		label := fmt.Sprintf("%s · %dx%d · %s", ref.Name, ref.Width, ref.Height, ref.Handle())
		parts = append(parts, sanitizeTerminalSingleLine(label))
	}
	return m.styles.StatusText.Render(wrapText("  image · "+strings.Join(parts, "  |  "), max(1, width)))
}

func (m *Model) renderPendingImagesStatus(width int) string {
	if len(m.pendingImages) == 0 {
		return ""
	}
	names := make([]string, 0, len(m.pendingImages))
	for _, attachment := range m.pendingImages {
		names = append(names, sanitizeTerminalSingleLine(attachment.Ref.Name))
	}
	sort.Strings(names)
	detail := fmt.Sprintf("%d/%d · %s", len(names), maxPendingImages, strings.Join(names, ", "))
	titleLimit := 0
	if width >= 72 {
		titleLimit = 24
	}
	if session := sessionDisplayLabel(m.sessionPublicID, m.activeSessionTitle, titleLimit); session != "" {
		detail = session + " · " + detail
	}
	return m.renderDecisionPrompt(
		"Images ready", truncateDisplay(detail, max(1, width-18)),
		keyHint{Key: "/image clear", Action: "remove"},
	)
}

func (m *Model) renderPlainImageList() string {
	if len(m.pendingImages) == 0 {
		return "No images are attached to the pending prompt."
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d pending image attachment%s:\n", len(m.pendingImages), pluralSuffix(len(m.pendingImages)))
	for _, attachment := range m.pendingImages {
		name := sanitizeTerminalSingleLine(attachment.Ref.Name)
		fmt.Fprintf(&builder, "- %s · %dx%d · %d bytes · %s\n",
			name, attachment.Ref.Width, attachment.Ref.Height,
			attachment.Ref.SizeBytes, attachment.Ref.Handle())
	}
	return strings.TrimRight(builder.String(), "\n")
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func imageOnlySessionTitle(images []pendingImageAttachment) string {
	if len(images) == 0 {
		return "Image review"
	}
	prefix := "Image · "
	if len(images) > 1 {
		prefix = "Images · "
	}
	title := prefix + images[0].Ref.Name
	if len(images) > 1 {
		title += fmt.Sprintf(" +%d", len(images)-1)
	}
	return sanitizeTerminalSingleLine(title)
}

func pastedImagePaths(content string) ([]string, bool) {
	if len(content) > maxImagePathPasteBytes {
		return nil, false
	}
	content = strings.TrimSpace(canonicalPasteContent(content))
	if len(content) > maxImagePathPasteBytes {
		return nil, false
	}
	if content == "" {
		return nil, false
	}
	fields, err := splitQuotedFields(content)
	if err != nil || len(fields) == 0 || len(fields) > maxImagePathPasteFields {
		return nil, false
	}

	paths := make([]string, 0, min(len(fields), maxPendingImages))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		path, ok := pastedImageFieldPath(field)
		if !ok {
			return nil, false
		}
		key := filepath.Clean(path)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		paths = append(paths, path)
	}
	return paths, len(paths) > 0
}

func pastedImageFieldPath(field string) (string, bool) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(field), "file:") {
		parsed, err := url.Parse(field)
		if err != nil || !strings.EqualFold(parsed.Scheme, "file") || parsed.Host != "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", false
		}
		field, err = url.PathUnescape(parsed.EscapedPath())
		if err != nil || !filepath.IsAbs(field) {
			return "", false
		}
	} else if imagePathHasURIScheme(field) {
		return "", false
	}
	if field == "" || strings.ContainsFunc(field, unicode.IsControl) {
		return "", false
	}
	switch strings.ToLower(filepath.Ext(field)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return field, true
	default:
		return "", false
	}
}

func imagePathHasURIScheme(value string) bool {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 {
		return false
	}
	// Keep Windows drive-qualified paths local even when parsing on Unix.
	if colon == 1 && len(value) > 2 && isASCIIAlpha(value[0]) && (value[2] == '/' || value[2] == '\\') {
		return false
	}
	if !isASCIIAlpha(value[0]) {
		return false
	}
	for index := 1; index < colon; index++ {
		character := value[index]
		if !isASCIIAlpha(character) && (character < '0' || character > '9') && character != '+' && character != '-' && character != '.' {
			return false
		}
	}
	return true
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func pastedImagePath(content string) (string, bool) {
	paths, ok := pastedImagePaths(content)
	if !ok || len(paths) != 1 {
		return "", false
	}
	return paths[0], true
}

func attachmentRefs(images []pendingImageAttachment) []imageasset.Ref {
	refs := make([]imageasset.Ref, len(images))
	for index := range images {
		refs[index] = images[index].Ref
	}
	return refs
}

func attachmentData(images []pendingImageAttachment) []llm.ImageData {
	data := make([]llm.ImageData, len(images))
	for index := range images {
		data[index] = images[index].Image
		data[index].Data = append([]byte(nil), images[index].Image.Data...)
	}
	return data
}

func imageRefsFromMessages(images []llm.ImageData) []imageasset.Ref {
	refs := make([]imageasset.Ref, 0, len(images))
	for _, image := range images {
		if image.ValidateReference() != nil {
			continue
		}
		refs = append(refs, imageasset.Ref{
			Digest: image.SHA256, MIMEType: image.MediaType, Name: image.Name,
			SizeBytes: image.Size, Width: image.Width, Height: image.Height,
		})
	}
	return refs
}

type imageConversationUsage struct {
	count  int
	bytes  int64
	pixels uint64
}

func (usage *imageConversationUsage) add(ref imageasset.Ref) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}
	usage.count++
	usage.bytes += ref.SizeBytes
	usage.pixels += uint64(ref.Width) * uint64(ref.Height)
	if usage.count > maxConversationImages {
		return fmt.Errorf("the active conversation already carries %d images (limit %d); run /image forget-history", usage.count, maxConversationImages)
	}
	if usage.bytes > maxConversationImageBytes {
		return fmt.Errorf("active image context exceeds the %d MiB aggregate limit; run /image forget-history", maxConversationImageBytes>>20)
	}
	if usage.pixels > maxConversationImagePixels {
		return fmt.Errorf("active image context exceeds the %d-megapixel aggregate limit; run /image forget-history", maxConversationImagePixels/1_000_000)
	}
	return nil
}

func validateImageConversationBudget(messages []llm.Message, pending []imageasset.Ref) error {
	var usage imageConversationUsage
	for _, message := range messages {
		for _, ref := range imageRefsFromMessages(message.Images) {
			if err := usage.add(ref); err != nil {
				return err
			}
		}
	}
	for _, ref := range pending {
		if err := usage.add(ref); err != nil {
			return err
		}
	}
	return nil
}

func messagesRequireVision(messages []llm.Message) bool {
	for _, message := range messages {
		if len(message.Images) > 0 {
			return true
		}
	}
	return false
}

func (m *Model) currentModelSupportsVision() bool {
	descriptor, ok := m.ollamaModelDescriptor(m.model)
	if !ok || !hasOllamaCapability(descriptor.Capabilities, "vision") {
		return false
	}
	return m.modelPinned || (descriptor.Source == OllamaModelLocal && descriptor.AutoRoutable)
}

func (m *Model) ensureVisionModel() error {
	if m.currentModelSupportsVision() {
		return nil
	}
	if m.modelPinned {
		return fmt.Errorf("model %q is pinned but does not advertise vision; choose a vision model with Ctrl+P", sanitizeTerminalSingleLine(m.model))
	}
	candidates := append([]OllamaModelDescriptor(nil), m.ollamaModels...)
	sort.SliceStable(candidates, func(i, j int) bool {
		iLocal := candidates[i].Source == OllamaModelLocal
		jLocal := candidates[j].Source == OllamaModelLocal
		if iLocal != jLocal {
			return iLocal
		}
		if candidates[i].AutoRoutable != candidates[j].AutoRoutable {
			return candidates[i].AutoRoutable
		}
		return candidates[i].Name < candidates[j].Name
	})
	for _, candidate := range candidates {
		if candidate.Source != OllamaModelLocal || !candidate.Selectable || !candidate.Fit || !candidate.AutoRoutable || candidate.RequiresConsent ||
			!hasOllamaCapability(candidate.Capabilities, "vision") {
			continue
		}
		if m.modelManager != nil {
			m.prepareModelSwitch()
			if err := m.modelManager.SetCurrentModel(candidate.Name); err != nil {
				continue
			}
		}
		m.setCurrentModelProjection(candidate.Name)
		for index := range m.ollamaModels {
			m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(candidate.Name)
		}
		return nil
	}
	return fmt.Errorf("no admitted Ollama model advertises vision; open Ctrl+P and install or select a vision-capable model")
}
