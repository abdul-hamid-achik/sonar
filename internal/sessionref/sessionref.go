// Package sessionref formats and parses short, user-facing session handles.
// Durable storage and JSON continue to use the underlying positive int64 ID;
// public handles are independent random mini-hashes stored on the session row.
package sessionref

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Length is the fixed number of lowercase hex characters in a public session
// handle. Fixed length keeps input unambiguous (no git-style prefix matching).
const Length = 7

// New generates a cryptographically random lowercase hex public session id.
func New() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate session public id: %w", err)
	}
	// 4 bytes → 8 hex chars; take Length so the public surface stays git-like.
	return hex.EncodeToString(raw[:])[:Length], nil
}

// Format returns the display handle for a stored public id. Invalid values
// yield the empty string so callers can fall back without panicking.
func Format(publicID string) string {
	if !Valid(publicID) {
		return ""
	}
	return strings.ToLower(publicID)
}

// Parse accepts a user-supplied session handle and returns the normalized
// public id. Handles are case-insensitive lowercase hex of fixed Length.
// Sequential S-prefixed ids and bare integers are intentionally rejected.
func Parse(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if !Valid(normalized) {
		return "", fmt.Errorf("invalid session reference %q", value)
	}
	return normalized, nil
}

// Valid reports whether publicID is a well-formed stored or display handle.
func Valid(publicID string) bool {
	if len(publicID) != Length {
		return false
	}
	for i := 0; i < len(publicID); i++ {
		c := publicID[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
