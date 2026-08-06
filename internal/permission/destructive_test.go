package permission

import "testing"

// TestDestructiveCommandWarningNamesTheDamage covers the gap the approval modal
// had: Reason explains which rule was tripped, which for a destructive command
// is frequently true and beside the point.
func TestDestructiveCommandWarningNamesTheDamage(t *testing.T) {
	warns := map[string]string{
		"rm -rf build":                  "directory tree",
		"rm -fr build":                  "directory tree",
		"rm notes.txt":                  "deletes files",
		"git reset --hard":              "uncommitted",
		"git reset --hard HEAD~3":       "uncommitted",
		"git clean -fd":                 "untracked",
		"git push --force origin main":  "rewrites published history",
		"git push -f":                   "rewrites published history",
		"git push --delete origin old":  "deletes a remote branch",
		"git branch -D feature":         "deletes a branch",
		"git checkout -- .":             "overwrites working-tree files",
		"git stash drop":                "discards stashed changes",
		"git filter-branch --all":       "rewrites every commit",
		"chmod -R 777 .":                "whole tree",
		"npm publish":                   "cannot be recalled",
		"kubectl delete pod api":        "deletes cluster resources",
		"terraform destroy":             "real infrastructure",
		"docker system prune -af":       "containers, images or volumes",
		"dd if=/dev/zero of=/dev/disk0": "raw blocks",
		// A destructive segment anywhere in a compound is still destructive.
		"cd /repo && rm -rf dist":      "directory tree",
		"echo cleaning; git clean -fd": "untracked",
	}
	for command, want := range warns {
		t.Run("warns/"+command, func(t *testing.T) {
			got := DestructiveCommandWarning(command)
			if got == "" {
				t.Fatalf("no warning for a destructive command")
			}
			if !contains(got, want) {
				t.Fatalf("warning %q does not mention %q", got, want)
			}
		})
	}

	quiet := []string{
		"", "  ",
		"go test ./...",
		"git status --short",
		"git log --oneline -3",
		// Switching branches is not the destructive form of checkout.
		"git checkout main",
		"git push origin main",
		"git branch --list",
		"chmod 644 main.go",
		"npm test",
		"kubectl get pods",
		// The table reads leading words of real segments, so a pattern quoted
		// inside an argument is a search rather than a deletion.
		`grep -n "rm -rf" notes.md`,
		`echo "git push --force"`,
	}
	for _, command := range quiet {
		t.Run("quiet/"+command, func(t *testing.T) {
			if got := DestructiveCommandWarning(command); got != "" {
				t.Fatalf("an ordinary command was labelled destructive: %q", got)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
