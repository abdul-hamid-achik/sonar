package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/tools"
)

const (
	maxToolCaptureBytes = 1024 * 1024
	maxFileReadBytes    = 8 * 1024 * 1024
	maxCopyBytes        = 64 * 1024 * 1024
)

func (a *Agent) toolsBuiltinToolDefs() []llm.ToolDef {
	defs := tools.AllToolDefs()
	filtered := make([]llm.ToolDef, 0, len(defs))
	for _, def := range defs {
		// internal/tools is drift-synced byte-identical with the sibling repo
		// and still defines consult_experts. sonar has no expert runtime — a
		// hosted provider serves one model per profile and exposes no local
		// inventory — so the definition must never reach the model.
		if def.Name == "consult_experts" {
			continue
		}
		if def.Name == "load_skill" && !a.hasSkillLoader() {
			continue
		}
		filtered = append(filtered, def)
	}
	// Subagent tools are sonar-only (internal/tools stays drift-synced) and
	// never reach a child: a subagent that could spawn subagents is a fork
	// bomb with an API bill.
	if !runningAsSubagent() {
		filtered = append(filtered, agentToolDef(), agentOutputToolDef())
	}
	return filtered
}

func (a *Agent) isToolsTool(name string) bool {
	return tools.IsBuiltinTool(name) || isSubagentTool(name)
}

func (a *Agent) handleToolsTool(ctx context.Context, tc llm.ToolCall) (string, bool) {
	switch tc.Name {
	case "grep":
		return a.handleGrep(ctx, tc.Arguments)
	case "read":
		return a.handleRead(tc.Arguments)
	case "write":
		return a.handleWrite(tc.Arguments)
	case "glob":
		return a.handleGlob(ctx, tc.Arguments)
	case "bash":
		return a.handleBash(ctx, tc.Arguments)
	case "bash_output":
		return a.handleBashOutput(tc.Arguments)
	case "ls":
		return a.handleLs(ctx, tc.Arguments)
	case "find":
		return a.handleFind(ctx, tc.Arguments)
	case "diff":
		return a.handleDiff(ctx, tc.Arguments)
	case "edit":
		return a.handleEdit(tc.Arguments)
	case "mkdir":
		return a.handleMkdir(tc.Arguments)
	case "remove":
		return a.handleRemove(tc.Arguments)
	case "copy":
		return a.handleCopy(tc.Arguments)
	case "move":
		return a.handleMove(tc.Arguments)
	case "exists":
		return a.handleExists(tc.Arguments)
	case "load_skill":
		return a.handleLoadSkill(tc.Arguments)
	case "agent":
		return a.handleAgentSpawn(tc.Arguments)
	case "agent_output":
		return a.handleAgentOutput(tc.Arguments)
	default:
		return fmt.Sprintf("unknown tool: %s", tc.Name), true
	}
}

func (a *Agent) getArgString(args map[string]any, key, defaultValue string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return defaultValue
}

func (a *Agent) getArgInt(args map[string]any, key string, defaultValue int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if n == "" {
				return defaultValue
			}
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return defaultValue
}

func (a *Agent) getArgBool(args map[string]any, key string, defaultValue bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return defaultValue
}

func (a *Agent) resolvePath(path string) (string, error) {
	resolved, workspaceErr := a.resolveWorkspacePath(path)
	if workspaceErr == nil {
		return resolved, nil
	}
	resolved, additionalErr := a.resolveAdditionalWritePath(path)
	if additionalErr == nil {
		return resolved, nil
	}
	if !errors.Is(additionalErr, errOutsideAdditionalWriteAuthority) {
		return "", additionalErr
	}
	return "", workspaceErr
}

func (a *Agent) resolveWorkspacePath(path string) (string, error) {
	filesystem := a.filesystemContext()
	lexicalRoot := filesystem.workDir
	if lexicalRoot == "" {
		var err error
		lexicalRoot, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
	}

	lexicalRoot, err := filepath.Abs(lexicalRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	root := lexicalRoot
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}

	candidate := path
	requestedAbsolute := filepath.IsAbs(candidate)
	if !requestedAbsolute {
		candidate = filepath.Join(lexicalRoot, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	lexicalRel, lexicalInside, err := workspaceRelative(lexicalRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve lexical path %q: %w", path, err)
	}
	if !lexicalInside && !requestedAbsolute {
		return "", fmt.Errorf("path %q escapes workspace %q", path, root)
	}
	if lexicalInside && pathIgnoredWithContent(filesystem.ignoreContent, lexicalRel) {
		return "", fmt.Errorf("path %q is excluded by .agentignore", path)
	}
	candidate, err = resolveExistingAncestor(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}

	rel, inside, err := physicalCanonicalRelative(root, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if !inside {
		return "", fmt.Errorf("path %q escapes workspace %q", path, root)
	}
	if pathIgnoredWithContent(filesystem.ignoreContent, rel) {
		return "", fmt.Errorf("path %q is excluded by .agentignore", path)
	}
	return filepath.Join(root, rel), nil
}

// resolveDestructivePath confines a remove/rename operand by canonicalizing
// its parent while deliberately preserving the final path component. This
// makes approval match the visible object: removing or moving `link` acts on
// the symlink itself, not on the file or directory it points to.
func (a *Agent) resolveDestructivePath(path string) (string, error) {
	filesystem := a.filesystemContext()
	lexicalRoot := filesystem.workDir
	if lexicalRoot == "" {
		var err error
		lexicalRoot, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
	}
	lexicalRoot, err := filepath.Abs(lexicalRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	root := lexicalRoot
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}

	candidate := path
	requestedAbsolute := filepath.IsAbs(candidate)
	if !requestedAbsolute {
		candidate = filepath.Join(lexicalRoot, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	lexicalRel, lexicalInside, err := workspaceRelative(lexicalRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve lexical path %q: %w", path, err)
	}
	if !lexicalInside && !requestedAbsolute {
		return "", fmt.Errorf("path %q escapes workspace %q", path, root)
	}
	if lexicalInside && pathIgnoredWithContent(filesystem.ignoreContent, lexicalRel) {
		return "", fmt.Errorf("path %q is excluded by .agentignore", path)
	}
	if filepath.Clean(candidate) == filepath.Clean(lexicalRoot) {
		return filepath.Clean(root), nil
	}
	if rootInfo, rootErr := os.Stat(root); rootErr == nil {
		if candidateInfo, candidateErr := os.Lstat(candidate); candidateErr == nil && os.SameFile(rootInfo, candidateInfo) {
			return filepath.Clean(root), nil
		}
	}
	parent, err := resolveExistingAncestor(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve parent for %q: %w", path, err)
	}
	parentRel, inside, err := physicalCanonicalRelative(root, parent)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if !inside {
		return "", fmt.Errorf("path %q escapes workspace %q", path, root)
	}
	parent = filepath.Join(root, parentRel)
	name := filepath.Base(candidate)
	candidate = filepath.Join(parent, name)
	if info, statErr := os.Lstat(candidate); statErr == nil {
		actualName, nameErr := physicalEntryName(parent, name, info)
		if nameErr != nil {
			return "", fmt.Errorf("resolve destructive path entry %q: %w", path, nameErr)
		}
		name = actualName
		candidate = filepath.Join(parent, name)
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect destructive path %q: %w", path, statErr)
	}
	rel := filepath.Join(parentRel, name)
	if pathIgnoredWithContent(filesystem.ignoreContent, rel) {
		return "", fmt.Errorf("path %q is excluded by .agentignore", path)
	}
	return filepath.Clean(candidate), nil
}

func workspaceRelative(root, candidate string) (string, bool, error) {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", false, err
	}
	inside := rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	return rel, inside, nil
}

// resolveExistingAncestor canonicalizes symlinks even for a path that does
// not exist yet by resolving its closest existing ancestor first.
func resolveExistingAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (a *Agent) pathIgnored(path string) bool {
	return pathIgnoredWithContent(a.filesystemContext().ignoreContent, path)
}

func ignorePatternMatches(pattern, cleanPath string) bool {
	if cleanPath == pattern || strings.HasPrefix(cleanPath, pattern+"/") {
		return true
	}
	if re, err := regexp.Compile(globPatternToRegex(pattern)); err == nil && re.MatchString(cleanPath) {
		return true
	}
	if strings.Contains(pattern, "/") {
		return false
	}
	for _, part := range strings.Split(cleanPath, "/") {
		if matched, _ := filepath.Match(pattern, part); matched {
			return true
		}
	}
	return false
}
