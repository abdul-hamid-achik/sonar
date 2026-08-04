//go:build !darwin

package ui

import (
	"context"
	"errors"
)

var errClipboardImageUnavailable = errors.New("clipboard image is unavailable")

func readClipboardImage(context.Context) (string, []byte, error) {
	return "", nil, errClipboardImageUnavailable
}
