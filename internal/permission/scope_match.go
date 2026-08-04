package permission

import (
	"path/filepath"
	"strings"
	"unicode"
)

// shellControlMarkers split or rebind shell authority. A bash-prefix grant
// never applies to commands that contain them; those stay exact-request only.
var shellControlMarkers = []string{
	"&&", "||", ";", "|", "\n", "`", "$(", "${", ">", "<",
}

// multiWordBashRunners need a subcommand before a stable prefix is formed.
var multiWordBashRunners = map[string]struct{}{
	"go": {}, "npm": {}, "npx": {}, "pnpm": {}, "yarn": {}, "bun": {},
	"git": {}, "cargo": {}, "docker": {}, "kubectl": {}, "helm": {},
	"python": {}, "python3": {}, "pip": {}, "pip3": {}, "uv": {},
	"task": {}, "make": {}, "just": {}, "poetry": {}, "composer": {},
}

// DeriveBashPrefix extracts a safe session/workspace prefix from an approved
// command. Compound shell commands return false so only exact-request applies.
func DeriveBashPrefix(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" || BashCommandHasControl(command) {
		return "", false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	first := fields[0]
	// Reject path-looking binaries for durable-style prefixes; session may still
	// store them, but prefer the basename only for relative ./bin style.
	if strings.Contains(first, "/") || strings.Contains(first, `\`) {
		first = filepath.Base(first)
		if first == "" || first == "." || first == ".." {
			return "", false
		}
	}
	if _, multi := multiWordBashRunners[first]; multi && len(fields) >= 2 {
		second := fields[1]
		if second == "" || strings.ContainsAny(second, ";&|") {
			return first, true
		}
		return first + " " + second, true
	}
	return first, true
}

// BashPrefixMatches reports whether command is authorized by a literal prefix
// under the host contract: exact match or prefix + space, never across shell
// control. Prefer BashPatternMatches for user-authored rules that may use *.
func BashPrefixMatches(command, prefix string) bool {
	command = strings.TrimSpace(command)
	prefix = strings.TrimSpace(prefix)
	if command == "" || prefix == "" {
		return false
	}
	if BashCommandHasControl(command) {
		return false
	}
	if command == prefix {
		return true
	}
	return strings.HasPrefix(command, prefix+" ")
}

// BashPatternMatches authorizes a command against a durable/session pattern.
// Patterns without * use prefix matching. Patterns with * support Claude-style
// forms such as "git status *" / "go test *" (trailing wildcard only).
// Compound shell commands never match.
func BashPatternMatches(command, pattern string) bool {
	command = strings.TrimSpace(command)
	pattern = strings.TrimSpace(pattern)
	if command == "" || pattern == "" {
		return false
	}
	if BashCommandHasControl(command) {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return BashPrefixMatches(command, pattern)
	}
	normalized, ok := NormalizeBashPattern(pattern)
	if !ok {
		return false
	}
	return matchBashTrailingGlob(command, normalized)
}

// matchBashTrailingGlob implements "prefix *" / "prefix*": the command must
// equal the literal head or start with head+" " (when the pattern ends with
// a separate * token) or head (when * is glued to the last token).
func matchBashTrailingGlob(command, pattern string) bool {
	// Normalized globs are either "tok tok *" or end with a single * on the last field.
	fields := strings.Fields(pattern)
	if len(fields) == 0 {
		return false
	}
	if fields[len(fields)-1] == "*" {
		head := strings.Join(fields[:len(fields)-1], " ")
		if head == "" {
			return false
		}
		return BashPrefixMatches(command, head)
	}
	// Glued form: last field ends with *
	last := fields[len(fields)-1]
	if !strings.HasSuffix(last, "*") || strings.Count(last, "*") != 1 {
		return false
	}
	literalLast := strings.TrimSuffix(last, "*")
	headFields := append(append([]string{}, fields[:len(fields)-1]...), literalLast)
	head := strings.TrimSpace(strings.Join(headFields, " "))
	if head == "" {
		return false
	}
	// "go*" matches "go", "gofmt", "go test" — too broad. Require the glued *
	// only as a suffix after at least one character of the last token, and
	// match command fields with the same shape.
	cmdFields := strings.Fields(command)
	if len(cmdFields) < len(headFields) {
		return false
	}
	for i := 0; i < len(headFields)-1; i++ {
		if cmdFields[i] != headFields[i] {
			return false
		}
	}
	// Last required token is a prefix of the corresponding command field.
	return strings.HasPrefix(cmdFields[len(headFields)-1], literalLast)
}

// BashCommandHasControl reports shell operators that break single-command grants.
func BashCommandHasControl(command string) bool {
	for _, marker := range shellControlMarkers {
		if strings.Contains(command, marker) {
			return true
		}
	}
	// Unquoted $VAR expansion can rebind authority.
	if strings.Contains(command, "$") {
		return true
	}
	return false
}

// NormalizeBashPrefix validates and trims a user-supplied or derived prefix
// without wildcards. Prefer NormalizeBashPattern when * is allowed.
func NormalizeBashPrefix(prefix string) (string, bool) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.Contains(prefix, "*") {
		return "", false
	}
	if BashCommandHasControl(prefix) {
		return "", false
	}
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return "", false
	}
	for _, field := range fields {
		if field == "" {
			return "", false
		}
		for _, r := range field {
			if unicode.IsControl(r) {
				return "", false
			}
		}
	}
	return strings.Join(fields, " "), true
}

// NormalizeBashPattern accepts literal prefixes and restricted trailing globs
// such as "git status *" or "go test*". Bare "*", leading "*", and mid-pattern
// wildcards are rejected.
func NormalizeBashPattern(pattern string) (string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", false
	}
	if BashCommandHasControl(pattern) {
		return "", false
	}
	if !strings.Contains(pattern, "*") {
		return NormalizeBashPrefix(pattern)
	}
	fields := strings.Fields(pattern)
	if len(fields) == 0 {
		return "", false
	}
	starCount := 0
	for i, field := range fields {
		for _, r := range field {
			if unicode.IsControl(r) {
				return "", false
			}
		}
		starCount += strings.Count(field, "*")
		if strings.Contains(field, "*") {
			// Only the last field may contain *, and only as a trailing suffix
			// or the whole token "*".
			if i != len(fields)-1 {
				return "", false
			}
			if field == "*" {
				if len(fields) < 2 {
					return "", false // bare *
				}
				continue
			}
			if !strings.HasSuffix(field, "*") || strings.Count(field, "*") != 1 {
				return "", false
			}
			if strings.TrimSuffix(field, "*") == "" {
				return "", false
			}
		}
	}
	if starCount != 1 {
		return "", false
	}
	return strings.Join(fields, " "), true
}

// NormalizeMCPToolName requires an exact namespaced tool name server__tool.
func NormalizeMCPToolName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, " ") {
		return "", false
	}
	parts := strings.Split(name, "__")
	if len(parts) < 2 {
		return "", false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", false
		}
	}
	return name, true
}

// PathGrantMatches compares canonical absolute paths for session_path grants.
func PathGrantMatches(requestPath, grantedPath string) bool {
	requestPath = filepath.Clean(strings.TrimSpace(requestPath))
	grantedPath = filepath.Clean(strings.TrimSpace(grantedPath))
	if requestPath == "" || grantedPath == "" {
		return false
	}
	return requestPath == grantedPath
}

// NormalizeWritePath stores a workspace-relative path when the target is inside
// the workspace. Absolute paths outside the workspace are rejected.
func NormalizeWritePath(workspace, path string) (string, bool) {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	path = strings.TrimSpace(path)
	if workspace == "" || path == "" {
		return "", false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	absolute = filepath.Clean(absolute)
	ws := workspace
	if resolved, err := filepath.EvalSymlinks(ws); err == nil {
		ws = resolved
	}
	ws = filepath.Clean(ws)
	rel, err := filepath.Rel(ws, absolute)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// Store portable slash form for stability across platforms in JSON.
	return filepath.ToSlash(rel), true
}

// WritePathMatches reports whether absolutePath is covered by a stored
// workspace-relative grant.
func WritePathMatches(workspace, absolutePath, grantedRel string) bool {
	normalized, ok := NormalizeWritePath(workspace, absolutePath)
	if !ok {
		return false
	}
	grantedRel = filepath.ToSlash(filepath.Clean(strings.TrimSpace(grantedRel)))
	return normalized == grantedRel
}
