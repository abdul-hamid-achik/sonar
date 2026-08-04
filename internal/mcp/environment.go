package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// childEnvironment builds a deliberately small environment for stdio MCP
// servers. Credentials from the agent process must not become ambient
// capabilities of every server; a server receives extra values only when its
// configuration explicitly lists them.
func childEnvironment(inherited, explicit []string) []string {
	values := make(map[string]string)
	for _, entry := range inherited {
		key, value, ok := splitEnvironmentEntry(entry)
		if ok && safeInheritedEnvironmentKey(key) {
			values[key] = value
		}
	}
	for _, entry := range explicit {
		key, value, ok := splitEnvironmentEntry(entry)
		if ok {
			values[key] = value
		}
	}
	pathParts := filepath.SplitList(values["PATH"])
	pathParts = append(pathParts, standardExecutableDirs()...)
	values["PATH"] = strings.Join(uniqueStrings(pathParts), string(os.PathListSeparator))

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func splitEnvironmentEntry(entry string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(entry, "=")
	if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
		return "", "", false
	}
	return key, value, true
}

func safeInheritedEnvironmentKey(key string) bool {
	switch key {
	case "HOME", "PATH", "PWD", "SHELL", "TMPDIR", "TMP", "TEMP",
		"USER", "LOGNAME", "LANG", "TZ", "TERM", "COLORTERM", "NO_COLOR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "SYSTEMROOT", "WINDIR":
		return true
	default:
		return strings.HasPrefix(key, "LC_")
	}
}
