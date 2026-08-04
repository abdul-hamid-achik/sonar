package ice

import (
	"strings"
	"testing"
)

func TestFormatContext(t *testing.T) {
	tests := []struct {
		name       string
		convChunks []ContextChunk
		memChunks  []ContextChunk
		wantConv   bool // should contain "Relevant Past Conversations"
		wantMem    bool // should contain "Remembered Facts"
		wantEmpty  bool
	}{
		{
			name: "both conversation and memory chunks",
			convChunks: []ContextChunk{
				{Source: SourceConversation, Content: "past chat about Go"},
			},
			memChunks: []ContextChunk{
				{Source: SourceMemory, Content: "user prefers dark mode"},
			},
			wantConv: true,
			wantMem:  true,
		},
		{
			name: "conversations only",
			convChunks: []ContextChunk{
				{Source: SourceConversation, Content: "previous discussion"},
			},
			memChunks: nil,
			wantConv:  true,
			wantMem:   false,
		},
		{
			name:       "memories only",
			convChunks: nil,
			memChunks: []ContextChunk{
				{Source: SourceMemory, Content: "user name is Alice"},
			},
			wantConv: false,
			wantMem:  true,
		},
		{
			name:       "both empty",
			convChunks: nil,
			memChunks:  nil,
			wantEmpty:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatContext(tt.convChunks, tt.memChunks)

			if tt.wantEmpty {
				if got != "" {
					t.Errorf("expected empty string, got %q", got)
				}
				return
			}

			hasConv := strings.Contains(got, "Relevant Past Conversations")
			hasMem := strings.Contains(got, "Remembered Facts")

			if hasConv != tt.wantConv {
				t.Errorf("has conversations section = %v, want %v", hasConv, tt.wantConv)
			}
			if hasMem != tt.wantMem {
				t.Errorf("has memories section = %v, want %v", hasMem, tt.wantMem)
			}

			// Verify content is present in output.
			for _, c := range tt.convChunks {
				if !strings.Contains(got, c.Content) {
					t.Errorf("output missing conversation content %q", c.Content)
				}
			}
			for _, c := range tt.memChunks {
				if !strings.Contains(got, c.Content) {
					t.Errorf("output missing memory content %q", c.Content)
				}
			}
		})
	}
}
