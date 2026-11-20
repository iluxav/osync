// Package clipboardtest provides an in-memory clipboard.Clipboard for tests.
package clipboardtest

import (
	"context"
	"sync"
)

// Fake is an in-memory clipboard. Like a real one, it reports text placed by
// Write back through Watch, asynchronously.
type Fake struct {
	events chan string

	mu     sync.Mutex
	text   string
	writes int
}

// New returns an empty Fake.
func New() *Fake { return &Fake{events: make(chan string, 256)} }

// Copy acts as the user copying text on this machine.
func (f *Fake) Copy(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = text
	f.events <- text // under the lock: events arrive in the order content changed
}

// CopyPrivate acts as the user copying something that is never synced, such
// as a password: the content changes but Watch stays silent.
func (f *Fake) CopyPrivate(text string) {
	f.mu.Lock()
	f.text = text
	f.mu.Unlock()
}

// Text returns the current content.
func (f *Fake) Text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

// Writes returns how many times Write was called.
func (f *Fake) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// Read implements clipboard.Clipboard.
func (f *Fake) Read(context.Context) (string, error) {
	return f.Text(), nil
}

// Write implements clipboard.Clipboard.
func (f *Fake) Write(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = text
	f.writes++
	f.events <- text
	return nil
}

// Watch implements clipboard.Clipboard.
func (f *Fake) Watch(ctx context.Context, onText func(string)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case text := <-f.events:
			onText(text)
		}
	}
}
