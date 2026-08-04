package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/execpath"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const maxTrustedMCPExecutableBytes int64 = 256 << 20

// resolveExecutable keeps stdio MCP startup reliable when sonar is
// launched from Finder or another GUI with a minimal PATH.
func resolveExecutable(command string) (string, error) {
	return execpath.Resolve(command)
}

func standardExecutableDirs() []string {
	return execpath.StandardDirs()
}

// verifyTrustedExecutable re-reads a repository-trusted executable immediately
// before process construction. Configuration trust already pins an absolute,
// symlink-resolved path; this second check catches replacement after Load and
// before startup. The OS pathname remains the final execution authority, so
// callers must keep its directory protected from concurrent writes.
func verifyTrustedExecutable(path, expected string) error {
	if expected == "" {
		return nil
	}
	if len(expected) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(expected, "sha256:") {
		return fmt.Errorf("invalid trusted executable digest")
	}
	contents, err := safeio.NewReader().ReadRegularFileNoFollow(
		path, maxTrustedMCPExecutableBytes, safeio.StartupReadTimeout,
	)
	if err != nil {
		return fmt.Errorf("re-read trusted executable: %w", err)
	}
	actual := fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("trusted executable changed after repository approval")
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
