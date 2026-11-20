//go:build darwin

package clipboard

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// How often the pasteboard is checked. macOS has no change notification.
const pollInterval = time.Second

// typesScript prints the type names on the general pasteboard, one per line.
const typesScript = `ObjC.import("AppKit"); ObjC.deepUnwrap($.NSPasteboard.generalPasteboard.types).join("\n")`

// Pasteboard types that apps set to keep content out of clipboard managers.
// See nspasteboard.org.
var privateTypes = []string{
	"org.nspasteboard.ConcealedType",
	"org.nspasteboard.TransientType",
}

// pasteboard drives the macOS clipboard through pbcopy and pbpaste.
type pasteboard struct {
	log *slog.Logger
	env []string
}

// New returns the clipboard of the current macOS login session.
func New(log *slog.Logger) (Clipboard, error) {
	// pbcopy and pbpaste pick the text encoding from the locale. launchd
	// agents run without one, which would mangle anything outside ASCII.
	return &pasteboard{log: log, env: append(os.Environ(), "LC_ALL=en_US.UTF-8")}, nil
}

func (p *pasteboard) Read(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pbpaste")
	cmd.Env = p.env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pbpaste: %w", err)
	}
	return string(out), nil
}

func (p *pasteboard) Write(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pbcopy")
	cmd.Env = p.env
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pbcopy: %w", err)
	}
	return nil
}

func (p *pasteboard) Watch(ctx context.Context, onText func(string)) error {
	last, _ := p.Read(ctx)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		text, err := p.Read(ctx)
		if err != nil || text == last {
			continue
		}
		last = text
		if syncable(text) && !p.private(ctx) {
			onText(text)
		}
	}
}

// private reports whether the current pasteboard content is marked as not to
// be recorded. If the types cannot be read the answer is yes: failing to sync
// is better than syncing a password.
func (p *pasteboard) private(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", typesScript).Output()
	if err != nil {
		p.log.Warn("cannot read pasteboard types, not syncing this copy", "err", err)
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, t := range privateTypes {
			if strings.TrimSpace(line) == t {
				return true
			}
		}
	}
	return false
}
