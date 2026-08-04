package skill

import (
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantName    string
		wantDesc    string
		wantContent string
		wantErr     bool
	}{
		{
			name:        "valid frontmatter",
			input:       "---\nname: test\ndescription: desc\n---\nBody content",
			wantName:    "test",
			wantDesc:    "desc",
			wantContent: "Body content",
		},
		{
			name:        "no frontmatter",
			input:       "Just body",
			wantContent: "Just body",
		},
		{
			name:        "missing closing delimiter",
			input:       "---\nname: test\nBody",
			wantContent: "---\nname: test\nBody",
		},
		{
			name:    "invalid YAML",
			input:   "---\n: :\n---\nbody",
			wantErr: true,
		},
		{
			name:        "empty body",
			input:       "---\nname: test\n---\n",
			wantName:    "test",
			wantContent: "",
		},
		{
			name:        "empty input",
			input:       "",
			wantContent: "",
		},
		{
			name:        "multiline body",
			input:       "---\nname: multi\n---\nline 1\nline 2\nline 3",
			wantName:    "multi",
			wantContent: "line 1\nline 2\nline 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill, err := parseFrontmatter(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if skill.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", skill.Name, tt.wantName)
			}
			if skill.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", skill.Description, tt.wantDesc)
			}
			if skill.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", skill.Content, tt.wantContent)
			}
		})
	}
}

func TestParseFrontmatterPreservesLongMarkdownLines(t *testing.T) {
	body := strings.Repeat("x", 128*1024)
	skill, err := parseFrontmatter("---\nname: long-line\ndescription: Long body\n---\n" + body)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if skill.Name != "long-line" || skill.Content != body {
		t.Fatalf("long-line skill was truncated: name=%q bytes=%d", skill.Name, len(skill.Content))
	}
}
