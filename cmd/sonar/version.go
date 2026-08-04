package main

import (
	"runtime/debug"
	"strings"
)

// effectiveVersion preserves GoReleaser's linker-injected version and falls
// back to module build metadata for `go install ...@version`. Source checkouts
// continue to report "dev".
func effectiveVersion() string {
	moduleVersion := ""
	sourceCheckout := false
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && strings.TrimSpace(setting.Value) != "" {
				sourceCheckout = true
				break
			}
		}
	}
	return resolveEffectiveVersion(version, moduleVersion, sourceCheckout)
}

func resolveEffectiveVersion(
	linkedVersion,
	moduleVersion string,
	sourceCheckout bool,
) string {
	linkedVersion = strings.TrimSpace(linkedVersion)
	if linkedVersion != "" && linkedVersion != "dev" && linkedVersion != "(devel)" {
		return strings.TrimPrefix(linkedVersion, "v")
	}
	// A local checkout can carry a pseudo-version in Main.Version on newer Go
	// toolchains. VCS settings distinguish it from `go install pkg@version`,
	// whose module-cache build has no checkout revision.
	if sourceCheckout {
		return "dev"
	}
	moduleVersion = strings.TrimSpace(moduleVersion)
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return strings.TrimPrefix(moduleVersion, "v")
	}
	return "dev"
}
