package ui

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxOutputDetailSourceBytes = 512 * 1024
	maxOutputDetailSourceRows  = 10_000
	maxOutputDetailPageBytes   = 64 * 1024
	maxOutputDetailPageRows    = 256
	maxOutputDetailStoreBytes  = 8 * 1024 * 1024
	maxOutputDetailStoreRefs   = 64
	// Admission scans and sanitizes the complete source to produce honest
	// totals. Rejecting larger inputs before any copy gives that working set a
	// hard bound; the ordinary transcript preview remains independently
	// context-capped by the agent.
	maxOutputDetailAdmissionBytes = 8 * 1024 * 1024
)

var (
	// ErrOutputDetailUnavailable deliberately conflates unknown, stale, and
	// evicted references. Callers must not infer whether a full result ever
	// existed from this transient viewer boundary.
	ErrOutputDetailUnavailable = errors.New("output detail is unavailable")
	// ErrOutputDetailCursor rejects cursors that do not identify a retained
	// row boundary. It never returns source content alongside the error.
	ErrOutputDetailCursor = errors.New("output detail cursor is invalid")
	// ErrOutputDetailPageBudget reports that the requested byte budget cannot
	// contain even the next complete UTF-8 rune. Returning an error avoids an
	// unchanged cursor loop and never emits malformed or missing source bytes.
	ErrOutputDetailPageBudget = errors.New("output detail page byte budget is too small")
	// ErrOutputDetailAdmissionLimit declines an ephemeral viewer capability
	// before sanitization when a source exceeds the bounded admission working
	// set. It does not affect the already-capped transcript receipt.
	ErrOutputDetailAdmissionLimit = errors.New("output detail exceeds the admission limit")
)

// OutputDetailRef is an opaque, process-local capability for one admitted
// output. Its identity is intentionally unexported, so encoding a bare ref
// cannot persist or disclose the capability. Fields that carry one must still
// use json:"-" to make the ephemeral ownership explicit.
type OutputDetailRef struct {
	id string
}

// String returns the opaque identity for in-process diagnostics and equality
// assertions. It must not be written to a session or transcript.
func (ref OutputDetailRef) String() string {
	return ref.id
}

// Valid reports whether ref has the bounded shape issued by this package. A
// valid shape is not proof that the store still owns the referenced output.
func (ref OutputDetailRef) Valid() bool {
	return validTranscriptID(ref.id)
}

// MarshalJSON makes the non-persistence contract explicit even when a caller
// encodes a bare ref outside OutputDetailReceipt. A capability can never be
// reconstructed from the resulting empty object.
func (ref OutputDetailRef) MarshalJSON() ([]byte, error) {
	return []byte("{}"), nil
}

// OutputDetailDigest is the only durable shape associated with full output.
// Counts describe terminal-safe UTF-8 after control-sequence sanitization.
// Total counts cover the complete sanitized source; retained counts cover the
// bounded prefix that may be paged during this process.
type OutputDetailDigest struct {
	TotalRows     uint64 `json:"total_rows"`
	RetainedRows  uint64 `json:"retained_rows"`
	TotalBytes    uint64 `json:"total_bytes"`
	RetainedBytes uint64 `json:"retained_bytes"`
	Truncated     bool   `json:"truncated"`
}

// Valid treats a decoded digest as untrusted scalar input. It accepts the
// useful zero shape, rejects impossible row/byte relationships, and enforces
// the production retention caps without granting loadability.
func (digest OutputDetailDigest) Valid() bool {
	if digest.RetainedRows > digest.TotalRows ||
		digest.RetainedBytes > digest.TotalBytes ||
		digest.RetainedRows > maxOutputDetailSourceRows ||
		digest.RetainedBytes > maxOutputDetailSourceBytes {
		return false
	}
	if (digest.TotalRows == 0) != (digest.TotalBytes == 0) {
		return false
	}
	if (digest.RetainedRows == 0) != (digest.RetainedBytes == 0) {
		// The one exception is a retained empty first row whose source contains
		// bytes beyond the prefix (for example, a row-cap cut before "\n").
		if digest.TotalRows == 0 ||
			digest.TotalRows <= digest.RetainedRows ||
			digest.RetainedRows != 1 ||
			digest.RetainedBytes != 0 {
			return false
		}
	}
	if digest.TotalRows > digest.TotalBytes &&
		digest.TotalRows-digest.TotalBytes > 1 {
		return false
	}
	if digest.RetainedRows > digest.RetainedBytes &&
		digest.RetainedRows-digest.RetainedBytes > 1 {
		return false
	}
	if digest.TotalRows > 0 && digest.RetainedRows == 0 {
		return false
	}
	isTruncated := digest.RetainedRows < digest.TotalRows ||
		digest.RetainedBytes < digest.TotalBytes
	return digest.Truncated == isTruncated
}

// OutputDetailReceipt pairs the ephemeral load capability with its persistable
// scalar digest. Artifact digests are intentionally not accepted by this
// store: knowing that an artifact exists does not grant output loadability.
type OutputDetailReceipt struct {
	Ref    OutputDetailRef    `json:"-"`
	Digest OutputDetailDigest `json:"digest"`
}

// OutputDetailCursor addresses a byte offset within one retained logical row.
// ByteOffset is needed because a single row may exceed the 64 KiB page budget.
type OutputDetailCursor struct {
	Row        int
	ByteOffset int
}

// OutputDetailPageRequest asks for a bounded page. Non-positive limits select
// the store defaults; larger limits are clamped to the hard page maxima.
type OutputDetailPageRequest struct {
	Ref       OutputDetailRef
	Cursor    OutputDetailCursor
	RowLimit  int
	ByteLimit int
}

// OutputDetailRow is one whole row or one UTF-8-safe fragment of a giant row.
// EndsRow means the fragment reaches the retained row boundary.
// SourceRowComplete additionally distinguishes a complete source row from the
// final partial row produced by source truncation.
type OutputDetailRow struct {
	Index             int
	Text              string
	StartsMidRow      bool
	EndsRow           bool
	SourceRowComplete bool
}

// OutputDetailPage contains only terminal-safe retained content. Next is valid
// when HasMore is true. Bytes counts Text bytes in Rows; newline delimiters are
// represented by row boundaries and therefore are not charged twice.
type OutputDetailPage struct {
	Rows    []OutputDetailRow
	Next    OutputDetailCursor
	HasMore bool
	Bytes   uint64
	Digest  OutputDetailDigest
}

type outputDetailLimits struct {
	sourceBytes int
	sourceRows  int
	pageBytes   int
	pageRows    int
	storeBytes  int
	storeRefs   int
}

var defaultOutputDetailLimits = outputDetailLimits{
	sourceBytes: maxOutputDetailSourceBytes,
	sourceRows:  maxOutputDetailSourceRows,
	pageBytes:   maxOutputDetailPageBytes,
	pageRows:    maxOutputDetailPageRows,
	storeBytes:  maxOutputDetailStoreBytes,
	storeRefs:   maxOutputDetailStoreRefs,
}

type outputDetailRowSpan struct {
	start          uint32
	end            uint32
	sourceComplete bool
}

type outputDetailEntry struct {
	ref    OutputDetailRef
	text   string
	rows   []outputDetailRowSpan
	digest OutputDetailDigest
	charge int
	lru    *list.Element
}

// OutputDetailStore owns bounded full-output prefixes for the lifetime of the
// active UI. Entries are immutable after admission; the mutex protects the
// map, aggregate byte accounting, and LRU order.
type OutputDetailStore struct {
	mu           sync.RWMutex
	limits       outputDetailLimits
	entries      map[OutputDetailRef]*outputDetailEntry
	lru          list.List
	chargedBytes int
}

// NewOutputDetailStore constructs a process-local store with the fixed
// production budgets documented above.
func NewOutputDetailStore() *OutputDetailStore {
	return newOutputDetailStore(defaultOutputDetailLimits)
}

func newOutputDetailStore(limits outputDetailLimits) *OutputDetailStore {
	limits = normalizeOutputDetailLimits(limits)
	return &OutputDetailStore{
		limits:  limits,
		entries: make(map[OutputDetailRef]*outputDetailEntry),
	}
}

// Admit sanitizes raw before retaining any bytes, records honest counts for
// the complete sanitized source, then admits only its bounded prefix.
func (store *OutputDetailStore) Admit(raw string) (OutputDetailReceipt, error) {
	if store == nil {
		return OutputDetailReceipt{}, ErrOutputDetailUnavailable
	}
	if len(raw) > maxOutputDetailAdmissionBytes {
		return OutputDetailReceipt{}, ErrOutputDetailAdmissionLimit
	}

	safe := sanitizeTerminalMultiline(raw)
	totalRows := outputDetailRowCount(safe)
	retained, retainedRows, finalRowComplete := retainOutputDetailPrefix(
		safe,
		store.limits.sourceBytes,
		store.limits.sourceRows,
	)
	digest := OutputDetailDigest{
		TotalRows:     uint64(totalRows),
		RetainedRows:  uint64(retainedRows),
		TotalBytes:    uint64(len(safe)),
		RetainedBytes: uint64(len(retained)),
		Truncated:     len(retained) != len(safe) || retainedRows != totalRows,
	}

	id, err := newTranscriptID("output_")
	if err != nil {
		return OutputDetailReceipt{}, fmt.Errorf("generate output detail reference: %w", err)
	}
	ref := OutputDetailRef{id: id}
	entry := &outputDetailEntry{
		ref:    ref,
		text:   retained,
		rows:   outputDetailRowSpans(retained, retainedRows, finalRowComplete),
		digest: digest,
	}
	entry.charge = outputDetailEntryCharge(entry)

	store.mu.Lock()
	defer store.mu.Unlock()
	if _, collision := store.entries[ref]; collision {
		return OutputDetailReceipt{}, errors.New("output detail reference collision")
	}
	entry.lru = store.lru.PushFront(entry)
	store.entries[ref] = entry
	store.chargedBytes += entry.charge
	store.evictLocked()

	if _, retainedByStore := store.entries[ref]; !retainedByStore {
		return OutputDetailReceipt{}, ErrOutputDetailUnavailable
	}
	return OutputDetailReceipt{Ref: ref, Digest: digest}, nil
}

// Page resolves one process-local ref into a bounded UTF-8-safe page. Unknown,
// stale, and evicted refs fail closed without a partial page. Cancellation is
// checked before lookup, during construction, and immediately before return.
func (store *OutputDetailStore) Page(
	ctx context.Context,
	request OutputDetailPageRequest,
) (OutputDetailPage, error) {
	if store == nil || ctx == nil || !request.Ref.Valid() {
		return OutputDetailPage{}, ErrOutputDetailUnavailable
	}
	if err := ctx.Err(); err != nil {
		return OutputDetailPage{}, err
	}
	if request.Cursor.Row < 0 || request.Cursor.ByteOffset < 0 {
		return OutputDetailPage{}, ErrOutputDetailCursor
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[request.Ref]
	if !ok {
		return OutputDetailPage{}, ErrOutputDetailUnavailable
	}
	if !validOutputDetailCursor(entry, request.Cursor) {
		return OutputDetailPage{}, ErrOutputDetailCursor
	}

	rowLimit := boundedOutputDetailLimit(request.RowLimit, store.limits.pageRows)
	byteLimit := boundedOutputDetailLimit(request.ByteLimit, store.limits.pageBytes)
	page, err := buildOutputDetailPage(ctx, entry, request.Cursor, rowLimit, byteLimit)
	if err != nil {
		return OutputDetailPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return OutputDetailPage{}, err
	}
	store.lru.MoveToFront(entry.lru)
	return page, nil
}

// Drop revokes one ephemeral ref and releases its retained bytes.
func (store *OutputDetailStore) Drop(ref OutputDetailRef) bool {
	if store == nil || !ref.Valid() {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[ref]
	if !ok {
		return false
	}
	store.removeLocked(entry)
	return true
}

// Available reports whether the store still owns ref. A valid reference shape
// is not sufficient because LRU eviction and explicit revocation are normal.
// This method never reveals content or refreshes recency.
func (store *OutputDetailStore) Available(ref OutputDetailRef) bool {
	if store == nil || !ref.Valid() {
		return false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	_, ok := store.entries[ref]
	return ok
}

// Len reports the current number of live ephemeral refs.
func (store *OutputDetailStore) Len() int {
	if store == nil {
		return 0
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.entries)
}

// RetainedBytes reports the conservative aggregate memory charge used by the
// global budget. It includes source bytes, the compact row-offset index, and a
// fixed allowance for the entry, ref, map, and LRU bookkeeping.
func (store *OutputDetailStore) RetainedBytes() uint64 {
	if store == nil {
		return 0
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return uint64(store.chargedBytes)
}

func normalizeOutputDetailLimits(limits outputDetailLimits) outputDetailLimits {
	if limits.sourceBytes <= 0 {
		limits.sourceBytes = defaultOutputDetailLimits.sourceBytes
	}
	if limits.sourceRows <= 0 {
		limits.sourceRows = defaultOutputDetailLimits.sourceRows
	}
	if limits.pageBytes <= 0 {
		limits.pageBytes = defaultOutputDetailLimits.pageBytes
	}
	if limits.pageRows <= 0 {
		limits.pageRows = defaultOutputDetailLimits.pageRows
	}
	if limits.storeBytes <= 0 {
		limits.storeBytes = defaultOutputDetailLimits.storeBytes
	}
	if limits.storeRefs <= 0 {
		limits.storeRefs = defaultOutputDetailLimits.storeRefs
	}
	// Production already has sourceBytes < storeBytes. Keeping the invariant in
	// reduced test configurations prevents a newly admitted entry from being
	// immediately evicted solely because it can never fit.
	if limits.sourceBytes > limits.storeBytes {
		limits.sourceBytes = limits.storeBytes
	}
	if limits.pageBytes > limits.sourceBytes {
		limits.pageBytes = limits.sourceBytes
	}
	return limits
}

func outputDetailRowCount(value string) int {
	if value == "" {
		return 0
	}
	rows := 1
	for index := range len(value) {
		if value[index] == '\n' {
			rows++
		}
	}
	return rows
}

func retainOutputDetailPrefix(value string, byteLimit, rowLimit int) (string, int, bool) {
	if value == "" {
		return "", 0, true
	}

	cut := len(value)
	if rows := outputDetailRowCount(value); rows > rowLimit {
		newlines := 0
		for index := range len(value) {
			if value[index] != '\n' {
				continue
			}
			newlines++
			if newlines == rowLimit {
				cut = index
				break
			}
		}
	}
	if cut > byteLimit {
		cut = byteLimit
	}
	for cut > 0 && cut < len(value) && !utf8.RuneStart(value[cut]) {
		cut--
	}

	retained := value[:cut]
	if retained != "" {
		// A caller may itself supply a small substring backed by a much larger
		// allocation. The store's byte accounting is a memory boundary, so
		// detach every admitted value, not only prefixes truncated here.
		retained = strings.Clone(retained)
	}
	retainedRows := outputDetailRowCount(retained)
	if retainedRows == 0 {
		// A bounded prefix may represent an admitted empty first row even when
		// its delimiter or first rune lies just beyond the byte/row cut.
		retainedRows = 1
	}
	finalRowComplete := cut == len(value) || (cut < len(value) && value[cut] == '\n')
	return retained, retainedRows, finalRowComplete
}

func outputDetailRowSpans(value string, rows int, finalRowComplete bool) []outputDetailRowSpan {
	if rows == 0 {
		return nil
	}
	spans := make([]outputDetailRowSpan, 0, rows)
	start := 0
	for index := range len(value) {
		if value[index] != '\n' {
			continue
		}
		spans = append(spans, outputDetailRowSpan{
			start: uint32(start), end: uint32(index), sourceComplete: true,
		})
		start = index + 1
	}
	spans = append(spans, outputDetailRowSpan{
		start: uint32(start), end: uint32(len(value)), sourceComplete: finalRowComplete,
	})
	return spans
}

func validOutputDetailCursor(entry *outputDetailEntry, cursor OutputDetailCursor) bool {
	if cursor.Row > len(entry.rows) {
		return false
	}
	if cursor.Row == len(entry.rows) {
		return cursor.ByteOffset == 0
	}
	row := entry.rows[cursor.Row]
	rowBytes := int(row.end - row.start)
	if cursor.ByteOffset > rowBytes {
		return false
	}
	if rowBytes > 0 && cursor.ByteOffset == rowBytes {
		// Store-generated cursors advance to the next row after consuming the
		// final byte. Reject an ambiguous forged cursor at the old row end.
		return false
	}
	if cursor.ByteOffset == 0 {
		return true
	}
	return utf8.RuneStart(entry.text[int(row.start)+cursor.ByteOffset])
}

func boundedOutputDetailLimit(requested, hard int) int {
	if requested <= 0 || requested > hard {
		return hard
	}
	return requested
}

func buildOutputDetailPage(
	ctx context.Context,
	entry *outputDetailEntry,
	cursor OutputDetailCursor,
	rowLimit int,
	byteLimit int,
) (OutputDetailPage, error) {
	page := OutputDetailPage{
		Rows:   make([]OutputDetailRow, 0, min(rowLimit, len(entry.rows)-cursor.Row)),
		Next:   cursor,
		Digest: entry.digest,
	}
	if cursor.Row == len(entry.rows) {
		return page, nil
	}

	rowIndex := cursor.Row
	byteOffset := cursor.ByteOffset
	pageBytes := 0
	for rowIndex < len(entry.rows) && len(page.Rows) < rowLimit {
		if len(page.Rows)%32 == 0 {
			if err := ctx.Err(); err != nil {
				return OutputDetailPage{}, err
			}
		}

		span := entry.rows[rowIndex]
		rowText := entry.text[int(span.start):int(span.end)]
		remaining := byteLimit - pageBytes
		if remaining <= 0 {
			break
		}

		tail := rowText[byteOffset:]
		take := len(tail)
		if take > remaining {
			take = utf8SafePrefixBytes(tail, remaining)
			if take == 0 {
				if len(page.Rows) > 0 {
					break
				}
				return OutputDetailPage{}, ErrOutputDetailPageBudget
			}
		}
		endOffset := byteOffset + take
		endsRow := endOffset == len(rowText)
		page.Rows = append(page.Rows, OutputDetailRow{
			Index:             rowIndex,
			Text:              tail[:take],
			StartsMidRow:      byteOffset > 0,
			EndsRow:           endsRow,
			SourceRowComplete: endsRow && span.sourceComplete,
		})
		pageBytes += take
		if endsRow {
			rowIndex++
			byteOffset = 0
		} else {
			byteOffset = endOffset
		}
	}

	page.Next = OutputDetailCursor{Row: rowIndex, ByteOffset: byteOffset}
	page.HasMore = rowIndex < len(entry.rows)
	page.Bytes = uint64(pageBytes)
	return page, nil
}

func utf8SafePrefixBytes(value string, limit int) int {
	if len(value) <= limit {
		return len(value)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return cut
}

func (store *OutputDetailStore) evictLocked() {
	for len(store.entries) > store.limits.storeRefs ||
		store.chargedBytes > store.limits.storeBytes {
		element := store.lru.Back()
		if element == nil {
			return
		}
		entry, ok := element.Value.(*outputDetailEntry)
		if !ok {
			store.lru.Remove(element)
			continue
		}
		store.removeLocked(entry)
	}
}

func (store *OutputDetailStore) removeLocked(entry *outputDetailEntry) {
	delete(store.entries, entry.ref)
	if entry.lru != nil {
		store.lru.Remove(entry.lru)
		entry.lru = nil
	}
	store.chargedBytes -= entry.charge
	if store.chargedBytes < 0 {
		store.chargedBytes = 0
	}
}

const (
	// Two uint32 offsets plus the completion bit occupy twelve bytes in the Go
	// layout used here. Charge that compact index explicitly rather than
	// allowing 64 refs × 10k rows to sit outside the 8 MiB budget.
	outputDetailRowSpanChargeBytes = 12
	// Conservatively accounts for the entry object, opaque string ref, map
	// bucket share, and list element. Exact allocator overhead is runtime-
	// specific; the budget needs a stable upper estimate, not byte accounting
	// suitable for a heap profiler.
	outputDetailEntryOverheadBytes = 128
)

func outputDetailEntryCharge(entry *outputDetailEntry) int {
	if entry == nil {
		return 0
	}
	return len(entry.text) +
		len(entry.rows)*outputDetailRowSpanChargeBytes +
		outputDetailEntryOverheadBytes
}
