package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCustomCommand(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantOK  bool
		wantCmd CustomCommand
	}{
		{
			name: "valid command",
			content: `---
name: review
description: Code review prompt
---
Review this code: {{input}}`,
			wantOK: true,
			wantCmd: CustomCommand{
				Name:        "review",
				Description: "Code review prompt",
				Template:    "Review this code: {{input}}",
			},
		},
		{
			name: "no description",
			content: `---
name: explain
---
Explain this: {{input}}`,
			wantOK: true,
			wantCmd: CustomCommand{
				Name:     "explain",
				Template: "Explain this: {{input}}",
			},
		},
		{
			name:    "no frontmatter",
			content: "just some text",
			wantOK:  false,
		},
		{
			name: "no name",
			content: `---
description: something
---
body`,
			wantOK: false,
		},
		{
			name: "no body",
			content: `---
name: empty
---`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := parseCustomCommand(tt.content)
			if ok != tt.wantOK {
				t.Fatalf("parseCustomCommand() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if cmd.Name != tt.wantCmd.Name {
				t.Errorf("Name = %q, want %q", cmd.Name, tt.wantCmd.Name)
			}
			if cmd.Description != tt.wantCmd.Description {
				t.Errorf("Description = %q, want %q", cmd.Description, tt.wantCmd.Description)
			}
			if cmd.Template != tt.wantCmd.Template {
				t.Errorf("Template = %q, want %q", cmd.Template, tt.wantCmd.Template)
			}
		})
	}
}

func TestParseCustomCommandRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"Help", "two words", "../escape", "-flag", "under/slash", strings.Repeat("a", maxCustomCommandNameBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseCustomCommand("---\nname: " + name + "\n---\nDo it"); ok {
				t.Fatalf("unsafe custom command name %q was accepted", name)
			}
		})
	}
	for _, name := range []string{"a", "review-code", "review_code", "review2"} {
		t.Run("valid_"+name, func(t *testing.T) {
			if command, ok := parseCustomCommand("---\nname: " + name + "\n---\nDo it"); !ok || command.Name != name {
				t.Fatalf("valid custom command name %q was rejected", name)
			}
		})
	}
}

func TestLoadCustomCommands(t *testing.T) {
	dir := t.TempDir()

	// Write a valid command file.
	err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(`---
name: review
description: Review code
---
Review: {{input}}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Write an invalid file (no frontmatter).
	err = os.WriteFile(filepath.Join(dir, "invalid.md"), []byte("just text"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Write a non-md file (should be ignored).
	err = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a command"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	cmds, err := LoadCustomCommands(dir)
	if err == nil || !strings.Contains(err.Error(), "invalid.md") {
		t.Fatalf("expected invalid-file warning, got %v", err)
	}
	if len(cmds) != 1 {
		t.Fatalf("LoadCustomCommands() returned %d commands, want 1", len(cmds))
	}
	if cmds[0].Name != "review" {
		t.Errorf("Name = %q, want %q", cmds[0].Name, "review")
	}
}

func TestLoadCustomCommands_MissingDir(t *testing.T) {
	cmds, err := LoadCustomCommands("/nonexistent/path")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 0 {
		t.Errorf("expected empty result for missing dir, got %d", len(cmds))
	}
}

func TestLoadCustomCommandsReportsUnsafeFileAndKeepsValidCommands(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("---\nname: stolen\n---\nsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "00-unsafe.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01-valid.md"), []byte("---\nname: valid\n---\nDo it"), 0o600); err != nil {
		t.Fatal(err)
	}

	commands, err := LoadCustomCommands(dir)
	if err == nil || !strings.Contains(err.Error(), "00-unsafe.md") {
		t.Fatalf("unsafe custom command warning = %v", err)
	}
	if len(commands) != 1 || commands[0].Name != "valid" {
		t.Fatalf("valid commands were suppressed: %#v", commands)
	}
}

func TestRegisterCustomCommands(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "test.md"), []byte(`---
name: testcmd
description: A test command
---
Do this: {{input}}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	if err := RegisterCustomCommands(reg, dir); err != nil {
		t.Fatal(err)
	}

	result := reg.Execute(&Context{}, "testcmd", []string{"hello", "world"})
	if result.Action != ActionSendPrompt {
		t.Errorf("Action = %v, want ActionSendPrompt", result.Action)
	}
	if result.Data != "Do this: hello world" {
		t.Errorf("Data = %q, want %q", result.Data, "Do this: hello world")
	}
}

func TestRegisterCustomCommandsCannotOverrideBuiltinOrEarlierCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "first.md"), []byte("---\nname: help\n---\nmalicious override"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.md"), []byte("---\nname: h\n---\nalias override"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	RegisterBuiltins(reg)
	err := RegisterCustomCommands(reg, dir)
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("collision warning = %v", err)
	}
	for _, name := range []string{"help", "h"} {
		result := reg.Execute(nil, name, nil)
		if result.Action != ActionShowHelp || result.Error != "" {
			t.Fatalf("/%s was overridden: %#v", name, result)
		}
	}
}
