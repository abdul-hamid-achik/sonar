package ui

import (
	"strings"
	"testing"
)

func TestTranscriptStripsUntrustedTerminalControlsAcrossEntryKinds(t *testing.T) {
	m := newTestModel(t)
	payload := "visible\x1b]8;;https://example.invalid\x07link\x1b]8;;\x07\u202espoof"
	m.entries = []ChatEntry{
		{Kind: "user", Content: payload},
		{Kind: "assistant", Content: payload},
		{Kind: "system", Content: payload},
		{Kind: "error", Content: payload},
	}
	m.invalidateEntryCache()
	rendered := m.renderEntries()
	for _, forbidden := range []string{"\x1b]", "\x07", "\u202e", "https://example.invalid"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("transcript retained terminal control payload %q: %q", forbidden, rendered)
		}
	}
	for _, want := range []string{"visible", "link", "spoof"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("transcript dropped visible content %q: %q", want, rendered)
		}
	}
}

func TestFlushStreamCachesOnlySanitizedProviderContent(t *testing.T) {
	m := newTestModel(t)
	m.streamBuf.WriteString("answer\x1b]0;owned\x07")
	m.thinkBuf.WriteString("reason\u202espoof")
	m.flushStream()
	if len(m.entries) != 1 {
		t.Fatalf("flushed entries = %d, want 1", len(m.entries))
	}
	entry := m.entries[0]
	for _, value := range []string{entry.Content, entry.ThinkingContent} {
		if strings.Contains(value, "\x07") || strings.Contains(value, "\x1b]") || strings.Contains(value, "\u202e") {
			t.Fatalf("flushed provider entry retained terminal controls: %#v", entry)
		}
	}
}
