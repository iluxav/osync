// Package clipboard reads and writes the system clipboard of the user's
// desktop session.
package clipboard

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// toolTimeout bounds one run of a clipboard tool. Reading the clipboard means
// asking the application that owns it, and a frozen application must not be
// able to stall the daemon.
const toolTimeout = 5 * time.Second

// MaxBytes is the largest text that is synced.
const MaxBytes = 4 << 20

// MaxImageBytes is the largest picture that is synced. A screenshot of a 5K
// display full of photographs stays under it.
const MaxImageBytes = 32 << 20

// Content is what a clipboard holds, which is one of three things: text,
// files copied in a file manager, or a picture.
//
// A copy is often offered in several forms at once, and then it is the
// first of these that it has: a file manager offers the paths of copied
// files as text, and a spreadsheet offers a picture of copied cells next to
// their content.
type Content struct {
	Text string
	// Files are the absolute paths of the copied files and folders.
	Files []string
	// Image is a picture that is on the clipboard as data and not as a file,
	// such as a screenshot. It is a PNG.
	Image []byte
}

// Equal reports whether c and o hold the same thing.
func (c Content) Equal(o Content) bool {
	return c.Text == o.Text && slices.Equal(c.Files, o.Files) && bytes.Equal(c.Image, o.Image)
}

// ValidImage reports whether data is a picture that may be synced: a PNG of
// no more than MaxImageBytes. Only the signature is looked at; whether the
// rest makes a picture is for the program it is pasted into.
func ValidImage(data []byte) bool {
	return len(data) <= MaxImageBytes && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
}

// Clipboard is the system clipboard, reduced to what sync needs.
type Clipboard interface {
	// Read returns what is on the clipboard right now. It fails when the
	// clipboard holds none of the three things Content can be.
	Read(ctx context.Context) (Content, error)

	// Write replaces the clipboard content with c.
	Write(ctx context.Context, c Content) error

	// Watch calls onCopy with the new content each time something that may
	// be synced lands on the clipboard, until ctx is done. Calls do not
	// overlap. It never reports the content that was already there when it
	// started, nor content that must stay on this machine: anything a
	// password manager marked as concealed, content that is neither text,
	// files nor a picture, and text or pictures over their size limits.
	//
	// Content placed by Write is reported like any other.
	Watch(ctx context.Context, onCopy func(Content)) error
}

// syncable reports whether text is something to send to other machines.
func syncable(text string) bool {
	return text != "" && len(text) <= MaxBytes && utf8.ValidString(text)
}

// keepRunning runs watch again each time it stops, until ctx is done. The
// pause between runs doubles while watch keeps failing soon after it starts.
func keepRunning(ctx context.Context, log *slog.Logger, what string, watch func() error) error {
	delay := time.Second
	for {
		started := time.Now()
		err := watch()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		log.Warn(what+" stopped, restarting", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// parseURIList returns the paths named by a text/uri-list, the form in which
// file managers put copied files on the clipboard. It returns nil unless
// every entry is a file on this machine: a list of web links is not a copy of
// files.
func parseURIList(list string) []string {
	var paths []string
	for line := range strings.SplitSeq(list, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
			return nil
		}
		paths = append(paths, filepath.Clean(u.Path))
	}
	return paths
}

// uriList is the inverse of parseURIList.
func uriList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString((&url.URL{Scheme: "file", Path: p}).String())
		b.WriteString("\r\n")
	}
	return b.String()
}
