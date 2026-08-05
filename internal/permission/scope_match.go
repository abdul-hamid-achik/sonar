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

// plainBashLeadingField reports whether a whitespace-delimited field is a bare
// shell word: no quoting, no escapes, no expansion or control characters. For
// such a field a naive strings.Fields split and sh's own tokenizer agree, so
// it is safe to lift into a derived prefix even when the rest of the command
// carries composition markers.
func plainBashLeadingField(field string) bool {
	if field == "" || strings.ContainsAny(field, "'\"\\`$&|;<>(){}#") {
		return false
	}
	for _, r := range field {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// bashTrivialSegmentLeaders are shell builtins the AUTO catalog admits
// unconditionally; a prefix for one of them is never what an approval prompt
// was about. Skipping them when choosing the derivation segment is the
// difference between offering "echo" for
// `echo "=== TODO ==="; grep -rn TODO src | head` — a placebo, since echo
// never prompted — and offering the grep that actually tripped the modal. In
// the audited session ten of the fourteen always presses landed on echo- or
// cd-led compounds exactly like that one.
var bashTrivialSegmentLeaders = map[string]struct{}{
	"echo": {}, "cd": {}, "true": {}, "false": {}, "pwd": {}, "test": {}, "printf": {}, ":": {}, "exit": {},
}

// inertBashStatusParameter mirrors the host scanner's rule: $?, $$, $# and $!
// are fixed by POSIX to decimal integers, so the ubiquitous
// `; echo "EXIT=$?"` tail cannot expand into a command or a path and does not
// forfeit derivation. Every other $ form remains dynamic and refuses.
func inertBashStatusParameter(runes []rune, index int) bool {
	if index+1 >= len(runes) {
		return false
	}
	switch runes[index+1] {
	case '?', '$', '#', '!':
		return true
	default:
		return false
	}
}

// firstDerivableBashSegment walks the command's top-level static segments with
// a quote-aware scan and returns the whitespace fields of the first segment
// worth deriving a prefix from: the first whose leading word is not a trivial
// builtin, falling back to the first trivial one when nothing else exists.
//
// It fails closed — nothing is offered — when the command contains dynamic
// syntax (a non-inert $, a backtick, grouping, input redirection) or any
// segment whose leading word is not a plain bare token: in both cases sh may
// parse a different command than a naive field split suggests. Sloppiness
// here can never over-grant — the offered prefix is displayed to the user
// before it is saved, and matching goes through the host's own splitter — but
// a misleading offer is still an offer, so ambiguity refuses.
func firstDerivableBashSegment(command string) ([]string, bool) {
	runes := []rune(command)
	var quote rune
	escaped := false
	segmentStart := 0
	var chosen []string
	var fallback []string
	closeSegment := func(end int) bool {
		fields := strings.Fields(string(runes[segmentStart:end]))
		if len(fields) == 0 {
			return true
		}
		if !plainBashLeadingField(fields[0]) {
			return false
		}
		if _, trivial := bashTrivialSegmentLeaders[fields[0]]; trivial {
			if fallback == nil {
				fallback = fields
			}
			return true
		}
		if chosen == nil {
			chosen = fields
		}
		return true
	}
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == '\'' {
			if r == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch r {
			case '"':
				quote = 0
			case '$':
				if !inertBashStatusParameter(runes, index) {
					return nil, false
				}
				index++
			case '`':
				return nil, false
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '$':
			if !inertBashStatusParameter(runes, index) {
				return nil, false
			}
			index++
		case '`', '(', ')', '{', '}', '<':
			return nil, false
		case ';', '\n', '|', '&':
			// && and || read as two boundaries with an empty segment between;
			// empty segments are skipped, so the effect is identical.
			if !closeSegment(index) {
				return nil, false
			}
			segmentStart = index + 1
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if !closeSegment(len(runes)) {
		return nil, false
	}
	if chosen != nil {
		return chosen, true
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

// DeriveBashPrefix extracts a safe session/workspace prefix from an approved
// command.
//
// A command whose control content is limited to static composition — &&, ||,
// ;, |, newlines, redirects, inert status parameters — derives the prefix
// from its first non-trivial segment instead of failing. This is what makes
// an "always"-style approval mean something for compound commands: in one
// audited AUTO session 33 of the 34 prompted commands carried a composition
// marker, so every always press fell back to an exact-request grant keyed on
// an argument hash a model never re-sends byte-identically — the user granted
// 14 times and zero grants ever fired. The derived prefix carries executable
// authority only: whole-command matching (BashPatternMatches) still refuses
// control-bearing commands, and segment matching (BashSegmentPatternMatches)
// applies only inside a composition the host has already validated, so the
// text outside the derived fields — including every other segment — gains
// nothing from the grant.
func DeriveBashPrefix(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", false
	}
	if !BashCommandHasControl(command) {
		return deriveBashPrefixFields(strings.Fields(command), false)
	}
	fields, ok := firstDerivableBashSegment(command)
	if !ok {
		return "", false
	}
	return deriveBashPrefixFields(fields, true)
}

func deriveBashPrefixFields(fields []string, compound bool) (string, bool) {
	if len(fields) == 0 {
		return "", false
	}
	first := fields[0]
	if compound && !plainBashLeadingField(first) {
		return "", false
	}
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
		if compound && !plainBashLeadingField(second) {
			return "", false
		}
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

// BashSegmentPatternMatches authorizes ONE already-split command segment
// against a saved prefix or pattern.
//
// It exists because BashPatternMatches deliberately refuses any command text
// bearing control markers: that guard is host-independent, so it cannot know
// whether a composition was validated, and it made saved prefixes unable to
// reach the segments of compound commands at all. This matcher instead takes
// the segment as the host's static split already produced it — words are argv
// elements with quoting resolved and every unquoted control marker consumed by
// the splitter — and matches field-wise against the pattern, so one argv word
// containing a space or a quoted marker can never satisfy two pattern fields
// (`go "test extra"` does not match "go test").
//
// The caller owns composition safety. The pattern supplies executable
// authority only: the host must still have validated splitting, dynamic
// syntax, redirect targets, and path operands before consulting this.
func BashSegmentPatternMatches(words []string, pattern string) bool {
	if len(words) == 0 {
		return false
	}
	normalized, ok := NormalizeBashPattern(pattern)
	if !ok {
		return false
	}
	patternFields := strings.Fields(normalized)
	if len(patternFields) == 0 {
		return false
	}
	gluedPrefix := ""
	glued := false
	switch last := patternFields[len(patternFields)-1]; {
	case last == "*":
		// "head *" matches its exact head or the head plus arguments, exactly
		// like the whole-command matcher's trailing-glob form.
		patternFields = patternFields[:len(patternFields)-1]
	case strings.HasSuffix(last, "*"):
		gluedPrefix = strings.TrimSuffix(last, "*")
		patternFields = patternFields[:len(patternFields)-1]
		glued = true
	}
	required := len(patternFields)
	if glued {
		required++
	}
	if required == 0 || len(words) < required {
		return false
	}
	for index, field := range patternFields {
		if words[index] != field {
			return false
		}
	}
	return !glued || strings.HasPrefix(words[len(patternFields)], gluedPrefix)
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
