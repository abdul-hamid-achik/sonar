package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCredentialFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// The decisive property: a shared secrets file holds far more than provider
// keys. Applying PATH would corrupt every subprocess the agent launches, and
// applying EDITOR would redirect the user's editor. Only catalog-recognised
// credential names may take effect.
func TestCredentialFileAppliesOnlyProviderKeys(t *testing.T) {
	path := writeCredentialFile(t, strings.Join([]string{
		"# a real-world secrets file",
		"PATH=/malicious/bin",
		"EDITOR=/malicious/editor",
		"OBSIDIAN_VAULT_PATH=/somewhere",
		"TVAULT_PASSPHRASE=hunter2",
		"export DEEPSEEK_API_KEY=sk-test-deepseek",
		"GROQ_API_KEY='sk-test-groq'",
		"", // trailing blank line
	}, "\n"), 0o600)

	originalPath := os.Getenv("PATH")
	// The ambient environment may already define these; assert they are
	// UNCHANGED rather than empty, or the test reads the shell, not the loader.
	originalVault := os.Getenv("OBSIDIAN_VAULT_PATH")
	originalEditor := os.Getenv("EDITOR")
	originalPassphrase := os.Getenv("TVAULT_PASSPHRASE")
	t.Setenv("DEEPSEEK_API_KEY", "")
	_ = os.Unsetenv("DEEPSEEK_API_KEY")
	_ = os.Unsetenv("GROQ_API_KEY")
	t.Cleanup(func() {
		_ = os.Unsetenv("DEEPSEEK_API_KEY")
		_ = os.Unsetenv("GROQ_API_KEY")
	})

	applied, warning, err := LoadCredentialEnvFile(path)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if warning != "" {
		t.Errorf("owner-only file warned: %q", warning)
	}

	if os.Getenv("PATH") != originalPath {
		t.Fatal("the credential file overwrote PATH")
	}
	if os.Getenv("EDITOR") != originalEditor {
		t.Fatal("the credential file overwrote EDITOR")
	}
	if os.Getenv("TVAULT_PASSPHRASE") != originalPassphrase {
		t.Fatal("an unrelated secret was applied")
	}
	if os.Getenv("OBSIDIAN_VAULT_PATH") != originalVault {
		t.Fatal("an unrelated variable was applied")
	}

	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "sk-test-deepseek" {
		t.Errorf("DEEPSEEK_API_KEY = %q, want it loaded", got)
	}
	if got := os.Getenv("GROQ_API_KEY"); got != "sk-test-groq" {
		t.Errorf("single-quoted GROQ_API_KEY = %q, want unquoted", got)
	}
	for _, name := range applied {
		if name != "DEEPSEEK_API_KEY" && name != "GROQ_API_KEY" {
			t.Errorf("applied an unexpected variable %q", name)
		}
	}
}

// An explicit export is a deliberate act; the file must never override it.
func TestCredentialFileDoesNotOverrideAnExistingVariable(t *testing.T) {
	path := writeCredentialFile(t, "DEEPSEEK_API_KEY=from-file\n", 0o600)
	t.Setenv("DEEPSEEK_API_KEY", "from-shell")

	applied, _, err := LoadCredentialEnvFile(path)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "from-shell" {
		t.Errorf("DEEPSEEK_API_KEY = %q, want the shell value to win", got)
	}
	if len(applied) != 0 {
		t.Errorf("reported %v as applied when nothing changed", applied)
	}
}

// A missing file is the normal case for most users and must not be an error.
func TestCredentialFileMissingIsNotAnError(t *testing.T) {
	applied, warning, err := LoadCredentialEnvFile(filepath.Join(t.TempDir(), "absent"))
	if err != nil || warning != "" || len(applied) != 0 {
		t.Fatalf("missing file: applied=%v warning=%q err=%v", applied, warning, err)
	}
	if applied, _, err := LoadCredentialEnvFile(""); err != nil || len(applied) != 0 {
		t.Fatalf("empty path: applied=%v err=%v", applied, err)
	}
}

// A loose-permission file still loads — refusing would strand a user mid-task —
// but the caller must be told so it can be surfaced.
func TestCredentialFileWarnsWhenReadableByOthers(t *testing.T) {
	path := writeCredentialFile(t, "DEEPSEEK_API_KEY=sk-loose\n", 0o644)
	_ = os.Unsetenv("DEEPSEEK_API_KEY")
	t.Cleanup(func() { _ = os.Unsetenv("DEEPSEEK_API_KEY") })

	applied, warning, err := LoadCredentialEnvFile(path)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if warning == "" {
		t.Error("a world-readable credential file produced no warning")
	}
	if len(applied) != 1 {
		t.Errorf("applied = %v, want the key still loaded", applied)
	}
	// The warning names the file, never the secret.
	if strings.Contains(warning, "sk-loose") {
		t.Errorf("warning leaked a credential value: %q", warning)
	}
}

// extra covers a private endpoint whose api_key_env the catalog cannot know.
func TestCredentialFileAcceptsAnExtraName(t *testing.T) {
	path := writeCredentialFile(t, "MY_PRIVATE_GATEWAY_KEY=sk-private\n", 0o600)
	_ = os.Unsetenv("MY_PRIVATE_GATEWAY_KEY")
	t.Cleanup(func() { _ = os.Unsetenv("MY_PRIVATE_GATEWAY_KEY") })

	if applied, _, err := LoadCredentialEnvFile(path); err != nil || len(applied) != 0 {
		t.Fatalf("unlisted name loaded without being requested: %v (%v)", applied, err)
	}
	applied, _, err := LoadCredentialEnvFile(path, "MY_PRIVATE_GATEWAY_KEY")
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if len(applied) != 1 || os.Getenv("MY_PRIVATE_GATEWAY_KEY") != "sk-private" {
		t.Errorf("extra name not applied: %v", applied)
	}
}

func TestParseCredentialLine(t *testing.T) {
	for _, test := range []struct {
		line  string
		name  string
		value string
		ok    bool
	}{
		{`export FOO_API_KEY=bar`, "FOO_API_KEY", "bar", true},
		{`  FOO_API_KEY = "bar"  `, "FOO_API_KEY", "bar", true},
		{`FOO_API_KEY='ba=r'`, "FOO_API_KEY", "ba=r", true},
		{`# FOO_API_KEY=bar`, "", "", false},
		{``, "", "", false},
		{`FOO_API_KEY=`, "", "", false},
		{`=bar`, "", "", false},
		{`not an assignment`, "", "", false},
		{`1BAD=x`, "", "", false},
	} {
		name, value, ok := parseCredentialLine(test.line)
		if ok != test.ok || name != test.name || value != test.value {
			t.Errorf("parseCredentialLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				test.line, name, value, ok, test.name, test.value, test.ok)
		}
	}
}

func TestCredentialEnvNamesComeFromTheCatalog(t *testing.T) {
	names := CredentialEnvNames()
	for _, want := range []string{"DEEPSEEK_API_KEY", "ANTHROPIC_API_KEY", "GROQ_API_KEY", "XAI_API_KEY"} {
		if _, ok := names[want]; !ok {
			t.Errorf("%s is not a recognised credential name", want)
		}
	}
	for _, unwanted := range []string{"PATH", "EDITOR", "HOME", "TVAULT_PASSPHRASE"} {
		if _, ok := names[unwanted]; ok {
			t.Errorf("%s must never be loadable as a credential", unwanted)
		}
	}
}
