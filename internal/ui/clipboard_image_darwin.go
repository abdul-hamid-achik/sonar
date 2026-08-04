//go:build darwin

package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

const maxClipboardImageBytes = 20 << 20

var errClipboardImageUnavailable = errors.New("clipboard image is unavailable")

const clipboardPNGAppleScript = `on run argv
set outputPath to item 1 of argv
try
  set imageData to the clipboard as «class PNGf»
on error
  error number 1
end try
set outputFile to open for access POSIX file outputPath with write permission
try
  set eof outputFile to 0
  write imageData to outputFile
on error messageText number messageNumber
  close access outputFile
  error messageText number messageNumber
end try
close access outputFile
end run`

// readClipboardImage reads the macOS pasteboard only after explicit Ctrl+V.
// The temporary file is owner-only and removed before the result is returned.
func readClipboardImage(ctx context.Context) (string, []byte, error) {
	file, err := os.CreateTemp("", "sonar-clipboard-*.png")
	if err != nil {
		return "", nil, errClipboardImageUnavailable
	}
	path := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return "", nil, errClipboardImageUnavailable
	}
	defer func() { _ = os.Remove(path) }()
	if err := os.Chmod(path, 0o600); err != nil {
		return "", nil, errClipboardImageUnavailable
	}
	command := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", clipboardPNGAppleScript, path)
	if err := command.Run(); err != nil {
		return "", nil, errClipboardImageUnavailable
	}
	opened, err := os.Open(path)
	if err != nil {
		return "", nil, errClipboardImageUnavailable
	}
	defer func() { _ = opened.Close() }()
	data, err := io.ReadAll(io.LimitReader(opened, maxClipboardImageBytes+1))
	if err != nil || len(data) == 0 {
		return "", nil, errClipboardImageUnavailable
	}
	if len(data) > maxClipboardImageBytes {
		return "", nil, fmt.Errorf("%w: image exceeds %d bytes", errClipboardImageUnavailable, maxClipboardImageBytes)
	}
	return "clipboard.png", data, nil
}
