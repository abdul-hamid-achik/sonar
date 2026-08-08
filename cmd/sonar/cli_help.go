package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

const legacyYoloWarning = "warning: --yolo is deprecated; use --skip-approvals. It skips approval prompts; it does not enable --auto."

func writeRootUsage(writer io.Writer, program string) {
	_, _ = fmt.Fprintf(writer, "Usage:\n  %s [options]\n  %s <command> [options]\n\n", program, program)
	_, _ = fmt.Fprintln(writer, "Commands:")
	_, _ = fmt.Fprintln(writer, "  init       Create an AGENTS.md starter file in the current workspace")
	_, _ = fmt.Fprintln(writer, "  logs       List recent session logs or follow the latest log")
	_, _ = fmt.Fprintln(writer, "  goal       Create, run, inspect, and reconcile durable goals")
	_, _ = fmt.Fprintln(writer, "  execution  Inspect and reconcile standalone execution effects")
	_, _ = fmt.Fprintln(writer, "  session    List, export, or repair saved sessions")
	_, _ = fmt.Fprintln(writer, "  providers  Refresh the embedded provider catalog snapshot")
	_, _ = fmt.Fprintln(writer, "  help       Show this help")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  -p, --prompt <text>       Run one non-interactive prompt, print the response, and exit")
	_, _ = fmt.Fprintln(writer, "      --tools <names>       Expose only these built-in tools in headless mode (comma-separated)")
	_, _ = fmt.Fprintln(writer, "      --mode <mode>         Headless authority: normal, plan, or auto (default: normal)")
	_, _ = fmt.Fprintln(writer, "      --auto                Shortcut for --mode auto (requires --prompt)")
	_, _ = fmt.Fprintln(writer, "      --plan                Shortcut for --mode plan (requires --prompt)")
	_, _ = fmt.Fprintln(writer, "      --skip-approvals      Skip approval prompts; host, scope, and tool boundaries still apply")
	_, _ = fmt.Fprintln(writer, "      --json                Emit one machine-readable turn receipt on stdout (requires --prompt)")
	_, _ = fmt.Fprintln(writer, "      --json-stream         Emit JSONL progress, then the turn receipt (requires --prompt)")
	_, _ = fmt.Fprintln(writer, "      --run-id <id>         Label the execution ledger with an external run identity")
	_, _ = fmt.Fprintln(writer, "      --turn-id <id>        Label this headless turn with an external turn identity")
	_, _ = fmt.Fprintln(writer, "      --actor <name>        Record an external actor label in the turn receipt")
	_, _ = fmt.Fprintln(writer, "      --resume <a1b2c3d|latest>  Restore a saved interactive session")
	_, _ = fmt.Fprintln(writer, "      --model <name>        Override the Ollama model")
	_, _ = fmt.Fprintln(writer, "      --agent <name>        Override the agent profile")
	_, _ = fmt.Fprintln(writer, "      --qwen-router         Use the optimized Qwen model router (experimental)")
	_, _ = fmt.Fprintln(writer, "      --yolo                Deprecated alias for --skip-approvals; does not enable --auto")
	_, _ = fmt.Fprintln(writer, "      --version             Print the build version and exit")
	_, _ = fmt.Fprintln(writer, "  -h, --help                Show this help")
}

func handleRootHelp(args []string, program string, stdout, stderr io.Writer) int {
	if len(args) != 0 && !isSoleHelpFlag(args) {
		_, _ = fmt.Fprintf(stderr, "help: unexpected argument %q\n", args[0])
		return 2
	}
	writeRootUsage(stdout, program)
	return 0
}

func isSoleHelpFlag(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}

func hasHelpFlag(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func flagParseExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0, true
	}
	return 2, true
}

func writeLegacyYoloWarning(writer io.Writer, enabled bool) {
	if enabled {
		_, _ = fmt.Fprintln(writer, legacyYoloWarning)
	}
}
