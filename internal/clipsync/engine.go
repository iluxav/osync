// Package clipsync decides which clipboard content wins. It turns local copies
// into clips for other machines and applies the clips they send. A copy is
// text, files or a picture; the rules are the same for all three.
package clipsync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/iluxav/osync/internal/clipboard"
)

// echoWindow is how long after something is copied here the same content
// being reported again is taken for a duplicate rather than the user copying
// it once more. It covers clipboard managers that re-announce content they
// have just taken over.
const echoWindow = 2 * time.Second

// Clip is one copy on its way between machines.
type Clip struct {
	// Stamp orders clips: the newest one wins everywhere. It is the copy time
	// in Unix milliseconds, pushed forward when needed to stay ahead of every
	// clip this machine has already seen, so that a clock running behind
	// cannot make a new copy lose to an old one.
	Stamp uint64 `json:"stamp"`
	// Origin identifies the daemon that produced the clip. It breaks ties
	// between clips with equal stamps and has no other meaning.
	Origin string `json:"origin"`
	// Text is the copied text. A clip of files or of a picture has none:
	// those travel beside the clip, and reach the engine through RemoteFiles
	// and RemoteImage.
	Text string `json:"text,omitempty"`
}

func (c Clip) newerThan(o Clip) bool {
	return c.Stamp > o.Stamp || (c.Stamp == o.Stamp && c.Origin > o.Origin)
}

// Stats counts what the engine has done since it started.
type Stats struct {
	Sent         int       `json:"sent"`
	Received     int       `json:"received"`
	LastSent     time.Time `json:"lastSent,omitzero"`
	LastReceived time.Time `json:"lastReceived,omitzero"`
}

// Engine keeps the local clipboard and the other machines in step.
type Engine struct {
	cb     clipboard.Clipboard
	send   func(Clip, clipboard.Content) bool
	log    *slog.Logger
	now    func() time.Time
	origin string

	mu       sync.Mutex
	clock    uint64    // highest stamp seen or issued
	cur      Clip      // the clip this machine last adopted; Text is not kept
	curHash  [32]byte  // hash of that clip's content; zero when the clipboard may not hold it
	curSince time.Time // when it was adopted
	stats    Stats
}

// New returns an Engine for cb that hands every local copy to send, as the
// clip to pass on and the content it stands for. send reports whether the
// copy is on its way. When it is not, because the files are too large to
// send for one, the copy is treated as never made: a clip that the other
// machines have not heard of must not outrank what they send next.
func New(cb clipboard.Clipboard, send func(Clip, clipboard.Content) bool, log *slog.Logger) *Engine {
	id := make([]byte, 8)
	rand.Read(id)
	return &Engine{cb: cb, send: send, log: log, now: time.Now, origin: hex.EncodeToString(id)}
}

// Run watches the local clipboard until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	return e.cb.Watch(ctx, func(c clipboard.Content) { e.local(ctx, c) })
}

// hashOf identifies content. Files are identified by their paths: the same
// files copied again are the same content, whatever was done to them since.
func hashOf(c clipboard.Content) [32]byte {
	h := sha256.New()
	switch {
	case len(c.Files) > 0:
		h.Write([]byte("\x00files"))
		for _, path := range c.Files {
			h.Write([]byte{0})
			h.Write([]byte(path))
		}
	case len(c.Image) > 0:
		h.Write([]byte("\x00image\x00"))
		h.Write(c.Image)
	default:
		h.Write([]byte(c.Text))
	}
	return [32]byte(h.Sum(nil))
}

// local handles the watcher reporting a copy on this machine's clipboard.
func (e *Engine) local(ctx context.Context, content clipboard.Content) {
	hash := hashOf(content)

	// Held throughout, so no clip from a peer is written between looking at
	// the clipboard and deciding, and clips are sent in stamp order.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Reports can lag behind the clipboard. Acting on one that no longer
	// describes it would resurrect old content: a report of our own write
	// arriving after a newer clip was adopted would look like a fresh copy,
	// and two machines doing that to each other never stop. Whatever
	// replaced this content is either ours or gets its own report.
	if current, err := e.cb.Read(ctx); err != nil || !current.Equal(content) {
		return
	}

	now := e.now()
	if hash == e.curHash && (e.cur.Origin != e.origin || now.Sub(e.curSince) < echoWindow) {
		// The clipboard holds what this machine already adopted. If that
		// came from a peer, this is our own write being reported back. If it
		// was copied here, it is a duplicate report unless enough time has
		// passed for it to be the user copying the same thing again, which
		// is worth resending: a peer may have moved on to something that is
		// not synced, such as a password, and a file may have been edited.
		return
	}
	clip := Clip{Stamp: max(e.clock+1, uint64(now.UnixMilli())), Origin: e.origin, Text: content.Text}
	if !e.send(clip, content) {
		return
	}
	e.clock = clip.Stamp
	e.adoptLocked(clip, hash, now)
	e.stats.Sent++
	e.stats.LastSent = now
	if content.Text != "" {
		e.log.Info("clipboard sent", "bytes", len(content.Text))
	}
}

// Remote handles a clip of text sent by another machine. It becomes the
// clipboard content unless this machine already holds something newer, and
// the result says whether it did.
func (e *Engine) Remote(ctx context.Context, clip Clip) bool {
	if clip.Text == "" || len(clip.Text) > clipboard.MaxBytes {
		return false
	}
	return e.remote(ctx, clip, clipboard.Content{Text: clip.Text})
}

// RemoteFiles handles a clip of files sent by another machine, once the
// files have arrived and are at paths. Like Remote, it reports whether the
// clip was adopted.
func (e *Engine) RemoteFiles(ctx context.Context, clip Clip, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	return e.remote(ctx, clip, clipboard.Content{Files: paths})
}

// RemoteImage handles a clip of a picture sent by another machine, once the
// picture has arrived. Like Remote, it reports whether the clip was adopted.
func (e *Engine) RemoteImage(ctx context.Context, clip Clip, png []byte) bool {
	if !clipboard.ValidImage(png) {
		return false
	}
	return e.remote(ctx, clip, clipboard.Content{Image: png})
}

// Expect is told about a clip whose files or picture are only now starting
// to arrive, and reports whether it would be adopted if they were here
// already. Hearing of the clip early keeps a copy made on this machine in
// the meantime ahead of it.
func (e *Engine) Expect(clip Clip) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = max(e.clock, clip.Stamp)
	return clip.newerThan(e.cur)
}

func (e *Engine) remote(ctx context.Context, clip Clip, content clipboard.Content) bool {
	hash := hashOf(content)

	// The lock is held across the write so that two clips arriving at once
	// reach the clipboard in the order they were judged.
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = max(e.clock, clip.Stamp)
	if !clip.newerThan(e.cur) {
		return false
	}
	now := e.now()
	e.adoptLocked(clip, hash, now)
	e.stats.Received++
	e.stats.LastReceived = now
	// Written even when the content matches what was adopted before: the
	// clipboard may since have been replaced by content that is not watched.
	if err := e.cb.Write(ctx, content); err != nil {
		e.curHash = [32]byte{}
		e.log.Error("clipboard write failed", "err", err)
		return true
	}
	if content.Text != "" {
		e.log.Info("clipboard received", "bytes", len(content.Text))
	}
	return true
}

func (e *Engine) adoptLocked(clip Clip, hash [32]byte, now time.Time) {
	clip.Text = ""
	e.cur, e.curHash, e.curSince = clip, hash, now
}

// Stats returns the counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats
}
