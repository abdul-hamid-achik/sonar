package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

func TestExplicitPromptPathCandidatesRecognizesQuotedPathsWithoutGrantCap(t *testing.T) {
	text := `read "/tmp/quoted path.md", '/tmp/single path.md'; ` +
		"`/tmp/backtick path.md` and /tmp/plain.md /tmp/fifth.md /tmp/plain.md"
	got := explicitPromptPathCandidates(text)
	want := []string{
		"/tmp/quoted path.md",
		"/tmp/single path.md",
		"/tmp/backtick path.md",
		"/tmp/plain.md",
		"/tmp/fifth.md",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
	if scan := scanExplicitPromptPaths(text); scan.MoreCandidates {
		t.Fatal("ordinary candidate count hit the anti-abuse scan limit")
	}
}

func TestExplicitPromptPathCandidatesRecognizesEscapedWhitespace(t *testing.T) {
	got := explicitPromptPathCandidates(`inspect /Users/me/Desktop/My\ Screenshot.jpg and /tmp/plain.txt`)
	want := []string{`/Users/me/Desktop/My Screenshot.jpg`, "/tmp/plain.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("escaped candidates = %#v, want %#v", got, want)
	}
}

func TestExplicitPromptPathCandidatesPreservesQuotedAndEscapedTerminalPunctuation(t *testing.T) {
	text := `inspect "/tmp/Project (copy)" and /tmp/Report\ final!`
	got := explicitPromptPathCandidates(text)
	want := []string{"/tmp/Project (copy)", "/tmp/Report final!"}
	if !slices.Equal(got, want) {
		t.Fatalf("punctuated candidates = %#v, want %#v", got, want)
	}
	for _, intent := range scanExplicitPromptPaths(text).Intents {
		if intent.Fallback != "" {
			t.Fatalf("exact quoted/escaped intent gained punctuation fallback: %#v", intent)
		}
	}
}

func TestPromptPathScanHasSeparateHardCandidateLimit(t *testing.T) {
	parts := make([]string, 0, maxPromptPathScanIntents+1)
	for index := 0; index <= maxPromptPathScanIntents; index++ {
		parts = append(parts, fmt.Sprintf("/tmp/candidate-%d", index))
	}
	scan := scanExplicitPromptPaths(strings.Join(parts, " "))
	if !scan.MoreCandidates || len(scan.Intents) != maxPromptPathScanIntents {
		t.Fatalf("hard-cap scan = intents:%d more:%v", len(scan.Intents), scan.MoreCandidates)
	}
}

func TestPromptPathHardCandidateLimitKeepsDraftAndSendsNothing(t *testing.T) {
	parts := make([]string, 0, maxPromptPathScanIntents+1)
	for index := 0; index <= maxPromptPathScanIntents; index++ {
		parts = append(parts, fmt.Sprintf("/missing/candidate-%d", index))
	}
	draft := "compare " + strings.Join(parts, " ")
	m := newTestModel(t)
	t.Cleanup(m.agent.Close)
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if !preflight.MoreCandidates || !preflight.CandidateLimitExceeded || len(preflight.Grants) != 0 {
		t.Fatalf("hard-limit preflight = %#v", preflight)
	}
	updated, cmd := m.Update(preflight)
	m = updated.(*Model)
	if cmd != nil || m.input.Value() != draft || len(m.promptHistory) != 0 || len(m.agent.Messages()) != 0 || len(m.agent.ReadGrants()) != 0 {
		t.Fatalf("hard limit sent or authorized work: cmd=%v input=%q history=%#v messages=%#v grants=%#v", cmd != nil, m.input.Value(), m.promptHistory, m.agent.Messages(), m.agent.ReadGrants())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "more than 32 distinct path candidates") || !strings.Contains(transcript, "nothing was sent") {
		t.Fatalf("hard-limit guidance missing: %s", transcript)
	}
}

func TestPromptPathOverflowKeepsDraftAndSendsNothing(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := make([]string, 0, maxPromptPathIntents+1)
	for index := 0; index <= maxPromptPathIntents; index++ {
		path := filepath.Join(external, fmt.Sprintf("evidence-%d.txt", index))
		if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	draft := "review " + strings.Join(paths, " ")

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if !preflight.MoreCandidates || len(preflight.Grants) != 0 {
		t.Fatalf("overflow preflight = %#v", preflight)
	}
	updated, cmd := m.Update(preflight)
	m = updated.(*Model)
	if cmd != nil || m.input.Value() != draft || len(m.promptHistory) != 0 || len(m.agent.Messages()) != 0 || len(m.agent.ReadGrants()) != 0 {
		t.Fatalf("overflow sent or authorized work: cmd=%v input=%q history=%#v messages=%#v grants=%#v", cmd != nil, m.input.Value(), m.promptHistory, m.agent.Messages(), m.agent.ReadGrants())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "more than 4 new temporary scopes") || !strings.Contains(transcript, "nothing was sent") {
		t.Fatalf("overflow guidance missing: %s", transcript)
	}
}

func TestPromptPathGrantLimitIgnoresWorkspacePathsAndCollapsesCoverage(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspacePaths := make([]string, 0, maxPromptPathIntents+1)
	externalPaths := make([]string, 0, maxPromptPathIntents+1)
	for index := 0; index <= maxPromptPathIntents; index++ {
		inside := filepath.Join(workspace, fmt.Sprintf("inside-%d.txt", index))
		outside := filepath.Join(external, fmt.Sprintf("outside-%d.txt", index))
		for _, path := range []string{inside, outside} {
			if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		workspacePaths = append(workspacePaths, inside)
		externalPaths = append(externalPaths, outside)
	}

	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	workspaceScan := scanExplicitPromptPaths(strings.Join(workspacePaths, " "))
	if grants, overflow := inspectPromptReadGrantIntents(ag, workspaceScan.Intents); overflow || len(grants) != 0 {
		t.Fatalf("workspace paths consumed external grant budget: grants=%#v overflow=%v", grants, overflow)
	}

	coveredScan := scanExplicitPromptPaths(strings.Join(append(externalPaths, external), " "))
	grants, overflow := inspectPromptReadGrantIntents(ag, coveredScan.Intents)
	t.Cleanup(func() { releaseReadGrants(grants) })
	canonicalExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if overflow || len(grants) != 1 || grants[0].Kind != agent.ReadGrantDirectory || grants[0].Path != canonicalExternal {
		t.Fatalf("covered grants = %#v overflow=%v", grants, overflow)
	}
}

func TestFreePromptPathPunctuationFallsBackOnlyWhenExactPathIsMissing(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(base, "report")
	punctuated := plain + ","
	if err := os.WriteFile(plain, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	scan := scanExplicitPromptPaths("inspect " + punctuated)
	grants, overflow := inspectPromptReadGrantIntents(ag, scan.Intents)
	canonicalPlain, err := filepath.EvalSymlinks(plain)
	if err != nil {
		t.Fatal(err)
	}
	if overflow || len(grants) != 1 || grants[0].Path != canonicalPlain {
		t.Fatalf("verified fallback grants = %#v overflow=%v", grants, overflow)
	}
	releaseReadGrants(grants)

	if err := os.WriteFile(punctuated, []byte("punctuated"), 0o600); err != nil {
		t.Fatal(err)
	}
	grants, overflow = inspectPromptReadGrantIntents(ag, scan.Intents)
	canonicalPunctuated, err := filepath.EvalSymlinks(punctuated)
	if err != nil {
		t.Fatal(err)
	}
	if overflow || len(grants) != 1 || grants[0].Path != canonicalPunctuated {
		t.Fatalf("exact punctuated grants = %#v overflow=%v", grants, overflow)
	}
	t.Cleanup(func() { releaseReadGrants(grants) })
}

func TestExplicitPromptPathCandidatesRecognizesTildeAndRejectsURLs(t *testing.T) {
	got := explicitPromptPathCandidates(`compare "~/notes with spaces.md" with https://example.com/a and @/tmp/local.md`)
	want := []string{"~/notes with spaces.md", "/tmp/local.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
}

func TestInspectPromptReadGrantsKeepsExactFilesNarrowAndDeduplicated(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(external, "requested.txt")
	if err := os.WriteFile(target, []byte("requested"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)

	grants := inspectPromptReadGrants(ag, []string{target, target})
	if len(grants) != 1 || grants[0].Kind != agent.ReadGrantExactFile {
		t.Fatalf("exact grants = %#v", grants)
	}
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if grants[0].Path != canonical {
		t.Fatalf("exact grant path = %q, want %q", grants[0].Path, canonical)
	}
	if grants[0].Path == filepath.Dir(grants[0].Path) || grants[0].Path == external {
		t.Fatalf("exact grant widened to a directory: %#v", grants[0])
	}
	releaseReadGrants(grants)

	// An explicitly named directory supersedes only contained candidates. It
	// is never inferred from the exact-file candidate above.
	grants = inspectPromptReadGrants(ag, []string{target, external})
	canonicalExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Kind != agent.ReadGrantDirectory || grants[0].Path != canonicalExternal {
		t.Fatalf("explicit directory did not supersede contained file: %#v", grants)
	}
	t.Cleanup(func() { releaseReadGrants(grants) })
}

func TestPromptPathPreflightIgnoresWorkspaceMissingAndCoveredPaths(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspaceFile := filepath.Join(workspace, "inside.txt")
	coveredFile := filepath.Join(external, "covered.txt")
	for path, content := range map[string]string{workspaceFile: "inside", coveredFile: "covered"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	if _, err := ag.AddReadRoot(external); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing.txt")
	if grants := inspectPromptReadGrants(ag, []string{workspaceFile, missing, coveredFile}); len(grants) != 0 {
		t.Fatalf("already-readable or missing paths requested authority: %#v", grants)
	}
}

func TestOrdinaryPromptExactFileApprovalAutoResumesOnce(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	for _, path := range []string{workspace, external} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(external, "requested file.txt")
	sibling := filepath.Join(external, "sibling.txt")
	for path, content := range map[string]string{target: "requested", sibling: "sibling"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	draft := `please summarize "` + target + `"`
	m.input.SetValue(draft)

	preflightCmd := m.submitInput()
	if preflightCmd == nil || !m.readScopeOpRunning || len(m.promptHistory) != 0 {
		t.Fatalf("preflight consumed prompt early: cmd=%v running=%v history=%#v", preflightCmd != nil, m.readScopeOpRunning, m.promptHistory)
	}
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(preflightCmd), 2*time.Second)
	updated, cmd := m.Update(preflight)
	m = updated.(*Model)
	if cmd != nil || m.readScopePrompt == nil || len(m.readScopePrompt.Grants) != 1 || m.readScopePrompt.Grants[0].Kind != agent.ReadGrantExactFile {
		t.Fatalf("preflight prompt = %#v, cmd=%v", m.readScopePrompt, cmd != nil)
	}
	plain := ansi.Strip(m.renderReadScopePrompt())
	for _, want := range []string{"exact external read-only file", filepath.Base(target), "never include", "siblings", "allow read-only", "deny", "cancel"} {
		if !strings.Contains(strings.ToLower(plain), strings.ToLower(want)) {
			t.Fatalf("exact-file approval missing %q:\n%s", want, plain)
		}
	}

	updated, mutationCmd := m.Update(charKey('y'))
	m = updated.(*Model)
	if mutationCmd == nil || !m.readScopeOpRunning || m.readScopePrompt != nil {
		t.Fatal("allow did not start exact-file grant mutation")
	}
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	updated, agentCmd := m.Update(receipt)
	m = updated.(*Model)
	if agentCmd == nil || m.state != StateWaiting || m.readScopePrompt != nil || m.readScopeOpRunning {
		t.Fatalf("approval did not resume one agent turn: cmd=%v state=%v prompt=%#v running=%v", agentCmd != nil, m.state, m.readScopePrompt, m.readScopeOpRunning)
	}
	if len(m.promptHistory) != 1 || m.promptHistory[0] != draft {
		t.Fatalf("auto-resume history = %#v", m.promptHistory)
	}
	if messages := m.agent.Messages(); len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != draft {
		t.Fatalf("auto-resume agent messages = %#v", messages)
	}
	userEntries := 0
	for _, entry := range m.entries {
		if entry.Kind == "user" && entry.Content == draft {
			userEntries++
		}
	}
	if userEntries != 1 {
		t.Fatalf("auto-resume visible user entries = %d; entries=%#v", userEntries, m.entries)
	}
	grants := m.agent.ReadGrants()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Kind != agent.ReadGrantExactFile || grants[0].Path != canonicalTarget {
		t.Fatalf("committed grants = %#v", grants)
	}
	inspection, err := m.agent.InspectReadPath(sibling)
	defer inspection.Release()
	if err != nil || inspection.AlreadyReadable {
		t.Fatalf("sibling inherited exact-file authority: %#v, %v", inspection, err)
	}
}

func TestOrdinaryPromptApprovalRejectsFileReplacedWhilePromptIsOpen(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "requested.txt")
	if err := os.WriteFile(target, []byte("approved-identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	draft := "inspect " + target
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	updated, _ := m.Update(preflight)
	m = updated.(*Model)
	if m.readScopePrompt == nil {
		t.Fatal("external path did not open approval")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replaced-identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, mutationCmd := m.Update(charKey('y'))
	m = updated.(*Model)
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	updated, agentCmd := m.Update(receipt)
	m = updated.(*Model)
	if agentCmd != nil || m.state == StateWaiting || len(m.promptHistory) != 0 || len(m.agent.ReadGrants()) != 0 {
		t.Fatalf("stale approval resumed or leaked authority: cmd=%v state=%v history=%#v grants=%#v", agentCmd != nil, m.state, m.promptHistory, m.agent.ReadGrants())
	}
	if m.input.Value() != draft {
		t.Fatalf("stale approval did not restore draft: %q", m.input.Value())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "changed after approval preview") {
		t.Fatalf("stale identity error is not actionable: %s", transcript)
	}
}

func TestOrdinaryPromptExternalPathDenyAndCancelRestoreExactDraft(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "requested.txt")
	if err := os.WriteFile(target, []byte("requested"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		key  rune
	}{
		{name: "deny", key: 'n'},
		{name: "cancel", key: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.agent.SetWorkDir(workspace)
			t.Cleanup(m.agent.Close)
			draft := "inspect " + target
			m.input.SetValue(draft)
			preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
			updated, _ := m.Update(preflight)
			m = updated.(*Model)
			if test.key == 0 {
				updated, _ = m.Update(escKey())
			} else {
				updated, _ = m.Update(charKey(test.key))
			}
			m = updated.(*Model)
			if m.input.Value() != draft || m.readScopePrompt != nil || len(m.promptHistory) != 0 || len(m.agent.ReadGrants()) != 0 {
				t.Fatalf("%s changed draft or authority: input=%q prompt=%#v history=%#v grants=%#v", test.name, m.input.Value(), m.readScopePrompt, m.promptHistory, m.agent.ReadGrants())
			}
		})
	}
}

func TestOrdinaryPromptPreflightCanonicalizesTildeSymlinkWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tilde and symlink fixture uses Unix path syntax")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	target := filepath.Join(home, "notes with spaces.txt")
	alias := filepath.Join(home, "notes alias.txt")
	if err := os.WriteFile(target, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	draft := `read "~/notes alias.txt"`
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(preflight.Grants) != 1 || preflight.Grants[0].Kind != agent.ReadGrantExactFile || preflight.Grants[0].Path != canonical {
		t.Fatalf("tilde symlink preflight = %#v, want %q", preflight.Grants, canonical)
	}
}

func TestOrdinaryPromptNoNewAuthorityStillSubmitsAfterPreflight(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	draft := "read " + inside + " and " + filepath.Join(t.TempDir(), "missing.txt")
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if len(preflight.Grants) != 0 {
		t.Fatalf("no-authority preflight = %#v", preflight.Grants)
	}
	updated, agentCmd := m.Update(preflight)
	m = updated.(*Model)
	if agentCmd == nil || m.state != StateWaiting || m.readScopePrompt != nil || len(m.promptHistory) != 1 {
		t.Fatalf("no-authority preflight did not submit: cmd=%v state=%v prompt=%#v history=%#v", agentCmd != nil, m.state, m.readScopePrompt, m.promptHistory)
	}
}

func TestPromptPathMutationIntentUsesExactDirectGrammar(t *testing.T) {
	tests := []struct {
		name string
		text string
		path string
		want bool
	}{
		{name: "direct update", text: "please update /external/repo", path: "/external/repo", want: true},
		{name: "polite request", text: "can you update /external/repo?", path: "/external/repo", want: true},
		{name: "spanish request", text: "por favor actualiza /external/repo", path: "/external/repo", want: true},
		{name: "workspace noun", text: "inspect workspace /external/repo", path: "/external/repo", want: false},
		{name: "configuration noun", text: "review configuration /external/repo", path: "/external/repo", want: false},
		{name: "additional adjective", text: "read additional file /external/x", path: "/external/x", want: false},
		{name: "generated adjective", text: "inspect generated output /external/x", path: "/external/x", want: false},
		{name: "counterfactual", text: "how would you update /external/repo?", path: "/external/repo", want: false},
		{name: "history", text: "why did it edit /external/repo?", path: "/external/repo", want: false},
		{name: "reference", text: "update /dest based on /reference", path: "/reference", want: false},
		{name: "copy source", text: "copy /source to /dest", path: "/source", want: false},
		{name: "copy destination", text: "copy /source to /dest", path: "/dest", want: true},
		{name: "add source", text: "add /external/image.png to the report", path: "/external/image.png", want: false},
		{name: "add destination", text: "add a line to /external/report.md", path: "/external/report.md", want: true},
		{name: "install source", text: "install /external/package.tgz", path: "/external/package.tgz", want: false},
		{name: "install destination", text: "install the package into /external/tool", path: "/external/tool", want: true},
		{name: "register source", text: "register /external/binary", path: "/external/binary", want: false},
		{name: "register destination", text: "register the tool in /external/registry", path: "/external/registry", want: true},
		{name: "integrate source", text: "integrate /external/library into this repo", path: "/external/library", want: false},
		{name: "integrate destination", text: "integrate the library into /external/repo", path: "/external/repo", want: true},
		{name: "integrate peer source", text: "integrate with /external/library", path: "/external/library", want: false},
		{name: "spanish peer source", text: "conecta con /external/service", path: "/external/service", want: false},
		{name: "update with external source", text: "update the current repo with /external/patch", path: "/external/patch", want: false},
		{name: "spanish update external source", text: "actualiza el repo con /external/datos", path: "/external/datos", want: false},
		{name: "summary source", text: "create a summary of /external/repo", path: "/external/repo", want: false},
		{name: "report source", text: "write a report about /external/repo", path: "/external/repo", want: false},
		{name: "after reading source", text: "update my notes after reading /external/repo", path: "/external/repo", want: false},
		{name: "documentation reference", text: "update documentation regarding /external/repo", path: "/external/repo", want: false},
		{name: "reviewing source", text: "update docs after reviewing /external/repo", path: "/external/repo", want: false},
		{name: "inspecting source", text: "fix the repo by inspecting /external/source", path: "/external/source", want: false},
		{name: "spanish summary source", text: "crea un resumen de /external/repo", path: "/external/repo", want: false},
		{name: "spanish report source", text: "escribe un reporte sobre /external/repo", path: "/external/repo", want: false},
		{name: "prior path verb masked", text: "compare /tmp/update with /external/repo", path: "/external/repo", want: false},
		{name: "quoted path verb masked", text: `read "/tmp/delete" and "/external/repo"`, path: "/external/repo", want: false},
		{name: "reported instruction", text: "the README says to update /external/repo", path: "/external/repo", want: false},
		{name: "quoted verb is data", text: `explain "update" /external/repo`, path: "/external/repo", want: false},
		{name: "inline code verb is data", text: "explain `update` /external/repo", path: "/external/repo", want: false},
		{name: "line sample is data", text: "explain this:\nupdate /external/repo", path: "/external/repo", want: false},
		{name: "blank line starts direct request", text: "the README says inspect /external/source\n\nupdate /external/repo", path: "/external/repo", want: true},
		{name: "quoted pathname remains writable", text: `update "/external/repo with spaces"`, path: "/external/repo with spaces", want: true},
		{name: "spanish subjunctive request", text: "quiero que actualices /external/repo", path: "/external/repo", want: true},
		{name: "explained phrase", text: "explain the phrase update /external/repo", path: "/external/repo", want: false},
		{name: "historical attempt", text: "the last agent tried to edit /external/repo", path: "/external/repo", want: false},
		{name: "planned action", text: "our plan says build /external/repo", path: "/external/repo", want: false},
		{name: "indirect negation", text: "I don't want you to update /external/repo", path: "/external/repo", want: false},
		{name: "multiple contractions preserve denial", text: "I don't think it's okay to update /external/repo", path: "/external/repo", want: false},
		{name: "not asking", text: "I am not asking you to update /external/repo", path: "/external/repo", want: false},
		{name: "agent negation", text: "please don't let the agent update /external/repo", path: "/external/repo", want: false},
		{name: "without prompt direct", text: "without asking, update /external/repo", path: "/external/repo", want: true},
		{name: "quoted copy destination", text: `copy "/tmp/from/source file" to "/external/dest repo"`, path: "/external/dest repo", want: true},
		{name: "move source", text: "move /source to /dest", path: "/source", want: true},
		{name: "move destination", text: "move /source to /dest", path: "/dest", want: true},
		{name: "single delete reference source", text: "delete references to /external/source from the current repo", path: "/external/source", want: false},
		{name: "single remove link source", text: "remove links to /external/source", path: "/external/source", want: false},
		{name: "suffix no change", text: "update /external/repo, but don't change anything", path: "/external/repo", want: false},
		{name: "suffix no modify", text: "update /external/repo, but don't modify it", path: "/external/repo", want: false},
		{name: "suffix sentence consent", text: `update "/external/repo". Ask me first.`, path: "/external/repo", want: false},
		{name: "suffix semicolon consent", text: "update /external/repo ; ask me first", path: "/external/repo", want: false},
		{name: "suffix late consent", text: "update /external/repo then carefully inspect the plan and all its assumptions with every relevant test and dependency before you ask me first", path: "/external/repo", want: false},
		{name: "suffix read correction", text: "update /external/repo, actually only review it", path: "/external/repo", want: false},
		{name: "suffix read only", text: "update /external/repo read-only", path: "/external/repo", want: false},
		{name: "suffix ask first", text: "update /external/repo, ask me first", path: "/external/repo", want: false},
		{name: "suffix withheld approval", text: "update /external/repo after I approve", path: "/external/repo", want: false},
		{name: "suffix conditional approval", text: "update /external/repo only if I approve", path: "/external/repo", want: false},
		{name: "suffix confirmation", text: "update /external/repo after confirmation", path: "/external/repo", want: false},
		{name: "suffix if approve", text: "update /external/repo if I approve", path: "/external/repo", want: false},
		{name: "suffix when approved", text: "update /external/repo when approved", path: "/external/repo", want: false},
		{name: "suffix once confirm", text: "update /external/repo once I confirm", path: "/external/repo", want: false},
		{name: "suffix when confirm", text: "update /external/repo when I confirm", path: "/external/repo", want: false},
		{name: "suffix upon approval", text: "update /external/repo upon approval", path: "/external/repo", want: false},
		{name: "suffix subject to approval", text: "update /external/repo subject to approval", path: "/external/repo", want: false},
		{name: "suffix later", text: "update /external/repo later, not now", path: "/external/repo", want: false},
		{name: "suffix tomorrow", text: "update /external/repo tomorrow", path: "/external/repo", want: false},
		{name: "suffix eventually", text: "update /external/repo eventually", path: "/external/repo", want: false},
		{name: "suffix when told", text: "update /external/repo when I say so", path: "/external/repo", want: false},
		{name: "suffix green light", text: "update /external/repo after I give you the green light", path: "/external/repo", want: false},
		{name: "suffix pending okay", text: "update /external/repo pending my okay", path: "/external/repo", want: false},
		{name: "suffix not now", text: "update /external/repo not now", path: "/external/repo", want: false},
		{name: "suffix not yet", text: "update /external/repo, but not yet", path: "/external/repo", want: false},
		{name: "suffix once told", text: "update /external/repo once I tell you", path: "/external/repo", want: false},
		{name: "suffix once say go", text: "update /external/repo once I say go", path: "/external/repo", want: false},
		{name: "suffix when ready", text: "update /external/repo when I am ready", path: "/external/repo", want: false},
		{name: "affirmative go ahead", text: "update /external/repo, go ahead", path: "/external/repo", want: true},
		{name: "affirmative permission", text: "update /external/repo, you have my permission", path: "/external/repo", want: true},
		{name: "affirmative approved", text: "update /external/repo, this is approved", path: "/external/repo", want: true},
		{name: "go ahead prefix", text: "go ahead and update /external/repo", path: "/external/repo", want: true},
		{name: "you can prefix", text: "you can update /external/repo", path: "/external/repo", want: true},
		{name: "failed test purpose", text: "update /external/repo to fix the failed tests", path: "/external/repo", want: true},
		{name: "command purpose", text: "update /external/repo so the command works", path: "/external/repo", want: true},
		{name: "suffix failed report", text: "update /external/repo failed", path: "/external/repo", want: false},
		{name: "suffix command report", text: "update /external/repo is the command shown in README", path: "/external/repo", want: false},
		{name: "topic tests", text: "write tests for /external/repo", path: "/external/repo", want: false},
		{name: "use source", text: "edit our config to use /external/tool", path: "/external/tool", want: false},
		{name: "reference source", text: "update references to /external/source", path: "/external/source", want: false},
		{name: "link source", text: "create a link to /external/source", path: "/external/source", want: false},
		{name: "report topic", text: "generate a report on /external/repo", path: "/external/repo", want: false},
		{name: "adapter source", text: "build an adapter around /external/api", path: "/external/api", want: false},
		{name: "tests against source", text: "configure tests against /external/api", path: "/external/api", want: false},
		{name: "spanish documentation topic", text: "crea documentación para /external/repo", path: "/external/repo", want: false},
		{name: "nearer review clause", text: "update docs while reviewing /external/reference", path: "/external/reference", want: false},
		{name: "nearer reading clause", text: "update notes while reading /external/source", path: "/external/source", want: false},
		{name: "nearer use clause", text: "update docs and use /external/source", path: "/external/source", want: false},
		{name: "nearer check clause", text: "update docs then check /external/reference", path: "/external/reference", want: false},
		{name: "nearer consult clause", text: "fix docs, consult /external/reference", path: "/external/reference", want: false},
		{name: "smart double quoted action", text: "“update” /external/repo", path: "/external/repo", want: false},
		{name: "smart single quoted action", text: "‘delete’ /external/repo", path: "/external/repo", want: false},
		{name: "double backtick action", text: "``update`` /external/repo", path: "/external/repo", want: false},
		{name: "double backtick with inner tick", text: "``literal ` tick`` /external/repo", path: "/external/repo", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scan := scanExplicitPromptPaths(test.text)
			for _, intent := range scan.Intents {
				if intent.Literal == test.path || intent.Fallback == test.path {
					if intent.Mutation != test.want {
						t.Fatalf("mutation(%q) = %v, want %v; intent=%#v", test.path, intent.Mutation, test.want, intent)
					}
					return
				}
			}
			t.Fatalf("path %q not scanned: %#v", test.path, scan.Intents)
		})
	}
}

func TestPromptPathPunctuationPreservesClauseBoundary(t *testing.T) {
	for _, separator := range []string{".", ";", "!", "?", "。", "！", "？", "…"} {
		scan := scanExplicitPromptPaths("update /tmp/a" + separator + " /tmp/b")
		if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || scan.Intents[1].Mutation {
			t.Fatalf("separator %q leaked write intent: %#v", separator, scan.Intents)
		}
	}
}

func TestPromptPathUnclosedQuotesNeverCreateAuthority(t *testing.T) {
	for _, draft := range []string{`"update /tmp/a`, `“update /tmp/a`, `'update /tmp/a`, `‘update /tmp/a`} {
		if scan := scanExplicitPromptPaths(draft); len(scan.Intents) != 0 {
			t.Fatalf("unclosed quote created authority for %q: %#v", draft, scan.Intents)
		}
	}
}

func TestPromptPathMutationIntentIgnoresFencedCodePaths(t *testing.T) {
	for _, text := range []string{
		"explain this:\n```sh\nupdate /external/repo\n```",
		"explain this:\n```sh\nupdate /external/repo",
		"revisa este ejemplo:\n~~~\nactualiza /external/repo\n~~~",
		"explain this:\n````md\n```\nupdate /external/repo\n````",
		"revisa esto:\n~~~~\n~~~\nactualiza /external/repo\n~~~~",
		"explain this:\n````md\nliteral ```` not a closing fence\nupdate /external/repo\n````",
		"revisa esto:\n~~~~\nliteral ~~~~ no cierra\nactualiza /external/repo\n~~~~",
	} {
		if scan := scanExplicitPromptPaths(text); len(scan.Intents) != 0 {
			t.Fatalf("fenced example created path authority for %q: %#v", text, scan.Intents)
		}
	}
}

func TestPromptPathExplicitNegationSuppressesAllAuthority(t *testing.T) {
	external := t.TempDir()
	for _, draft := range []string{
		"do not read " + external,
		"please don't update " + external,
		"no quiero que actualices " + external,
		"I don't want you to read " + external,
		"I am not asking you to inspect " + external,
		"do not just read " + external,
		"no quiero que leas " + external,
		"don't access " + external,
		"ignore " + external,
		"avoid " + external,
		"exclude " + external,
		"skip " + external,
		"no accedas " + external,
		"ignora " + external,
		"evita " + external,
		"excluye " + external,
		"omite " + external,
		"read " + external + ", but don't read it",
		"read " + external + " only after I approve",
		"without reading " + external,
		"leave " + external + " alone",
		"keep out of " + external,
		"keep away from " + external,
		"stay away from " + external,
		external + " — stay away",
		external + " is off limits",
		"anything except " + external,
		"everything but " + external,
		"don't do anything with " + external + " yet",
		"hold off on " + external,
	} {
		m := newTestModel(t)
		m.agent.SetWorkDir(t.TempDir())
		m.mode = ModeAuto
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) != 1 || !scan.Intents[0].Denied {
			m.agent.Close()
			t.Fatalf("explicit denial not recognized for %q: %#v", draft, scan.Intents)
		}
		reads, writes, unavailable, overflow := inspectPromptPathGrantIntents(m.agent, scan.Intents, true)
		m.agent.Close()
		if overflow || len(reads) != 0 || len(writes) != 0 || len(unavailable) != 0 {
			t.Fatalf("denied path gained authority for %q: reads=%#v writes=%#v unavailable=%#v overflow=%v", draft, reads, writes, unavailable, overflow)
		}
	}
}

func TestPromptPathPhysicalAliasAndDeniedRootDominate(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(external, "first.txt")
	alias := filepath.Join(external, "alias.txt")
	if err := os.WriteFile(first, []byte("same inode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)

	assertNoAuthority := func(draft string) {
		t.Helper()
		scan := scanExplicitPromptPaths(draft)
		reads, writes, unavailable, overflow := inspectPromptPathGrantIntents(ag, scan.Intents, true)
		defer releaseReadGrants(reads)
		defer releaseWriteGrants(writes)
		if overflow || len(reads) != 0 || len(writes) != 0 || len(unavailable) != 0 {
			t.Fatalf("denial did not dominate for %q: reads=%#v writes=%#v unavailable=%#v overflow=%v", draft, reads, writes, unavailable, overflow)
		}
	}
	assertNoAuthority("update " + first + " but do not update " + alias)
	assertNoAuthority("update " + alias + " but do not update " + first)
	assertNoAuthority("update " + first + " but do not access /")
	assertNoAuthority("read " + first + " but do not read /")
}

func TestPromptPathCanonicalAliasesApplyNeutralAndDenyDominance(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	child := filepath.Join(external, "child")
	for _, path := range []string{workspace, child} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", base)
	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)

	assertScopes := func(draft string, wantReads, wantWrites int) {
		t.Helper()
		scan := scanExplicitPromptPaths(draft)
		reads, writes, unavailable, overflow := inspectPromptPathGrantIntents(ag, scan.Intents, true)
		defer releaseReadGrants(reads)
		defer releaseWriteGrants(writes)
		if overflow || len(unavailable) != 0 || len(reads) != wantReads || len(writes) != wantWrites {
			t.Fatalf("scopes for %q: reads=%#v writes=%#v unavailable=%#v overflow=%v", draft, reads, writes, unavailable, overflow)
		}
	}
	assertScopes("update "+external+"/. but only review "+external, 1, 0)
	assertScopes("update "+external+", but do not update "+external, 0, 0)
	assertScopes("update ~/external but only review "+external, 1, 0)
	assertScopes("review "+external+" but do not read "+child, 0, 0)
	assertScopes("do not access ~. update ~/external", 0, 0)
	assertScopes("do not access //. update "+child, 0, 0)
}

func TestPromptPathTrailingConsentAppliesAcrossOnePathList(t *testing.T) {
	for _, draft := range []string{
		"update /external/a and /external/b, but ask me first",
		"update /external/a, /external/b, and /external/c after I approve",
	} {
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) < 2 {
			t.Fatalf("path list was not scanned for %q: %#v", draft, scan.Intents)
		}
		for _, intent := range scan.Intents {
			if intent.Mutation || !intent.Denied {
				t.Fatalf("withheld list consent leaked authority for %q: %#v", draft, scan.Intents)
			}
		}
	}

	// A later path with its own explicit action is independent; its correction
	// must not retroactively deny the earlier request.
	scan := scanExplicitPromptPaths("update /external/a. read /external/b only after I approve")
	if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || scan.Intents[0].Denied || !scan.Intents[1].Denied {
		t.Fatalf("independent path actions were merged: %#v", scan.Intents)
	}
}

func TestPromptPathAmbiguousRemoveDeleteStaysReadOnly(t *testing.T) {
	for _, draft := range []string{
		"remove /external/item from /external/destination",
		"delete references to /external/source from /external/destination",
		"elimina /external/elemento de /external/destino",
	} {
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) != 2 || scan.Intents[0].Mutation || scan.Intents[1].Mutation {
			t.Fatalf("ambiguous removal gained write authority for %q: %#v", draft, scan.Intents)
		}
	}
	single := scanExplicitPromptPaths("remove /external/item")
	if len(single.Intents) != 1 || !single.Intents[0].Mutation {
		t.Fatalf("single explicit removal lost bounded target authority: %#v", single.Intents)
	}
}

func TestPromptPathMutationIntentBindsEachPathIndependently(t *testing.T) {
	scan := scanExplicitPromptPaths("review /external/source and update /external/destination")
	if len(scan.Intents) != 2 || scan.Intents[0].Mutation || !scan.Intents[1].Mutation {
		t.Fatalf("independent intents = %#v", scan.Intents)
	}
}

func TestPromptPathLongConjunctiveWriteGrantsEveryDestination(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	first := filepath.Join(base, strings.Repeat("first-", 18))
	second := filepath.Join(base, strings.Repeat("second-", 18))
	for _, path := range []string{workspace, first, second} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	scan := scanExplicitPromptPaths("update " + first + " and " + second)
	if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || !scan.Intents[1].Mutation {
		t.Fatalf("long conjunctive destinations = %#v", scan.Intents)
	}
	ag := agent.New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	reads, writes, unavailable, overflow := inspectPromptPathGrantIntents(ag, scan.Intents, true)
	defer releaseReadGrants(reads)
	defer releaseWriteGrants(writes)
	if overflow || len(unavailable) != 0 || len(reads) != 2 || len(writes) != 2 {
		t.Fatalf("long conjunctive grants: reads=%d writes=%d unavailable=%#v overflow=%v", len(reads), len(writes), unavailable, overflow)
	}
}

func TestPromptPathMutationIntentDoesNotCrossIntoUnlabelledDataLines(t *testing.T) {
	for _, draft := range []string{
		"update /external/a\n/external/b",
		"update /external/a\nsource: /external/b",
		"update /external/a\nworkspace: /external/b",
		"update /external/a\ninput: /external/b",
	} {
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || scan.Intents[1].Mutation {
			t.Fatalf("write intent crossed a data line for %q: %#v", draft, scan.Intents)
		}
	}
	scan := scanExplicitPromptPaths("update /external/a\nread /external/b")
	if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || scan.Intents[1].Mutation {
		t.Fatalf("explicit read line inherited mutation: %#v", scan.Intents)
	}
}

func TestPromptPathMutationIntentDoesNotCrossSameLineDataLabels(t *testing.T) {
	for _, draft := range []string{
		"update /external/a, source: /external/b",
		"update /external/a source: /external/b",
		"update /external/a, workspace: /external/b",
		"update /external/a input: /external/b",
	} {
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) != 2 || !scan.Intents[0].Mutation || scan.Intents[1].Mutation {
			t.Fatalf("write intent crossed a same-line data label for %q: %#v", draft, scan.Intents)
		}
	}
}

func TestPromptPathIncompleteDelimiterRevokesEarlierAuthority(t *testing.T) {
	for _, draft := range []string{
		`update /external/a "but ask me first`,
		"update /external/a `but ask me first",
		"update /external/a\n```text\nbut ask me first",
		`read /external/a "but do not read it`,
	} {
		scan := scanExplicitPromptPaths(draft)
		if len(scan.Intents) != 1 || !scan.Intents[0].Denied || scan.Intents[0].Mutation {
			t.Fatalf("incomplete delimiter preserved authority for %q: %#v", draft, scan.Intents)
		}
	}
	control := scanExplicitPromptPaths(`update /external/a "example"`)
	if len(control.Intents) != 1 || control.Intents[0].Denied || !control.Intents[0].Mutation {
		t.Fatalf("closed delimiter revoked valid authority: %#v", control.Intents)
	}
}

func TestPromptPathMutationIntentRequiresDirectPriorConjunctionClause(t *testing.T) {
	for _, text := range []string{
		"the plan says inspect /external/a and update /external/b",
		"README says review /external/a then edit /external/b",
		"explain how to review /external/a and update /external/b",
		"do not review /external/a and update /external/b",
	} {
		scan := scanExplicitPromptPaths(text)
		if len(scan.Intents) != 2 || scan.Intents[1].Mutation {
			t.Fatalf("indirect conjunction gained write authority for %q: %#v", text, scan.Intents)
		}
	}
	positive := scanExplicitPromptPaths("review /external/a and update /external/b")
	if len(positive.Intents) != 2 || positive.Intents[0].Mutation || !positive.Intents[1].Mutation {
		t.Fatalf("direct conjunction lost bounded authority: %#v", positive.Intents)
	}
}

func TestPromptPathMutationIntentFailsClosedOnDuplicateCorrection(t *testing.T) {
	for _, text := range []string{
		"update /external/repo and do not update /external/repo",
		"update /external/repo then only review /external/repo",
	} {
		scan := scanExplicitPromptPaths(text)
		if len(scan.Intents) != 1 || scan.Intents[0].Mutation {
			t.Fatalf("conflicting duplicate gained write authority for %q: %#v", text, scan.Intents)
		}
	}
}

func TestPromptPathDuplicateOccurrenceBoundFailsClosed(t *testing.T) {
	parts := make([]string, 0, maxPromptPathScanIntents*2+1)
	for index := 0; index < maxPromptPathScanIntents*2; index++ {
		parts = append(parts, "update /external/repo")
	}
	parts = append(parts, "do not update /external/repo")
	scan := scanExplicitPromptPaths(strings.Join(parts, " and "))
	if !scan.MoreCandidates {
		t.Fatalf("duplicate occurrence overflow did not fail closed: %#v", scan)
	}
}

func TestAutoPromptCommitsTypedExternalWriteWithoutModalAndRevokesAtSettlement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external-repo")
	for _, path := range []string{workspace, external} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.mode = ModeAuto
	t.Cleanup(m.agent.Close)
	draft := "update " + external
	m.input.SetValue(draft)

	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if preflight.Authority != ModeAuto || len(preflight.Grants) != 1 || len(preflight.WriteGrants) != 1 || len(preflight.UnavailableWrites) != 0 {
		t.Fatalf("AUTO preflight = %#v", preflight)
	}
	updated, mutationCmd := m.Update(preflight)
	m = updated.(*Model)
	if mutationCmd == nil || m.readScopePrompt != nil || !m.readScopeOpRunning {
		t.Fatalf("AUTO opened a modal or skipped commit: cmd=%v prompt=%#v running=%v", mutationCmd != nil, m.readScopePrompt, m.readScopeOpRunning)
	}
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	updated, agentCmd := m.Update(receipt)
	m = updated.(*Model)
	if agentCmd == nil || m.state != StateWaiting || len(m.agent.WriteGrants()) != 1 || len(m.agent.ReadGrants()) != 1 {
		t.Fatalf("AUTO grant did not resume exactly once: cmd=%v state=%v reads=%#v writes=%#v", agentCmd != nil, m.state, m.agent.ReadGrants(), m.agent.WriteGrants())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "typed-write") || !strings.Contains(transcript, "shell remains confined") {
		t.Fatalf("AUTO grant receipt is not explicit: %s", transcript)
	}
	updated, _ = m.Update(AgentDoneMsg{TurnID: "auto-scope"})
	m = updated.(*Model)
	if len(m.agent.WriteGrants()) != 0 {
		t.Fatalf("write grants survived settled turn: %#v", m.agent.WriteGrants())
	}
	if len(m.agent.ReadGrants()) != 1 {
		t.Fatalf("process-local read grant did not survive settled turn: %#v", m.agent.ReadGrants())
	}
}

func TestPromptPathGrantReceiptRollsBackWhenResultIsStale(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.mode = ModeAuto
	t.Cleanup(m.agent.Close)
	m.input.SetValue("update " + external)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	updated, mutationCmd := m.Update(preflight)
	m = updated.(*Model)
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	if len(m.agent.ReadGrants()) != 1 || len(m.agent.WriteGrants()) != 1 {
		t.Fatalf("transaction did not apply before receipt: reads=%#v writes=%#v", m.agent.ReadGrants(), m.agent.WriteGrants())
	}
	m.readScopeOpToken++
	updated, agentCmd := m.Update(receipt)
	m = updated.(*Model)
	if agentCmd != nil || len(m.agent.ReadGrants()) != 0 || len(m.agent.WriteGrants()) != 0 {
		t.Fatalf("stale result leaked authority: cmd=%v reads=%#v writes=%#v", agentCmd != nil, m.agent.ReadGrants(), m.agent.WriteGrants())
	}
}

func TestPromptPathGrantReceiptRollsBackDuringShutdown(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.mode = ModeAuto
	t.Cleanup(m.agent.Close)
	m.input.SetValue("update " + external)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	updated, mutationCmd := m.Update(preflight)
	m = updated.(*Model)
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	if shutdownCmd := m.beginShutdown(); shutdownCmd == nil || !m.shuttingDown {
		t.Fatalf("shutdown did not wait for in-flight scope receipt: cmd=%v shutdown=%v", shutdownCmd != nil, m.shuttingDown)
	}
	updated, quitCmd := m.Update(receipt)
	m = updated.(*Model)
	if quitCmd == nil || !m.shutdownReady() || len(m.agent.ReadGrants()) != 0 || len(m.agent.WriteGrants()) != 0 {
		t.Fatalf("shutdown receipt did not roll back and quit: cmd=%v ready=%v reads=%#v writes=%#v", quitCmd != nil, m.shutdownReady(), m.agent.ReadGrants(), m.agent.WriteGrants())
	}
}

func TestCombinedPromptApprovalRejectsDirectoryReplacedWhileOpen(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	draft := "update " + external
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	updated, _ := m.Update(preflight)
	m = updated.(*Model)
	if m.readScopePrompt == nil || len(m.readScopePrompt.WriteGrants) != 1 {
		t.Fatalf("combined approval did not open: %#v", m.readScopePrompt)
	}
	if err := os.Rename(external, external+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	updated, mutationCmd := m.Update(charKey('y'))
	m = updated.(*Model)
	receipt := awaitCommandMessage[ReadScopeResultMsg](t, commandMessages(mutationCmd), 2*time.Second)
	updated, agentCmd := m.Update(receipt)
	m = updated.(*Model)
	if agentCmd != nil || m.input.Value() != draft || len(m.agent.ReadGrants()) != 0 || len(m.agent.WriteGrants()) != 0 {
		t.Fatalf("replaced directory inherited authority: cmd=%v draft=%q reads=%#v writes=%#v", agentCmd != nil, m.input.Value(), m.agent.ReadGrants(), m.agent.WriteGrants())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "changed after approval preview") {
		t.Fatalf("stale combined identity error is not actionable: %s", transcript)
	}
}

func TestNormalPromptShowsCombinedReadTypedWriteApproval(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	t.Cleanup(m.agent.Close)
	m.input.SetValue("update " + external)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	updated, cmd := m.Update(preflight)
	m = updated.(*Model)
	if cmd != nil || m.readScopePrompt == nil || len(m.readScopePrompt.WriteGrants) != 1 {
		t.Fatalf("NORMAL combined approval = cmd:%v prompt:%#v", cmd != nil, m.readScopePrompt)
	}
	plain := strings.ToLower(ansi.Strip(m.renderReadScopePrompt()))
	for _, want := range []string{"read + typed write", "shell", "exact scope", "write expires"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("combined approval missing %q:\n%s", want, plain)
		}
	}
}

func TestPlanPromptNeverDerivesExternalWriteAuthority(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.mode = ModePlan
	t.Cleanup(m.agent.Close)
	m.input.SetValue("update " + external)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if preflight.Authority != ModePlan || len(preflight.WriteGrants) != 0 || len(preflight.Grants) != 1 {
		t.Fatalf("PLAN preflight widened authority: %#v", preflight)
	}
}

func TestAutoPromptUnsafeExternalMutationFailsBeforeSending(t *testing.T) {
	m := newTestModel(t)
	m.agent.SetWorkDir(t.TempDir())
	m.mode = ModeAuto
	t.Cleanup(m.agent.Close)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	draft := "update " + missing
	m.input.SetValue(draft)
	preflight := awaitCommandMessage[PromptPathPreflightResultMsg](t, commandMessages(m.submitInput()), 2*time.Second)
	if len(preflight.UnavailableWrites) != 1 {
		t.Fatalf("unsafe mutation preflight = %#v", preflight)
	}
	updated, cmd := m.Update(preflight)
	m = updated.(*Model)
	if cmd != nil || m.input.Value() != draft || len(m.agent.Messages()) != 0 || len(m.agent.WriteGrants()) != 0 {
		t.Fatalf("unsafe mutation sent or leaked authority: cmd=%v input=%q messages=%#v writes=%#v", cmd != nil, m.input.Value(), m.agent.Messages(), m.agent.WriteGrants())
	}
	if transcript := entryText(m.entries); !strings.Contains(transcript, "Nothing was sent") || !strings.Contains(transcript, "shell fallback is not allowed") {
		t.Fatalf("unsafe mutation guidance missing: %s", transcript)
	}
}
