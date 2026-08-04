package goaladvisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	workspaceGitTimeout = 20 * time.Second
	workspaceGitCap     = 4 << 20
)

// WorkspaceRevision is the exact tracked and untracked Git state a Cortex
// verification receipt proves. It mirrors Cortex's revision identity so Local
// Agent can reject a terminal case whose workspace changed after verification.
type WorkspaceRevision struct {
	Commit      string
	DirtyDigest string
}

func (r WorkspaceRevision) Valid() bool {
	return strings.TrimSpace(r.Commit) != "" && strings.TrimSpace(r.DirtyDigest) != ""
}

// CurrentWorkspaceRevision computes Cortex's HEAD + dirty-tree identity using
// read-only Git commands. Output is bounded to Cortex's 4 MiB adapter backstop;
// untracked paths and bytes are included in stable lexical order.
func CurrentWorkspaceRevision(ctx context.Context, dir string) (WorkspaceRevision, error) {
	if ctx == nil {
		return WorkspaceRevision{}, fmt.Errorf("workspace revision context is nil")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return WorkspaceRevision{}, fmt.Errorf("workspace directory is required")
	}
	probeCtx, cancel := context.WithTimeout(ctx, workspaceGitTimeout)
	defer cancel()

	commit, err := runRevisionGit(probeCtx, dir, "rev-parse", "HEAD")
	if err != nil {
		return WorkspaceRevision{}, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	diff, err := runRevisionGit(probeCtx, dir, "diff", "--binary", "HEAD")
	if err != nil {
		return WorkspaceRevision{}, fmt.Errorf("git diff HEAD: %w", err)
	}
	untracked, _ := runRevisionGit(probeCtx, dir, "ls-files", "--others", "--exclude-standard")
	paths := splitRevisionLines(string(untracked))
	sort.Strings(paths)
	workspaceRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return WorkspaceRevision{}, fmt.Errorf("resolve workspace: %w", err)
	}

	hash := sha256.New()
	_, _ = hash.Write(diff)
	for _, path := range paths {
		_, _ = hash.Write([]byte("\x00" + filepath.ToSlash(path) + "\x00"))
		fullPath := filepath.Join(dir, path)
		resolvedPath, resolveErr := filepath.EvalSymlinks(fullPath)
		if resolveErr != nil {
			_, _ = hash.Write([]byte("<unreadable>"))
			continue
		}
		if !revisionPathWithin(workspaceRoot, resolvedPath) {
			return WorkspaceRevision{}, fmt.Errorf("untracked path %q resolves outside the workspace", path)
		}
		info, statErr := os.Stat(resolvedPath)
		if statErr != nil || !info.Mode().IsRegular() {
			_, _ = hash.Write([]byte("<unreadable>"))
			continue
		}
		file, openErr := os.Open(resolvedPath)
		if openErr != nil {
			_, _ = hash.Write([]byte("<unreadable>"))
			continue
		}
		copyErr := copyRevisionFile(probeCtx, hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return WorkspaceRevision{}, fmt.Errorf("hash untracked file %q: %w", path, err)
		}
	}
	return WorkspaceRevision{
		Commit:      strings.TrimSpace(string(commit)),
		DirtyDigest: fmt.Sprintf("sha256:%x", hash.Sum(nil)),
	}, nil
}

func revisionPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func copyRevisionFile(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, writeErr := destination.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func runRevisionGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var stdout, stderr cappedRevisionBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(string(stderr.Bytes()))
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("%s", detail)
	}
	return stdout.Bytes(), nil
}

type cappedRevisionBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedRevisionBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := workspaceGitCap - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.buffer.Write(value)
	} else if original > 0 {
		b.truncated = true
	}
	return original, nil
}

func (b *cappedRevisionBuffer) Bytes() []byte {
	result := append([]byte(nil), b.buffer.Bytes()...)
	if b.truncated {
		result = append(result, []byte("\n…(truncated)")...)
	}
	return result
}

func splitRevisionLines(value string) []string {
	result := make([]string, 0)
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}
