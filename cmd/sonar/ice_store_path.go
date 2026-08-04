package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
)

const managedICEStoreDir = ".config/sonar/ice"

var iceStoreHomeDir = os.UserHomeDir

func resolvedICEEngineConfig(cfg *config.Config, workspace, storePath string) ice.EngineConfig {
	embedModel := cfg.ICE.EmbedModel
	if embedModel == "" {
		embedModel = cfg.Model.EmbedModel
	}
	return ice.EngineConfig{
		EmbedModel: embedModel,
		StorePath:  storePath,
		NumCtx:     cfg.Ollama.NumCtx,
		Workspace:  workspace,
	}
}

// resolveICEStorePath confines an explicitly configured ICE store to a
// canonical-workspace-scoped directory under the user's managed application
// data. An empty value keeps ice.NewEngine's managed global default.
// Repository configuration therefore controls only a relative name, never a
// repository path or an arbitrary write elsewhere on the machine.
func resolveICEStorePath(workspace, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", nil
	}
	if filepath.IsAbs(configured) || filepath.VolumeName(configured) != "" {
		return "", fmt.Errorf("ice.store_path must be a relative name under managed per-workspace storage, got %q", configured)
	}

	clean := filepath.Clean(configured)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("ice.store_path escapes managed per-workspace storage: %q", configured)
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for ice.store_path: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for ice.store_path: %w", err)
	}

	home, err := iceStoreHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for ice.store_path: %w", err)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve home for ice.store_path: %w", err)
	}
	if resolvedHome, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolvedHome
	}

	workspaceHash := sha256.Sum256([]byte(root))
	managedRoot := filepath.Join(
		home,
		filepath.FromSlash(managedICEStoreDir),
		fmt.Sprintf("%x", workspaceHash[:8]),
	)
	candidate := filepath.Join(managedRoot, clean)
	rel, err := filepath.Rel(managedRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("ice.store_path escapes managed per-workspace storage: %q", configured)
	}

	current := managedRoot
	rootInfo, lstatErr := os.Lstat(current)
	if lstatErr == nil {
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("ice.store_path managed root must not be a symlink: %s", current)
		}
		if !rootInfo.IsDir() {
			return "", fmt.Errorf("ice.store_path managed root is not a directory: %s", current)
		}
		if rootInfo.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("ice.store_path managed root is not owner-only: %s (%o)", current, rootInfo.Mode().Perm())
		}
	} else if !errors.Is(lstatErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect ice.store_path managed root %s: %w", current, lstatErr)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, lstatErr := os.Lstat(current)
		if errors.Is(lstatErr, os.ErrNotExist) {
			// Once a parent is absent, no deeper component can exist yet. The
			// store will create the remaining directories inside this root.
			break
		}
		if lstatErr != nil {
			return "", fmt.Errorf("inspect ice.store_path component %s: %w", current, lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("ice.store_path must not traverse symlink %s", current)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("ice.store_path parent is not a directory: %s", current)
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return "", fmt.Errorf("ice.store_path destination is not a regular file: %s", current)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("ice.store_path component is not owner-only: %s (%o)", current, info.Mode().Perm())
		}
	}

	return candidate, nil
}
