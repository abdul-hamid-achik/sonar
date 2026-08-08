package ui

// The terminal tab is a signal surface for the person who is somewhere else —
// the same person the voice alerts channel exists for. windowTitleBase already
// leads with the model-generated session title, and the view suffixes progress
// (thinking/streaming/done/draft); this predicate adds the one state those
// cannot express: the harness is blocked on a human decision.
//
// It covers exactly the states a human must resolve: an approval or read
// scope prompt waiting, a Cortex decision, or a turn that finished while the
// window was unfocused. That last one clears the moment focus returns —
// arriving is acknowledging — while prompt states clear only when answered,
// because looking at a prompt is not deciding it.
func (m *Model) windowAttentionRequested() bool {
	if m == nil {
		return false
	}
	if m.pendingApproval != nil || m.readScopePrompt != nil {
		return true
	}
	if m.cortexDecisionActive() {
		return true
	}
	return m.turnUnseen
}
