package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// TestConfinementHoldsAgainstTheRealKernel is the only test in this package
// that proves anything. Every other assertion here is about the text of a
// profile, and a profile that reads correctly and enforces nothing is the
// exact failure this package exists to avoid: SBPL evaluates the LAST matching
// rule, so an ordering mistake produces a profile that loads, runs, and
// protects nothing, and it looks identical from the outside.
//
// So this one runs real commands through the real driver and checks what the
// kernel actually did.
func TestConfinementHoldsAgainstTheRealKernel(t *testing.T) {
	if !Available() {
		t.Skip("no confinement driver on this platform")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(workspace, "secrets")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(workspace, "app.txt"), "workspace content\n")
	write(filepath.Join(secret, "api.key"), "SECRET-VALUE\n")
	write(filepath.Join(outside, "host.txt"), "host content\n")

	policy := WorkspacePolicy(workspace, []string{secret}, []string{".env", "id_rsa"}, nil, false)

	run := func(t *testing.T, script string) (string, error) {
		t.Helper()
		command, err := Wrap(policy, "/bin/sh", []string{"-c", script})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		command.Dir = workspace
		output, err := command.CombinedOutput()
		return string(output), err
	}

	t.Run("workspace stays usable", func(t *testing.T) {
		// A sandbox that breaks ordinary work gets turned off, so this case is
		// as load-bearing as the denials below.
		output, err := run(t, "cat app.txt && echo written > new.txt && cat new.txt")
		if err != nil {
			t.Fatalf("confined workspace work failed: %v\n%s", err, output)
		}
		if !strings.Contains(output, "workspace content") || !strings.Contains(output, "written") {
			t.Fatalf("workspace read/write did not both succeed:\n%s", output)
		}
	})

	t.Run("secret is unreadable", func(t *testing.T) {
		output, _ := run(t, "cat secrets/api.key")
		if strings.Contains(output, "SECRET-VALUE") {
			t.Fatalf("the confined command read a denied secret:\n%s", output)
		}
	})

	t.Run("secret is unwritable", func(t *testing.T) {
		// The deny-write rules for secrets are emitted AFTER the workspace
		// re-allow precisely so a secret inside the workspace stays protected.
		// Without that ordering this passes reads and fails here.
		// The write is expected to fail; what matters is the file afterward.
		_, _ = run(t, "echo clobbered > secrets/api.key")
		content, err := os.ReadFile(filepath.Join(secret, "api.key"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "SECRET-VALUE") {
			t.Fatalf("a denied secret was overwritten: %q", string(content))
		}
	})

	t.Run("the default policy deliberately keeps TMPDIR writable", func(t *testing.T) {
		// Worth pinning rather than leaving implicit, because it is the widest
		// hole in the default policy and it is there on purpose: `go build`
		// creates its work directory under TMPDIR and fails outright without
		// it. It also means "outside the workspace" and "denied" are not the
		// same set — which is why the denial case below builds its own policy.
		escape := filepath.Join(outside, "tmp-write.txt")
		if _, err := run(t, "echo written > "+escape); err != nil {
			t.Fatalf("TMPDIR write failed, which breaks every toolchain: %v", err)
		}
		if _, err := os.Stat(escape); err != nil {
			t.Fatalf("TMPDIR is not writable under the default policy: %v", err)
		}
	})

	t.Run("writes outside every writable root are denied", func(t *testing.T) {
		// A policy with no cache allowances, so the temp directory below is
		// genuinely outside every permitted root and the denial is the
		// mechanism's, not an accident of where t.TempDir happens to sit.
		minimal := Policy{Workspace: workspace}.Normalize()
		command, err := Wrap(minimal, "/bin/sh", []string{"-c", "echo escaped > " + filepath.Join(outside, "escaped.txt")})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		command.Dir = workspace
		_ = command.Run()
		if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
			t.Fatal("the confined command wrote outside every writable root")
		}
	})

	t.Run("network is denied", func(t *testing.T) {
		// Resolution and connection are both blocked; asserting on the absence
		// of a successful body keeps this independent of which one fails first
		// and of whether the machine is online at all.
		output, err := run(t, "curl -sS -m 5 http://example.com")
		if err == nil && strings.Contains(strings.ToLower(output), "<html") {
			t.Fatalf("the confined command reached the network:\n%s", output)
		}
	})

	t.Run("network can be granted", func(t *testing.T) {
		granted := policy
		granted.AllowNetwork = true
		command, err := Wrap(granted, "/bin/sh", []string{"-c", "exit 0"})
		if err != nil {
			t.Fatalf("wrap with network: %v", err)
		}
		command.Dir = workspace
		if err := command.Run(); err != nil {
			t.Fatalf("a network-granted confinement failed to run at all: %v", err)
		}
	})
}

func TestPolicyNormalizationResolvesSymlinks(t *testing.T) {
	// macOS matches Seatbelt subpaths against the RESOLVED path, so an
	// unresolved rule matches nothing and the profile silently protects
	// nothing. /tmp -> /private/tmp is the case that actually bit during
	// development: `go build` failed on its work directory because the TMPDIR
	// rule named a symlink.
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	policy := Policy{Workspace: link, UnreadablePaths: []string{link}}.Normalize()
	if policy.Workspace == link {
		t.Fatalf("workspace kept its symlink spelling: %q", policy.Workspace)
	}
	if len(policy.UnreadablePaths) != 1 || policy.UnreadablePaths[0] == link {
		t.Fatalf("unreadable path kept its symlink spelling: %#v", policy.UnreadablePaths)
	}
}

func TestPolicyWithoutWorkspaceFailsClosed(t *testing.T) {
	if _, err := Wrap(Policy{}, "/bin/sh", []string{"-c", "true"}); err == nil {
		t.Fatal("a policy with no workspace produced a runnable command")
	}
}

func TestNormalizeIsIdempotentAndDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b", "a", "c"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	policy := Policy{
		Workspace: root,
		WritablePaths: []string{
			filepath.Join(root, "c"), filepath.Join(root, "a"),
			filepath.Join(root, "b"), filepath.Join(root, "a"),
		},
	}
	once := policy.Normalize()
	twice := once.Normalize()
	if strings.Join(once.WritablePaths, "\x00") != strings.Join(twice.WritablePaths, "\x00") {
		t.Fatalf("normalize is not idempotent:\n%#v\n%#v", once, twice)
	}
	if len(once.WritablePaths) != 3 {
		t.Fatalf("duplicates survived normalization: %#v", once.WritablePaths)
	}
	if !sortedAscending(once.WritablePaths) {
		t.Fatalf("normalized paths are not deterministic: %#v", once.WritablePaths)
	}
}

func sortedAscending(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] > values[index] {
			return false
		}
	}
	return true
}

// TestSecretComponentsAreDeniedAtAnyDepth covers the rule that does the real
// work: a secret is a NAME, not a location, and the catalog can only refuse the
// paths a MODEL types. A workspace script reading ./config/.env is invisible to
// argv inspection and must still fail.
func TestSecretComponentsAreDeniedAtAnyDepth(t *testing.T) {
	if !Available() {
		t.Skip("no confinement driver on this platform")
	}
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "config", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(workspace, ".env"):        "ROOT-SECRET\n",
		filepath.Join(nested, ".env"):           "NESTED-SECRET\n",
		filepath.Join(nested, "id_rsa"):         "KEY-SECRET\n",
		filepath.Join(nested, "id_rsa.pub"):     "PUB-SECRET\n",
		filepath.Join(workspace, "environment"): "NOT-A-SECRET\n",
		filepath.Join(nested, "credentials.md"): "NOT-A-SECRET\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	policy := WorkspacePolicy(workspace, nil, []string{".env", "id_rsa*", "credentials"}, nil, false)
	run := func(script string) string {
		t.Helper()
		command, err := Wrap(policy, "/bin/sh", []string{"-c", script})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		command.Dir = workspace
		output, _ := command.CombinedOutput()
		return string(output)
	}

	for name, script := range map[string]string{
		"workspace root":  "cat .env",
		"nested":          "cat config/deep/.env",
		"prefix match":    "cat config/deep/id_rsa",
		"prefix suffixed": "cat config/deep/id_rsa.pub",
	} {
		t.Run("denies/"+name, func(t *testing.T) {
			if strings.Contains(run(script), "SECRET") {
				t.Fatalf("a secret component was readable: %s", script)
			}
		})
	}

	// The anchoring is the other half. A component rule that also matched
	// substrings would deny ordinary files and read as a broken toolchain.
	for name, script := range map[string]string{
		"substring prefix": "cat environment",
		"substring suffix": "cat config/deep/credentials.md",
	} {
		t.Run("allows/"+name, func(t *testing.T) {
			if !strings.Contains(run(script), "NOT-A-SECRET") {
				t.Fatalf("component rule over-matched an ordinary file: %s", script)
			}
		})
	}
}

func TestUnexpressibleSecretComponentFailsClosed(t *testing.T) {
	if !Available() {
		t.Skip("no confinement driver on this platform")
	}
	_, err := Wrap(Policy{Workspace: t.TempDir(), UnreadableComponents: []string{"we*rd*"}}.Normalize(),
		"/bin/sh", []string{"-c", "true"})
	if err == nil {
		t.Fatal("an inexpressible secret pattern produced a runnable command")
	}
}

// TestHostSecretPolicyIsEnforcedByTheKernel is the join between the two layers.
//
// internal/config owns one list of secret component globs. The path checks in
// internal/agent evaluate it, and this proves the kernel refuses the same names
// for a subprocess those checks never see — a workspace script opening
// ./config/.env, which no argv inspection can catch. If the two ever disagree,
// this fails rather than the disagreement being discovered in a session.
func TestHostSecretPolicyIsEnforcedByTheKernel(t *testing.T) {
	if !Available() {
		t.Skip("no confinement driver on this platform")
	}
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "svc", "conf")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory carrying a public template name must still hide what is
	// under it — the exception is a leaf rule, not a subtree rule.
	templateDir := filepath.Join(workspace, ".env.example")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		filepath.Join(workspace, ".env"):         "SECRET\n",
		filepath.Join(nested, ".env"):            "SECRET\n",
		filepath.Join(nested, ".env.production"): "SECRET\n",
		filepath.Join(nested, "server.pem"):      "SECRET\n",
		filepath.Join(nested, "tls.key"):         "SECRET\n",
		filepath.Join(nested, "id_ed25519"):      "SECRET\n",
		filepath.Join(nested, "credentials"):     "SECRET\n",
		filepath.Join(nested, ".npmrc"):          "SECRET\n",
		filepath.Join(templateDir, "inner.txt"):  "SECRET\n",
	}
	readable := map[string]string{
		// The template leaf exception, and two names that only LOOK like
		// secrets. `environment` is the anchoring case: a component rule that
		// matched substrings would deny it and read as a broken toolchain.
		filepath.Join(nested, ".env.sample"): "PUBLIC\n",
		filepath.Join(nested, "environment"): "PUBLIC\n",
		filepath.Join(nested, "main.go"):     "PUBLIC\n",
	}
	// `.env.example.txt` matches `.env.*` and is NOT an exact template name, so
	// both layers deny it. Asserting that here keeps the exception narrow: it
	// is a leaf-NAME rule, not a leaf-PREFIX one.
	secrets[filepath.Join(nested, ".env.example.txt")] = "SECRET\n"
	for path, content := range secrets {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range readable {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	policy := WorkspacePolicy(workspace, nil,
		config.HostSecretComponents(), config.HostSecretPublicLeaves(), false)

	read := func(path string) string {
		t.Helper()
		command, err := Wrap(policy, "/bin/sh", []string{"-c", "cat " + path})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		command.Dir = workspace
		output, _ := command.CombinedOutput()
		return string(output)
	}

	for path := range secrets {
		t.Run("denies/"+filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			if strings.Contains(read(path), "SECRET") {
				t.Fatalf("the kernel allowed a read the host secret policy denies: %s", path)
			}
			// The catalog and the kernel must agree, or one of them is wrong.
			if !config.HostSecretPathIgnored(path) {
				t.Fatalf("path checks admit what the sandbox denies: %s", path)
			}
		})
	}
	for path := range readable {
		t.Run("allows/"+filepath.Base(path), func(t *testing.T) {
			if !strings.Contains(read(path), "PUBLIC") {
				t.Fatalf("the sandbox denied a file the host secret policy admits: %s", path)
			}
			if config.HostSecretPathIgnored(path) {
				t.Fatalf("path checks deny what the sandbox admits: %s", path)
			}
		})
	}
}
