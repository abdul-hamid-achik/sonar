package agent

import (
	"debug/buildinfo"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	minervaCommandPath = "github.com/abdul-hamid-achik/minerva/cmd/minerva"
	minervaModulePath  = "github.com/abdul-hamid-achik/minerva"
)

var minervaQueryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var minervaQuerySlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var minervaFilterLiteral = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)
var minervaHeadCount = regexp.MustCompile(`^[0-9]{1,3}$`)

// autoScopedMinervaWorkspaceCommandAllowed is a host-owned contract for
// verifying a locally built Minerva CLI inside the active workspace. This is
// intentionally not a generic ./bin/* escape hatch:
//   - the physical executable must be <workspace>/bin/minerva;
//   - it must be a regular, executable, non-set-ID Go binary with Minerva's
//     exact main-package and module identities; and
//   - argv must match one of the bounded query-only forms below.
//
// The process is still workspace execution, not a pure read: like `go test`,
// repository-owned code can have arbitrary behavior. AUTO already declares
// that trust boundary. Mutation-oriented Minerva commands and broad output
// surfaces remain behind approval or an exact trusted MCP/MCPHub contract.
func (a *Agent) autoScopedMinervaWorkspaceCommandAllowed(rawExecutable string, args []string, baseDir string) bool {
	resolved, ok := a.autoScopedMinervaWorkspacePath(rawExecutable, args, baseDir)
	if !ok {
		return false
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	identity, err := buildinfo.ReadFile(resolved)
	if err != nil || identity.Path != minervaCommandPath || identity.Main.Path != minervaModulePath {
		return false
	}
	for _, setting := range identity.Settings {
		if setting.Key == "-buildmode" {
			return setting.Value == "exe"
		}
	}
	return false
}

func (a *Agent) autoScopedPlannedMinervaWorkspaceCommandAllowed(words []string, baseDir string) bool {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	_, ok := a.autoScopedMinervaWorkspacePath(words[0], words[1:], baseDir)
	return ok
}

func (a *Agent) autoScopedMinervaWorkspacePath(rawExecutable string, args []string, baseDir string) (string, bool) {
	if filepath.Base(rawExecutable) != "minerva" || rawPathHasParentTraversal(rawExecutable) ||
		!autoScopedMinervaQueryArgsAllowed(args) {
		return "", false
	}
	root, err := a.resolveWorkspacePath(".")
	if err != nil {
		return "", false
	}
	candidate := rawExecutable
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(baseDir, candidate)
	}
	resolved, err := a.resolveWorkspacePath(candidate)
	if err != nil || filepath.Clean(resolved) != filepath.Join(filepath.Clean(root), "bin", "minerva") {
		return "", false
	}
	return resolved, true
}

func (a *Agent) autoScopedMinervaBuildCommandAllowed(words []string, baseDir string) bool {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) < 4 || words[0] != "go" || words[1] != "build" {
		return false
	}
	var output, target string
	for index := 2; index < len(words); index++ {
		argument := words[index]
		switch {
		case argument == "-o" && index+1 < len(words):
			if output != "" {
				return false
			}
			index++
			output = words[index]
		case strings.HasPrefix(argument, "-o="):
			if output != "" {
				return false
			}
			output = strings.TrimPrefix(argument, "-o=")
		case argument == "./cmd/minerva" || argument == "cmd/minerva":
			if target != "" {
				return false
			}
			target = argument
		default:
			return false
		}
	}
	if target == "" {
		return false
	}
	if _, ok := a.autoScopedMinervaWorkspacePath(output, []string{"--version"}, baseDir); !ok {
		return false
	}
	root, err := a.resolveWorkspacePath(".")
	if err != nil {
		return false
	}
	moduleFile, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	defer func() { _ = moduleFile.Close() }()
	module, err := io.ReadAll(io.LimitReader(moduleFile, (1<<20)+1))
	if err != nil || len(module) > 1<<20 {
		return false
	}
	for _, line := range strings.Split(string(module), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1] == minervaModulePath
		}
	}
	return false
}

func autoScopedMinervaQueryArgsAllowed(args []string) bool {
	if len(args) > 4 {
		return false
	}
	total := 0
	for _, argument := range args {
		total += len(argument)
		if strings.TrimSpace(argument) != argument || strings.ContainsAny(argument, "\x00\r\n") ||
			strings.Contains(argument, "://") {
			return false
		}
	}
	if total > 256 {
		return false
	}
	if len(args) == 0 {
		return false
	}
	if len(args) == 1 && stringIn(args[0], "--version", "--help", "-h", "help") {
		return true
	}
	if len(args) == 1 && stringIn(args[0], "analytics", "suggest") {
		return true
	}
	if len(args) < 2 {
		return false
	}
	optionalJSON := func(rest []string) bool {
		return len(rest) == 0 || len(rest) == 1 && rest[0] == "--json"
	}
	switch args[0] {
	case "skill", "profile":
		return args[1] == "list" && optionalJSON(args[2:])
	case "stack":
		return args[1] == "check" && optionalJSON(args[2:])
	case "template":
		return args[1] == "list" && optionalJSON(args[2:]) ||
			args[1] == "show" && len(args) == 3 && minervaQuerySlug.MatchString(args[2])
	case "evidence":
		return args[1] == "docs" && len(args) == 2
	case "analytics", "suggest":
		return optionalJSON(args[1:])
	case "help":
		if len(args) > 3 {
			return false
		}
		for _, argument := range args[1:] {
			if !minervaQueryName.MatchString(argument) || !stringIn(argument,
				"skill", "profile", "stack", "template", "evidence", "analytics", "suggest",
				"list", "check", "docs",
			) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (a *Agent) autoScopedMinervaOutputFilterAllowed(words []string) bool {
	if len(words) == 0 || filepath.Base(words[0]) != words[0] || !a.autoCommandExecutableAllowed(words[0]) {
		return false
	}
	switch words[0] {
	case "grep":
		return len(words) == 2 && minervaFilterLiteral.MatchString(words[1])
	case "head":
		lines := 0
		var err error
		switch {
		case len(words) == 2 && strings.HasPrefix(words[1], "-") && minervaHeadCount.MatchString(strings.TrimPrefix(words[1], "-")):
			lines, err = strconv.Atoi(strings.TrimPrefix(words[1], "-"))
		case len(words) == 3 && words[1] == "-n" && minervaHeadCount.MatchString(words[2]):
			lines, err = strconv.Atoi(words[2])
		case len(words) == 2 && strings.HasPrefix(words[1], "--lines=") && minervaHeadCount.MatchString(strings.TrimPrefix(words[1], "--lines=")):
			lines, err = strconv.Atoi(strings.TrimPrefix(words[1], "--lines="))
		default:
			return false
		}
		return err == nil && lines >= 1 && lines <= 200
	default:
		return false
	}
}
