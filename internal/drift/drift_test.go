// Package drift compares this harness against its sibling checkout.
//
// sonar was forked from local-agent and most of the machinery came across
// unchanged. Nothing compares the two, so a fix lands in one and not the other
// and no one finds out until the behaviour differs in front of a user. That has
// already happened more than once: cmd/…/provider_selection.go was converted to
// a single provider-type predicate in sonar and left as a hand-written list in
// local-agent, and AGENTS.md drifted from CLAUDE.md into describing a website
// that does not exist.
//
// These tests do two different jobs, and the difference matters:
//
//   - syncedPackages must stay identical, module path aside. A diff there is a
//     failure, because a change to one of them was meant for both.
//   - everything else is reported, not gated. sonar deliberately diverges in
//     llm, config, agent, and ui; a gate there would be noise, and noise gets
//     silenced. The report is for a human to read when touching shared code.
//
// Both skip when the sibling checkout is absent, so a clone of one repository
// alone still passes. That means CI cannot enforce this — it is a local tool,
// and saying so is better than pretending a skipped test is a passing one.
package drift

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// syncedPackages are byte-identical today, module path aside, and are expected
// to stay that way. Removing a package from this list is a decision to let the
// two harnesses diverge there — make it deliberately, with a reason.
//
// expertselector was removed from this list when sonar deleted expert
// consultation outright: the feature selected several distinct models from a
// LOCAL multi-model Ollama inventory, and sonar's hosted providers serve one
// model per profile with no inventory to select from. local-agent keeps the
// package because it keeps the local inventory; sonar no longer has the
// package at all, which is divergence by decision, not drift.
var syncedPackages = []string{
	"controlplane",
	"execution",
	"imageasset",
	"initcmd",
	"netpolicy",
	"reconciliation",
	"resource",
	"sessionref",
	"tools",
	"workunit",
}

const (
	thisModule    = "github.com/abdul-hamid-achik/sonar"
	siblingModule = "github.com/abdul-hamid-achik/local-agent"
	siblingDir    = "local-agent"
)

func TestSharedPackagesHaveNotDrifted(t *testing.T) {
	here, there := checkouts(t)

	for _, pkg := range syncedPackages {
		t.Run(pkg, func(t *testing.T) {
			ours := filepath.Join(here, "internal", pkg)
			theirs := filepath.Join(there, "internal", pkg)
			if _, err := os.Stat(theirs); err != nil {
				t.Fatalf("%s is absent from the sibling checkout; if that is intended, drop it from syncedPackages with a reason", pkg)
			}
			for _, difference := range comparePackage(t, ours, theirs) {
				t.Errorf("%s", difference)
			}
		})
	}
}

// TestSharedPackageDriftReport never fails. It prints what differs so the
// divergence is visible when someone runs the package, instead of being
// discovered by a user. Read it with `go test ./internal/drift/ -v`.
func TestSharedPackageDriftReport(t *testing.T) {
	here, there := checkouts(t)

	entries, err := os.ReadDir(filepath.Join(here, "internal"))
	if err != nil {
		t.Fatalf("read internal/: %v", err)
	}
	synced := make(map[string]bool, len(syncedPackages))
	for _, pkg := range syncedPackages {
		synced[pkg] = true
	}

	var clean []string
	for _, entry := range entries {
		if !entry.IsDir() || synced[entry.Name()] || entry.Name() == "drift" {
			// This package is mirrored, not shared: the two copies name
			// different modules on purpose, so it is always "different".
			continue
		}
		pkg := entry.Name()
		theirs := filepath.Join(there, "internal", pkg)
		if _, err := os.Stat(theirs); err != nil {
			t.Logf("%-18s here only, absent from %s", pkg, siblingDir)
			continue
		}
		differences := comparePackage(t, filepath.Join(here, "internal", pkg), theirs)
		if len(differences) == 0 {
			clean = append(clean, pkg)
			continue
		}
		t.Logf("%-18s %d file(s) differ", pkg, len(differences))
	}
	if len(clean) > 0 {
		sort.Strings(clean)
		t.Logf("identical but not pinned: %s", strings.Join(clean, ", "))
		t.Logf("a package listed here is a candidate for syncedPackages")
	}
}

// comparePackage returns one message per file that differs, is missing, or is
// extra. Comparison normalizes the module path: an import line naming a
// different module is the fork, not drift.
func comparePackage(t *testing.T, ours, theirs string) []string {
	t.Helper()
	ourFiles := goFiles(t, ours)
	theirFiles := goFiles(t, theirs)

	var out []string
	for _, name := range sortedKeys(ourFiles) {
		theirBody, ok := theirFiles[name]
		if !ok {
			out = append(out, fmt.Sprintf("%s exists here and not in the sibling", name))
			continue
		}
		if ourFiles[name] != theirBody {
			out = append(out, fmt.Sprintf("%s differs beyond the module path", name))
		}
	}
	for _, name := range sortedKeys(theirFiles) {
		if _, ok := ourFiles[name]; !ok {
			out = append(out, fmt.Sprintf("%s exists in the sibling and not here", name))
		}
	}
	return out
}

func goFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name())) // #nosec G304 -- test-local checkout path
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		normalized := strings.ReplaceAll(string(body), thisModule, "MODULE")
		normalized = strings.ReplaceAll(normalized, siblingModule, "MODULE")
		out[entry.Name()] = normalized
	}
	return out
}

// checkouts locates this module's root and its sibling, skipping when the
// sibling is not checked out beside it.
func checkouts(t *testing.T) (here, there string) {
	t.Helper()
	here = moduleRoot(t)
	there = filepath.Join(filepath.Dir(here), siblingDir)
	if _, err := os.Stat(filepath.Join(there, "go.mod")); err != nil {
		t.Skipf("sibling checkout %s is absent; drift cannot be compared from one repository alone", there)
	}
	return here, there
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
