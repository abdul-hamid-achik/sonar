package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveBashPrefix(t *testing.T) {
	tests := []struct {
		command string
		want    string
		ok      bool
	}{
		{command: "go test ./...", want: "go test", ok: true},
		{command: "npm run build", want: "npm run", ok: true},
		{command: "ls -la", want: "ls", ok: true},
		{command: "true", want: "true", ok: true},
		// Static composition derives from a segment. The grant carries
		// executable authority only — whole-command matching still refuses
		// control-bearing commands, and segment matching happens only inside a
		// composition the host validated — so the text outside the derived
		// fields (including every other segment) gains nothing from the grant.
		{command: "go test ./... && rm -rf /", want: "go test", ok: true},
		{command: "swift test 2>&1 | xcbeautify", want: "swift", ok: true},
		// node is not a multi-word runner, so the compound form derives the
		// same single-field prefix its simple form always has.
		{command: "node scripts/check.js && go test ./...", want: "node", ok: true},
		{command: "go build ./... 2>/dev/null; go vet ./...", want: "go build", ok: true},
		{command: "go test&&rm -rf /", want: "go test", ok: true},
		// The derivation segment is the first NON-TRIVIAL one: offering "echo"
		// or "cd" for these — the audited session's dominant shapes — kept the
		// always press a placebo for the command that actually prompted.
		{command: `echo "=== TODO ==="; grep -rn TODO packages | head -5`, want: "grep", ok: true},
		{command: "cd native/ios && xcodebuild test -scheme Core", want: "xcodebuild", ok: true},
		{command: "xcrun simctl list devices available 2>/dev/null | grep -i iphone | head -4", want: "xcrun", ok: true},
		{command: "echo hi && echo bye", want: "echo", ok: true},
		// The inert status parameter is fixed by POSIX to a decimal integer —
		// the same rule the host scanner applies — so the ubiquitous
		// `; echo "EXIT=$?"` tail does not forfeit derivation.
		{command: `bun run lint > /tmp/vn-lint.log 2>&1; echo "LINT_EXIT=$?" >> /tmp/vn-lint.log`, want: "bun run", ok: true},
		// Dynamic content stays non-derivable, and a segment whose leading
		// word is not a plain bare token fails the whole derivation: sh would
		// not parse it the way a naive field split does.
		{command: "echo $HOME", want: "", ok: false},
		{command: "echo `date` && go test ./...", want: "", ok: false},
		{command: `"go" test && rm -rf /`, want: "", ok: false},
		{command: "for p in a b; do echo $p; done", want: "", ok: false},
		{command: "", want: "", ok: false},
	}
	for _, tt := range tests {
		got, ok := DeriveBashPrefix(tt.command)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("DeriveBashPrefix(%q) = %q, %v; want %q, %v", tt.command, got, ok, tt.want, tt.ok)
		}
	}
}

func TestBashPrefixMatches(t *testing.T) {
	if !BashPrefixMatches("go test ./internal/agent", "go test") {
		t.Fatal("expected prefix match")
	}
	if !BashPrefixMatches("go test", "go test") {
		t.Fatal("expected exact match")
	}
	if BashPrefixMatches("gotest ./x", "go test") {
		t.Fatal("should require word boundary via space")
	}
	if BashPrefixMatches("go test ./x && true", "go test") {
		t.Fatal("compound commands must not match")
	}
}

func TestBashPatternMatchesTrailingGlob(t *testing.T) {
	if !BashPatternMatches("git status", "git status *") {
		t.Fatal("exact head should match trailing glob")
	}
	if !BashPatternMatches("git status -sb", "git status *") {
		t.Fatal("args should match trailing glob")
	}
	if BashPatternMatches("git log", "git status *") {
		t.Fatal("different subcommand must not match")
	}
	if BashPatternMatches("git status && rm -rf /", "git status *") {
		t.Fatal("compound must not match")
	}
	if !BashPatternMatches("go test ./...", "go test") {
		t.Fatal("literal prefix via pattern matcher")
	}
	if _, ok := NormalizeBashPattern("*"); ok {
		t.Fatal("bare * rejected")
	}
	if _, ok := NormalizeBashPattern("* status"); ok {
		t.Fatal("leading * rejected")
	}
	if _, ok := NormalizeBashPattern("git * status"); ok {
		t.Fatal("mid * rejected")
	}
	if got, ok := NormalizeBashPattern("git status *"); !ok || got != "git status *" {
		t.Fatalf("normalize = %q, %v", got, ok)
	}
}

func TestBashSegmentPatternMatches(t *testing.T) {
	if !BashSegmentPatternMatches([]string{"go", "test", "./..."}, "go test") {
		t.Fatal("segment with extra args should match a literal prefix")
	}
	if !BashSegmentPatternMatches([]string{"go", "test"}, "go test") {
		t.Fatal("exact segment should match a literal prefix")
	}
	if !BashSegmentPatternMatches([]string{"go", "test"}, "go test *") {
		t.Fatal("trailing glob should match its exact head")
	}
	if !BashSegmentPatternMatches([]string{"node", "scripts/check.js", "--verbose"}, "node scripts/check.js") {
		t.Fatal("two-field prefix should match with a variant tail")
	}
	if !BashSegmentPatternMatches([]string{"gofmt", "-l"}, "gofm*") {
		t.Fatal("glued glob should match by word prefix")
	}
	if BashSegmentPatternMatches([]string{"go"}, "go test") {
		t.Fatal("segment shorter than the pattern head must not match")
	}
	if BashSegmentPatternMatches([]string{"go", "test extra"}, "go test") {
		t.Fatal("one argv word containing a space must not satisfy two pattern fields")
	}
	if BashSegmentPatternMatches([]string{"gotest"}, "go") {
		t.Fatal("field matching must keep the word boundary")
	}
	if BashSegmentPatternMatches(nil, "go") {
		t.Fatal("empty segment must not match")
	}
	if BashSegmentPatternMatches([]string{"go", "test"}, "*") {
		t.Fatal("bare glob must stay rejected")
	}
}

func TestNormalizeMCPToolName(t *testing.T) {
	if name, ok := NormalizeMCPToolName("mcphub__mcphub_list_servers"); !ok || name != "mcphub__mcphub_list_servers" {
		t.Fatalf("got %q, %v", name, ok)
	}
	if _, ok := NormalizeMCPToolName("mcphub_list_servers"); ok {
		t.Fatal("bare names must fail")
	}
}

func TestNormalizeWritePathAndMatch(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "src")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, ok := NormalizeWritePath(ws, target)
	if !ok || rel != "src/main.go" {
		t.Fatalf("rel = %q, %v", rel, ok)
	}
	if !WritePathMatches(ws, target, "src/main.go") {
		t.Fatal("expected path match")
	}
	if WritePathMatches(ws, filepath.Join(ws, "other.go"), "src/main.go") {
		t.Fatal("different path must not match")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if _, ok := NormalizeWritePath(ws, outside); ok {
		t.Fatal("outside workspace rejected")
	}
}

func TestDeriveBashPrefixFromSegment(t *testing.T) {
	cases := []struct {
		name   string
		words  []string
		prefix string
		ok     bool
	}{
		{name: "executable", words: []string{"grep", "-rn", "TODO", "src"}, prefix: "grep", ok: true},
		{name: "runner keeps its subcommand", words: []string{"go", "test", "./..."}, prefix: "go test", ok: true},
		// Quoting is resolved by the time a segment reaches here, so an argv
		// word may hold a space. A bare runner would be a wider grant than the
		// segment needs, and the field-wise matcher could never confirm the
		// two-field form, so the derivation refuses instead of widening.
		{name: "runner without a derivable subcommand", words: []string{"go", "test extra"}, ok: false},
		{name: "runner alone", words: []string{"git"}, ok: false},
		{name: "leading word with whitespace", words: []string{"my prog", "-v"}, ok: false},
		{name: "leading word with a marker", words: []string{"gr$ep", "-n"}, ok: false},
		{name: "empty", words: nil, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix, ok := DeriveBashPrefixFromSegment(tc.words)
			if ok != tc.ok || prefix != tc.prefix {
				t.Fatalf("got (%q, %v), want (%q, %v)", prefix, ok, tc.prefix, tc.ok)
			}
			if !ok {
				return
			}
			// Whatever is derived here must be matched by the matcher that will
			// re-assess the same segment, or the grant is another placebo.
			if !BashSegmentPatternMatches(tc.words, prefix) {
				t.Fatalf("derived prefix %q does not match its own segment %q", prefix, tc.words)
			}
		})
	}
}

func TestApprovalBashPrefixPrefersTheHostNamedSegment(t *testing.T) {
	compound := "cd /w && sed -n 1,5p a.go && echo x && grep -n TODO a.go"

	// No host-named segment: unchanged whole-command derivation.
	prefix, ok := ApprovalBashPrefix(ApprovalPreview{Command: compound}, "")
	if !ok || prefix != "sed" {
		t.Fatalf("fallback derivation changed: %q %v", prefix, ok)
	}

	// Host named one: it wins.
	prefix, ok = ApprovalBashPrefix(ApprovalPreview{Command: compound, CommandPrefix: "grep"}, "")
	if !ok || prefix != "grep" {
		t.Fatalf("host-named prefix ignored: %q %v", prefix, ok)
	}

	// A malformed host value can never become a grant; the fallback answers.
	prefix, ok = ApprovalBashPrefix(ApprovalPreview{Command: compound, CommandPrefix: "grep | rm"}, "")
	if !ok || prefix != "sed" {
		t.Fatalf("malformed host prefix was not rejected: %q %v", prefix, ok)
	}

	// The command may live only on the request when no preview carries it.
	prefix, ok = ApprovalBashPrefix(ApprovalPreview{}, "go test ./...")
	if !ok || prefix != "go test" {
		t.Fatalf("fallback command was not consulted: %q %v", prefix, ok)
	}
}
