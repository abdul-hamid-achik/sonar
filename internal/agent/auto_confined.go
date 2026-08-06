package agent

import "strings"

// Confined admission: what the catalog may stop asking about once the kernel
// is enforcing the boundary instead of argv.
//
// The scoped-shell catalog answers one question — is this command line, read as
// text, provably confined to the workspace — and every refusal it issues is a
// consequence of that question being hard. `xcrun`, `node -e`, an uncatalogued
// binary, a search that walks a directory: each is refused not because the
// operation is dangerous but because argv cannot prove it is not.
//
// With sandbox confinement active, the three things those refusals were
// protecting are enforced by the operating system for EVERY command, catalogued
// or not: reads of the host secret paths are denied, writes outside the
// workspace and the toolchain caches are denied, and the network is denied.
// A refusal that exists only to establish one of those has nothing left to add.
//
// # What confinement does not cover, and therefore still asks
//
// The sandbox protects everything outside the workspace from the command. It
// cannot protect the workspace from itself: the workspace is writable by
// construction, so a confined `rm -rf .` still destroys uncommitted work, and
// `git push --force` still rewrites published history through a network the
// operator may have granted. Those are decisions a human owns, and confinement
// changes nothing about them.
//
// So this widening is deliberately not "allow everything". It is: stop asking
// about the class of refusal whose entire justification the kernel has taken
// over, and keep asking about the class it never touched.
//
// # Why this is not the catalog with a flag
//
// The catalog stays exactly as it was, and is consulted first. Confinement adds
// a second chance for commands it already refused — it never overrides an
// admission, never widens a path check, and never relaxes composition. A
// command that the catalog admits under NORMAL authority is admitted here for
// the same reasons; a command this function admits is one the catalog refused
// for a reason confinement has made moot.

// autoConfinedDestructiveVerbs are the operations a sandbox does not make safe.
//
// Every one of them acts INSIDE the workspace or on published state, which is
// exactly the region confinement leaves writable. They are matched on the
// segment's leading word, and the list is short on purpose: it is not trying to
// enumerate danger, only to name the operations whose damage the kernel
// boundary provably does not contain.
var autoConfinedDestructiveVerbs = map[string]struct{}{
	"rm": {}, "rmdir": {}, "shred": {}, "srm": {}, "unlink": {},
	"mv": {}, "dd": {}, "mkfs": {}, "fdisk": {}, "parted": {}, "diskutil": {},
	"chmod": {}, "chown": {}, "chgrp": {}, "kill": {}, "killall": {}, "pkill": {},
	"sudo": {}, "su": {}, "doas": {},
}

// autoConfinedRefusalIsMoot reports whether a catalog refusal exists only to
// establish something the sandbox now enforces.
//
// Composition and path refusals stay refusals. Dynamic shell syntax is not a
// containment question — it is a question about whether the host and the shell
// are reading the same command at all, and confinement does not make two
// different parses agree. A redirect target outside the workspace is already
// denied by the kernel, but admitting it here would let the model believe a
// write succeeded when the sandbox silently refused it, which is worse than an
// approval prompt.
func autoConfinedRefusalIsMoot(reason autoCommandReason) bool {
	switch reason {
	case autoCommandReasonExecutable,
		autoCommandReasonExecutableUncatalogued,
		autoCommandReasonArguments,
		autoCommandReasonHostToolAvailable:
		return true
	default:
		return false
	}
}

// assessConfinedCommand re-assesses a catalog-refused command under active
// confinement.
//
// It runs only after assessAutoScopedCommand has refused, and only when the
// sandbox is actually applied to this host's shell — not when it is merely
// configured. A posture the platform cannot honor must never widen anything,
// which is why SandboxActive() asks both questions.
func (a *Agent) assessConfinedCommand(command string) autoCommandAssessment {
	assessment := a.assessAutoScopedCommand(command)
	if assessment.admitted() || !a.SandboxActive() {
		return assessment
	}
	if !autoConfinedRefusalIsMoot(assessment.reason) {
		return assessment
	}
	commands, _, _, ok := splitStaticShellCommands(command)
	if !ok || len(commands) == 0 {
		return assessment
	}
	// Every segment is checked, not only the one that refused. A compound whose
	// grep is moot and whose `rm -rf` is not must still ask, and asking about
	// the whole command is the only answer that is true of all of it.
	allowNetwork := a.SandboxPosture().AllowNetwork
	for _, words := range commands {
		for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		if !autoConfinedSegmentAdmissible(words) {
			return assessment
		}
		// A command whose only path to success is a resource the policy denies
		// must not be widened. The premise here is "the kernel makes this
		// safe"; for `npm install` under a network-denying sandbox the kernel
		// does not make it safe, it makes it fail — and an unattended,
		// guaranteed failure is worse than the approval it replaced, because
		// the human who would have approved it never sees it.
		if !allowNetwork && autoConfinedNeedsNetwork(words) {
			return assessment
		}
	}
	assessment.disposition = autoCommandAdmitted
	assessment.reason = autoCommandReasonAllowed
	// The effect is workspace execution: confinement bounds where a command can
	// reach, not what it does inside those bounds, so this is the widest class
	// the catalog itself assigns rather than a promotion to read-only.
	assessment.effect = autoCommandEffectWorkspaceExecution
	assessment.refusedSegment = -1
	return assessment
}

// autoConfinedSegmentAdmissible reports whether one segment is safe to admit on
// confinement alone.
func autoConfinedSegmentAdmissible(words []string) bool {
	executable := words[0]
	// A path-qualified spelling is refused for the same reason the catalog
	// refuses it: PATH resolution is the host's provenance check, and a
	// workspace-planted binary must not gain authority because the kernel
	// happens to be watching where it writes.
	if strings.ContainsAny(executable, "/\\") {
		return false
	}
	if _, destructive := autoConfinedDestructiveVerbs[executable]; destructive {
		return false
	}
	if executable == "git" {
		// git is the one catalogued verb family whose damage is published
		// rather than local: a force push rewrites history that other clones
		// have, and no filesystem boundary reaches it. The catalog's own
		// read-only allowlist already decided which git forms are safe, so
		// confinement adds nothing here and defers to it.
		return false
	}
	return true
}

// autoConfinedNetworkSubcommands names the first argument that turns a local
// tool into a fetching one. A package manager reads a manifest offline and
// installs from a registry online, and only the second form is a certain
// failure under a network-denying sandbox.
var autoConfinedNetworkSubcommands = map[string]map[string]struct{}{
	"npm":      {"install": {}, "i": {}, "ci": {}, "add": {}, "update": {}, "audit": {}, "publish": {}},
	"pnpm":     {"install": {}, "i": {}, "add": {}, "update": {}, "fetch": {}, "publish": {}},
	"yarn":     {"install": {}, "add": {}, "upgrade": {}, "publish": {}},
	"bun":      {"install": {}, "add": {}, "update": {}, "publish": {}},
	"pip":      {"install": {}, "download": {}},
	"pip3":     {"install": {}, "download": {}},
	"cargo":    {"fetch": {}, "install": {}, "update": {}, "publish": {}},
	"gem":      {"install": {}, "update": {}, "fetch": {}},
	"composer": {"install": {}, "update": {}, "require": {}},
	"brew":     {"install": {}, "update": {}, "upgrade": {}, "fetch": {}},
	"apt":      {"install": {}, "update": {}, "upgrade": {}},
	"apt-get":  {"install": {}, "update": {}, "upgrade": {}},
}

// autoConfinedNetworkExecutables reach the network in every form they have, so
// no subcommand distinguishes them.
var autoConfinedNetworkExecutables = map[string]struct{}{
	"curl": {}, "wget": {}, "ssh": {}, "scp": {}, "sftp": {}, "rsync": {},
	"ping": {}, "nc": {}, "netcat": {}, "telnet": {}, "ftp": {}, "gh": {}, "glab": {},
}

// autoConfinedNeedsNetwork reports whether a segment obviously cannot succeed
// without the network.
//
// It is a certainty test, not a safety one, and the difference decides what
// belongs in it. `go build` may fetch a missing module and usually does not;
// admitting it and letting a cold cache fail once is a better trade than
// prompting on every build. `npm install` has no offline meaning at all. Only
// commands in the second category are here, which is why the list can stay
// short and does not need to be complete: an omission costs one confusing
// failure, and a false entry costs an approval on work that would have run.
func autoConfinedNeedsNetwork(words []string) bool {
	executable := words[0]
	if _, always := autoConfinedNetworkExecutables[executable]; always {
		return true
	}
	if autoConfinedGoNeedsNetwork(words) {
		return true
	}
	subcommands, known := autoConfinedNetworkSubcommands[executable]
	if !known || len(words) < 2 {
		return false
	}
	// `npm run <script>` is the manifest's own code and may do anything; it is
	// judged by the script-name rules, not here.
	_, fetching := subcommands[words[1]]
	return fetching
}

// autoConfinedGoNeedsNetwork covers the `go mod` forms that fetch. They are
// spelled as three words, so they are checked apart from the table above.
//
// In practice only the widening path reaches this: `go mod tidy` is already
// catalogued and admitted before confinement is consulted, so a cold module
// cache can still fail it under a network-denying sandbox. That predates all
// of this and belongs to the catalog's own admission, not here.
func autoConfinedGoNeedsNetwork(words []string) bool {
	return len(words) >= 3 && words[0] == "go" && words[1] == "mod" &&
		(words[2] == "download" || words[2] == "tidy")
}
