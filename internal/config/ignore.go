package config

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const maxAgentIgnoreBytes int64 = 256 << 10

const effectiveIgnoreBoundary = "# --- sonar host secret policy (repository rules cannot override) ---"

var publicEnvTemplateNames = map[string]struct{}{
	".env.dist":     {},
	".env.example":  {},
	".env.sample":   {},
	".env.template": {},
}

var agentIgnoreReader = safeio.NewReader()

//go:embed default_agentignore
var defaultAgentIgnoreContent string

// IgnorePatterns holds parsed .agentignore patterns.
type IgnorePatterns struct {
	patterns []string
	raw      string // original file content for injection into system prompt
}

// LoadIgnoreFile reads and parses an .agentignore file from the given directory.
// Returns nil if no .agentignore file exists (not an error).
func LoadIgnoreFile(dir string) *IgnorePatterns {
	patterns, _ := LoadIgnoreFileWithError(dir)
	return patterns
}

// LoadIgnoreFileWithError is the diagnostic form used at startup. Missing
// files are not errors; unsafe, oversized, or timed-out inputs are rejected.
func LoadIgnoreFileWithError(dir string) (*IgnorePatterns, error) {
	path := filepath.Join(dir, ".agentignore")
	data, err := agentIgnoreReader.ReadRegularFileNoFollow(path, maxAgentIgnoreBytes, safeio.StartupReadTimeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read .agentignore: %w", err)
	}

	var patterns []string
	var rawLines []string

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), int(maxAgentIgnoreBytes)+1)
	for scanner.Scan() {
		line := scanner.Text()
		rawLines = append(rawLines, line)

		trimmed := strings.TrimSpace(line)
		// Skip empty lines and comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		patterns = append(patterns, trimmed)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse .agentignore: %w", err)
	}

	return &IgnorePatterns{
		patterns: patterns,
		raw:      strings.Join(rawLines, "\n"),
	}, nil
}

// Match returns true if the given path should be ignored.
// Returns false if the receiver is nil.
func (ip *IgnorePatterns) Match(path string) bool {
	if ip == nil || len(ip.patterns) == 0 {
		return false
	}

	// Normalise the path separators and remove trailing slashes for comparison.
	path = filepath.ToSlash(path)
	cleanPath := strings.TrimSuffix(path, "/")

	for _, pattern := range ip.patterns {
		pat := strings.TrimSuffix(pattern, "/")

		// Check each component of the path against the pattern.
		// e.g. "node_modules" should match "node_modules", "node_modules/foo",
		// and "src/node_modules/bar".
		parts := strings.Split(cleanPath, "/")
		for _, part := range parts {
			if matched, _ := filepath.Match(pat, part); matched {
				return true
			}
		}

		// Also try matching the full path with the pattern (for glob patterns
		// that include path separators like "build/output").
		if matched, _ := filepath.Match(pat, cleanPath); matched {
			return true
		}

		// Prefix match: path starts with the pattern directory.
		if strings.HasPrefix(cleanPath, pat+"/") || cleanPath == pat {
			return true
		}
	}

	return false
}

// Raw returns the raw file content for system prompt injection.
// Returns an empty string if the receiver is nil.
func (ip *IgnorePatterns) Raw() string {
	if ip == nil {
		return ""
	}
	return ip.raw
}

// EffectiveIgnoreContent combines one workspace policy with sonar's
// host-owned secret exclusions. A boundary keeps the two policies independent:
// repository rules may make access stricter, but cannot negate a host deny.
func EffectiveIgnoreContent(workspacePolicy string) string {
	if workspace, hasHostDefaults := IgnorePolicyLayers(workspacePolicy); hasHostDefaults {
		workspacePolicy = workspace
	}
	parts := make([]string, 0, 3)
	if workspacePolicy = strings.TrimSpace(workspacePolicy); workspacePolicy != "" {
		parts = append(parts, workspacePolicy)
	}
	parts = append(parts, effectiveIgnoreBoundary)
	if defaults := strings.TrimSpace(defaultAgentIgnoreContent); defaults != "" {
		parts = append(parts, defaults)
	}
	return strings.Join(parts, "\n")
}

// IgnorePolicyLayers separates an effective policy into the repository-owned
// portion and a flag indicating that host secret defaults must be enforced.
// A boundary is trusted only when the complete embedded host suffix follows it;
// a repository-controlled lookalike line therefore remains ordinary policy.
func IgnorePolicyLayers(content string) (workspacePolicy string, hasHostDefaults bool) {
	normalized := strings.TrimSpace(content)
	hostSuffix := effectiveIgnoreBoundary
	if defaults := strings.TrimSpace(defaultAgentIgnoreContent); defaults != "" {
		hostSuffix += "\n" + defaults
	}
	if normalized != hostSuffix && !strings.HasSuffix(normalized, "\n"+hostSuffix) {
		return content, false
	}
	workspacePolicy = strings.TrimSpace(strings.TrimSuffix(normalized, hostSuffix))
	return workspacePolicy, true
}

// HostSecretPathIgnored reports whether path is denied by sonar's
// non-overridable secret policy. Conventional environment templates are exact
// leaf-file exceptions only: a repository may still exclude them, and a
// directory named .env.example never makes its descendants readable.
func HostSecretPathIgnored(path string) bool {
	cleanPath := strings.Trim(filepath.ToSlash(filepath.Clean(path)), "/")
	if cleanPath == "" || cleanPath == "." {
		return false
	}
	parts := strings.Split(cleanPath, "/")
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if strings.HasPrefix(part, ".env.") {
			_, publicTemplate := publicEnvTemplateNames[part]
			if !publicTemplate || i != len(parts)-1 {
				return true
			}
			continue
		}
		if part == ".env" || part == ".npmrc" || part == ".netrc" || part == "credentials" || part == ".aws" || part == ".ssh" {
			return true
		}
		for _, pattern := range []string{"*.pem", "*.key", "id_rsa*", "id_ed25519*", "*.p12", "*.keystore"} {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
	}
	return false
}

// EffectiveRaw returns the enforcement policy used for workspace file access.
// Raw StructuredContent and file contents never enter this policy.
func (ip *IgnorePatterns) EffectiveRaw() string {
	return EffectiveIgnoreContent(ip.Raw())
}

// Patterns returns the list of patterns.
// Returns nil if the receiver is nil.
func (ip *IgnorePatterns) Patterns() []string {
	if ip == nil {
		return nil
	}
	return ip.patterns
}
