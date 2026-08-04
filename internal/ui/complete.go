package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

type Completion struct {
	Label       string
	Insert      string
	Category    string
	Description string
	SearchTerms string
	Index       int
}

type Completer struct {
	registry       *command.Registry
	commands       []*command.Command
	models         []string
	providers      []string
	skills         []string
	agents         []string
	workDir        string
	ignorePatterns *config.IgnorePatterns
}

func NewCompleter(cmdReg *command.Registry, models, skills, agents []string, _ *mcp.Registry) *Completer {
	workDir, _ := os.Getwd()
	return &Completer{
		registry: cmdReg,
		commands: cmdReg.All(),
		models:   models,
		skills:   skills,
		agents:   agents,
		workDir:  workDir,
	}
}

func (c *Completer) Complete(input string) []Completion {
	var completions []Completion

	if strings.HasPrefix(input, "/") {
		completions = c.completeCommand(input)
	} else if strings.HasPrefix(input, "@") {
		completions = c.completeAgentOrFile(input)
	} else if strings.HasPrefix(input, "#") {
		completions = c.completeSkill(input)
	}

	return completions
}

// CompleteStatic returns only in-memory completion sources. The UI uses this
// from Update so filesystem listing and walking are always deferred to a
// cancellable tea.Cmd.
func (c *Completer) CompleteStatic(input string) []Completion {
	if c == nil {
		return nil
	}
	switch {
	case strings.HasPrefix(input, "/"):
		return c.completeCommand(input)
	case strings.HasPrefix(input, "@"):
		return c.completeAgents(input)
	case strings.HasPrefix(input, "#"):
		return c.completeSkill(input)
	default:
		return nil
	}
}

func (c *Completer) completeCommand(input string) []Completion {
	var completions []Completion
	input = strings.TrimPrefix(input, "/")
	if commandName, argument, hasArgument := strings.Cut(input, " "); hasArgument {
		return c.completeCommandAction(commandName, strings.TrimSpace(argument))
	}

	for _, cmd := range c.commands {
		matches := strings.HasPrefix(cmd.Name, input)
		for _, alias := range cmd.Aliases {
			matches = matches || strings.HasPrefix(alias, input)
		}
		if !matches {
			continue
		}
		completions = append(completions, Completion{
			Label:       "/" + cmd.Name,
			Insert:      "/" + cmd.Name + " ",
			Category:    "command",
			Description: cmd.Description,
			SearchTerms: strings.Join(append([]string{cmd.Name, cmd.Usage}, cmd.Aliases...), " "),
		})
	}

	return completions
}

func (c *Completer) completeCommandAction(commandName, prefix string) []Completion {
	if c == nil || c.registry == nil {
		return nil
	}
	commandName = strings.ToLower(strings.TrimSpace(commandName))
	if commandName == "g" {
		commandName = "goal"
	}
	switch commandName {
	case "provider", "providers", "prov":
		return c.completeProviderArgs(prefix)
	case "model", "models", "m", "ml":
		return c.completeModelArgs(prefix)
	}
	states := c.registry.Actions(commandName, nil)
	completions := make([]Completion, 0, len(states))
	for _, state := range states {
		spec := state.Spec
		matchesPrefix := strings.HasPrefix(strings.ToLower(spec.Argument), strings.ToLower(prefix)) ||
			strings.HasPrefix(strings.ToLower(spec.Title), strings.ToLower(prefix))
		for _, alias := range spec.Aliases {
			matchesPrefix = matchesPrefix || strings.HasPrefix(strings.ToLower(alias), strings.ToLower(prefix))
		}
		search := strings.ToLower(strings.Join(append([]string{
			spec.Argument, spec.Title, spec.Description,
		}, spec.Aliases...), " "))
		if prefix != "" && !matchesPrefix {
			continue
		}
		completions = append(completions, Completion{
			Label:       spec.CommandText(),
			Insert:      spec.CommandText() + " ",
			Category:    "action",
			Description: spec.Description,
			SearchTerms: search,
		})
	}
	return completions
}

func (c *Completer) completeAgentOrFile(input string) []Completion {
	completions := c.completeAgents(input)
	input = strings.TrimPrefix(input, "@")

	// Always append file results (not just when no agents match)
	completions = append(completions, c.completeFile(input)...)

	return completions
}

func (c *Completer) completeAgents(input string) []Completion {
	input = strings.TrimPrefix(input, "@")
	var completions []Completion
	for _, agent := range c.agents {
		if strings.HasPrefix(agent, input) {
			completions = append(completions, Completion{
				Label:    "@" + agent,
				Insert:   "@" + agent + " ",
				Category: "agent",
			})
		}
	}
	return completions
}

func (c *Completer) completeFile(input string) []Completion {
	var completions []Completion

	// Determine the directory to list
	dir := c.workDir
	if strings.Contains(input, "/") {
		// User is typing a path
		lastSlash := strings.LastIndex(input, "/")
		dirPart := input[:lastSlash]
		if !strings.HasPrefix(dirPart, "/") {
			dirPart = filepath.Join(c.workDir, dirPart)
		}
		if info, err := os.Stat(dirPart); err == nil && info.IsDir() {
			dir = dirPart
		}
	}

	// Read directory entries
	entries, err := os.ReadDir(dir)
	if err != nil {
		return completions
	}

	prefix := input
	if strings.Contains(input, "/") {
		prefix = input[strings.LastIndex(input, "/")+1:]
	}

	for _, entry := range entries {
		name := entry.Name()
		// Skip hidden files unless user explicitly types .
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		// Skip entries matching ignore patterns.
		if c.ignorePatterns.Match(name) {
			continue
		}

		if strings.HasPrefix(name, prefix) {
			isDir := entry.IsDir()
			displayName := name
			insertName := name

			if isDir {
				displayName += "/"
				insertName += "/"
			}

			// Build full path relative to input
			if strings.Contains(input, "/") {
				dirPath := input[:strings.LastIndex(input, "/")+1]
				displayName = dirPath + displayName
				insertName = dirPath + insertName
			} else if dir != c.workDir {
				relPath, _ := filepath.Rel(c.workDir, dir)
				if relPath != "." {
					displayName = relPath + "/" + name
					if isDir {
						displayName += "/"
					} else {
						insertName = relPath + "/" + insertName
					}
				}
			}

			category := "file"
			if isDir {
				category = "folder"
			}

			completions = append(completions, Completion{
				Label:    "@" + displayName,
				Insert:   "@" + insertName + " ",
				Category: category,
			})
		}
	}

	return completions
}

// CompleteFilePath lists directory contents at a given relative path.
// Used for folder drill-down in the completion modal.
func (c *Completer) CompleteFilePath(relPath string) []Completion {
	var completions []Completion

	dir := filepath.Join(c.workDir, relPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return completions
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Skip entries matching ignore patterns.
		if c.ignorePatterns.Match(name) {
			continue
		}

		isDir := entry.IsDir()
		displayName := name
		insertPath := relPath
		if insertPath != "" && !strings.HasSuffix(insertPath, "/") {
			insertPath += "/"
		}
		insertPath += name

		if isDir {
			displayName += "/"
		}

		category := "file"
		if isDir {
			category = "folder"
		}

		completions = append(completions, Completion{
			Label:    displayName,
			Insert:   "@" + insertPath + " ",
			Category: category,
		})
	}

	return completions
}

// WorkspaceCompletions performs every filesystem-backed completion operation.
// Callers must run it inside a tea.Cmd; context cancellation stops directory
// iteration and the bounded recursive search as soon as control returns from
// the underlying filesystem call.
func (c *Completer) WorkspaceCompletions(ctx context.Context, query, currentPath string) []Completion {
	if c == nil || ctx == nil {
		return nil
	}
	listed := c.listWorkspaceCompletions(ctx, query, currentPath)
	if err := ctx.Err(); err != nil || currentPath != "" || strings.TrimSpace(query) == "" {
		return listed
	}

	results := c.SearchFiles(ctx, query)
	existing := make(map[string]bool, len(listed))
	for _, item := range listed {
		existing[item.Insert] = true
	}
	for _, item := range results {
		if !existing[item.Insert] {
			listed = append(listed, item)
			existing[item.Insert] = true
		}
	}
	return listed
}

func (c *Completer) listWorkspaceCompletions(ctx context.Context, query, currentPath string) []Completion {
	query = filepath.ToSlash(strings.TrimSpace(query))
	currentPath = strings.Trim(filepath.ToSlash(strings.TrimSpace(currentPath)), "/")
	directoryPath := currentPath
	prefix := query
	if currentPath == "" {
		if slash := strings.LastIndex(query, "/"); slash >= 0 {
			directoryPath = strings.Trim(query[:slash], "/")
			prefix = query[slash+1:]
		}
	}
	if filepath.IsAbs(directoryPath) || directoryPath == ".." || strings.HasPrefix(directoryPath, "../") {
		return nil
	}

	root, err := filepath.Abs(c.workDir)
	if err != nil {
		return nil
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	directory := filepath.Join(root, filepath.FromSlash(directoryPath))
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil
	}
	within, err := filepath.Rel(root, directory)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}

	items := make([]Completion, 0, len(entries))
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		relative := filepath.ToSlash(filepath.Join(directoryPath, name))
		if c.ignorePatterns.Match(relative) || !strings.HasPrefix(name, prefix) {
			continue
		}

		isDirectory := entry.IsDir()
		display := name
		if currentPath == "" && directoryPath != "" {
			display = strings.TrimSuffix(directoryPath, "/") + "/" + display
		}
		if isDirectory {
			display += "/"
		}
		label := display
		if currentPath == "" {
			label = "@" + display
		}
		category := "file"
		insertPath := relative
		if isDirectory {
			category = "folder"
			insertPath += "/"
		}
		items = append(items, Completion{
			Label:    label,
			Insert:   "@" + insertPath + " ",
			Category: category,
		})
	}
	return items
}

func (c *Completer) completeSkill(input string) []Completion {
	var completions []Completion
	input = strings.TrimPrefix(input, "#")

	for _, skill := range c.skills {
		if strings.HasPrefix(skill, input) {
			completions = append(completions, Completion{
				Label:    "#" + skill,
				Insert:   "#" + skill + " ",
				Category: "skill",
			})
		}
	}

	return completions
}

// FilterCompletions searches visible labels, useful descriptions, and hidden
// metadata such as command aliases without turning aliases into duplicate rows.
func FilterCompletions(items []Completion, query string) []Completion {
	if query == "" {
		return items
	}
	q := strings.ToLower(query)
	var filtered []Completion
	for _, item := range items {
		haystack := strings.ToLower(strings.Join([]string{item.Label, item.Description, item.SearchTerms}, " "))
		if strings.Contains(haystack, q) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// SearchFiles performs a bounded, cancellation-aware filename search inside
// the workspace. Completion must never invoke MCP behind the permission and
// profile-scope broker; semantic search remains an explicit Cortex/MCP action.
func (c *Completer) SearchFiles(ctx context.Context, query string) []Completion {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	const maxResults = 10
	results := make([]Completion, 0, maxResults)
	_ = filepath.WalkDir(c.workDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return filepath.SkipAll
		default:
		}
		if path == c.workDir {
			return nil
		}
		relative, err := filepath.Rel(c.workDir, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if c.ignorePatterns.Match(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || isCompletionHeavyDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(strings.ToLower(relative), query) {
			return nil
		}
		if completion, ok := c.searchResultCompletion(relative); ok {
			completion.Description = "workspace filename match"
			results = append(results, completion)
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return results
}

func isCompletionHeavyDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build", "target", "__pycache__", ".venv":
		return true
	default:
		return false
	}
}

func (c *Completer) searchResultCompletion(path string) (Completion, bool) {
	path = strings.TrimSpace(strings.Trim(path, "`\"'"))
	if path == "" {
		return Completion{}, false
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(c.workDir, path)
	}
	absolute, err := filepath.Abs(absolute)
	if err != nil {
		return Completion{}, false
	}
	relative, err := filepath.Rel(c.workDir, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Completion{}, false
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return Completion{}, false
	}
	relative = filepath.ToSlash(relative)
	if c.ignorePatterns.Match(relative) {
		return Completion{}, false
	}
	return Completion{
		Label:       "@" + relative,
		Insert:      "@" + relative + " ",
		Category:    "search_result",
		Description: "workspace match",
	}, true
}

func (c *Completer) UpdateModels(models []string) {
	c.models = models
}

func (c *Completer) UpdateProviders(providers []string) {
	c.providers = providers
}

func (c *Completer) UpdateSkills(skills []string) {
	c.skills = skills
}

func (c *Completer) UpdateAgents(agents []string) {
	c.agents = agents
}

func (c *Completer) completeProviderArgs(prefix string) []Completion {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	fixed := []struct {
		arg, desc string
	}{
		{"list", "List configured provider profiles"},
	}
	var out []Completion
	for _, item := range fixed {
		if prefix != "" && !strings.HasPrefix(item.arg, prefix) {
			continue
		}
		out = append(out, Completion{
			Label:       "/provider " + item.arg,
			Insert:      "/provider " + item.arg + " ",
			Category:    "action",
			Description: item.desc,
			SearchTerms: item.arg,
		})
	}
	for _, name := range c.providers {
		lower := strings.ToLower(name)
		if prefix != "" && !strings.HasPrefix(lower, prefix) {
			continue
		}
		out = append(out, Completion{
			Label:       "/provider " + name,
			Insert:      "/provider " + name + " ",
			Category:    "provider",
			Description: "Switch inference provider",
			SearchTerms: name,
		})
	}
	return out
}

func (c *Completer) completeModelArgs(prefix string) []Completion {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	fixed := []struct {
		arg, desc string
	}{
		{"list", "List admitted models"},
		{"auto", "Resume automatic local model routing"},
	}
	var out []Completion
	for _, item := range fixed {
		if prefix != "" && !strings.HasPrefix(item.arg, prefix) {
			continue
		}
		out = append(out, Completion{
			Label:       "/model " + item.arg,
			Insert:      "/model " + item.arg + " ",
			Category:    "action",
			Description: item.desc,
			SearchTerms: item.arg,
		})
	}
	for _, name := range c.models {
		lower := strings.ToLower(name)
		if prefix != "" && !strings.HasPrefix(lower, prefix) {
			continue
		}
		out = append(out, Completion{
			Label:       "/model " + name,
			Insert:      "/model " + name + " ",
			Category:    "model",
			Description: "Switch model",
			SearchTerms: name,
		})
	}
	return out
}

// SetIgnorePatterns sets the ignore patterns used to filter file completions.
func (c *Completer) SetIgnorePatterns(patterns *config.IgnorePatterns) {
	c.ignorePatterns = patterns
}
