package config

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

// maxCredentialFileBytes bounds the env file so a mistargeted path cannot make
// the harness read something enormous at startup.
const maxCredentialFileBytes = 1 << 20

// CredentialEnvNames returns every environment variable name that could hold a
// provider credential, derived from the catalog.
//
// This set is the allowlist for LoadCredentialEnvFile. A shared secrets file
// typically holds far more than provider keys — PATH and EDITOR are common, and
// applying those would corrupt the process. Loading strictly by name means an
// unrelated entry in the file can never take effect.
func CredentialEnvNames() map[string]struct{} {
	names := make(map[string]struct{}, 64)
	for _, id := range catalog.ProviderIDs() {
		if env := catalog.APIKeyEnv(id); env != "" {
			names[env] = struct{}{}
		}
	}
	return names
}

// LoadCredentialEnvFile reads provider credentials from a KEY=VALUE file into
// the process environment and returns the NAMES it applied, sorted by
// appearance. Values are never returned, logged, or echoed.
//
// Rules, in order:
//
//   - A name outside CredentialEnvNames (plus extra) is ignored, not applied.
//   - A variable already present in the environment is left alone, so an
//     explicit `export` on the command line always wins over the file.
//   - A missing file is not an error; the harness simply has nothing to load.
//
// The file should be owner-only. A world- or group-readable file still loads —
// refusing would strand a user mid-task — but the caller receives a warning it
// is expected to surface.
func LoadCredentialEnvFile(path string, extra ...string) (applied []string, warning string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, "", nil
	}
	if strings.HasPrefix(path, "~/") {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, "", fmt.Errorf("credentials: resolve home for %q: %w", path, homeErr)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("credentials: %w", statErr)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("credentials: %s is a directory", path)
	}
	if info.Size() > maxCredentialFileBytes {
		return nil, "", fmt.Errorf("credentials: %s exceeds %d bytes", path, maxCredentialFileBytes)
	}
	if mode := info.Mode().Perm(); mode&fs.FileMode(0o077) != 0 {
		warning = fmt.Sprintf("credential file %s is readable by others (mode %04o); chmod 600 it", path, mode)
	}

	file, openErr := os.Open(path) // #nosec G304 -- operator-selected credential path
	if openErr != nil {
		return nil, warning, fmt.Errorf("credentials: %w", openErr)
	}
	defer func() { _ = file.Close() }()

	allowed := CredentialEnvNames()
	for _, name := range extra {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = struct{}{}
		}
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCredentialFileBytes)
	for scanner.Scan() {
		name, value, ok := parseCredentialLine(scanner.Text())
		if !ok {
			continue
		}
		if _, permitted := allowed[name]; !permitted {
			continue
		}
		if _, present := os.LookupEnv(name); present {
			// An explicit export outranks the file.
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return applied, warning, fmt.Errorf("credentials: set %s: %w", name, err)
		}
		applied = append(applied, name)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return applied, warning, fmt.Errorf("credentials: read %s: %w", path, scanErr)
	}
	return applied, warning, nil
}

// parseCredentialLine extracts NAME and VALUE from one shell-style assignment.
// It handles a leading `export`, surrounding single or double quotes, and
// skips comments and blank lines. It deliberately does not expand variables or
// run command substitution: this is a credential list, not a script.
func parseCredentialLine(line string) (name, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	line = strings.TrimSpace(line)
	equals := strings.Index(line, "=")
	if equals <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(line[:equals])
	value = strings.TrimSpace(line[equals+1:])
	if err := validateEnvVarName(name); err != nil {
		return "", "", false
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	if value == "" {
		return "", "", false
	}
	return name, value, true
}
