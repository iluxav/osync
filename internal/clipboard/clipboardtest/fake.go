// Package clipboardtest provides an in-memory clipboard.Clipboard for tests.
package clipboardtest

import (
	"context"
	"sync"

	"github.com/iluxav/osync/internal/clipboard"
)

// Fake is an in-memory clipboard. Like a real one, it reports content placed
// by Write back through Watch, asynchronously.
type Fake struct {
	events chan clipboard.Content

	mu      sync.Mutex
	content clipboard.Content
	writes  int
}

// New returns an empty Fake.
func New() *Fake { return &Fake{events: make(chan clipboard.Content, 256)} }

// Copy acts as the user copying text on this machine.
func (f *Fake) Copy(text string) { f.set(clipboard.Content{Text: text}) }

// CopyFiles acts as the user copying files or folders in a file manager.
func (f *Fake) CopyFiles(paths ...string) { f.set(clipboard.Content{Files: paths}) }

// CopyImage acts as the user taking a screenshot to the clipboard.
func (f *Fake) CopyImage(png []byte) { f.set(clipboard.Content{Image: png}) }

func (f *Fake) set(c clipboard.Content) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content = c
	f.events <- c // under the lock: events arrive in the order content changed
}

// CopyPrivate acts as the user copying something that is never synced, such
// as a password: the content changes but Watch stays silent.
func (f *Fake) CopyPrivate(text string) {
	f.mu.Lock()
	f.content = clipboard.Content{Text: text}
	f.mu.Unlock()
}

// Text returns the text on the clipboard, which is empty while it holds
// files or a picture.
func (f *Fake) Text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content.Text
}

// Files returns the files on the clipboard.
func (f *Fake) Files() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content.Files
}

// Image returns the picture on the clipboard.
func (f *Fake) Image() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content.Image
}

// Writes returns how many times Write was called.
func (f *Fake) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// Read implements clipboard.Clipboard.
func (f *Fake) Read(context.Context) (clipboard.Content, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content, nil
}

// Write implements clipboard.Clipboard.
func (f *Fake) Write(_ context.Context, c clipboard.Content) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content = c
	f.writes++
	f.events <- c
	return nil
}

// Watch implements clipboard.Clipboard.
func (f *Fake) Watch(ctx context.Context, onCopy func(clipboard.Content)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c := <-f.events:
			onCopy(c)
		}
	}
}
