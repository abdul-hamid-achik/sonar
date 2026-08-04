package ui

// preemptTranscriptSearch releases the search footer before a higher-authority
// asynchronous surface commits its owner state. closeTranscriptSearch restores
// the opening semantic anchor, follow intent, and composer focus synchronously;
// search never mutates the viewport's line-style hook. Its returned
// repaint/blink commands are intentionally discarded because the incoming
// owner immediately replaces the footer and establishes its own focus.
func (m *Model) preemptTranscriptSearch() {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	_ = m.closeTranscriptSearch(true)
}
