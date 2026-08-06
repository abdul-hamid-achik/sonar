package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/sandbox"
)

func confinedAgent(t *testing.T, enabled bool) (*Agent, string) {
	t.Helper()
	workspace := t.TempDir()
	for name, content := range map[string]string{
		"app.go": "package app\n",
		".env":   "API_KEY=SECRET\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetSandboxPosture(SandboxPosture{Enabled: enabled})
	return ag, workspace
}

// TestConfinementWidensOnlyTheRefusalsItMakesMoot is the payoff for building
// the sandbox: a refusal that existed only to prove containment from argv has
// nothing left to prove once the kernel is proving it for every command.
func TestConfinementWidensOnlyTheRefusalsItMakesMoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	if !sandbox.Available() {
		t.Skip("no confinement driver on this platform")
	}
	installGrantTestExecutables(t, "xcrun", "grep", "node", "rm", "curl")
	confined, workspace := confinedAgent(t, true)
	unconfined, _ := confinedAgent(t, false)

	// Refused for want of a containment proof, and confined either way.
	widened := []string{
		"xcrun --version",                    // installed but uncatalogued
		"grep -rn TODO .",                    // a walk the ignore policy cannot see
		"node -e 'console.log(1)'",           // inline interpreter code
		"grep -rn TODO . && xcrun --version", // every segment moot
	}
	for _, command := range widened {
		t.Run("widens/"+command, func(t *testing.T) {
			if unconfined.assessConfinedCommand(command).admitted() {
				t.Fatalf("precondition: this command is admitted without confinement")
			}
			if !confined.assessConfinedCommand(command).admitted() {
				t.Fatalf("confinement did not widen a refusal it makes moot")
			}
		})
	}

	// The sandbox leaves the workspace writable and cannot reach published
	// state, so these keep asking. `curl` is not here: it is refused for a
	// different reason entirely — it cannot succeed under a network-denying
	// policy — and TestConfinementDoesNotWidenIntoACertainFailure owns that
	// case, including the part where granting the network widens it.
	stillAsks := []string{
		"rm -rf .",                       // destroys uncommitted work inside the box
		"git push --force",               // rewrites history no filesystem bounds
		"grep -rn TODO . && rm -rf dist", // one moot segment does not carry the other
		"./evil payload",                 // provenance is the host's, not the kernel's
		"echo $(cat .env)",               // dynamic syntax: two parsers, not containment
		"sed -n 1,5p " + filepath.Join(t.TempDir(), "outside.txt"), // path authority
	}
	for _, command := range stillAsks {
		t.Run("still-asks/"+command, func(t *testing.T) {
			if confined.assessConfinedCommand(command).admitted() {
				t.Fatalf("confinement admitted a command it does not make safe")
			}
		})
	}

	// Confinement never changes an admission the catalog already made, and
	// never changes what the catalog decides about paths.
	if !confined.assessConfinedCommand("cat app.go").admitted() {
		t.Fatal("confinement refused a command the catalog admits")
	}
	if confined.assessConfinedCommand("cat .env").admitted() {
		t.Fatal("confinement widened a path the ignore policy denies")
	}
	_ = workspace
}

// A posture the platform cannot honor must widen nothing. Enabled is a request;
// SandboxActive is the answer, and only the answer may relax a refusal.
func TestConfinementNeverWidensWithoutAnActiveSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "xcrun")
	ag, _ := confinedAgent(t, false)
	if ag.SandboxActive() {
		t.Fatal("precondition: this agent should not be confined")
	}
	if ag.assessConfinedCommand("xcrun --version").admitted() {
		t.Fatal("an unconfined agent widened a catalog refusal")
	}
}

// The reason a user reads has to be the reason the host acted on. Explaining a
// prompt confinement had already prevented would describe a decision nobody
// made.
func TestConfinedApprovalReasonMatchesTheDecision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	if !sandbox.Available() {
		t.Skip("no confinement driver on this platform")
	}
	installGrantTestExecutables(t, "xcrun", "rm")
	confined, _ := confinedAgent(t, true)
	if reason := confined.autoCommandApprovalReason(AuthorityAutoScoped, "xcrun --version"); reason != "" {
		t.Fatalf("a widened command still carries a refusal reason: %q", reason)
	}
	if reason := confined.autoCommandApprovalReason(AuthorityAutoScoped, "rm -rf ."); reason == "" {
		t.Fatal("a command that still asks carries no reason")
	}
}

// TestConfinementDoesNotWidenIntoACertainFailure covers a regression the
// widening itself introduced.
//
// Before it, `npm install` prompted: a human saw it, approved, and it worked.
// After, it was admitted — because "arguments outside the catalog" is a
// containment refusal the kernel had made moot — and then failed against a
// network the same sandbox denies. That trades a useful question for an
// unattended, guaranteed failure the person who could have answered never sees.
//
// The premise of widening is that the kernel makes a command safe. For a
// fetching command under a network-denying policy it does not make it safe, it
// makes it fail.
func TestConfinementDoesNotWidenIntoACertainFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	if !sandbox.Available() {
		t.Skip("no confinement driver on this platform")
	}
	installGrantTestExecutables(t, "npm", "pip", "curl", "go", "xcrun")
	denied, workspace := confinedAgent(t, true)
	if err := os.WriteFile(filepath.Join(workspace, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	granted, _ := confinedAgent(t, true)
	granted.SetSandboxPosture(SandboxPosture{Enabled: true, AllowNetwork: true})

	fetching := []string{
		"npm install", "npm ci", "npm i", "pip install requests",
		"go mod download", "curl https://example.com",
		// One fetching segment is enough to keep the whole command asking.
		"xcrun --version && npm install",
	}
	for _, command := range fetching {
		t.Run("asks/"+command, func(t *testing.T) {
			if denied.assessConfinedCommand(command).admitted() {
				t.Fatal("a command that cannot succeed without the network ran unattended")
			}
			// Granting the network restores the widening: the failure was the
			// reason to ask, and it is gone.
			if !granted.assessConfinedCommand(command).admitted() {
				t.Fatal("a network-granted confinement still refused a fetching command")
			}
		})
	}

	// The offline forms of the same tools are unaffected. This is a certainty
	// test, not a safety one: `go build` may fetch a missing module and usually
	// does not, so one cold-cache failure is a better trade than prompting on
	// every build.
	for _, command := range []string{"npm test", "npm run lint", "go build ./...", "xcrun --version"} {
		t.Run("still-widens/"+command, func(t *testing.T) {
			if !denied.assessConfinedCommand(command).admitted() {
				t.Fatal("an offline command was refused for needing the network")
			}
		})
	}
}
