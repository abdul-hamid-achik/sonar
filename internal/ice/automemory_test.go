package ice

import (
	"path/filepath"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

func TestParseAndSave(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCount int
		wantTags  [][]string // expected tags per saved memory, in order
	}{
		{
			name:      "valid FACT and DECISION lines",
			input:     "FACT: user likes Go\nDECISION: use postgres",
			wantCount: 2,
			wantTags:  [][]string{{"fact", "auto"}, {"decision", "auto"}},
		},
		{
			name:      "NONE saves nothing",
			input:     "NONE",
			wantCount: 0,
		},
		{
			name:      "empty lines are skipped",
			input:     "\n\n\n",
			wantCount: 0,
		},
		{
			name:      "invalid type is skipped",
			input:     "UNKNOWN: something",
			wantCount: 0,
		},
		{
			name:      "missing colon format is skipped",
			input:     "this has no colon",
			wantCount: 0,
		},
		{
			name:      "PREFERENCE type",
			input:     "PREFERENCE: dark mode",
			wantCount: 1,
			wantTags:  [][]string{{"preference", "auto"}},
		},
		{
			name:      "TODO type",
			input:     "TODO: fix the bug",
			wantCount: 1,
			wantTags:  [][]string{{"todo", "auto"}},
		},
		{
			name:      "mixed valid and invalid",
			input:     "FACT: real fact\nBAD: not valid\nTODO: real todo",
			wantCount: 2,
			wantTags:  [][]string{{"fact", "auto"}, {"todo", "auto"}},
		},
		{
			name:      "empty content after type is skipped",
			input:     "FACT: ",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			memPath := filepath.Join(dir, "memories.json")
			ms := memory.NewStore(memPath)

			am := &AutoMemory{memStore: ms}
			err := am.parseAndSave(tt.input)
			if err != nil {
				t.Fatalf("parseAndSave returned error: %v", err)
			}

			if ms.Count() != tt.wantCount {
				t.Errorf("memory count = %d, want %d", ms.Count(), tt.wantCount)
			}

			if tt.wantTags != nil {
				recent, _ := ms.Recent(tt.wantCount)
				// Recent returns most-recent first, so reverse for order comparison.
				for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
					recent[i], recent[j] = recent[j], recent[i]
				}
				for i, wantTags := range tt.wantTags {
					if i >= len(recent) {
						t.Errorf("missing memory at index %d", i)
						continue
					}
					got := recent[i].Tags
					if len(got) != len(wantTags) {
						t.Errorf("memory[%d] tags = %v, want %v", i, got, wantTags)
						continue
					}
					for j := range wantTags {
						if got[j] != wantTags[j] {
							t.Errorf("memory[%d] tag[%d] = %q, want %q", i, j, got[j], wantTags[j])
						}
					}
				}
			}
		})
	}
}
