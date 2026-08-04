package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/abdul-hamid-achik/sonar/internal/initcmd"
	"github.com/abdul-hamid-achik/sonar/internal/logging"
)

func handleInit(args []string) int {
	return handleInitIO(args, os.Stdout, os.Stderr)
}

func handleInitIO(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sonar init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeInitUsage(stdout) }
	force := flags.Bool("force", false, "replace an existing AGENTS.md")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "init: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if err := initcmd.Run(".", initcmd.Options{Force: *force}); err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "AGENTS.md created successfully.")
	return 0
}

func writeInitUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar init [--force]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --force     Replace an existing AGENTS.md")
	_, _ = fmt.Fprintln(writer, "  -h, --help  Show this help")
}

// handleLogs implements the "logs" subcommand. With -f it execs tail -f on
// the latest log file; otherwise it lists recent sessions.
func handleLogs(args []string) int {
	return handleLogsIO(args, os.Stdout, os.Stderr)
}

func handleLogsIO(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sonar logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeLogsUsage(stdout) }
	follow := flags.Bool("f", false, "follow the latest log")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "logs: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	if *follow {
		latest, err := logging.LatestLogPath()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "logs: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "following %s\n", latest)
		tailBin, err := exec.LookPath("tail")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "logs: tail not found: %v\n", err)
			return 1
		}
		// Replace the process with tail -f.
		if err := syscall.Exec(tailBin, []string{"tail", "-f", latest}, os.Environ()); err != nil {
			_, _ = fmt.Fprintf(stderr, "logs: exec tail: %v\n", err)
			return 1
		}
		return 0
	}

	entries, err := logging.ListLogs(20)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "logs: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(stdout, "No log files found in", logging.LogDir())
		return 0
	}

	_, _ = fmt.Fprintf(stdout, "Recent sessions (%s):\n\n", logging.LogDir())
	for _, entry := range entries {
		name := filepath.Base(entry.Path)
		sizeKB := float64(entry.Size) / 1024
		_, _ = fmt.Fprintf(stdout, "  %-30s  %s  %6.1f KB\n", name, entry.ModTime.Format("2006-01-02 15:04:05"), sizeKB)
	}
	_, _ = fmt.Fprintln(stdout, "\nTip: run `sonar logs -f` to follow the latest log.")
	return 0
}

func writeLogsUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar logs [-f]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  -f          Follow the latest log")
	_, _ = fmt.Fprintln(writer, "  -h, --help  Show this help")
}
