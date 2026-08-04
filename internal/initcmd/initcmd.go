package initcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// projectMarker maps a marker file name to its detected project type.
var projectMarkers = map[string]string{
	"go.mod":              "Go",
	"go.sum":              "Go",
	"package.json":        "Node.js",
	"Cargo.toml":          "Rust",
	"pyproject.toml":      "Python",
	"requirements.txt":    "Python",
	"setup.py":            "Python",
	"Pipfile":             "Python",
	"Gemfile":             "Ruby",
	"pom.xml":             "Java (Maven)",
	"build.gradle":        "Java (Gradle)",
	"build.gradle.kts":    "Kotlin (Gradle)",
	"CMakeLists.txt":      "C/C++ (CMake)",
	"Makefile":            "Make",
	"Taskfile.yml":        "Taskfile",
	"Taskfile.yaml":       "Taskfile",
	"docker-compose.yml":  "Docker Compose",
	"docker-compose.yaml": "Docker Compose",
	"Dockerfile":          "Docker",
	".gitignore":          "Git",
}

// Options configures the behaviour of Run.
type Options struct {
	// Force overwrites an existing AGENTS.md.
	Force bool
}

// Run scans dir for project markers and generates an AGENTS.md file.
// It returns an error if AGENTS.md already exists unless opts.Force is true.
func Run(dir string, opts Options) error {
	agentPath := filepath.Join(dir, "AGENTS.md")
	legacyAgentPath := filepath.Join(dir, "AGENT.md")

	if err := validateAgentDestination(agentPath, opts.Force); err != nil {
		return err
	}
	if err := rejectLegacyAgentShadow(agentPath, legacyAgentPath); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading directory: %w", err)
	}

	// Detect project types from marker files.
	detectedTypes := detectProjectTypes(entries)

	// Build directory listing.
	listing := buildDirectoryListing(dir, entries)

	// Generate AGENTS.md content.
	content := generateAgentMD(detectedTypes, listing)

	if err := rejectLegacyAgentShadow(agentPath, legacyAgentPath); err != nil {
		return err
	}
	if err := writeAgentFileAtomically(dir, agentPath, content, opts.Force); err != nil {
		return fmt.Errorf("writing AGENTS.md: %w", err)
	}

	return nil
}

func rejectLegacyAgentShadow(agentPath, legacyPath string) error {
	if _, err := os.Lstat(agentPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect AGENTS.md before legacy check: %w", err)
	}

	legacyInfo, err := os.Lstat(legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy AGENT.md: %w", err)
	}
	kind := "file"
	if legacyInfo.Mode()&os.ModeSymlink != 0 {
		kind = "symlink"
	} else if !legacyInfo.Mode().IsRegular() {
		kind = legacyInfo.Mode().Type().String()
	}
	return fmt.Errorf("legacy AGENT.md %s exists at %s; refusing to create AGENTS.md because it would shadow authored instructions—review it, then rename or migrate AGENT.md to AGENTS.md", kind, legacyPath)
}

func validateAgentDestination(path string, force bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect AGENTS.md: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing AGENTS.md symlink %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular AGENTS.md destination %s (%s)", path, info.Mode().Type())
	}
	if !force {
		return fmt.Errorf("AGENTS.md already exists in %s (use --force to overwrite)", filepath.Dir(path))
	}
	return nil
}

func writeAgentFileAtomically(dir, path, content string, force bool) error {
	tmp, err := os.CreateTemp(dir, ".AGENTS.md-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if _, err := io.WriteString(tmp, content); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}

	if force {
		// Recheck immediately before the atomic replacement. A symlink appearing
		// after this check is still replaced as a directory entry by Rename; it is
		// never followed, so its target cannot be modified.
		if err := validateAgentDestination(path, true); err != nil {
			return err
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("replace destination: %w", err)
		}
	} else {
		// A same-directory hard link atomically publishes the fully synced temp
		// file without replacing anything that appeared after the initial check.
		if err := os.Link(tmpPath, path); err != nil {
			if destinationErr := validateAgentDestination(path, false); destinationErr != nil {
				return destinationErr
			}
			return fmt.Errorf("publish destination: %w", err)
		}
		if err := os.Remove(tmpPath); err != nil {
			return fmt.Errorf("remove temporary link: %w", err)
		}
	}
	if err := syncAgentDirectory(dir); err != nil {
		return err
	}
	return nil
}

func syncAgentDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open destination directory for sync: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync destination directory: %w", err)
	}
	return nil
}

// detectProjectTypes returns a deduplicated, sorted list of project types
// found based on marker files in the directory entries.
func detectProjectTypes(entries []os.DirEntry) []string {
	seen := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if pt, ok := projectMarkers[e.Name()]; ok {
			seen[pt] = true
		}
	}

	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// buildDirectoryListing returns a formatted string of top-level files and
// first-level subdirectory contents.
func buildDirectoryListing(dir string, entries []os.DirEntry) string {
	var b strings.Builder

	var files []string
	var dirs []string

	for _, e := range entries {
		name := e.Name()
		// Skip hidden files/dirs except well-known ones.
		if strings.HasPrefix(name, ".") && name != ".gitignore" {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, name)
		} else {
			files = append(files, name)
		}
	}

	sort.Strings(files)
	sort.Strings(dirs)

	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}

	for _, d := range dirs {
		b.WriteString(d + "/\n")
		subEntries, err := os.ReadDir(filepath.Join(dir, d))
		if err != nil {
			continue
		}
		var subNames []string
		for _, se := range subEntries {
			n := se.Name()
			if strings.HasPrefix(n, ".") {
				continue
			}
			if se.IsDir() {
				subNames = append(subNames, n+"/")
			} else {
				subNames = append(subNames, n)
			}
		}
		sort.Strings(subNames)
		for _, sn := range subNames {
			b.WriteString("  " + sn + "\n")
		}
	}

	return b.String()
}

// generateAgentMD produces the Markdown content for the AGENTS.md file.
func generateAgentMD(projectTypes []string, listing string) string {
	var b strings.Builder

	b.WriteString("# AGENTS.md\n\n")

	// Project type section.
	b.WriteString("## Project Type\n\n")
	if len(projectTypes) == 0 {
		b.WriteString("Unknown\n")
	} else {
		b.WriteString(strings.Join(projectTypes, ", ") + "\n")
	}

	// Directory structure.
	b.WriteString("\n## Directory Structure\n\n")
	b.WriteString("```\n")
	b.WriteString(listing)
	b.WriteString("```\n")

	// Placeholder sections.
	b.WriteString("\n## Build Commands\n\n")
	b.WriteString("<!-- Add build, test, and run commands here -->\n")

	b.WriteString("\n## Architecture\n\n")
	b.WriteString("<!-- Describe the high-level architecture here -->\n")

	b.WriteString("\n## Key Files\n\n")
	b.WriteString("<!-- List important files and their purposes here -->\n")

	b.WriteString("\n## Notes\n\n")
	b.WriteString("<!-- Any additional notes for the agent -->\n")

	return b.String()
}
