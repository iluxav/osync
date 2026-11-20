//go:build linux

package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The one MIME type that is watched. Asking wl-paste for it by its exact name
// means the watcher only fires for plain text; an image copied from a browser
// also offers text/html, which a looser "any text" match would hand over.
const textType = "text/plain;charset=utf-8"

// watchScript is run by wl-paste for every clipboard change, with the new
// content on stdin. It prints one line: the state wl-paste reports, a space,
// and the content in base64, cut off just past MaxBytes. Having wl-paste hand
// over state and content together matters: reading them separately could pair
// the "not sensitive" state of one copy with the content of the next.
//
// The trailing cat drains what head left unread, so the application that owns
// the clipboard never writes into a closed pipe.
var watchScript = `printf '%s ' "${CLIPBOARD_STATE:-unknown}"; head -c ` +
	strconv.Itoa(MaxBytes+1) + ` | base64 -w0; cat >/dev/null; echo`

var errTooOld = errors.New("wl-clipboard is too old to tell passwords from other text; version 2.2 or newer is needed")

// wayland drives the clipboard through wl-clipboard, which works on
// compositors with a data-control protocol (Hyprland, Sway, KDE).
type wayland struct {
	log     *slog.Logger
	primary bool // use the primary selection instead; tests do, to leave the real clipboard alone
}

// New returns the clipboard of the current Wayland session.
func New(log *slog.Logger) (Clipboard, error) {
	for _, tool := range []string{"wl-paste", "wl-copy"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s not found: install wl-clipboard", tool)
		}
	}
	return &wayland{log: log}, nil
}

func (w *wayland) args(args ...string) []string {
	if w.primary {
		return append([]string{"--primary"}, args...)
	}
	return args
}

func (w *wayland) Read(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wl-paste", w.args("--no-newline", "--type", textType)...).Output()
	if err != nil {
		return "", fmt.Errorf("wl-paste: %w", err)
	}
	return string(out), nil
}

func (w *wayland) Write(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-copy", w.args("--type", "text/plain")...)
	cmd.Stdin = strings.NewReader(text)
	// Stdout and Stderr stay unset on purpose. wl-copy forks a process that
	// serves the clipboard until the next copy and keeps stderr open; given a
	// pipe there, Run would wait for that process instead of returning.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy: %w", err)
	}
	return nil
}

func (w *wayland) Watch(ctx context.Context, onText func(string)) error {
	delay := time.Second
	for {
		started := time.Now()
		err := w.watchOnce(ctx, onText)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errTooOld) {
			// Restarting cannot fix this. Clips from other machines still
			// arrive, so stay up rather than fail.
			w.log.Error("clipboard is not watched", "err", err)
			<-ctx.Done()
			return ctx.Err()
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		w.log.Warn("clipboard watcher stopped, restarting", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// watchOnce runs one wl-paste watcher until it exits.
func (w *wayland) watchOnce(ctx context.Context, onText func(string)) error {
	// wl-paste announces the existing content as its first event. Remember
	// what that is so it is not mistaken for a fresh copy.
	startup, _ := w.Read(ctx)

	cmd := exec.CommandContext(ctx, "wl-paste",
		w.args("--type", textType, "--watch", "sh", "-c", watchScript)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wl-paste: %w", err)
	}

	lines := bufio.NewReader(stdout)
	first := true
	var fatal error
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			break
		}
		text, ok, err := parseWatchLine(line)
		if err != nil {
			fatal = err
			cmd.Process.Kill()
			break
		}
		if first {
			first = false
			if text == startup {
				continue
			}
		}
		if ok {
			onText(text)
		}
	}
	err = cmd.Wait()
	if fatal != nil {
		return fatal
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("wl-paste: %s", msg)
	}
	return fmt.Errorf("wl-paste exited: %v", err)
}

// parseWatchLine decodes one line printed by watchScript. ok is false for
// content that must not be synced.
func parseWatchLine(line string) (text string, ok bool, err error) {
	state, encoded, found := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	if !found {
		return "", false, fmt.Errorf("unexpected clipboard watcher output %q", line)
	}
	switch state {
	case "data":
	case "unknown":
		// Without CLIPBOARD_STATE there is no way to tell a copied password
		// from ordinary text, so nothing is synced at all.
		return "", false, errTooOld
	default: // "sensitive", "nil", "clear", and whatever wl-paste adds later
		return "", false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false, fmt.Errorf("clipboard watcher output is not base64: %w", err)
	}
	text = string(raw)
	return text, syncable(text), nil
}
