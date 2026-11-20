// Package clipboard reads and writes the system clipboard of the user's
// desktop session.
package clipboard

import (
	"context"
	"time"
	"unicode/utf8"
)

// toolTimeout bounds one run of a clipboard tool. Reading the clipboard means
// asking the application that owns it, and a frozen application must not be
// able to stall the daemon.
const toolTimeout = 5 * time.Second

// MaxBytes is the largest text that is synced.
const MaxBytes = 4 << 20

// Clipboard is the system clipboard, reduced to what sync needs.
type Clipboard interface {
	// Read returns the text on the clipboard right now. It fails when the
	// clipboard holds no text.
	Read(ctx context.Context) (string, error)

	// Write replaces the clipboard content with text.
	Write(ctx context.Context, text string) error

	// Watch calls onText with the new content each time text that may be
	// synced lands on the clipboard, until ctx is done. It never reports the
	// content that was already there when it started, nor content that must
	// stay on this machine: anything a password manager marked as concealed,
	// non-text content, and text larger than MaxBytes.
	//
	// Text placed by Write is reported like any other.
	Watch(ctx context.Context, onText func(text string)) error
}

// syncable reports whether text is something to send to other machines.
func syncable(text string) bool {
	return text != "" && len(text) <= MaxBytes && utf8.ValidString(text)
}
