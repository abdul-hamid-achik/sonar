package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const maxCustomCommandBytes int64 = 1 << 20

const maxCustomCommandNameBytes = 64

var customCommandReader = safeio.NewReader()

// CustomCommand represents a user-defined command loaded from a markdown file.
type CustomCommand struct {
	Name        string
	Description string
	Template    string // prompt template with {{input}} placeholder
}

// LoadCustomCommands reads .md files from the commands directory and returns
// parsed custom commands. Each file should have YAML-like frontmatter:
//
//	---
//	name: review
//	description: Code review prompt
//	---
//	Review this code: {{input}}
func LoadCustomCommands(dir string) ([]CustomCommand, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read custom command directory %s: %w", dir, err)
	}

	var cmds []CustomCommand
	var warnings []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := customCommandReader.ReadRegularFileNoFollow(filepath.Join(dir, entry.Name()), maxCustomCommandBytes, safeio.StartupReadTimeout)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("load custom command %s: %w", entry.Name(), err))
			continue
		}
		if cmd, ok := parseCustomCommand(string(data)); ok {
			cmds = append(cmds, cmd)
		} else {
			warnings = append(warnings, fmt.Errorf("parse custom command %s: expected frontmatter name and non-empty template", entry.Name()))
		}
	}
	return cmds, errors.Join(warnings...)
}

// parseCustomCommand parses a markdown file with YAML frontmatter.
func parseCustomCommand(content string) (CustomCommand, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return CustomCommand{}, false
	}

	// Find end of frontmatter.
	rest := content[3:]
	idx := strings.Index(rest, "---")
	if idx < 0 {
		return CustomCommand{}, false
	}

	frontmatter := rest[:idx]
	body := strings.TrimSpace(rest[idx+3:])

	cmd := CustomCommand{Template: body}

	// Parse simple key: value pairs from frontmatter.
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "name":
			cmd.Name = val
		case "description":
			cmd.Description = val
		}
	}

	if !validCustomCommandName(cmd.Name) || cmd.Template == "" {
		return CustomCommand{}, false
	}

	return cmd, true
}

func validCustomCommandName(name string) bool {
	if name == "" || len(name) > maxCustomCommandNameBytes {
		return false
	}
	for index, r := range name {
		if r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' || index > 0 && (r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

// RegisterCustomCommands loads and registers custom commands from the given directory.
func RegisterCustomCommands(r *Registry, dir string) error {
	cmds, loadErr := LoadCustomCommands(dir)
	var registrationWarnings []error
	for _, cc := range cmds {
		if existing, collision := r.commands[cc.Name]; collision {
			registrationWarnings = append(registrationWarnings, fmt.Errorf(
				"register custom command %q: spelling is already owned by /%s", cc.Name, existing.Name,
			))
			continue
		}
		// Capture for closure.
		tmpl := cc.Template
		desc := cc.Description
		if desc == "" {
			desc = "Custom command"
		}

		r.Register(&Command{
			Name:        cc.Name,
			Description: desc,
			Handler: func(_ *Context, args []string) Result {
				input := strings.Join(args, " ")
				prompt := strings.ReplaceAll(tmpl, "{{input}}", input)
				return Result{
					Action: ActionSendPrompt,
					Data:   prompt,
				}
			},
		})
	}
	return errors.Join(loadErr, errors.Join(registrationWarnings...))
}
