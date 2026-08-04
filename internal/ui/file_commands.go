package ui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

const (
	maxLoadedContextBytes int64 = 32 << 10
	maxImportBytes        int64 = 16 << 20
)

func exportConversationCmd(workDir, path string, content []byte, force bool, token uint64) tea.Cmd {
	return func() (message tea.Msg) {
		defer func() {
			if recovered := recover(); recovered != nil {
				message = ExportResultMsg{Token: token, Path: path, Err: fmt.Errorf("export panic recovered: %v", recovered)}
			}
		}()
		exportPath, err := writeConversationExport(workDir, path, content, force)
		return ExportResultMsg{Token: token, Path: exportPath, Err: err}
	}
}

type exportPublishedWarning struct{ cause error }

func (warning *exportPublishedWarning) Error() string {
	return fmt.Sprintf("export was published, but durability cleanup reported: %v", warning.cause)
}

func (warning *exportPublishedWarning) Unwrap() error { return warning.cause }

func exportWasPublished(err error) bool {
	var warning *exportPublishedWarning
	return errors.As(err, &warning)
}

// writeConversationExport publishes a complete transcript atomically. New
// files default to owner-only permissions because prompts and tool receipts
// can contain private local data. Existing regular files require --force and
// retain their permissions. A final symlink or special file is never followed.
func writeConversationExport(workDir, requestedPath string, data []byte, force bool) (string, error) {
	parent, destination, destinationName, err := openExportParent(workDir, requestedPath)
	if err != nil {
		return requestedPath, err
	}
	defer func() { _ = parent.Close() }()

	mode := os.FileMode(0o600)
	info, err := parent.Lstat(destinationName)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return destination, fmt.Errorf("refusing symbolic-link destination %s", destination)
		}
		if !info.Mode().IsRegular() {
			return destination, fmt.Errorf("refusing non-regular destination %s (%s)", destination, info.Mode().Type())
		}
		if !force {
			return destination, fmt.Errorf("destination already exists: %s (use /export --force %s to replace it)", destination, requestedPath)
		}
		mode = info.Mode().Perm()
	case errors.Is(err, os.ErrNotExist):
		// A missing destination is published without replacement below.
	case err != nil:
		return destination, fmt.Errorf("inspect export destination: %w", err)
	}

	tmp, tmpName, err := createExportTemp(parent)
	if err != nil {
		return destination, fmt.Errorf("create export temporary file: %w", err)
	}
	defer func() { _ = parent.Remove(tmpName) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return destination, fmt.Errorf("set export permissions: %w", err)
	}
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		_ = tmp.Close()
		return destination, fmt.Errorf("write export: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return destination, fmt.Errorf("sync export: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return destination, fmt.Errorf("close export: %w", err)
	}

	if force {
		// Rename replaces the directory entry itself, never a symlink target.
		if err := parent.Rename(tmpName, destinationName); err != nil {
			return destination, fmt.Errorf("publish export: %w", err)
		}
	} else {
		// Link provides no-replace publication on every supported platform: if
		// anything appeared after Lstat, it fails with EEXIST instead of being
		// overwritten. Removing the temp name leaves the published inode.
		if err := parent.Link(tmpName, destinationName); err != nil {
			return destination, fmt.Errorf("publish new export without replacement: %w", err)
		}
		if err := parent.Remove(tmpName); err != nil {
			return destination, &exportPublishedWarning{cause: fmt.Errorf("temporary link cleanup failed: %w", err)}
		}
	}

	directory, err := parent.Open(".")
	if err != nil {
		return destination, &exportPublishedWarning{cause: fmt.Errorf("open parent for sync failed: %w", err)}
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return destination, &exportPublishedWarning{cause: fmt.Errorf("parent sync failed: %w", syncErr)}
	}
	if closeErr != nil {
		return destination, &exportPublishedWarning{cause: fmt.Errorf("parent close failed: %w", closeErr)}
	}
	return destination, nil
}

func openExportParent(workDir, requestedPath string) (*os.Root, string, string, error) {
	requestedPath = strings.TrimSpace(requestedPath)
	if requestedPath == "" {
		return nil, "", "", errors.New("export path is empty")
	}

	relative := !filepath.IsAbs(requestedPath)
	if !relative {
		destination, err := filepath.Abs(filepath.Clean(requestedPath))
		if err != nil {
			return nil, "", "", fmt.Errorf("resolve export path: %w", err)
		}
		parent, err := os.OpenRoot(filepath.Dir(destination))
		if err != nil {
			return nil, "", "", fmt.Errorf("open export parent: %w", err)
		}
		return parent, destination, filepath.Base(destination), nil
	}

	clean := filepath.Clean(requestedPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "", "", fmt.Errorf("relative export path escapes workspace: %s", requestedPath)
	}
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return nil, "", "", fmt.Errorf("resolve working directory: %w", err)
		}
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	canonicalWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve workspace: %w", err)
	}
	workspaceRoot, err := os.OpenRoot(canonicalWorkDir)
	if err != nil {
		return nil, "", "", fmt.Errorf("open workspace for export: %w", err)
	}
	parent, err := workspaceRoot.OpenRoot(filepath.Dir(clean))
	_ = workspaceRoot.Close()
	if err != nil {
		return nil, "", "", fmt.Errorf("relative export parent escapes workspace or is unavailable: %w", err)
	}
	return parent, filepath.Join(canonicalWorkDir, clean), filepath.Base(clean), nil
}

func createExportTemp(parent *os.Root) (*os.File, string, error) {
	for range 100 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".sonar-export-" + hex.EncodeToString(random[:])
		file, err := parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate a unique export temporary file")
}

func parseSlashCommandInput(input string) (string, []string, error) {
	fields, err := splitQuotedFields(strings.TrimSpace(strings.TrimPrefix(input, "/")))
	if err != nil {
		return "", nil, err
	}
	if len(fields) == 0 {
		return "", nil, errors.New("empty slash command")
	}
	return fields[0], fields[1:], nil
}

// splitQuotedFields is intentionally a parser, not a shell: it supports
// quoted paths and escaped whitespace but never performs expansion or command
// substitution.
func splitQuotedFields(input string) ([]string, error) {
	runes := []rune(input)
	fields := make([]string, 0, 4)
	var field strings.Builder
	var quote rune
	started := false
	flush := func() {
		if started {
			fields = append(fields, field.String())
			field.Reset()
			started = false
		}
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == quote {
				quote = 0
				started = true
				continue
			}
			if quote == '"' && r == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\') {
				i++
				field.WriteRune(runes[i])
				started = true
				continue
			}
			field.WriteRune(r)
			started = true
			continue
		}

		switch {
		case unicode.IsSpace(r):
			flush()
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\\' && i+1 < len(runes) && (unicode.IsSpace(runes[i+1]) || runes[i+1] == '\'' || runes[i+1] == '"' || runes[i+1] == '\\'):
			i++
			field.WriteRune(runes[i])
			started = true
		default:
			field.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flush()
	return fields, nil
}

// handleContextLoadResult applies a tokened context-load receipt.
func (m *Model) handleContextLoadResult(msg ContextLoadResultMsg) {
	if !m.fileLoading || msg.Token != m.fileOpToken {
		return
	}
	m.fileLoading = false
	if !m.shuttingDown {
		m.input.Focus()
	}
	if msg.Err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Load failed: %v", msg.Err)})
	} else {
		m.loadedFile = msg.Path
		m.manualLoadedContext = msg.Data
		m.syncLoadedContext()
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Loaded context: %s (%d bytes)", msg.Path, len(msg.Data))})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
}
