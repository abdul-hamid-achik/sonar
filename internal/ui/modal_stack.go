package ui

import "errors"

// ModalStackCapacity bounds nested viewer state and prevents an accidental
// input loop from retaining an unbounded overlay chain.
const ModalStackCapacity = 8

var (
	ErrModalInvalid    = errors.New("invalid modal")
	ErrModalDuplicate  = errors.New("duplicate modal identity")
	ErrModalStackFull  = errors.New("modal stack is full")
	ErrModalStackEmpty = errors.New("modal stack is empty")
)

// FocusOwner identifies a stable, scalar focus destination.
type FocusOwner uint8

const (
	FocusOwnerUnknown FocusOwner = iota
	FocusOwnerComposer
	FocusOwnerTranscript
	FocusOwnerModal
)

// Valid reports whether owner can receive restored focus.
func (owner FocusOwner) Valid() bool {
	return owner >= FocusOwnerComposer && owner <= FocusOwnerModal
}

// FocusToken identifies a focus destination without retaining a Bubbles model
// or callback. ControlID optionally identifies a child control within a modal.
type FocusToken struct {
	Owner     FocusOwner
	OverlayID OverlayID
	ControlID string
}

// Valid reports whether token is internally consistent and terminal-safe.
func (token FocusToken) Valid() bool {
	if !token.Owner.Valid() {
		return false
	}
	if token.ControlID != "" && !validOpaqueUIID(token.ControlID) {
		return false
	}
	if token.Owner == FocusOwnerModal {
		return token.OverlayID.Valid()
	}
	return token.OverlayID == "" && token.ControlID == ""
}

// DefaultFocusToken is the final focus fallback after a modal chain closes.
func DefaultFocusToken() FocusToken {
	return FocusToken{Owner: FocusOwnerComposer}
}

// ModalKind identifies the new viewer family hosted by ModalStack.
type ModalKind uint8

const (
	ModalKindUnknown ModalKind = iota
	ModalKindOutputViewer
	ModalKindDiffViewer
)

// Valid reports whether kind is a supported stacked viewer.
func (kind ModalKind) Valid() bool {
	return kind >= ModalKindOutputViewer && kind <= ModalKindDiffViewer
}

// ModalInstance is the payload-free identity of one stacked viewer.
type ModalInstance struct {
	ID     OverlayID
	Kind   ModalKind
	Origin EntityRef
}

// Valid reports whether modal can safely enter the stack.
func (modal ModalInstance) Valid() bool {
	return modal.ID.Valid() && modal.Kind.Valid() &&
		modal.Origin.Kind != EntityKindNone && modal.Origin.Valid()
}

// FocusToken returns the root focus identity for modal.
func (modal ModalInstance) FocusToken() FocusToken {
	return FocusToken{Owner: FocusOwnerModal, OverlayID: modal.ID}
}

type modalFrame struct {
	modal        ModalInstance
	restoreFocus FocusToken
}

// OverlayProjection is the complete scalar state needed to compose and route
// the top stacked modal. Legacy overlays remain outside this projection.
type OverlayProjection struct {
	Active bool
	Depth  int
	Top    ModalInstance
	Focus  FocusToken
}

// ModalStack manages nested viewer identity and focus restoration. Bubble Tea's
// smart parent remains responsible for child Update and View calls.
type ModalStack struct {
	frames []modalFrame
}

// Len returns the number of stacked modal viewers.
func (stack *ModalStack) Len() int {
	if stack == nil {
		return 0
	}
	return len(stack.frames)
}

// Empty reports whether the stack has no active viewer.
func (stack *ModalStack) Empty() bool {
	return stack.Len() == 0
}

// Push adds modal and records the focus destination active before it opened.
// Invalid or stale modal focus falls back to the current top modal, then the
// composer.
func (stack *ModalStack) Push(modal ModalInstance, currentFocus FocusToken) error {
	if stack == nil || !modal.Valid() {
		return ErrModalInvalid
	}
	if len(stack.frames) >= ModalStackCapacity {
		return ErrModalStackFull
	}
	if stack.contains(modal.ID) {
		return ErrModalDuplicate
	}
	stack.frames = append(stack.frames, modalFrame{
		modal:        modal,
		restoreFocus: stack.normalizeRestoreFocus(currentFocus),
	})
	return nil
}

// Top returns the active modal without exposing stack storage.
func (stack *ModalStack) Top() (ModalInstance, bool) {
	if stack == nil || len(stack.frames) == 0 {
		return ModalInstance{}, false
	}
	return stack.frames[len(stack.frames)-1].modal, true
}

// Replace swaps the active modal at the same depth and preserves the original
// restoration chain. An identity already used lower in the stack is rejected.
func (stack *ModalStack) Replace(modal ModalInstance) (ModalInstance, error) {
	if stack == nil || len(stack.frames) == 0 {
		return ModalInstance{}, ErrModalStackEmpty
	}
	if !modal.Valid() {
		return ModalInstance{}, ErrModalInvalid
	}
	topIndex := len(stack.frames) - 1
	for index := range topIndex {
		if stack.frames[index].modal.ID == modal.ID {
			return ModalInstance{}, ErrModalDuplicate
		}
	}
	previous := stack.frames[topIndex].modal
	stack.frames[topIndex].modal = modal
	return previous, nil
}

// Pop removes the active modal and returns the live focus destination. When a
// parent modal remains it always owns focus; after the last modal closes, a
// stale restoration token falls back to fallback and finally the composer.
func (stack *ModalStack) Pop(fallback FocusToken) (ModalInstance, FocusToken, bool) {
	if stack == nil || len(stack.frames) == 0 {
		return ModalInstance{}, resolveUnstackedFocus(FocusToken{}, fallback), false
	}
	topIndex := len(stack.frames) - 1
	frame := stack.frames[topIndex]
	stack.frames = stack.frames[:topIndex]
	if top, exists := stack.Top(); exists {
		return frame.modal, top.FocusToken(), true
	}
	return frame.modal, resolveUnstackedFocus(frame.restoreFocus, fallback), true
}

// Clear removes the entire chain in visual dismissal order (top first) and
// restores the focus captured before the bottom modal opened.
func (stack *ModalStack) Clear(fallback FocusToken) ([]ModalInstance, FocusToken) {
	if stack == nil || len(stack.frames) == 0 {
		return nil, resolveUnstackedFocus(FocusToken{}, fallback)
	}
	bottomRestore := stack.frames[0].restoreFocus
	removed := make([]ModalInstance, 0, len(stack.frames))
	for index := len(stack.frames) - 1; index >= 0; index-- {
		removed = append(removed, stack.frames[index].modal)
	}
	stack.frames = nil
	return removed, resolveUnstackedFocus(bottomRestore, fallback)
}

// Projection returns the active top modal and current stack depth.
func (stack *ModalStack) Projection() OverlayProjection {
	top, exists := stack.Top()
	if !exists {
		return OverlayProjection{}
	}
	return OverlayProjection{
		Active: true,
		Depth:  stack.Len(),
		Top:    top,
		Focus:  top.FocusToken(),
	}
}

func (stack *ModalStack) normalizeRestoreFocus(preferred FocusToken) FocusToken {
	if preferred.Valid() {
		if preferred.Owner != FocusOwnerModal || stack.contains(preferred.OverlayID) {
			return preferred
		}
	}
	if top, exists := stack.Top(); exists {
		return top.FocusToken()
	}
	return DefaultFocusToken()
}

func (stack *ModalStack) contains(id OverlayID) bool {
	if stack == nil {
		return false
	}
	for _, frame := range stack.frames {
		if frame.modal.ID == id {
			return true
		}
	}
	return false
}

func resolveUnstackedFocus(preferred, fallback FocusToken) FocusToken {
	if preferred.Valid() && preferred.Owner != FocusOwnerModal {
		return preferred
	}
	if fallback.Valid() && fallback.Owner != FocusOwnerModal {
		return fallback
	}
	return DefaultFocusToken()
}
