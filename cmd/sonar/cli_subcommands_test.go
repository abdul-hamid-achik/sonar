package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHandleRootHelpUsesPublicCLIContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := handleRootHelp(nil, "sonar", &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{"sonar <command>", "--auto", "--plan", "init", "execution"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("help omitted %q:\n%s", expected, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleRootHelp([]string{"goal"}, "sonar", &stdout, &stderr); code != 2 {
		t.Fatalf("help with topic exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unexpected argument "goal"`) {
		t.Fatalf("unexpected help error: %s", stderr.String())
	}
}

func TestInitHelpAndInvalidArgumentsHaveNoSideEffects(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	if code := handleInitIO([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("init --help exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sonar init [--force]") {
		t.Fatalf("unexpected init help: %s", stdout.String())
	}
	assertPathDoesNotExist(t, filepath.Join(workDir, "AGENTS.md"))

	for _, args := range [][]string{{"--unknown"}, {"destination"}, {"--force", "destination"}} {
		stdout.Reset()
		stderr.Reset()
		if code := handleInitIO(args, &stdout, &stderr); code != 2 {
			t.Fatalf("init %q exit = %d, want 2", args, code)
		}
		assertPathDoesNotExist(t, filepath.Join(workDir, "AGENTS.md"))
	}
}

func TestLogsHelpAndInvalidArgumentsHaveNoSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var stdout, stderr bytes.Buffer
	if code := handleLogsIO([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("logs --help exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sonar logs [-f]") {
		t.Fatalf("unexpected logs help: %s", stdout.String())
	}
	logDir := filepath.Join(home, ".config", "sonar", "logs")
	assertPathDoesNotExist(t, logDir)

	for _, args := range [][]string{{"--follow"}, {"session.log"}, {"-f", "session.log"}} {
		stdout.Reset()
		stderr.Reset()
		if code := handleLogsIO(args, &stdout, &stderr); code != 2 {
			t.Fatalf("logs %q exit = %d, want 2", args, code)
		}
		assertPathDoesNotExist(t, logDir)
	}
}

func TestGoalSubcommandHelpDoesNotOpenDurableStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	for _, test := range []struct {
		name    string
		command string
		args    []string
		want    []string
	}{
		{
			name: "list", command: "list", args: []string{"--help"},
			want: []string{"sonar goal list [options]", "--limit N", "--json"},
		},
		{
			name: "list after flag", command: "list", args: []string{"--json", "--help"},
			want: []string{"sonar goal list [options]", "--limit N", "--json"},
		},
		{
			name: "show", command: "show", args: []string{"--help"},
			want: []string{"sonar goal show [options] <session-id>", "--json"},
		},
		{
			name: "pending", command: "pending", args: []string{"--help"},
			want: []string{"sonar goal pending [options] <session-id>", "--limit N", "--json"},
		},
		{
			name: "open", command: "open", args: []string{"--help"},
			want: []string{"sonar goal open --objective TEXT [options]", "--criterion TEXT", "--max-eval-tokens N", "--max-wall-time DURATION"},
		},
		{
			name: "run after positional", command: "run", args: []string{"42", "--help"},
			want: []string{"sonar goal run <session-id> --prompt TEXT [options]", "--skip-approvals", "--model NAME"},
		},
		{
			name: "recover after positional", command: "recover", args: []string{"42", "--help"},
			want: []string{
				"sonar goal recover", "Required with --apply:", "--item ID",
				"--observed-at RFC3339", "never resumes execution", "no force override",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{test.command}, test.args...)
			if code := handleGoalCommandIO(args, &stdout, &stderr); code != 0 {
				t.Fatalf("goal %s %q exit = %d, want 0; stderr=%s", test.command, test.args, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("goal %s help wrote to stderr: %q", test.command, stderr.String())
			}
			for _, expected := range test.want {
				if !strings.Contains(stdout.String(), expected) {
					t.Fatalf("goal %s help omitted %q:\n%s", test.command, expected, stdout.String())
				}
			}
			assertHelpLinesAtMost(t, stdout.String(), 100)
			assertDefaultDatabaseDoesNotExist(t, home)
		})
	}
}

func TestExecutionSubcommandHelpDoesNotOpenDurableStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := handleExecutionCommandIO([]string{"recover", "session", "--json", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("execution recover --help exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("execution help wrote to stderr: %q", stderr.String())
	}
	for _, expected := range []string{
		"sonar execution recover", "SESSION_ID EXECUTION_ID", "Required with --apply:",
		"--revision N", "--event-id N", "--observed-at RFC3339", "never retries a tool",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("execution help omitted %q:\n%s", expected, stdout.String())
		}
	}
	assertHelpLinesAtMost(t, stdout.String(), 100)
	assertDefaultDatabaseDoesNotExist(t, home)
}

func TestRecoveryHelpFitsCommonTerminals(t *testing.T) {
	for _, test := range []struct {
		name   string
		write  func(io.Writer)
		wanted []string
	}{
		{
			name: "goal", write: writeGoalUsage,
			wanted: []string{
				"goal recover [--json] <session-id>",
				"goal recover --apply --item ID ... --observed-at RFC3339",
				"never resumes execution", "no force override",
			},
		},
		{
			name: "execution", write: writeExecutionUsage,
			wanted: []string{
				"execution recover [--json] SESSION_ID EXECUTION_ID",
				"exact inspection tokens and typed evidence",
				"never retries a tool", "immutable execution ledger",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			test.write(&output)
			for _, expected := range test.wanted {
				if !strings.Contains(output.String(), expected) {
					t.Fatalf("help omitted %q:\n%s", expected, output.String())
				}
			}
			assertHelpLinesAtMost(t, output.String(), 100)
		})
	}
}

func assertHelpLinesAtMost(t *testing.T, output string, limit int) {
	t.Helper()
	for index, line := range strings.Split(output, "\n") {
		if width := utf8.RuneCountInString(line); width > limit {
			t.Fatalf("help line %d is %d columns, want at most %d:\n%s", index+1, width, limit, line)
		}
	}
}

func TestUnknownGoalAndExecutionCommandsDoNotOpenDurableStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	for name, runCommand := range map[string]func() int{
		"goal": func() int {
			return handleGoalCommandIO([]string{"unknown"}, &bytes.Buffer{}, &bytes.Buffer{})
		},
		"execution": func() int {
			return handleExecutionCommandIO([]string{"unknown"}, &bytes.Buffer{}, &bytes.Buffer{})
		},
	} {
		if code := runCommand(); code != 2 {
			t.Fatalf("%s unknown command exit = %d, want 2", name, code)
		}
		assertDefaultDatabaseDoesNotExist(t, home)
	}
}

func TestGoalAndExecutionHelpRejectUnexpectedArgumentsWithoutOpeningDurableStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	for _, test := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
	}{
		{name: "goal", run: handleGoalCommandIO},
		{name: "execution", run: handleExecutionCommandIO},
	} {
		for _, help := range []string{"help", "-h", "--help"} {
			t.Run(test.name+" "+help, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if code := test.run([]string{help, "extra"}, &stdout, &stderr); code != 2 {
					t.Fatalf("%s %s extra exit = %d, want 2", test.name, help, code)
				}
				if !strings.Contains(stderr.String(), `unexpected argument "extra"`) {
					t.Fatalf("%s %s extra error = %q", test.name, help, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("%s %s extra wrote help to stdout: %q", test.name, help, stdout.String())
				}
				assertDefaultDatabaseDoesNotExist(t, home)
			})
		}
	}
}

func TestLegacyYoloWarningExplainsMigrationAndAuthority(t *testing.T) {
	var output bytes.Buffer
	writeLegacyYoloWarning(&output, true)
	for _, expected := range []string{"--yolo is deprecated", "use --skip-approvals", "does not enable --auto"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("warning omitted %q: %s", expected, output.String())
		}
	}

	output.Reset()
	writeLegacyYoloWarning(&output, false)
	if output.Len() != 0 {
		t.Fatalf("disabled legacy warning wrote %q", output.String())
	}
}

func assertDefaultDatabaseDoesNotExist(t *testing.T, home string) {
	t.Helper()
	assertPathDoesNotExist(t, filepath.Join(home, ".config", "sonar", "sonar.db"))
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path %q exists or returned unexpected error: %v", path, err)
	}
}
