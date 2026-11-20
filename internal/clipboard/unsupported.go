//go:build !linux && !darwin

package clipboard

import (
	"errors"
	"log/slog"
)

// New reports that this operating system has no clipboard backend.
func New(*slog.Logger) (Clipboard, error) {
	return nil, errors.New("clipboard sync is only implemented for Linux (Wayland) and macOS")
}
