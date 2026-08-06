package agent

import (
	"os/exec"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/sandbox"
)

// SandboxPosture is the process-local confinement setting for shell
// subprocesses. It is deliberately separate from AuthorityMode: authority
// decides whether a command may START, confinement decides what it can TOUCH
// once it has, and conflating them would make widening one silently widen the
// other.
type SandboxPosture struct {
	// Enabled confines every shell subprocess.
	Enabled bool
	// AllowNetwork lifts the network denial for confined commands.
	AllowNetwork bool
}

// SetSandboxPosture installs the confinement setting for subsequent commands.
func (a *Agent) SetSandboxPosture(posture SandboxPosture) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.sandboxPosture = posture
	a.mu.Unlock()
}

// SandboxPosture returns the confinement setting.
func (a *Agent) SandboxPosture() SandboxPosture {
	if a == nil {
		return SandboxPosture{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sandboxPosture
}

// SandboxActive reports whether confinement is both requested and available.
//
// The two are reported as one answer because every caller needs the same
// conjunction, and because the alternative — a caller that checks Enabled
// alone — would describe a boundary the platform is not enforcing.
func (a *Agent) SandboxActive() bool {
	return a.SandboxPosture().Enabled && sandbox.Available()
}

// sandboxPolicy derives the confinement for one command from the host's own
// policy rather than from a second list.
//
// The secret components come from internal/config, which is the same list the
// workspace path checks evaluate; the workspace comes from the same
// activeWorkDir the path checks resolve against. Deriving both here is what
// makes "the sandbox and the catalog agree" a property of construction rather
// than of two lists staying in step.
func (a *Agent) sandboxPolicy() sandbox.Policy {
	posture := a.SandboxPosture()
	return sandbox.WorkspacePolicy(
		a.activeWorkDir(),
		nil,
		config.HostSecretComponents(),
		config.HostSecretPublicLeaves(),
		posture.AllowNetwork,
	)
}

// confineShellCommand confines a prepared shell command in place when
// confinement is active, and leaves it untouched when it is not.
//
// A failure is returned rather than swallowed: running unconfined after the
// operator asked for confinement is a decision, and it is not one this
// function makes silently.
func (a *Agent) confineShellCommand(command *exec.Cmd) error {
	if command == nil || !a.SandboxActive() {
		return nil
	}
	return sandbox.Apply(a.sandboxPolicy(), command)
}
