package ui

import (
	"errors"
	"testing"
)

func testModal(id string, kind ModalKind) ModalInstance {
	return ModalInstance{
		ID:   OverlayID(id),
		Kind: kind,
		Origin: EntityRef{
			Kind:    EntityKindTranscriptBlock,
			BlockID: BlockID("blk_" + id),
		},
	}
}

func TestModalStackPushProjectionAndNestedPopRestore(t *testing.T) {
	t.Parallel()

	var stack ModalStack
	output := testModal("output_1", ModalKindOutputViewer)
	diff := testModal("diff_1", ModalKindDiffViewer)
	composer := DefaultFocusToken()

	if err := stack.Push(output, composer); err != nil {
		t.Fatalf("Push(output): %v", err)
	}
	if err := stack.Push(diff, output.FocusToken()); err != nil {
		t.Fatalf("Push(diff): %v", err)
	}

	projection := stack.Projection()
	if !projection.Active || projection.Depth != 2 || projection.Top != diff ||
		projection.Focus != diff.FocusToken() {
		t.Fatalf("projection = %#v", projection)
	}

	removed, focus, ok := stack.Pop(FocusToken{Owner: FocusOwnerTranscript})
	if !ok || removed != diff || focus != output.FocusToken() {
		t.Fatalf("nested Pop = (%#v, %#v, %v)", removed, focus, ok)
	}
	removed, focus, ok = stack.Pop(FocusToken{Owner: FocusOwnerTranscript})
	if !ok || removed != output || focus != composer {
		t.Fatalf("final Pop = (%#v, %#v, %v)", removed, focus, ok)
	}
	if !stack.Empty() || stack.Projection() != (OverlayProjection{}) {
		t.Fatalf("empty stack retained state: %#v", stack.Projection())
	}
}

func TestModalStackPushNormalizesStaleFocus(t *testing.T) {
	t.Parallel()

	var stack ModalStack
	output := testModal("output_1", ModalKindOutputViewer)
	diff := testModal("diff_1", ModalKindDiffViewer)
	if err := stack.Push(output, FocusToken{
		Owner:     FocusOwnerModal,
		OverlayID: OverlayID("already_gone"),
	}); err != nil {
		t.Fatalf("Push(output): %v", err)
	}
	if err := stack.Push(diff, FocusToken{
		Owner:     FocusOwnerModal,
		OverlayID: OverlayID("stale_modal"),
	}); err != nil {
		t.Fatalf("Push(diff): %v", err)
	}

	_, focus, _ := stack.Pop(DefaultFocusToken())
	if focus != output.FocusToken() {
		t.Fatalf("nested stale focus restored to %#v", focus)
	}
	_, focus, _ = stack.Pop(FocusToken{Owner: FocusOwnerTranscript})
	if focus != DefaultFocusToken() {
		t.Fatalf("initial stale focus restored to %#v", focus)
	}
}

func TestModalStackReplacePreservesDepthAndRestoreChain(t *testing.T) {
	t.Parallel()

	var stack ModalStack
	output := testModal("output_1", ModalKindOutputViewer)
	diff := testModal("diff_1", ModalKindDiffViewer)
	replacement := testModal("diff_2", ModalKindDiffViewer)
	if err := stack.Push(output, FocusToken{Owner: FocusOwnerTranscript}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Push(diff, output.FocusToken()); err != nil {
		t.Fatal(err)
	}

	previous, err := stack.Replace(replacement)
	if err != nil || previous != diff {
		t.Fatalf("Replace = (%#v, %v)", previous, err)
	}
	if stack.Len() != 2 || stack.Projection().Top != replacement {
		t.Fatalf("replace changed depth/projection: %#v", stack.Projection())
	}
	_, focus, _ := stack.Pop(DefaultFocusToken())
	if focus != output.FocusToken() {
		t.Fatalf("replace lost parent restoration: %#v", focus)
	}
	_, focus, _ = stack.Pop(DefaultFocusToken())
	if focus.Owner != FocusOwnerTranscript {
		t.Fatalf("replace lost root restoration: %#v", focus)
	}
}

func TestModalStackRejectsInvalidDuplicateAndOverflow(t *testing.T) {
	t.Parallel()

	var nilStack *ModalStack
	if err := nilStack.Push(testModal("output_1", ModalKindOutputViewer), DefaultFocusToken()); !errors.Is(err, ErrModalInvalid) {
		t.Fatalf("nil Push error = %v", err)
	}

	var stack ModalStack
	if _, err := stack.Replace(testModal("output_1", ModalKindOutputViewer)); !errors.Is(err, ErrModalStackEmpty) {
		t.Fatalf("empty Replace error = %v", err)
	}
	invalid := []ModalInstance{
		{},
		{ID: OverlayID("output_1"), Kind: ModalKindOutputViewer},
		{ID: OverlayID("bad id"), Kind: ModalKindOutputViewer},
		{ID: OverlayID("output_1"), Kind: ModalKindUnknown},
		{
			ID:   OverlayID("output_1"),
			Kind: ModalKindOutputViewer,
			Origin: EntityRef{
				Kind:    EntityKindTranscriptBlock,
				BlockID: BlockID("bad id"),
			},
		},
	}
	for _, modal := range invalid {
		if err := stack.Push(modal, DefaultFocusToken()); !errors.Is(err, ErrModalInvalid) {
			t.Errorf("Push(%#v) error = %v", modal, err)
		}
	}

	for index := 0; index < ModalStackCapacity; index++ {
		modal := testModal("modal_"+string(rune('a'+index)), ModalKindOutputViewer)
		if err := stack.Push(modal, DefaultFocusToken()); err != nil {
			t.Fatalf("Push(%d): %v", index, err)
		}
	}
	if err := stack.Push(testModal("overflow", ModalKindOutputViewer), DefaultFocusToken()); !errors.Is(err, ErrModalStackFull) {
		t.Fatalf("overflow error = %v", err)
	}

	top, _ := stack.Top()
	if err := stack.Push(top, DefaultFocusToken()); !errors.Is(err, ErrModalStackFull) {
		// Capacity wins deterministically before duplicate inspection.
		t.Fatalf("full duplicate error = %v", err)
	}
	stack.Pop(DefaultFocusToken())
	if err := stack.Push(stack.frames[0].modal, DefaultFocusToken()); !errors.Is(err, ErrModalDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}

	if _, err := stack.Replace(stack.frames[0].modal); !errors.Is(err, ErrModalDuplicate) {
		t.Fatalf("replacement lower duplicate error = %v", err)
	}
}

func TestModalStackClearUsesBottomRestoreAndTopFirstOrder(t *testing.T) {
	t.Parallel()

	var stack ModalStack
	modals := []ModalInstance{
		testModal("output_1", ModalKindOutputViewer),
		testModal("diff_1", ModalKindDiffViewer),
		testModal("output_2", ModalKindOutputViewer),
	}
	root := FocusToken{Owner: FocusOwnerTranscript}
	if err := stack.Push(modals[0], root); err != nil {
		t.Fatal(err)
	}
	if err := stack.Push(modals[1], modals[0].FocusToken()); err != nil {
		t.Fatal(err)
	}
	if err := stack.Push(modals[2], modals[1].FocusToken()); err != nil {
		t.Fatal(err)
	}

	removed, focus := stack.Clear(DefaultFocusToken())
	if len(removed) != 3 || removed[0] != modals[2] ||
		removed[1] != modals[1] || removed[2] != modals[0] {
		t.Fatalf("clear order = %#v", removed)
	}
	if focus != root {
		t.Fatalf("clear focus = %#v, want %#v", focus, root)
	}
	if !stack.Empty() {
		t.Fatal("Clear retained frames")
	}

	removed, focus = stack.Clear(FocusToken{})
	if removed != nil || focus != DefaultFocusToken() {
		t.Fatalf("empty Clear = (%#v, %#v)", removed, focus)
	}
}

func TestModalStackPopEmptyUsesOnlyLiveFallback(t *testing.T) {
	t.Parallel()

	var stack ModalStack
	_, focus, ok := stack.Pop(FocusToken{Owner: FocusOwnerTranscript})
	if ok || focus.Owner != FocusOwnerTranscript {
		t.Fatalf("empty Pop = focus %#v ok %v", focus, ok)
	}
	_, focus, ok = stack.Pop(FocusToken{
		Owner:     FocusOwnerModal,
		OverlayID: OverlayID("gone"),
	})
	if ok || focus != DefaultFocusToken() {
		t.Fatalf("empty Pop stale modal = focus %#v ok %v", focus, ok)
	}
}

func TestFocusTokenAndModalInstanceValidation(t *testing.T) {
	t.Parallel()

	validTokens := []FocusToken{
		DefaultFocusToken(),
		{Owner: FocusOwnerTranscript},
		{Owner: FocusOwnerModal, OverlayID: OverlayID("output_1")},
		{Owner: FocusOwnerModal, OverlayID: OverlayID("output_1"), ControlID: "search"},
	}
	for _, token := range validTokens {
		if !token.Valid() {
			t.Errorf("valid token rejected: %#v", token)
		}
	}
	invalidTokens := []FocusToken{
		{},
		{Owner: FocusOwnerComposer, OverlayID: OverlayID("output_1")},
		{Owner: FocusOwnerTranscript, ControlID: "search"},
		{Owner: FocusOwnerModal},
		{Owner: FocusOwnerModal, OverlayID: OverlayID("bad id")},
		{Owner: FocusOwnerModal, OverlayID: OverlayID("output_1"), ControlID: "bad id"},
	}
	for _, token := range invalidTokens {
		if token.Valid() {
			t.Errorf("invalid token accepted: %#v", token)
		}
	}

	if testModal("output_1", ModalKindOutputViewer).FocusToken() !=
		(FocusToken{Owner: FocusOwnerModal, OverlayID: OverlayID("output_1")}) {
		t.Fatal("modal focus token was not scalar/stable")
	}
}
