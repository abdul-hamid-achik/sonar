package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

// providersSnapshotPath is the embedded catalog snapshot inside a source
// checkout. The binary embeds this file at build time, so a refresh only takes
// effect after a rebuild.
var providersSnapshotPath = filepath.Join("internal", "catalog", "providers.json")

func handleProvidersCommand(args []string) int {
	return handleProvidersCommandIO(args, os.Stdout, os.Stderr)
}

func handleProvidersCommandIO(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeProvidersUsage(stdout)
		return 0
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			_, _ = fmt.Fprintf(stderr, "providers: unexpected argument %q after %s\n", args[1], args[0])
			return 2
		}
		writeProvidersUsage(stdout)
		return 0
	}
	if args[0] != "refresh" {
		_, _ = fmt.Fprintf(stderr, "providers: unknown command %q\n", args[0])
		writeProvidersUsage(stderr)
		return 2
	}
	if hasHelpFlag(args[1:]) {
		return handleProvidersRefresh([]string{"--help"}, stdout, stderr)
	}
	return handleProvidersRefresh(args[1:], stdout, stderr)
}

func handleProvidersRefresh(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("providers refresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeProvidersUsage(stdout) }
	url := flags.String("url", "", "catalog endpoint (default "+catalog.DefaultCatwalkURL+")")
	dryRun := flags.Bool("dry-run", false, "report what would change without writing")
	if code, done := flagParseExitCode(flags.Parse(args)); done {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "providers refresh: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	var summary catalog.RefreshSummary
	var err error
	if *dryRun {
		summary, err = catalog.DryRun(context.Background(), *url, providersSnapshotPath)
	} else {
		summary, err = catalog.Refresh(context.Background(), *url, providersSnapshotPath)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "providers refresh: %v\n", err)
		return 1
	}

	if *dryRun {
		_, _ = fmt.Fprintf(stdout, "dry run: %s\n", summary.String())
		if summary.Changed {
			_, _ = fmt.Fprintf(stdout, "would rewrite %s\n", providersSnapshotPath)
		}
		return 0
	}
	_, _ = fmt.Fprintln(stdout, summary.String())
	if summary.Changed {
		_, _ = fmt.Fprintf(stdout, "wrote %s — the snapshot is embedded at build time, so rebuild sonar for it to take effect\n", providersSnapshotPath)
	}
	return 0
}

func writeProvidersUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage:")
	_, _ = fmt.Fprintln(writer, "  sonar providers refresh [--url URL] [--dry-run]")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "Options:")
	_, _ = fmt.Fprintln(writer, "  --url URL    Catalog endpoint (default: "+catalog.DefaultCatwalkURL+")")
	_, _ = fmt.Fprintln(writer, "  --dry-run    Report what would change without writing")
	_, _ = fmt.Fprintln(writer, "  -h, --help   Show this help")
	_, _ = fmt.Fprintln(writer)
	_, _ = fmt.Fprintln(writer, "refresh fetches the live Catwalk catalog and rewrites internal/catalog/")
	_, _ = fmt.Fprintln(writer, "providers.json, which is embedded at build time; rebuild sonar for the")
	_, _ = fmt.Fprintln(writer, "change to take effect. The write is atomic, and a truncated fetch is")
	_, _ = fmt.Fprintln(writer, "refused so a bad fetch never costs a working catalog.")
}
