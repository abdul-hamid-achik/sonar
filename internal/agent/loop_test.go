package agent

import (
	"strings"
	"testing"
)

func TestFormatToolArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		want     string
		contains []string
		maxLen   int
	}{
		{
			name: "empty map",
			args: map[string]any{},
			want: "",
		},
		{
			name:     "simple map",
			args:     map[string]any{"key": "value"},
			contains: []string{"key=", `"value"`},
		},
		{
			name:   "long args truncated at 60",
			args:   map[string]any{"data": strings.Repeat("a", 300)},
			maxLen: 60,
		},
		{
			name:     "multiple args",
			args:     map[string]any{"path": "/tmp/test", "command": "ls"},
			contains: []string{"path=", "command="},
		},
		{
			name:     "numeric args",
			args:     map[string]any{"count": 42, "ratio": 3.14},
			contains: []string{"count=42", "ratio=3.14"},
		},
		{
			name:     "array args",
			args:     map[string]any{"items": []any{1, 2, 3}},
			contains: []string{"items=", "[3 items]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatToolArgs(tt.args)

			if tt.want != "" && got != tt.want {
				t.Errorf("FormatToolArgs() = %q, want %q", got, tt.want)
			}

			for _, substr := range tt.contains {
				if !strings.Contains(got, substr) {
					t.Errorf("FormatToolArgs() = %q, missing %q", got, substr)
				}
			}

			if tt.maxLen > 0 {
				if len(got) > tt.maxLen {
					t.Errorf("FormatToolArgs() len = %d, want <= %d", len(got), tt.maxLen)
				}
				// Check for truncation indicator (either "..." in value or at end)
				if !strings.Contains(got, "...") {
					t.Errorf("FormatToolArgs() should contain '...' when truncated, got %q", got)
				}
			}
		})
	}
}
