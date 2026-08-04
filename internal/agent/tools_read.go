package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func (a *Agent) handleGrep(ctx context.Context, args map[string]any) (string, bool) {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: grep cancelled: %v", err), true
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "error: pattern is required", true
	}

	requestedPath := a.getArgString(args, "path", a.activeWorkDir())
	readable, err := a.resolveReadablePath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()
	path := readable.absolute
	include := a.getArgString(args, "include", "")
	context := a.getArgInt(args, "context", 3)
	if context < 0 {
		context = 0
	}
	if context > 20 {
		context = 20
	}
	maxResults := a.MaxGrepResults()

	if _, err := readable.stat(); err != nil {
		return fmt.Sprintf("error: path does not exist: %s", path), true
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("error: invalid regex pattern: %v", err), true
	}

	// Validate the include glob before walking. filepath.Match's error was
	// folded into the same branch as "did not match", so a malformed pattern
	// skipped every file and returned a confident "No matches found".
	if include != "" {
		if _, matchErr := filepath.Match(include, "probe"); matchErr != nil {
			return fmt.Sprintf("error: invalid include pattern: %v", matchErr), true
		}
	}

	var results []string
	coverage := &searchCoverage{}
	err = readable.walk(func(filePath string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			coverage.skipUnlistable()
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if filePath != path && readable.ignored(a, filePath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			if filePath != path && shouldSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		if include != "" {
			// The pattern was validated above, so a non-match here is a real
			// non-match rather than a swallowed error.
			if matched, _ := filepath.Match(include, info.Name()); !matched {
				return nil
			}
		}

		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}

		content, err := readable.readBoundedAt(filePath, maxFileReadBytes)
		if err != nil {
			coverage.skipUnreadable(info, maxFileReadBytes)
			return nil
		}
		coverage.scan()

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if re.MatchString(line) {
				relPath, _ := filepath.Rel(path, filePath)

				ctxStart := i - context
				if ctxStart < 0 {
					ctxStart = 0
				}
				ctxEnd := i + context + 1
				if ctxEnd > len(lines) {
					ctxEnd = len(lines)
				}

				results = append(results, fmt.Sprintf("%s:%d: %s", relPath, i+1, line))

				if context > 0 && ctxStart < i {
					for j := ctxStart; j < i; j++ {
						if len(results) < maxResults {
							results = append(results, fmt.Sprintf("  %d: %s", j+1, lines[j]))
						}
					}
				}
				if context > 0 && i+1 < ctxEnd {
					for j := i + 1; j < ctxEnd; j++ {
						if len(results) < maxResults {
							results = append(results, fmt.Sprintf("  %d: %s", j+1, lines[j]))
						}
					}
				}

				if len(results) >= maxResults {
					results = append(results, fmt.Sprintf("\n... (truncated, max %d results)", maxResults))
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Sprintf("error walking directory: %v", err), true
	}

	if len(results) == 0 {
		return coverage.qualify(fmt.Sprintf("No matches found for pattern: %s", pattern)), false
	}

	return coverage.qualify(strings.Join(results, "\n")), false
}

func (a *Agent) handleRead(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		return "error: path is required", true
	}

	readable, err := a.resolveReadablePath(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()

	data, err := readable.readBounded(maxFileReadBytes)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err), true
	}

	lines := strings.Split(string(data), "\n")

	offset := a.getArgInt(args, "offset", 1)
	limit := a.getArgInt(args, "limit", 0)

	if offset > len(lines) {
		return "error: offset beyond file length", true
	}

	if offset > 1 {
		lines = lines[offset-1:]
	}

	if limit > 0 && len(lines) > limit {
		remaining := len(lines) - limit
		lines = lines[:limit]
		content := strings.Join(lines, "\n")
		content += fmt.Sprintf("\n\n... (%d more lines)", remaining)
		return content, false
	}

	return strings.Join(lines, "\n"), false
}

func (a *Agent) handleGlob(ctx context.Context, args map[string]any) (string, bool) {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: glob cancelled: %v", err), true
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "error: pattern is required", true
	}

	requestedPath := a.getArgString(args, "path", a.activeWorkDir())
	readable, err := a.resolveReadablePath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()
	path := readable.absolute

	if _, err := readable.stat(); err != nil {
		return fmt.Sprintf("error: path does not exist: %s", path), true
	}

	basePattern := filepath.Join(path, pattern)
	_ = basePattern

	matcher, err := regexp.Compile(globPatternToRegex(pattern))
	if err != nil {
		return fmt.Sprintf("error: invalid pattern: %v", err), true
	}

	var matches []string
	coverage := &searchCoverage{}
	err = readable.walk(func(filePath string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			coverage.skipUnlistable()
			return nil
		}
		coverage.scan()
		if filePath != path && readable.ignored(a, filePath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() && filePath != path && shouldSkipDir(info.Name()) {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(path, filePath)
		if err != nil || rel == "." {
			return nil
		}

		rel = filepath.ToSlash(rel)
		if matcher.MatchString(rel) {
			matches = append(matches, rel)
			if len(matches) >= a.MaxGrepResults() {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("error walking directory: %v", err), true
	}

	if len(matches) == 0 {
		return coverage.qualify(fmt.Sprintf("No files match pattern: %s", pattern)), false
	}

	sort.Strings(matches)
	return coverage.qualify(strings.Join(matches, "\n")), false
}

func (a *Agent) handleLs(ctx context.Context, args map[string]any) (string, bool) {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: ls cancelled: %v", err), true
	}
	requestedPath := a.getArgString(args, "path", a.activeWorkDir())
	readable, err := a.resolveReadablePath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()
	path := readable.absolute

	entries, hadEntries, err := readable.readDirBounded(ctx, a.MaxGrepResults(), func(entry os.DirEntry) bool {
		return !readable.ignored(a, filepath.Join(path, entry.Name()))
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Sprintf("error: ls cancelled: %v", contextErr), true
		}
		return fmt.Sprintf("error reading directory: %v", err), true
	}
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: ls cancelled: %v", err), true
	}

	if !hadEntries {
		return "Directory is empty", false
	}

	var dirs []string
	var files []string

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Sprintf("error: ls cancelled: %v", err), true
		}
		name := e.Name()
		if e.IsDir() {
			dirs = append(dirs, name+"/")
		} else {
			files = append(files, name)
		}
	}

	var result strings.Builder
	for _, d := range dirs {
		result.WriteString(d + "\n")
	}
	for _, f := range files {
		result.WriteString(f + "\n")
	}

	return result.String(), false
}

func (a *Agent) handleFind(ctx context.Context, args map[string]any) (string, bool) {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: find cancelled: %v", err), true
	}
	name, _ := args["name"].(string)
	if name == "" {
		return "error: name is required", true
	}

	requestedPath := a.getArgString(args, "path", a.activeWorkDir())
	readable, err := a.resolveReadablePath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()
	path := readable.absolute
	fileType := a.getArgString(args, "type", "")

	if _, err := readable.stat(); err != nil {
		return fmt.Sprintf("error: path does not exist: %s", path), true
	}

	matchesName := func(base string) (bool, error) {
		return filepath.Match(name, base)
	}
	if _, err := matchesName("probe"); err != nil {
		return fmt.Sprintf("error: invalid name pattern: %v", err), true
	}

	var results []string
	coverage := &searchCoverage{}
	err = readable.walk(func(filePath string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			coverage.skipUnlistable()
			return nil
		}
		coverage.scan()
		if filePath != path && readable.ignored(a, filePath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if shouldSkipDir(info.Name()) && filePath != path {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		isDir := info.IsDir()
		if fileType == "f" && isDir {
			return nil
		}
		if fileType == "d" && !isDir {
			return nil
		}

		matched, matchErr := matchesName(info.Name())
		if matchErr != nil {
			return matchErr
		}
		if matched {
			relPath, _ := filepath.Rel(path, filePath)
			if relPath != "." {
				if isDir {
					results = append(results, relPath+"/")
				} else {
					results = append(results, relPath)
				}
			}
			if len(results) >= a.MaxGrepResults() {
				return filepath.SkipAll
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Sprintf("error walking directory: %v", err), true
	}

	if len(results) == 0 {
		return coverage.qualify(fmt.Sprintf("No files/directories found matching: %s", name)), false
	}

	return coverage.qualify(strings.Join(results, "\n")), false
}

func (a *Agent) handleExists(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		return "error: path is required", true
	}

	readable, err := a.resolveReadablePath(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()
	path = readable.absolute

	info, err := readable.stat()
	if os.IsNotExist(err) {
		return fmt.Sprintf("false: %s does not exist", path), false
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}

	if info.IsDir() {
		return fmt.Sprintf("true: %s (directory)", path), false
	}
	return fmt.Sprintf("true: %s (file, %d bytes)", path, info.Size()), false
}

func shouldSkipDir(name string) bool {
	switch name {
	case "node_modules", ".git", "__pycache__", ".venv", "venv",
		"dist", "build", "target", ".cache", ".npm",
		".svn", "CVS", ".hg", ".bzr":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func globPatternToRegex(pattern string) string {
	pattern = filepath.ToSlash(pattern)

	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(pattern[i])
		default:
			b.WriteByte(pattern[i])
		}
	}
	b.WriteString("$")
	return b.String()
}
