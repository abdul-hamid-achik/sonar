package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
	"unsafe"
)

func TestOutputDetailStoreSanitizesBeforeRetentionAndPersistsOnlyDigest(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	raw := "\x1b]0;spoof\x07first\x1b[31m red\x1b[0m\u202esecond\r\nthird\x00"
	const want = "first redsecond\nthird"

	receipt, err := store.Admit(raw)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !receipt.Ref.Valid() || receipt.Ref.String() == "" {
		t.Fatalf("Admit returned invalid ref: %#v", receipt.Ref)
	}
	if receipt.Digest.TotalBytes != uint64(len(want)) ||
		receipt.Digest.RetainedBytes != uint64(len(want)) {
		t.Fatalf("byte digest = %#v, want %d safe bytes", receipt.Digest, len(want))
	}
	if receipt.Digest.TotalRows != 2 || receipt.Digest.RetainedRows != 2 ||
		receipt.Digest.Truncated {
		t.Fatalf("row digest = %#v, want two untruncated rows", receipt.Digest)
	}

	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := outputDetailPageText(page); got != want {
		t.Fatalf("page text = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"\x1b", "\x07", "\u202e", "\x00", "spoof"} {
		if strings.Contains(outputDetailPageText(page), forbidden) {
			t.Errorf("page retained forbidden terminal content %q", forbidden)
		}
	}

	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if strings.Contains(string(encoded), receipt.Ref.String()) ||
		strings.Contains(string(encoded), `"Ref"`) ||
		strings.Contains(string(encoded), `"ref"`) {
		t.Fatalf("persisted receipt leaked ephemeral ref: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"total_rows":2`) {
		t.Fatalf("persisted receipt omitted scalar digest: %s", encoded)
	}
	bareRef, err := json.Marshal(receipt.Ref)
	if err != nil {
		t.Fatalf("marshal bare ref: %v", err)
	}
	if string(bareRef) != "{}" {
		t.Fatalf("bare opaque ref encoded as %s, want {}", bareRef)
	}
}

func TestOutputDetailDigestCountsCompleteSanitizedSourceBeforeTruncation(t *testing.T) {
	t.Parallel()

	store := newOutputDetailStore(outputDetailLimits{
		sourceBytes: 13,
		sourceRows:  3,
		pageBytes:   64,
		pageRows:    256,
		storeBytes:  1024,
		storeRefs:   8,
	})
	const source = "one\ntwo\nthree\nfour\nfive"
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantDigest := OutputDetailDigest{
		TotalRows: 5, RetainedRows: 3,
		TotalBytes: uint64(len(source)), RetainedBytes: uint64(len("one\ntwo\nthree")),
		Truncated: true,
	}
	if receipt.Digest != wantDigest {
		t.Fatalf("digest = %#v, want %#v", receipt.Digest, wantDigest)
	}

	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := outputDetailPageText(page); got != "one\ntwo\nthree" {
		t.Fatalf("retained page = %q", got)
	}
	if len(page.Rows) != 3 || !page.Rows[2].SourceRowComplete {
		t.Fatalf("row-limit prefix must end on a complete third row: %#v", page.Rows)
	}
}

func TestOutputDetailStoreRejectsOversizedAdmissionBeforeRetention(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	raw := strings.Repeat("x", maxOutputDetailAdmissionBytes+1)
	receipt, err := store.Admit(raw)
	if !errors.Is(err, ErrOutputDetailAdmissionLimit) ||
		receipt != (OutputDetailReceipt{}) {
		t.Fatalf("oversized admission = (%#v, %v)", receipt, err)
	}
	if store.Len() != 0 || store.RetainedBytes() != 0 {
		t.Fatalf("oversized admission mutated store: refs=%d bytes=%d",
			store.Len(), store.RetainedBytes())
	}
}

func TestRetainedOutputDetailPrefixDetachesTruncatedSourceBacking(t *testing.T) {
	t.Parallel()

	source := strings.Repeat("large-output-row\n", 128*1024)
	retained, rows, complete := retainOutputDetailPrefix(source, 1024, 4)
	if retained != "large-output-row\nlarge-output-row\nlarge-output-row\nlarge-output-row" ||
		rows != 4 || !complete {
		t.Fatalf("retained prefix = %q, rows=%d complete=%t", retained, rows, complete)
	}
	if unsafe.StringData(retained) == unsafe.StringData(source) {
		t.Fatal("truncated prefix retained the complete source backing array")
	}

	backing := strings.Repeat("b", 4*maxOutputDetailSourceBytes)
	smallSubstring := backing[:32]
	retained, rows, complete = retainOutputDetailPrefix(smallSubstring, 64, 4)
	if retained != smallSubstring || rows != 1 || !complete {
		t.Fatalf("untruncated substring = %q, rows=%d complete=%t", retained, rows, complete)
	}
	if unsafe.StringData(retained) == unsafe.StringData(smallSubstring) {
		t.Fatal("untruncated substring retained its caller-owned backing array")
	}
}

func TestOutputDetailDigestValidRejectsUntrustedImpossibleShapes(t *testing.T) {
	t.Parallel()

	valid := []OutputDetailDigest{
		{},
		{TotalRows: 1, RetainedRows: 1, TotalBytes: 4, RetainedBytes: 4},
		{TotalRows: 5, RetainedRows: 3, TotalBytes: 20, RetainedBytes: 12, Truncated: true},
		{TotalRows: 2, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 0, Truncated: true},
	}
	for index, digest := range valid {
		if !digest.Valid() {
			t.Errorf("valid digest %d rejected: %#v", index, digest)
		}
	}

	invalid := []OutputDetailDigest{
		{TotalRows: 1},
		{TotalBytes: 1},
		{TotalRows: 1, RetainedRows: 1, TotalBytes: 1, Truncated: true},
		{TotalRows: 1, TotalBytes: 1},
		{TotalRows: 1, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 2},
		{TotalRows: 1, RetainedRows: 2, TotalBytes: 1, RetainedBytes: 1},
		{TotalRows: 1, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 1, Truncated: true},
		{TotalRows: 2, RetainedRows: 1, TotalBytes: 2, RetainedBytes: 1},
		{TotalRows: 4, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 1, Truncated: true},
		{
			TotalRows: maxOutputDetailSourceRows + 1, RetainedRows: maxOutputDetailSourceRows + 1,
			TotalBytes: maxOutputDetailSourceRows, RetainedBytes: maxOutputDetailSourceRows,
		},
		{
			TotalRows: 1, RetainedRows: 1,
			TotalBytes: maxOutputDetailSourceBytes + 1, RetainedBytes: maxOutputDetailSourceBytes + 1,
		},
	}
	for index, digest := range invalid {
		if digest.Valid() {
			t.Errorf("invalid digest %d accepted: %#v", index, digest)
		}
	}
}

func TestOutputDetailSourceByteLimitPreservesUTF8AndMarksPartialRow(t *testing.T) {
	t.Parallel()

	store := newOutputDetailStore(outputDetailLimits{
		sourceBytes: 7,
		sourceRows:  10,
		pageBytes:   7,
		pageRows:    10,
		storeBytes:  512,
		storeRefs:   4,
	})
	const source = "αβγδε"
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if receipt.Digest.TotalBytes != uint64(len(source)) ||
		receipt.Digest.RetainedBytes != uint64(len("αβγ")) ||
		!receipt.Digest.Truncated {
		t.Fatalf("digest = %#v", receipt.Digest)
	}
	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Text != "αβγ" ||
		!page.Rows[0].EndsRow || page.Rows[0].SourceRowComplete {
		t.Fatalf("partial UTF-8 row = %#v", page.Rows)
	}
	if !utf8.ValidString(page.Rows[0].Text) {
		t.Fatalf("retained page is invalid UTF-8: %q", page.Rows[0].Text)
	}
}

func TestOutputDetailDefaultSourceCapsAreExact(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	source := strings.Repeat("z", maxOutputDetailSourceBytes+77)
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit bytes: %v", err)
	}
	if receipt.Digest.TotalBytes != uint64(len(source)) ||
		receipt.Digest.RetainedBytes != maxOutputDetailSourceBytes ||
		receipt.Digest.RetainedRows != 1 ||
		!receipt.Digest.Truncated {
		t.Fatalf("byte-capped digest = %#v", receipt.Digest)
	}

	rowStore := NewOutputDetailStore()
	rowsSource := strings.Repeat("\n", maxOutputDetailSourceRows+1)
	rowReceipt, err := rowStore.Admit(rowsSource)
	if err != nil {
		t.Fatalf("Admit rows: %v", err)
	}
	if rowReceipt.Digest.TotalRows != maxOutputDetailSourceRows+2 ||
		rowReceipt.Digest.RetainedRows != maxOutputDetailSourceRows ||
		!rowReceipt.Digest.Truncated {
		t.Fatalf("row-capped digest = %#v", rowReceipt.Digest)
	}
}

func TestOutputDetailPageEnforcesRowAndByteCaps(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	source := strings.Repeat("x\n", 299) + "x"
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	page, err := store.Page(context.Background(), OutputDetailPageRequest{
		Ref: receipt.Ref, RowLimit: maxOutputDetailPageRows + 500,
		ByteLimit: maxOutputDetailPageBytes + 500,
	})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Rows) != maxOutputDetailPageRows {
		t.Fatalf("page rows = %d, want hard cap %d", len(page.Rows), maxOutputDetailPageRows)
	}
	if page.Bytes > maxOutputDetailPageBytes {
		t.Fatalf("page bytes = %d, over hard cap", page.Bytes)
	}
	if !page.HasMore || page.Next != (OutputDetailCursor{Row: maxOutputDetailPageRows}) {
		t.Fatalf("next cursor = %#v, has_more=%v", page.Next, page.HasMore)
	}

	small, err := store.Page(context.Background(), OutputDetailPageRequest{
		Ref: receipt.Ref, RowLimit: 2, ByteLimit: 2,
	})
	if err != nil {
		t.Fatalf("small Page: %v", err)
	}
	if len(small.Rows) != 2 || small.Bytes != 2 ||
		small.Next != (OutputDetailCursor{Row: 2}) {
		t.Fatalf("small page = %#v", small)
	}
}

func TestOutputDetailPageFragmentsGiantRowsWithoutLyingAboutLimits(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	source := strings.Repeat("界", 30_000)
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	first, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("first Page: %v", err)
	}
	if len(first.Rows) != 1 || first.Bytes > maxOutputDetailPageBytes ||
		!utf8.ValidString(first.Rows[0].Text) {
		t.Fatalf("first giant-row page = %#v", first)
	}
	if first.Rows[0].StartsMidRow || first.Rows[0].EndsRow ||
		first.Next.Row != 0 || first.Next.ByteOffset != int(first.Bytes) ||
		!first.HasMore {
		t.Fatalf("first giant-row cursor/flags = %#v", first)
	}

	second, err := store.Page(context.Background(), OutputDetailPageRequest{
		Ref: receipt.Ref, Cursor: first.Next,
	})
	if err != nil {
		t.Fatalf("second Page: %v", err)
	}
	if len(second.Rows) != 1 || !second.Rows[0].StartsMidRow ||
		!second.Rows[0].EndsRow || !second.Rows[0].SourceRowComplete ||
		second.Bytes > maxOutputDetailPageBytes || second.HasMore {
		t.Fatalf("second giant-row page = %#v", second)
	}
	if got := first.Rows[0].Text + second.Rows[0].Text; got != source {
		t.Fatalf("fragment round trip bytes = %d, want %d", len(got), len(source))
	}
}

func TestOutputDetailPagePreservesEmptyRows(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	receipt, err := store.Admit("\n")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Rows) != 2 || page.Bytes != 0 || page.HasMore {
		t.Fatalf("empty-row page = %#v", page)
	}
	for index, row := range page.Rows {
		if row.Index != index || row.Text != "" || !row.EndsRow || !row.SourceRowComplete {
			t.Errorf("row %d = %#v", index, row)
		}
	}

	empty, err := store.Admit("")
	if err != nil {
		t.Fatalf("Admit empty: %v", err)
	}
	emptyPage, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: empty.Ref})
	if err != nil {
		t.Fatalf("Page empty: %v", err)
	}
	if len(emptyPage.Rows) != 0 || emptyPage.HasMore ||
		emptyPage.Digest != (OutputDetailDigest{}) {
		t.Fatalf("empty page = %#v", emptyPage)
	}
}

func TestOutputDetailPagePreservesTrailingNewlineAsCompleteEmptyRow(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	receipt := mustAdmitOutputDetail(t, store, "line\n")
	if receipt.Digest.TotalRows != 2 || receipt.Digest.RetainedRows != 2 {
		t.Fatalf("trailing-newline digest = %#v", receipt.Digest)
	}
	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Rows) != 2 ||
		page.Rows[0].Text != "line" ||
		page.Rows[1].Text != "" ||
		!page.Rows[1].EndsRow ||
		!page.Rows[1].SourceRowComplete {
		t.Fatalf("trailing-newline rows = %#v", page.Rows)
	}
}

func TestOutputDetailPageRejectsBudgetSmallerThanNextRune(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	receipt := mustAdmitOutputDetail(t, store, "界")
	page, err := store.Page(context.Background(), OutputDetailPageRequest{
		Ref: receipt.Ref, ByteLimit: 2,
	})
	if !errors.Is(err, ErrOutputDetailPageBudget) || !outputDetailPageIsZero(page) {
		t.Fatalf("undersized rune Page = (%#v, %v)", page, err)
	}
}

func TestOutputDetailStoreLRUEvictsLeastRecentlyUsedByBytes(t *testing.T) {
	t.Parallel()

	store := newOutputDetailStore(outputDetailLimits{
		sourceBytes: 64,
		sourceRows:  16,
		pageBytes:   64,
		pageRows:    16,
		storeBytes:  288,
		storeRefs:   3,
	})
	first := mustAdmitOutputDetail(t, store, "aaaa")
	second := mustAdmitOutputDetail(t, store, "bbbb")
	if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: first.Ref}); err != nil {
		t.Fatalf("touch first: %v", err)
	}
	third := mustAdmitOutputDetail(t, store, "cccc")

	if store.Len() != 2 || store.RetainedBytes() != 288 {
		t.Fatalf("store accounting: refs=%d bytes=%d", store.Len(), store.RetainedBytes())
	}
	if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: second.Ref}); !errors.Is(err, ErrOutputDetailUnavailable) {
		t.Fatalf("least-recent second Page error = %v", err)
	}
	for label, ref := range map[string]OutputDetailRef{"first": first.Ref, "third": third.Ref} {
		if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: ref}); err != nil {
			t.Errorf("%s should remain available: %v", label, err)
		}
	}
}

func TestOutputDetailStoreChargesCompactRowIndexAgainstGlobalBudget(t *testing.T) {
	t.Parallel()

	store := newOutputDetailStore(outputDetailLimits{
		sourceBytes: 64,
		sourceRows:  16,
		pageBytes:   64,
		pageRows:    16,
		storeBytes:  500,
		storeRefs:   64,
	})
	receipts := make([]OutputDetailReceipt, 4)
	for index := range receipts {
		receipts[index] = mustAdmitOutputDetail(t, store, "\n")
	}
	const perEntryCharge = 1 + 2*outputDetailRowSpanChargeBytes + outputDetailEntryOverheadBytes
	if store.Len() != 3 {
		t.Fatalf("Len = %d, want row-index charge to evict one entry", store.Len())
	}
	if store.RetainedBytes() != 3*perEntryCharge ||
		store.RetainedBytes() > 500 {
		t.Fatalf("charged bytes = %d, want %d within cap", store.RetainedBytes(), 3*perEntryCharge)
	}
	if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: receipts[0].Ref}); !errors.Is(err, ErrOutputDetailUnavailable) {
		t.Fatalf("oldest row-heavy ref error = %v", err)
	}
}

func TestOutputDetailStoreLRUEvictsByReferenceCap(t *testing.T) {
	t.Parallel()

	store := newOutputDetailStore(outputDetailLimits{
		sourceBytes: 64,
		sourceRows:  16,
		pageBytes:   64,
		pageRows:    16,
		storeBytes:  1024,
		storeRefs:   2,
	})
	first := mustAdmitOutputDetail(t, store, "a")
	second := mustAdmitOutputDetail(t, store, "b")
	if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: first.Ref}); err != nil {
		t.Fatalf("touch first: %v", err)
	}
	third := mustAdmitOutputDetail(t, store, "c")

	if store.Len() != 2 {
		t.Fatalf("Len = %d, want 2", store.Len())
	}
	if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: second.Ref}); !errors.Is(err, ErrOutputDetailUnavailable) {
		t.Fatalf("least-recent second Page error = %v", err)
	}
	for _, ref := range []OutputDetailRef{first.Ref, third.Ref} {
		if _, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: ref}); err != nil {
			t.Errorf("live ref Page: %v", err)
		}
	}
}

func TestOutputDetailPageFailsClosedForCancelledStaleAndInvalidRequests(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	receipt := mustAdmitOutputDetail(t, store, "safe")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := store.Page(ctx, OutputDetailPageRequest{Ref: receipt.Ref})
	if !errors.Is(err, context.Canceled) || !outputDetailPageIsZero(page) {
		t.Fatalf("cancelled Page = (%#v, %v)", page, err)
	}

	page, err = store.Page(context.Background(), OutputDetailPageRequest{
		Ref: receipt.Ref, Cursor: OutputDetailCursor{Row: 99},
	})
	if !errors.Is(err, ErrOutputDetailCursor) || !outputDetailPageIsZero(page) {
		t.Fatalf("invalid cursor Page = (%#v, %v)", page, err)
	}

	unicodeReceipt := mustAdmitOutputDetail(t, store, "界")
	page, err = store.Page(context.Background(), OutputDetailPageRequest{
		Ref: unicodeReceipt.Ref, Cursor: OutputDetailCursor{ByteOffset: 1},
	})
	if !errors.Is(err, ErrOutputDetailCursor) || !outputDetailPageIsZero(page) {
		t.Fatalf("mid-rune cursor Page = (%#v, %v)", page, err)
	}

	if !store.Drop(receipt.Ref) || store.Drop(receipt.Ref) {
		t.Fatal("Drop must revoke exactly once")
	}
	page, err = store.Page(context.Background(), OutputDetailPageRequest{Ref: receipt.Ref})
	if !errors.Is(err, ErrOutputDetailUnavailable) || !outputDetailPageIsZero(page) {
		t.Fatalf("stale Page = (%#v, %v)", page, err)
	}
	page, err = store.Page(context.Background(), OutputDetailPageRequest{Ref: OutputDetailRef{}})
	if !errors.Is(err, ErrOutputDetailUnavailable) || !outputDetailPageIsZero(page) {
		t.Fatalf("empty-ref Page = (%#v, %v)", page, err)
	}
}

func TestOutputDetailPageCancellationWhileWaitingForStoreFailsClosed(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	receipt := mustAdmitOutputDetail(t, store, strings.Repeat("row\n", 100))
	store.mu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		page OutputDetailPage
		err  error
	}, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		page, err := store.Page(ctx, OutputDetailPageRequest{Ref: receipt.Ref})
		result <- struct {
			page OutputDetailPage
			err  error
		}{page: page, err: err}
	}()
	<-started
	cancel()
	store.mu.Unlock()

	got := <-result
	if !errors.Is(got.err, context.Canceled) || !outputDetailPageIsZero(got.page) {
		t.Fatalf("blocked cancelled Page = (%#v, %v)", got.page, got.err)
	}
}

func TestOutputDetailStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	stable := mustAdmitOutputDetail(t, store, strings.Repeat("stable\n", 100))
	const workers = 8
	const iterations = 100

	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for worker := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := range iterations {
				page, err := store.Page(context.Background(), OutputDetailPageRequest{
					Ref: stable.Ref, RowLimit: 4, ByteLimit: 64,
				})
				if err != nil {
					errs <- fmt.Errorf("worker %d page %d: %w", worker, iteration, err)
					return
				}
				if len(page.Rows) != 4 {
					errs <- fmt.Errorf("worker %d page rows = %d", worker, len(page.Rows))
					return
				}
				transient, err := store.Admit(fmt.Sprintf("worker-%d-%d", worker, iteration))
				if err != nil {
					errs <- fmt.Errorf("worker %d admit %d: %w", worker, iteration, err)
					return
				}
				if !store.Drop(transient.Ref) {
					errs <- fmt.Errorf("worker %d drop %d failed", worker, iteration)
					return
				}
				_ = store.Len()
				_ = store.RetainedBytes()
			}
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func mustAdmitOutputDetail(t *testing.T, store *OutputDetailStore, raw string) OutputDetailReceipt {
	t.Helper()
	receipt, err := store.Admit(raw)
	if err != nil {
		t.Fatalf("Admit(%q): %v", raw, err)
	}
	return receipt
}

func outputDetailPageText(page OutputDetailPage) string {
	rows := make([]string, len(page.Rows))
	for index, row := range page.Rows {
		rows[index] = row.Text
	}
	return strings.Join(rows, "\n")
}

func outputDetailPageIsZero(page OutputDetailPage) bool {
	return page.Rows == nil &&
		page.Next == (OutputDetailCursor{}) &&
		!page.HasMore &&
		page.Bytes == 0 &&
		page.Digest == (OutputDetailDigest{})
}
