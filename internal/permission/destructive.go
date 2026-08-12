package permission

import "strings"

// DestructiveCommandWarning names, in one short clause, the durable damage a
// command can do if the reader approves it without thinking.
//
// It is deliberately a static table and deliberately independent of the
// host-authored Reason beside it. Reason answers "why are you being asked" —
// which policy rule the command tripped — and a command can trip a rule for a
// reason that says nothing about consequence. `git reset --hard` trips the
// catalog because git's mutating verbs are not catalogued, and the reader is
// told exactly that while the sentence they actually needed was "this discards
// uncommitted changes".
//
// It reads argv only. It never runs anything, never asks a model, and adds no
// latency to a prompt that is already blocking a human: an approval is the one
// screen in the harness where a round-trip is paid for by someone waiting.
//
// A miss here costs nothing — the command still requires approval, and the
// exact text is still on screen. That asymmetry is why the table stays short
// and certain rather than clever: every entry has to be true of every command
// that matches it.
func DestructiveCommandWarning(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	// Only the segments matter, and only their leading words. Scanning the raw
	// string would let a pattern match inside a quoted argument — `grep "rm
	// -rf" notes.md` is a search, not a deletion.
	for _, segment := range splitLeadingWords(command) {
		if warning := destructiveSegmentWarning(segment); warning != "" {
			return warning
		}
	}
	return ""
}

func destructiveSegmentWarning(words []string) string {
	if len(words) == 0 {
		return ""
	}
	head := words[0]
	rest := words[1:]
	switch head {
	case "rm":
		if hasAnyFlagLetter(rest, 'r', 'R') {
			return "deletes a directory tree; nothing here goes to a trash can"
		}
		return "deletes files; nothing here goes to a trash can"
	case "git":
		return destructiveGitWarning(rest)
	case "shred", "srm":
		return "overwrites file contents so they cannot be recovered"
	case "dd":
		return "writes raw blocks; a wrong target overwrites a disk"
	case "mkfs", "fdisk", "parted", "diskutil":
		return "operates on disks and partitions, not files"
	case "chmod", "chown":
		if hasAnyFlagLetter(rest, 'R') || containsExact(rest, "--recursive") {
			return "changes permissions across a whole tree"
		}
	case "kill", "killall", "pkill":
		return "terminates running processes, which may lose their unsaved work"
	case "docker", "podman":
		if containsExact(rest, "prune") || containsExact(rest, "rm") || containsExact(rest, "rmi") {
			return "removes containers, images or volumes that may hold state"
		}
	case "npm", "pnpm", "yarn", "bun":
		if containsExact(rest, "publish") {
			return "publishes a package version; a published version cannot be recalled"
		}
	case "terraform", "pulumi":
		if containsExact(rest, "destroy") || containsExact(rest, "apply") {
			return "changes real infrastructure, and may delete resources"
		}
	case "kubectl":
		if containsExact(rest, "delete") {
			return "deletes cluster resources"
		}
	}
	return ""
}

func destructiveGitWarning(rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	switch rest[0] {
	case "reset":
		if containsExact(rest, "--hard") {
			return "discards uncommitted changes in the working tree"
		}
	case "checkout", "restore":
		// A path operand is the destructive form: it overwrites the file with
		// the committed version. Switching branches is not.
		if containsExact(rest, ".") || containsExact(rest, "--") {
			return "overwrites working-tree files with their committed contents"
		}
	case "clean":
		return "deletes untracked files, including ones never committed anywhere"
	case "push":
		if containsExact(rest, "--force") || containsExact(rest, "-f") ||
			containsExact(rest, "--force-with-lease") {
			return "rewrites published history; collaborators' clones diverge"
		}
		if containsExact(rest, "--delete") {
			return "deletes a remote branch"
		}
	case "branch":
		if containsExact(rest, "-D") || containsExact(rest, "--delete") {
			return "deletes a branch, and -D deletes it even if unmerged"
		}
	case "filter-branch", "filter-repo":
		return "rewrites every commit in the repository"
	case "stash":
		if containsExact(rest, "drop") || containsExact(rest, "clear") {
			return "discards stashed changes, which are not recoverable from history"
		}
	}
	return ""
}

// splitLeadingWords returns the whitespace fields of each top-level segment.
//
// It reuses the quote-aware scan the prefix derivation relies on, so a control
// character inside quotes cannot invent a segment boundary that the shell will
// not honor — the difference between reading `echo "a && rm -rf /"` as one
// harmless echo and as two commands.
func splitLeadingWords(command string) [][]string {
	var segments [][]string
	runes := []rune(command)
	var quote rune
	escaped := false
	start := 0
	closeSegment := func(end int) {
		if fields := strings.Fields(string(runes[start:end])); len(fields) > 0 {
			segments = append(segments, fields)
		}
	}
	for index := 0; index < len(runes); index++ {
		character := runes[index]
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
		case ';', '\n', '|', '&':
			closeSegment(index)
			start = index + 1
		}
	}
	closeSegment(len(runes))
	return segments
}

func containsExact(args []string, want string) bool {
	for _, argument := range args {
		if argument == want {
			return true
		}
	}
	return false
}

// hasAnyFlagLetter reports a short flag carrying one of the letters, including
// inside a POSIX cluster: `rm -rf` and `rm -fr` and `rm -r` all count.
func hasAnyFlagLetter(args []string, letters ...rune) bool {
	for _, argument := range args {
		if len(argument) < 2 || argument[0] != '-' || argument[1] == '-' {
			continue
		}
		for _, letter := range letters {
			if strings.ContainsRune(argument[1:], letter) {
				return true
			}
		}
	}
	return false
}

// DestructiveBashPattern reports whether a bash approval pattern (literal or
// trailing " *") names a command whose durable damage the host already knows
// how to describe. Used to refuse workspace-persisted prefixes that would
// silently re-approve rm/git-reset/… families on later sessions.
func DestructiveBashPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	if strings.HasSuffix(pattern, " *") {
		pattern = strings.TrimSpace(strings.TrimSuffix(pattern, " *"))
	} else if strings.HasSuffix(pattern, "*") && !strings.HasSuffix(pattern, `\*`) {
		pattern = strings.TrimSpace(strings.TrimSuffix(pattern, "*"))
	}
	return DestructiveCommandWarning(pattern)
}
