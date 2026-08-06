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
