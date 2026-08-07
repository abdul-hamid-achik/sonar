package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
)

// seatbeltLogTag appears on every deny rule. macOS writes it to the system
// sandbox violation log, so an operator asking "what did the sandbox stop"
// has one string to search for instead of a guess.
const seatbeltLogTag = "sonar-sandbox"

// sandboxExecPath is the macOS confinement driver.
//
// sandbox-exec has carried a deprecation notice since 10.14 and still works on
// every macOS since. Naming it in one place is deliberate: when Apple does
// remove it, Available() is the single thing that has to start answering
// false, and every caller already handles that.
const sandboxExecPath = "/usr/bin/sandbox-exec"

func Available() bool {
	info, err := exec.LookPath(sandboxExecPath)
	return err == nil && info != ""
}

// networkNamespaceAvailable is always true on macOS: Seatbelt applies the
// network denial in the same profile as the filesystem rules, so there is no
// separate capability to probe.
func networkNamespaceAvailable() bool { return true }

func wrapCommand(policy Policy, name string, args []string) (string, []string, error) {
	profile, err := seatbeltProfile(policy)
	if err != nil {
		return "", nil, err
	}
	// -p takes the profile as an argument rather than a file. A temp file would
	// need a lifetime, a cleanup, and a window in which another process could
	// swap it between write and exec; an argv string has none of those.
	wrapped := append([]string{"-p", profile, name}, args...)
	return sandboxExecPath, wrapped, nil
}

// seatbeltProfile renders the policy as SBPL.
//
// Rule order is load-bearing and runs opposite for the two file classes:
//
//   - READ is deny-then-allow. The default is permissive, each secret path is
//     denied, and nothing re-allows them. A later allow would win, so there
//     are none.
//   - WRITE is deny-everything-then-allow. The blanket deny comes first and
//     the workspace and toolchain caches are re-allowed after it, because in
//     SBPL the LAST matching rule decides.
//
// Getting that order backwards produces a profile that loads, runs, and
// enforces nothing, which is the failure mode worth naming: it looks
// identical to a working one from the outside.
func seatbeltProfile(policy Policy) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	// Reads: deny the named secrets. Nothing re-allows below, so a path listed
	// here cannot be read by any process in the tree.
	for _, path := range policy.UnreadablePaths {
		fmt.Fprintf(&b, "(deny file-read* (subpath %s) (with message %q))\n",
			seatbeltString(path), seatbeltLogTag)
	}
	for _, component := range policy.UnreadableComponents {
		pattern, ok := seatbeltComponentRegex(component)
		if !ok {
			// A component sonar cannot express exactly is refused rather than
			// approximated. An over-broad regex would deny reads nobody asked
			// to deny and look like a broken toolchain; an under-broad one
			// would protect nothing while reading as though it did.
			return "", fmt.Errorf("sandbox: cannot express secret component %q as a profile rule", component)
		}
		fmt.Fprintf(&b, "(deny file-read* (regex #%s) (with message %q))\n", pattern, seatbeltLogTag)
		fmt.Fprintf(&b, "(deny file-write* (regex #%s) (with message %q))\n", pattern, seatbeltLogTag)
	}
	// Public leaves are re-allowed AFTER the denies, because the last matching
	// rule wins. They are anchored to the end of the path, which is what makes
	// the exception a leaf rule: `.env.example` is a readable template, and a
	// DIRECTORY named `.env.example` still hides everything beneath it.
	for _, leaf := range policy.ReadableLeaves {
		pattern, ok := seatbeltLeafRegex(leaf)
		if !ok {
			return "", fmt.Errorf("sandbox: cannot express public leaf %q as a profile rule", leaf)
		}
		fmt.Fprintf(&b, "(allow file-read* (regex #%s))\n", pattern)
	}

	// Writes: deny everywhere, then re-allow the workspace and the caches.
	fmt.Fprintf(&b, "(deny file-write* (with message %q))\n", seatbeltLogTag)
	fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", seatbeltString(policy.Workspace))
	for _, path := range policy.WritablePaths {
		// A writable path inside the workspace is already covered, and emitting
		// it again would be noise in a profile an operator may have to read.
		if containedBy(path, policy.Workspace) {
			continue
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", seatbeltString(path))
	}
	// /dev/null and the tty are not writes anyone means to confine; a shell
	// that cannot open them fails in ways that look nothing like a policy.
	b.WriteString("(allow file-write* (subpath \"/dev\"))\n")

	// A secret must not be writable either. These come last so they beat the
	// workspace re-allow above: an ignored path INSIDE the workspace would
	// otherwise be writable, which would let a command truncate the .env it
	// was not allowed to read.
	for _, path := range policy.UnreadablePaths {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s) (with message %q))\n",
			seatbeltString(path), seatbeltLogTag)
	}

	if !policy.AllowNetwork {
		fmt.Fprintf(&b, "(deny network* (with message %q))\n", seatbeltLogTag)
	}
	return b.String(), nil
}

// seatbeltComponentRegex turns a secret NAME into a Seatbelt regex that
// matches that name as a whole path component at any depth.
//
// The supported grammar is deliberately one character wide: a literal name,
// optionally ending in `*`. That covers every pattern the host secret policy
// actually uses — `.env`, `.npmrc`, `.netrc`, `credentials`, `id_rsa*` — and
// refuses anything else rather than guessing. A regex assembled from a
// pattern language nobody bounded is how a security rule ends up matching
// either everything or nothing.
//
// Anchoring on `/` at both ends is what makes it a component match: `.env`
// must not also deny `my.environment`, and `credentials` must not deny
// `credentials.md`.
func seatbeltComponentRegex(component string) (string, bool) {
	leadingAny := strings.HasPrefix(component, "*")
	trailingAny := strings.HasSuffix(component, "*")
	literal := strings.TrimSuffix(strings.TrimPrefix(component, "*"), "*")
	if literal == "" || strings.ContainsAny(literal, `*?[]()|\^$+{}"`) {
		return "", false
	}
	quoted := seatbeltRegexLiteral(literal)
	// `[^/]*` on either side keeps the wildcard inside ONE component: `*.pem`
	// must not match `certs.pem.d/other`, and the component still has to close.
	head := "/"
	if leadingAny {
		head = "/[^/]*"
	}
	tail := "(/|$)"
	if trailingAny {
		tail = "[^/]*(/|$)"
	}
	return `"` + head + quoted + tail + `"`, true
}

// seatbeltLeafRegex matches an exact name only as the final path component.
func seatbeltLeafRegex(leaf string) (string, bool) {
	if leaf == "" || strings.ContainsAny(leaf, `*?[]()|\^$+{}"/`) {
		return "", false
	}
	return `"/` + seatbeltRegexLiteral(leaf) + `$"`, true
}

// seatbeltRegexLiteral escapes the metacharacters that survive the caller's
// grammar check. `.` is the one that matters: unescaped, `.env` would also
// match `xenv`, which is a rule that reads correct and denies the wrong files.
func seatbeltRegexLiteral(value string) string {
	return strings.NewReplacer(".", `\.`, "-", `\-`, "+", `\+`).Replace(value)
}

// seatbeltString quotes a path for SBPL. A path that cannot be represented
// without escaping is refused by the caller rather than escaped, because an
// escaping bug in a security profile fails silently open.
func seatbeltString(path string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(path, `\`, `\\`), `"`, `\"`) + `"`
}
