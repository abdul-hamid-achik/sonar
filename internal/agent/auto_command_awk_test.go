package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// awk earned its catalog slot in session 4d01085: two approval prompts for
// column extraction, the one text job the built-in read tools cannot do.
// The admitted slice is print-only, following sed's precedent.
func TestAwkPrintProgramsAreAutoScoped(t *testing.T) {
	for _, args := range [][]string{
		{"{print $1}"},
		{"{print $1, $3}", "notes.txt"},
		{"-F", ",", "{print $2}", "data.csv"},
		{"-F,", "{print $2}", "data.csv"},
		{"-v", "n=3", "{print $n}", "data.csv"},
		{"NR==2 {print}", "notes.txt"},
		{"/error/ {print $0}", "app.log"},
		{"--", "{print NF}", "notes.txt"},
	} {
		if !autoScopedAwkCommandAllowed(args) {
			t.Errorf("awk %v should be auto-scoped", args)
		}
	}
}

func TestAwkEffectfulProgramsStayGated(t *testing.T) {
	for _, args := range [][]string{
		// Redirection and pipes are how awk writes files and runs commands.
		{"{print $1 > \"out.txt\"}"},
		{"{print $1 | \"sh\"}"},
		// Comparison operators share the redirect characters; parsing awk to
		// tell them apart is a guess this catalog does not make.
		{"$3 > 100 {print}"},
		{"{system(\"rm -rf /\")}"},
		{"{SYSTEM(\"x\")}"},
		{"{while ((\"cmd\" | getline line) > 0) print line}"},
		{"BEGIN {print ENVIRON[\"API_KEY\"]}"},
		// Program files and unknown options can reach dialect exec modes.
		{"-f", "prog.awk", "notes.txt"},
		{"--file", "prog.awk"},
		{"-i", "inplace", "{print}", "notes.txt"},
		// GNU getopt permutation bait after the program.
		{"{print}", "-i", "inplace"},
		// No program at all.
		{"-F", ","},
		{},
		{"-"},
	} {
		if autoScopedAwkCommandAllowed(args) {
			t.Errorf("awk %v must stay approval-gated", args)
		}
	}
}

// The program word is data, not a path: a pattern-action beginning with a
// slash used to read as an absolute path outside the workspace, the exact
// misclassification autoSedProgramArgument exists for.
func TestAwkProgramTextIsExemptFromPathAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	ag := New(nil, nil, 4096)
	dir := t.TempDir()
	ag.SetWorkDir(dir)
	if err := os.WriteFile(filepath.Join(dir, "app.log"), []byte("error one\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := `awk '/error/ {print $0}' app.log`
	if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
		t.Fatalf("read-only awk pattern-action costs an approval: %#v", assessment)
	}
}
