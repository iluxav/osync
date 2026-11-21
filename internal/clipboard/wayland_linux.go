//go:build linux

package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// The one text type that is watched. Asking wl-paste for it by its exact
	// name means the watcher only fires for plain text; an image copied from
	// a browser also offers text/html, which a looser "any text" match would
	// hand over.
	textType = "text/plain;charset=utf-8"
	// What file managers offer for copied files: one file:// URI per line.
	filesType = "text/uri-list"
	// What screenshot tools and browsers offer for a picture.
	imageType = "image/png"
)

// watched are the types a copy is looked for in, and for each what makes
// content of that type worth reporting and the size past which it is not.
var watched = []struct {
	typ   string
	limit int
	valid func(data string) bool
}{
	{textType, MaxBytes, syncable},
	{filesType, MaxBytes, syncable},
	{imageType, MaxImageBytes, func(data string) bool { return data != "" && ValidImage([]byte(data)) }},
}

// watchScript returns what wl-paste runs for every clipboard change, with
// the new content on stdin. It prints one line: the state wl-paste reports, a
// space, and the content in base64, cut off just past limit. Having wl-paste
// hand over state and content together matters: reading them separately
// could pair the "not sensitive" state of one copy with the content of the
// next.
//
// The trailing cat drains what head left unread, so the application that owns
// the clipboard never writes into a closed pipe.
func watchScript(limit int) string {
	return `printf '%s ' "${CLIPBOARD_STATE:-unknown}"; head -c ` +
		strconv.Itoa(limit+1) + ` | base64 -w0; cat >/dev/null; echo`
}

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

func (w *wayland) Read(ctx context.Context) (Content, error) {
	if files := w.readFiles(ctx); len(files) > 0 {
		return Content{Files: files}, nil
	}
	if text, err := w.readType(ctx, textType); err == nil {
		return Content{Text: text}, nil
	}
	image, err := w.readType(ctx, imageType)
	if err != nil {
		return Content{}, err
	}
	return Content{Image: []byte(image)}, nil
}

// types returns the names of the types the clipboard content is offered in.
func (w *wayland) types(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "wl-paste", w.args("--list-types")...).Output()
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// readType returns the clipboard content in one of the types it is offered in.
func (w *wayland) readType(ctx context.Context, typ string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wl-paste", w.args("--no-newline", "--type", typ)...).Output()
	if err != nil {
		return "", fmt.Errorf("wl-paste: %w", err)
	}
	return string(out), nil
}

// readFiles returns the files on the clipboard, or nil when it holds
// something else.
func (w *wayland) readFiles(ctx context.Context) []string {
	list, err := w.readType(ctx, filesType)
	if err != nil {
		return nil
	}
	return parseURIList(list)
}

func (w *wayland) Write(ctx context.Context, c Content) error {
	var (
		typ            = "text/plain"
		data io.Reader = strings.NewReader(c.Text)
	)
	switch {
	case len(c.Files) > 0:
		// wl-copy offers any text/ type as plain text too, so the files
		// paste into a file manager and their URIs into a text field.
		typ, data = filesType, strings.NewReader(uriList(c.Files))
	case len(c.Image) > 0:
		typ, data = imageType, bytes.NewReader(c.Image)
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-copy", w.args("--type", typ)...)
	cmd.Stdin = data
	// Stdout and Stderr stay unset on purpose. wl-copy forks a process that
	// serves the clipboard until the next copy and keeps stderr open; given a
	// pipe there, Run would wait for that process instead of returning.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy: %w", err)
	}
	return nil
}

func (w *wayland) Watch(ctx context.Context, onCopy func(Content)) error {
	// The watchers report through one door, so calls never overlap.
	var mu sync.Mutex
	report := func(c Content) {
		mu.Lock()
		defer mu.Unlock()
		onCopy(c)
	}
	// One watcher per type: wl-paste hands over state and content together
	// only for the type it was told to watch.
	var wg sync.WaitGroup
	for _, kind := range watched {
		wg.Go(func() {
			keepRunning(ctx, w.log, "clipboard watcher for "+kind.typ, func() error {
				err := w.watchOnce(ctx, kind.typ, kind.limit, kind.valid, report)
				if errors.Is(err, errTooOld) && ctx.Err() == nil {
					// Restarting cannot fix this. Clips from other machines
					// still arrive, so stay up rather than fail.
					w.log.Error("clipboard is not watched", "type", kind.typ, "err", err)
					<-ctx.Done()
				}
				return err
			})
		})
	}
	wg.Wait()
	return ctx.Err()
}

// watchOnce runs one wl-paste watcher for typ until it exits.
func (w *wayland) watchOnce(ctx context.Context, typ string, limit int, valid func(string) bool, onCopy func(Content)) error {
	// wl-paste announces the existing content as its first event. Remember
	// what that is so it is not mistaken for a fresh copy.
	startup, _ := w.readType(ctx, typ)

	cmd := exec.CommandContext(ctx, "wl-paste",
		w.args("--type", typ, "--watch", "sh", "-c", watchScript(limit))...)
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
		data, ok, err := parseWatchLine(line, valid)
		if err != nil {
			fatal = err
			cmd.Process.Kill()
			break
		}
		if first {
			first = false
			if data == startup {
				continue
			}
		}
		if !ok {
			continue
		}
		if c, ok := w.content(ctx, typ, data); ok {
			onCopy(c)
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

// content turns what a watcher for typ was handed into what was copied. ok is
// false when this watcher is not the one to report it.
//
// A copy offered in several forms sets off several watchers. Only one may
// report it: files before text, text before a picture. See Content.
func (w *wayland) content(ctx context.Context, typ, data string) (c Content, ok bool) {
	switch typ {
	case filesType:
		files := parseURIList(data)
		return Content{Files: files}, len(files) > 0
	case imageType:
		if slices.Contains(w.types(ctx), textType) || len(w.readFiles(ctx)) > 0 {
			return Content{}, false
		}
		return Content{Image: []byte(data)}, true
	default:
		if len(w.readFiles(ctx)) > 0 {
			return Content{}, false
		}
		return Content{Text: data}, true
	}
}

// parseWatchLine decodes one line printed by watchScript. ok is false for
// content that must not be synced: what a password manager marked, and what
// valid turns down.
func parseWatchLine(line string, valid func(string) bool) (data string, ok bool, err error) {
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
	data = string(raw)
	return data, valid(data), nil
}
