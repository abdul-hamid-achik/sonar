package agent

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

var autoCommandAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^=]*$`)
var autoCommandAssignmentValue = regexp.MustCompile(`^[A-Za-z0-9_.-]*$`)
var autoSedPrintProgram = regexp.MustCompile(`^(?:[0-9]+|\$)(?:,(?:[0-9]+|\$))?[pP]$`)

const (
	maxAutoCommandBytes    = 16 * 1024
	maxAutoCommandSegments = 16
	maxAutoCommandWords    = 256
)

type autoCommandDisposition uint8

const (
	autoCommandRequiresApproval autoCommandDisposition = iota
	autoCommandAdmitted
)

type autoCommandEffect uint8

const (
	autoCommandEffectNone autoCommandEffect = iota
	autoCommandEffectReadOnly
	autoCommandEffectWorkspaceMutation
	autoCommandEffectWorkspaceExecution
)

type autoCommandReason uint8

const (
	autoCommandReasonAllowed autoCommandReason = iota
	autoCommandReasonEmpty
	autoCommandReasonBounds
	autoCommandReasonDynamicSyntax
	autoCommandReasonAmbiguousComposition
	autoCommandReasonExecutable
	autoCommandReasonArguments
	autoCommandReasonPathAuthority
)

// autoCommandAssessment is the bounded host-owned projection of one AUTO
// shell admission decision. It deliberately retains neither the raw command
// nor its arguments: those may contain private values and already live on the
// execution request. The typed effect and provenance flags keep the authority
// boundary inspectable without turning arbitrary shell text into policy.
type autoCommandAssessment struct {
	disposition         autoCommandDisposition
	effect              autoCommandEffect
	reason              autoCommandReason
	segments            int
	usesReadGrant       bool
	workspaceExecutable bool
}

func (assessment autoCommandAssessment) admitted() bool {
	return assessment.disposition == autoCommandAdmitted
}

type autoSimpleCommandAssessment struct {
	allowed             bool
	effect              autoCommandEffect
	reason              autoCommandReason
	usesReadGrant       bool
	workspaceExecutable bool
}

type autoPathAuthority uint8

const (
	autoPathDenied autoPathAuthority = iota
	autoPathWorkspace
	autoPathReadGrant
)

// autoScopedCommandAllowed recognizes a deliberately bounded shell subset for
// AUTO. It is not a shell sandbox: the subprocess still runs under the host's
// ordinary account. The catalog exists to keep routine local build, test,
// formatting, and inspection work flowing while Git, dynamic expansion,
// destructive commands, network-facing CLIs, redirection to files, and
// workspace escapes continue through interactive approval.
func (a *Agent) autoScopedCommandAllowed(command string) bool {
	return a.assessAutoScopedCommand(command).admitted()
}

func (a *Agent) assessAutoScopedCommand(command string) autoCommandAssessment {
	assessment := autoCommandAssessment{disposition: autoCommandRequiresApproval}
	// The policy scanner and the POSIX shell must observe the same character
	// stream. Invalid UTF-8 can otherwise be normalized differently by rune
	// iteration and by the child process, turning a rejected token boundary into
	// an executable one.
	if !utf8.ValidString(command) {
		assessment.reason = autoCommandReasonDynamicSyntax
		return assessment
	}
	if strings.TrimSpace(command) == "" {
		assessment.reason = autoCommandReasonEmpty
		return assessment
	}
	if len(command) > maxAutoCommandBytes {
		assessment.reason = autoCommandReasonBounds
		return assessment
	}
	if strings.ContainsRune(command, '\r') ||
		hasShellLineContinuation(command) || hasDynamicShellSyntax(command) || hasUnquotedShellGlob(command) {
		assessment.reason = autoCommandReasonDynamicSyntax
		return assessment
	}
	commands, separators, ok := splitStaticShellCommands(command)
	if !ok || len(commands) == 0 || len(commands) > maxAutoCommandSegments {
		assessment.reason = autoCommandReasonBounds
		return assessment
	}
	assessment.segments = len(commands)
	for _, words := range commands {
		if len(words) > maxAutoCommandWords {
			assessment.reason = autoCommandReasonBounds
			return assessment
		}
	}
	if len(commands) > 1 && staticCommandsContainExecutable(commands, "cd") {
		// A failed cd must not fall through to a later command. A pipeline after
		// an &&-guarded command is safe (`cd x && query | bounded-filter`), but a
		// cd that is itself a pipeline member runs in an isolated subshell and
		// cannot establish the base directory modeled below.
		for index, separator := range separators {
			if separator == ";" || separator == "||" || separator == "|" &&
				(staticCommandExecutable(commands[index]) == "cd" || staticCommandExecutable(commands[index+1]) == "cd") {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
		}
	}
	baseDir := a.activeWorkDir()
	plannedMinervaBuild := false
	minervaPrefixEligible := true
	minervaOutputPipeline := false
	for index, words := range commands {
		simple := a.assessAutoScopedSimpleCommand(words, baseDir)
		isMinervaOutputFilter := false
		if minervaOutputPipeline {
			if index == 0 || separators[index-1] != "|" || !a.autoScopedMinervaOutputFilterAllowed(words) {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
			simple = autoSimpleCommandAssessment{
				allowed: true, effect: autoCommandEffectReadOnly, reason: autoCommandReasonAllowed,
			}
			isMinervaOutputFilter = true
		} else if !simple.allowed && plannedMinervaBuild && index > 0 && separators[index-1] == "&&" &&
			a.autoScopedPlannedMinervaWorkspaceCommandAllowed(words, baseDir) {
			simple = autoSimpleCommandAssessment{
				allowed: true, effect: autoCommandEffectWorkspaceExecution,
				reason: autoCommandReasonAllowed, workspaceExecutable: true,
			}
		}
		if !simple.allowed {
			assessment.reason = simple.reason
			return assessment
		}
		if simple.workspaceExecutable {
			// Minerva is a bounded query surface, not a generic pipeline source or
			// filter. Accept only an optional leading cd or an immediately preceding
			// exact Minerva build producer. No already-trusted binary may be replaced
			// by a different command earlier in the same shell request.
			for _, separator := range separators[:index] {
				if separator != "&&" {
					assessment.reason = autoCommandReasonAmbiguousComposition
					return assessment
				}
			}
			if !minervaPrefixEligible && !plannedMinervaBuild ||
				index > 0 && separators[index-1] != "&&" {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
			if index != len(commands)-1 {
				if separators[index] != "|" {
					assessment.reason = autoCommandReasonAmbiguousComposition
					return assessment
				}
				minervaOutputPipeline = true
			}
		} else if isMinervaOutputFilter {
			if index != len(commands)-1 && separators[index] != "|" {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
		}
		if simple.effect > assessment.effect {
			assessment.effect = simple.effect
		}
		assessment.usesReadGrant = assessment.usesReadGrant || simple.usesReadGrant
		assessment.workspaceExecutable = assessment.workspaceExecutable || simple.workspaceExecutable
		isMinervaBuild := a.autoScopedMinervaBuildCommandAllowed(words, baseDir)
		isCD := staticCommandExecutable(words) == "cd"
		if !isCD && !isMinervaBuild && !simple.workspaceExecutable && !isMinervaOutputFilter {
			minervaPrefixEligible = false
		}
		if isCD && plannedMinervaBuild {
			minervaPrefixEligible = false
		}
		plannedMinervaBuild = isMinervaBuild
		if isCD {
			var cdOK bool
			baseDir, cdOK = a.autoScopedCDTarget(words, baseDir)
			if !cdOK {
				assessment.reason = autoCommandReasonPathAuthority
				return assessment
			}
		}
	}
	assessment.disposition = autoCommandAdmitted
	assessment.reason = autoCommandReasonAllowed
	return assessment
}

func hasShellLineContinuation(command string) bool {
	// POSIX shells remove backslash-newline before tokenization. The bounded
	// scanner must never authorize a different token stream than sh executes.
	return strings.Contains(command, "\\\n") || strings.Contains(command, "\\\r")
}

func hasDynamicShellSyntax(command string) bool {
	var quote rune
	escaped := false
	for _, character := range command {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '$', '`':
				return true
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '$', '`', '(', ')', '{', '}':
			return true
		}
	}
	return false
}

func hasUnquotedShellGlob(command string) bool {
	var quote rune
	escaped := false
	for _, character := range command {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// splitStaticShellCommands accepts quoted words and foreground &&, ||, ; and
// pipe composition. Expansion, grouping, backgrounding, and file redirection
// are rejected before this point or by the scanner. This keeps command-name
// checks meaningful even when several routine development commands are joined.
func splitStaticShellCommands(command string) ([][]string, []string, bool) {
	trimmed := strings.TrimSpace(command)
	if strings.HasSuffix(trimmed, "&&") || strings.HasSuffix(trimmed, "||") ||
		strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, "&") {
		return nil, nil, false
	}
	var (
		commands   [][]string
		separators []string
		words      []string
		word       strings.Builder
		quote      rune
		escaped    bool
		wordOpen   bool
	)
	flushWord := func() {
		if wordOpen {
			words = append(words, word.String())
			word.Reset()
			wordOpen = false
		}
	}
	flushCommand := func() bool {
		flushWord()
		if len(words) == 0 {
			return false
		}
		commands = append(commands, words)
		words = nil
		return true
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			word.WriteRune(r)
			wordOpen = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			// Within double quotes, POSIX sh only consumes backslash before
			// $, `, ", and backslash (line continuations were rejected above).
			// Preserve it before every other rune so validation sees the same
			// filesystem token the shell will use.
			if quote == '"' && i+1 < len(runes) && !strings.ContainsRune("$`\"\\", runes[i+1]) {
				word.WriteRune(r)
				wordOpen = true
				continue
			}
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if !wordOpen {
			if redirectLength := staticDescriptorRedirectLength(runes, i); redirectLength > 0 {
				i += redirectLength - 1
				continue
			}
		}
		switch r {
		case '\'', '"':
			quote = r
			wordOpen = true
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';', '|', '&':
			if r == '&' && (i+1 >= len(runes) || runes[i+1] != '&') {
				return nil, nil, false
			}
			separator := string(r)
			if r == '|' && i+1 < len(runes) && runes[i+1] == '|' {
				i++
				separator = "||"
			} else if r == '&' {
				i++
				separator = "&&"
			} else if r == '\n' {
				separator = ";"
			}
			if !flushCommand() {
				return nil, nil, false
			}
			separators = append(separators, separator)
		case '<', '>':
			return nil, nil, false
		default:
			if unicode.IsControl(r) {
				return nil, nil, false
			}
			word.WriteRune(r)
			wordOpen = true
		}
	}
	if escaped || quote != 0 {
		return nil, nil, false
	}
	flushWord()
	if len(words) > 0 {
		commands = append(commands, words)
	}
	if len(separators) != len(commands)-1 {
		return nil, nil, false
	}
	return commands, separators, len(commands) > 0
}

func staticDescriptorRedirectLength(command []rune, offset int) int {
	for _, redirect := range []string{"2>&1", "1>&2"} {
		candidate := []rune(redirect)
		if offset+len(candidate) > len(command) {
			continue
		}
		matched := true
		for index := range candidate {
			if command[offset+index] != candidate[index] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		next := offset + len(candidate)
		if next < len(command) && !unicode.IsSpace(command[next]) && !strings.ContainsRune(";|&\n", command[next]) {
			continue
		}
		return len(candidate)
	}
	return 0
}

func staticCommandExecutable(words []string) string {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 || filepath.Base(words[0]) != words[0] {
		return ""
	}
	return words[0]
}

func staticCommandsContainExecutable(commands [][]string, executable string) bool {
	for _, words := range commands {
		if staticCommandExecutable(words) == executable {
			return true
		}
	}
	return false
}

func (a *Agent) autoScopedCDTarget(words []string, baseDir string) (string, bool) {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 || words[0] != "cd" {
		return "", false
	}
	args := words[1:]
	if len(args) == 2 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 || rawPathHasParentTraversal(args[0]) {
		return "", false
	}
	target := args[0]
	if target == "-" {
		return "", false
	}
	if !filepath.IsAbs(target) && baseDir != "" {
		target = filepath.Join(baseDir, target)
	}
	// Shell directory changes stay confined to the primary workspace. A
	// temporary write grant is authority for typed host operations, not a way to
	// turn an external directory into an ambient shell working directory.
	resolved, err := a.resolveWorkspacePath(target)
	return resolved, err == nil
}

func (a *Agent) assessAutoScopedSimpleCommand(words []string, baseDir string) autoSimpleCommandAssessment {
	assessment := autoSimpleCommandAssessment{reason: autoCommandReasonExecutable}
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return assessment
	}
	rawExecutable := words[0]
	executable := filepath.Base(words[0])
	if executable == "." || executable == "source" || executable == "eval" || executable == "exec" || executable == "env" {
		return assessment
	}
	// A workspace Minerva binary is the one deliberately narrow exception
	// to the host-path catalog. It is admitted only after physical workspace,
	// Go build-identity, install-location, and exact query-argv validation. This
	// lets AUTO verify the local CLI it just built while Minerva's ordinary
	// product integration continues through its exact trusted MCPHub route.
	if executable != rawExecutable {
		if a.autoScopedMinervaWorkspaceCommandAllowed(rawExecutable, words[1:], baseDir) {
			assessment.allowed = true
			assessment.effect = autoCommandEffectWorkspaceExecution
			assessment.reason = autoCommandReasonAllowed
			assessment.workspaceExecutable = true
		}
		return assessment
	}
	// Every other AUTO shell executable must resolve through the host-owned
	// catalog. A generic path-qualified workspace binary can hide mutation or
	// networking behind an innocent-looking argument and remains approval-gated.
	if !a.autoCommandExecutableAllowed(executable) {
		return assessment
	}
	assessment.effect = autoCommandEffectForExecutable(executable, words[1:])
	args := words[1:]
	// Temporary external scopes are typed host capabilities. They intentionally
	// never become ambient raw-shell authority, even for otherwise read-only
	// commands such as cat or sed.
	allowReadGrants := false
	for index, word := range args {
		if autoCommandNonPathArgument(executable, args, index) {
			continue
		}
		authority := a.autoCommandPathAssessment(word, baseDir, allowReadGrants)
		if authority == autoPathDenied {
			assessment.reason = autoCommandReasonPathAuthority
			return assessment
		}
		assessment.usesReadGrant = assessment.usesReadGrant || authority == autoPathReadGrant
	}

	attachedAuthority, attachedOK := a.autoScopedAttachedPathOptionsAssessment(executable, args, baseDir, allowReadGrants)
	if !attachedOK {
		assessment.reason = autoCommandReasonPathAuthority
		return assessment
	}
	assessment.usesReadGrant = assessment.usesReadGrant || attachedAuthority == autoPathReadGrant
	allowed := false
	switch executable {
	case "cd":
		if len(args) == 2 && args[0] == "--" {
			args = args[1:]
		}
		allowed = len(args) == 1 && a.autoCommandCandidatePathAssessment(args[0], baseDir, false) != autoPathDenied
	case "go":
		allowed = autoScopedGoCommandAllowed(args)
	case "git":
		allowed = autoScopedGitCommandAllowed(args)
	case "npm":
		allowed = autoScopedPackageCommandAllowed(args, "test")
	case "pnpm", "yarn":
		allowed = autoScopedPackageCommandAllowed(args, "test", "build", "lint", "check", "typecheck")
	case "bun":
		allowed = autoScopedPackageCommandAllowed(args, "test", "lint")
	case "cargo":
		allowed = autoScopedCargoCommandAllowed(args)
	case "swift":
		allowed = autoScopedSwiftCommandAllowed(args)
	case "sed":
		allowed = autoScopedSedCommandAllowed(args)
	case "find", "rg", "grep", "tree", "du", "ls":
		// Recursive shell inspection can discover or read descendants that the
		// host-owned ignore policy excludes (for example `rg --no-ignore .`,
		// `tree -a .`, or `ls -Ra .`). Built-in list/grep/read operations enforce
		// that policy, so raw search and directory-enumeration processes remain
		// approval-gated in AUTO even for workspace operands.
		allowed = false
	case "sort":
		allowed = !containsLongOptionPrefix(args, "--compress-program", "--files0-from")
	case "printf":
		allowed = autoScopedPrintfCommandAllowed(args)
	case "file":
		allowed = !containsArg(args, "-C", "-S", "-f", "-z", "-Z", "-m", "-M") &&
			!containsLongOptionPrefix(args, "--compile", "--files-from", "--magic-file", "--no-sandbox", "--uncompress", "--uncompress-noreport") &&
			!containsClusteredShortOption(args, "CSfzZmM", "P")
	case "date":
		allowed = len(args) == 0
	case "wc":
		allowed = !containsLongOptionPrefix(args, "--files0-from")
	case "diff":
		// Directory comparison follows nested symlinks by default on BSD/GNU,
		// and pagination can launch `pr`. The built-in diff and /changes paths
		// provide host-confined inspection, so raw diff stays approval-gated.
		allowed = false
	case "mkdir", "touch":
		allowed = len(args) > 0 && !argumentContainsPath(args, "/dev/null")
	case "eslint":
		allowed = !containsArg(args, "--inspect-config", "--init", "--mcp")
	case "prettier":
		allowed = !containsArg(args, "--plugin")
	case "tail":
		allowed = !containsArg(args, "-f", "-F") &&
			!containsLongOptionPrefix(args, "--follow", "--retry") &&
			!containsClusteredShortOption(args, "fF", "")
	case "tsc":
		allowed = autoScopedTSCCommandAllowed(args)
	case "golangci-lint":
		allowed = autoScopedGolangCILintCommandAllowed(args)
	case "gofmt", "staticcheck",
		"pwd", "cat", "head", "uniq", "cut", "tr", "stat", "which", "basename", "dirname", "realpath", "echo", "true", "false", "test", "cmp":
		allowed = true
	default:
		allowed = false
	}
	if !allowed {
		assessment.reason = autoCommandReasonArguments
		return assessment
	}
	assessment.allowed = true
	assessment.reason = autoCommandReasonAllowed
	return assessment
}

func autoCommandNonPathArgument(executable string, args []string, index int) bool {
	if index < 0 || index >= len(args) {
		return false
	}
	if executable == "go" && len(args) > 0 && args[0] == "test" {
		argument := args[index]
		if strings.HasPrefix(argument, "-run=") || strings.HasPrefix(argument, "--run=") {
			return true
		}
		return index > 0 && stringIn(args[index-1], "-run", "--run")
	}
	if executable == "rg" || executable == "grep" {
		return autoSearchPatternArgument(args, index, executable == "rg")
	}
	return false
}

// autoSearchPatternArgument distinguishes regex text from filesystem operands
// for rg/grep. A leading slash in a pattern is data, not an external read; file
// operands and path-taking options still pass through workspace resolution.
func autoSearchPatternArgument(args []string, target int, ripgrep bool) bool {
	// GNU/BSD option parsing permits pattern options after positional operands.
	// If -e/-f (or rg --files mode) exists, every bare positional is a file path;
	// only an explicit -e value is inline pattern data.
	patternSeen := searchHasNoPositionalPattern(args, ripgrep)
	endOptions := false
	nextValue := byte(0) // 'p' pattern, 'o' other option value
	for index, argument := range args {
		if nextValue != 0 {
			if nextValue == 'p' {
				if index == target {
					return true
				}
				patternSeen = true
			}
			nextValue = 0
			continue
		}
		if !endOptions && argument == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(argument, "--") {
			name, _, attached := strings.Cut(argument, "=")
			if name == "--regexp" {
				if attached {
					if index == target {
						return true
					}
					patternSeen = true
				} else {
					nextValue = 'p'
				}
				continue
			}
			if !attached && searchLongOptionTakesValue(name, ripgrep) {
				nextValue = 'o'
			}
			continue
		}
		if !endOptions && len(argument) > 1 && argument[0] == '-' {
			cluster := argument[1:]
			for offset, option := range cluster {
				if option == 'e' {
					if offset+1 < len(cluster) {
						if index == target {
							return true
						}
						patternSeen = true
					} else {
						nextValue = 'p'
					}
					break
				}
				if searchShortOptionTakesValue(option, ripgrep) {
					if offset+1 == len(cluster) {
						nextValue = 'o'
					}
					break
				}
			}
			continue
		}
		if !patternSeen {
			if index == target {
				return true
			}
			patternSeen = true
		}
	}
	return false
}

func searchHasNoPositionalPattern(args []string, ripgrep bool) bool {
	endOptions := false
	skipNext := false
	for _, argument := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if !endOptions && argument == "--" {
			endOptions = true
			continue
		}
		if endOptions {
			continue
		}
		if strings.HasPrefix(argument, "--") {
			name, _, attached := strings.Cut(argument, "=")
			if name == "--regexp" || name == "--file" || ripgrep && name == "--files" {
				return true
			}
			if !attached && searchLongOptionTakesValue(name, ripgrep) {
				skipNext = true
			}
			continue
		}
		if len(argument) <= 1 || argument[0] != '-' {
			continue
		}
		cluster := argument[1:]
		for offset, option := range cluster {
			if option == 'e' || option == 'f' {
				return true
			}
			if searchShortOptionTakesValue(option, ripgrep) {
				if offset+1 == len(cluster) {
					skipNext = true
				}
				break
			}
		}
	}
	return false
}

func searchShortOptionTakesValue(option rune, ripgrep bool) bool {
	if ripgrep {
		return strings.ContainsRune("ABCEMTdfgjmrt", option)
	}
	return strings.ContainsRune("ABCDdfm", option)
}

func searchLongOptionTakesValue(name string, ripgrep bool) bool {
	common := []string{"--after-context", "--before-context", "--context", "--regexp", "--file", "--max-count"}
	if stringIn(name, common...) {
		return true
	}
	if ripgrep {
		return stringIn(name,
			"--encoding", "--max-columns", "--type-not", "--max-depth", "--glob", "--threads",
			"--replace", "--type", "--ignore-file", "--sort", "--sortr", "--path-separator",
			"--field-context-separator", "--field-match-separator", "--engine", "--colors",
			"--hyperlink-format", "--max-filesize",
		)
	}
	return stringIn(name,
		"--devices", "--directories", "--exclude", "--exclude-from", "--include", "--label", "--binary-files",
	)
}

func (a *Agent) autoCommandExecutableAllowed(executable string) bool {
	if stringIn(executable, "cd", "echo", "false", "printf", "pwd", "test", "true") {
		return true
	}
	resolved, err := exec.LookPath(executable)
	if err != nil || !filepath.IsAbs(resolved) {
		return false
	}
	// Reject both a lexical workspace executable and an external symlink whose
	// physical target is inside the workspace. The shell may still prefer a
	// builtin for names above, but every catalogued external command must resolve
	// through a host path the agent cannot create with confined writes.
	if root := strings.TrimSpace(a.activeWorkDir()); root != "" {
		absoluteRoot, rootErr := filepath.Abs(root)
		absoluteResolved, resolvedErr := filepath.Abs(resolved)
		if rootErr != nil || resolvedErr != nil {
			return false
		}
		if _, inside, relativeErr := workspaceRelative(absoluteRoot, absoluteResolved); relativeErr != nil || inside {
			return false
		}
	}
	return !a.pathWithinWorkspace(resolved)
}

func autoCommandEffectForExecutable(executable string, args []string) autoCommandEffect {
	switch executable {
	case "mkdir", "touch":
		return autoCommandEffectWorkspaceMutation
	case "sort":
		if autoSortWritesOutput(args) {
			return autoCommandEffectWorkspaceMutation
		}
		return autoCommandEffectReadOnly
	case "gofmt":
		if containsArg(args, "-w") {
			return autoCommandEffectWorkspaceMutation
		}
		return autoCommandEffectReadOnly
	case "go", "npm", "pnpm", "yarn", "bun", "cargo", "swift", "eslint", "prettier", "tsc", "golangci-lint", "staticcheck":
		return autoCommandEffectWorkspaceExecution
	default:
		return autoCommandEffectReadOnly
	}
}

func autoSortWritesOutput(args []string) bool {
	for _, argument := range args {
		if argument == "-o" || strings.HasPrefix(argument, "--out") {
			return true
		}
		if len(argument) > 2 && argument[0] == '-' && argument[1] != '-' && strings.ContainsRune(argument[1:], 'o') {
			return true
		}
	}
	return false
}

func autoScopedPrintfCommandAllowed(args []string) bool {
	for _, argument := range args {
		// printf is commonly a shell builtin. In Bash and Zsh, -v assigns a shell
		// variable; changing PATH can make the following catalogued command resolve
		// to attacker-controlled workspace code.
		if argument == "-v" || strings.HasPrefix(argument, "-v") {
			return false
		}
	}
	return true
}

func (a *Agent) autoScopedAttachedPathOptionsAssessment(executable string, args []string, baseDir string, allowReadGrants bool) (autoPathAuthority, bool) {
	var options []string
	switch executable {
	case "find", "make", "just", "rg", "grep":
		options = []string{"-f"}
	case "file":
		options = []string{"-f", "-m"}
	case "touch":
		options = []string{"-r"}
	case "task":
		options = []string{"-t", "-d"}
	case "sort":
		options = []string{"-o", "-T"}
	case "tree":
		options = []string{"-o"}
	case "golangci-lint":
		options = []string{"-c"}
	case "eslint":
		options = []string{"-c", "-o"}
	case "tsc":
		options = []string{"-p"}
	}
	if executable == "make" {
		options = append(options, "-C", "-I")
	}
	if executable == "just" {
		options = append(options, "-d")
	}
	for _, argument := range args {
		value, attached := clusteredShortOptionValue(argument, options)
		if !attached {
			continue
		}
		authority := a.autoCommandPathAssessment(value, baseDir, allowReadGrants)
		if authority == autoPathDenied {
			return autoPathDenied, false
		}
		if authority == autoPathReadGrant {
			return autoPathReadGrant, true
		}
	}
	return autoPathWorkspace, true
}

func clusteredShortOptionValue(argument string, options []string) (string, bool) {
	if len(argument) < 3 || argument[0] != '-' || argument[1] == '-' {
		return "", false
	}
	cluster := argument[1:]
	for index := range cluster {
		option := "-" + string(cluster[index])
		if !stringIn(option, options...) {
			continue
		}
		value := strings.TrimPrefix(cluster[index+1:], "=")
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}

func autoScopedPackageCommandAllowed(args []string, direct ...string) bool {
	return len(args) == 1 && firstArgIn(args, direct...)
}

func autoScopedGoCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test", "vet", "list", "env", "version", "doc", "fmt") &&
		(len(args) < 2 || args[0] != "mod" || !stringIn(args[1], "tidy", "verify", "why", "graph")) {
		return false
	}
	// These options either mutate user-global Go configuration or delegate to
	// another executable. They need an explicit decision even when the outer Go
	// command itself is routine.
	return !containsArgumentNamePrefix(args, "-test.fuzz", "--test.fuzz") && !containsArg(args,
		"-w", "-u", "-exec", "--exec", "-toolexec", "--toolexec", "-vettool", "--vettool",
		"-ldflags", "--ldflags", "-gccgoflags", "--gccgoflags", "-gcflags", "--gcflags", "-asmflags", "--asmflags",
		"-fuzz", "--fuzz",
	)
}

// autoScopedGitCommandAllowed admits the git subcommands that inspect history
// or working-tree state, and only in argument forms that cannot write a file or
// hand control to another program.
//
// Git was previously excluded outright. That made orienting — the single most
// common thing an agent does before touching anything — cost an interactive
// approval, and because dispatch preserves model order, one `git status` stalled
// every read-only tool queued behind it in the same batch.
//
// Subcommands are allowlisted, never denylisted, because too many git verbs read
// in one argument form and destroy in another: `branch -D` deletes, a bare
// `tag <name>` creates, `config k v` writes, a bare `stash` pushes the worktree.
// None of those appear here, and adding one requires proving it cannot mutate
// under ANY accepted argument.
//
// A global option before the subcommand (-c, -C, --git-dir, --work-tree,
// --exec-path, --config-env) is refused implicitly: args[0] must itself be a
// catalogued subcommand, and such an option would occupy that slot. This matters
// more than it looks — `git -c diff.external=... diff` turns a read into an
// arbitrary execution. Aliases need no handling: git does not permit an alias to
// shadow a built-in subcommand.
//
// Caveat worth stating: a repository or user gitconfig that already sets
// diff.external or core.pager will run that program under `git diff` with no
// flag at all. That is the operator's own configured tooling, not something a
// model can introduce here, and it is equally true of everything else on PATH.
func autoScopedGitCommandAllowed(args []string) bool {
	if !firstArgIn(args,
		"status", "log", "show", "diff", "rev-parse", "ls-files", "blame", "describe", "shortlog",
	) {
		return false
	}
	// --output makes diff/show write a file; --ext-diff, --extcmd, and
	// --textconv each hand the content to an external program named by config;
	// --no-index escapes the repository into arbitrary filesystem paths.
	// containsLongOptionPrefix also refuses unique-prefix abbreviations such as
	// --out for --output.
	if containsLongOptionPrefix(args, "--output", "--ext-diff", "--extcmd", "--textconv", "--no-index") {
		return false
	}
	// Short forms the long-option guard cannot see.
	return !containsArg(args, "-o", "-O")
}

func autoScopedCargoCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test", "check", "fmt", "clippy", "metadata", "doc", "bench") {
		return false
	}
	// Cargo configuration can select runner/compiler wrapper executables; doc
	// --open launches an external application. Both exceed routine build/test
	// authority even when the primary Cargo subcommand is catalogued.
	return !containsArg(args, "--config", "--open")
}

func autoScopedSwiftCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test") || containsResponseFileOperand(args) {
		return false
	}
	return !containsArg(args, "--disable-sandbox", "-Xcc", "-Xswiftc", "-Xlinker", "-Xcxx")
}

func autoScopedTSCCommandAllowed(args []string) bool {
	return !containsResponseFileOperand(args) &&
		!containsArg(args, "-w", "--watch", "--clean", "--typeRoots", "--rootDirs") &&
		!containsClusteredShortOption(args, "w", "")
}

func autoScopedGolangCILintCommandAllowed(args []string) bool {
	if firstArgIn(args, "run", "fmt", "linters", "version") {
		return true
	}
	return len(args) == 2 && args[0] == "config" && args[1] == "verify"
}

func autoScopedSedCommandAllowed(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for len(args) > 0 && stringIn(args[0], "-n", "--quiet", "--silent", "-E", "-r") {
		args = args[1:]
	}
	if len(args) < 2 {
		return false
	}
	// Admit the common read-only inspection form (`sed -n 1,120p file`).
	// General sed programs can use `w` to create files and some variants can
	// execute commands, so they remain approval-gated. GNU getopt may permute a
	// later `-i` or `-e` ahead of the program; reject every post-program option
	// unless an explicit `--` makes the remaining words unambiguous filenames.
	if !autoSedPrintProgram.MatchString(args[0]) {
		return false
	}
	files := args[1:]
	if len(files) > 0 && files[0] == "--" {
		files = files[1:]
		return len(files) > 0
	}
	if len(files) == 0 {
		return false
	}
	for _, file := range files {
		if strings.HasPrefix(file, "-") {
			return false
		}
	}
	return true
}

func autoCommandAssignmentAllowed(assignment string) bool {
	if !autoCommandAssignment.MatchString(assignment) {
		return false
	}
	name, value, _ := strings.Cut(assignment, "=")
	return stringIn(name, "CI", "NO_COLOR", "FORCE_COLOR") && autoCommandAssignmentValue.MatchString(value)
}

func (a *Agent) autoCommandPathAssessment(word, baseDir string, allowReadGrants bool) autoPathAuthority {
	result := a.autoCommandCandidatePathAssessment(word, baseDir, allowReadGrants)
	if result == autoPathDenied {
		return autoPathDenied
	}
	if _, value, found := strings.Cut(word, "="); found {
		valueResult := a.autoCommandCandidatePathAssessment(value, baseDir, allowReadGrants)
		if valueResult == autoPathDenied {
			return autoPathDenied
		}
		if valueResult == autoPathReadGrant {
			result = autoPathReadGrant
		}
	}
	return result
}

func (a *Agent) autoCommandCandidatePathAssessment(candidate, baseDir string, allowReadGrants bool) autoPathAuthority {
	if candidate == "" || candidate == "/dev/null" {
		return autoPathWorkspace
	}
	if strings.HasPrefix(candidate, "~") || rawPathHasParentTraversal(candidate) {
		return autoPathDenied
	}
	if !filepath.IsAbs(candidate) && baseDir != "" {
		candidate = filepath.Join(baseDir, candidate)
	}
	// Resolve even relative operands so a workspace symlink cannot turn an
	// apparently confined shell command into an external read or write.
	if a.pathWithinWorkspace(candidate) {
		return autoPathWorkspace
	}
	if !allowReadGrants {
		return autoPathDenied
	}
	readable, err := a.resolveReadablePath(candidate)
	if err != nil {
		return autoPathDenied
	}
	defer func() { _ = readable.close() }()
	// Exact-file grants are intentionally capable of naming sensitive files for
	// host-owned read tools after explicit consent. Raw shell execution is a
	// weaker boundary, so conventional secret paths stay approval-gated even
	// when an exact read grant exists.
	if config.HostSecretPathIgnored(readable.absolute) {
		return autoPathDenied
	}
	return autoPathReadGrant
}

func rawPathHasParentTraversal(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(character rune) bool {
		return character == '/' || character == '\\'
	}) {
		if component == ".." {
			return true
		}
	}
	return false
}

func (a *Agent) pathWithinWorkspace(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	// Keep shell authority narrower than typed write grants. Explicit external
	// write scopes are consumed by built-in write/edit/mkdir and trusted routed
	// workspace tools; they never make a raw shell operand "workspace-local".
	_, err := a.resolveWorkspacePath(path)
	return err == nil
}

func firstArgIn(args []string, allowed ...string) bool {
	return len(args) > 0 && stringIn(args[0], allowed...)
}

func stringIn(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func containsArg(args []string, denied ...string) bool {
	for _, arg := range args {
		for _, candidate := range denied {
			if arg == candidate || strings.HasPrefix(arg, candidate+"=") {
				return true
			}
		}
	}
	return false
}

// containsLongOptionPrefix fails closed for GNU-style unique long-option
// abbreviations. Several catalogued tools accept prefixes such as
// --files0-fro for --files0-from, so exact string matching is insufficient.
func containsLongOptionPrefix(args []string, denied ...string) bool {
	for _, argument := range args {
		name, _, _ := strings.Cut(argument, "=")
		if !strings.HasPrefix(name, "--") || name == "--" {
			continue
		}
		for _, full := range denied {
			if strings.HasPrefix(full, name) {
				return true
			}
		}
	}
	return false
}

func containsArgumentNamePrefix(args []string, denied ...string) bool {
	for _, argument := range args {
		name, _, _ := strings.Cut(argument, "=")
		for _, prefix := range denied {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	return false
}

func containsResponseFileOperand(args []string) bool {
	for _, argument := range args {
		if strings.HasPrefix(argument, "@") {
			return true
		}
	}
	return false
}

func argumentContainsPath(args []string, path string) bool {
	for _, argument := range args {
		if strings.Contains(argument, path) {
			return true
		}
	}
	return false
}

// containsClusteredShortOption finds denied flags inside POSIX short-option
// clusters (for example, `-bz` and `-Pz`). Once an option that consumes the
// rest of its token is encountered, later bytes are its value rather than
// flags and are deliberately ignored.
func containsClusteredShortOption(args []string, denied, valueTaking string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if len(argument) < 3 || argument[0] != '-' || argument[1] == '-' {
			continue
		}
		for _, option := range argument[1:] {
			if strings.ContainsRune(denied, option) {
				return true
			}
			if strings.ContainsRune(valueTaking, option) {
				break
			}
		}
	}
	return false
}
