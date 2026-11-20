// Package clipsync decides which clipboard content wins. It turns local copies
// into clips for other machines and applies the clips they send.
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

// echoWindow is how long after text is copied here the same text being
// reported again is taken for a duplicate rather than the user copying it
// once more. It covers clipboard managers that re-announce content they have
// just taken over.
const echoWindow = 2 * time.Second

// Clip is one piece of copied text on its way between machines.
type Clip struct {
	// Stamp orders clips: the newest one wins everywhere. It is the copy time
	// in Unix milliseconds, pushed forward when needed to stay ahead of every
	// clip this machine has already seen, so that a clock running behind
	// cannot make a new copy lose to an old one.
	Stamp uint64 `json:"stamp"`
	// Origin identifies the daemon that produced the clip. It breaks ties
	// between clips with equal stamps and has no other meaning.
	Origin string `json:"origin"`
	Text   string `json:"text"`
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
	send   func(Clip)
	log    *slog.Logger
	now    func() time.Time
	origin string

	mu       sync.Mutex
	clock    uint64    // highest stamp seen or issued
	cur      Clip      // the clip this machine last adopted; Text is not kept
	curHash  [32]byte  // hash of that clip's text; zero when the clipboard may not hold it
	curSince time.Time // when it was adopted
	stats    Stats
}

// New returns an Engine for cb that hands every local copy to send.
func New(cb clipboard.Clipboard, send func(Clip), log *slog.Logger) *Engine {
	id := make([]byte, 8)
	rand.Read(id)
	return &Engine{cb: cb, send: send, log: log, now: time.Now, origin: hex.EncodeToString(id)}
}

// Run watches the local clipboard until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	return e.cb.Watch(ctx, func(text string) { e.local(ctx, text) })
}

// local handles the watcher reporting text on this machine's clipboard.
func (e *Engine) local(ctx context.Context, text string) {
	hash := sha256.Sum256([]byte(text))

	// Held throughout, so no clip from a peer is written between looking at
	// the clipboard and deciding, and clips are sent in stamp order.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Reports can lag behind the clipboard. Acting on one that no longer
	// describes it would resurrect old text: a report of our own write
	// arriving after a newer clip was adopted would look like a fresh copy,
	// and two machines doing that to each other never stop. Whatever
	// replaced this text is either ours or gets its own report.
	if current, err := e.cb.Read(ctx); err != nil || current != text {
		return
	}

	now := e.now()
	if hash == e.curHash && (e.cur.Origin != e.origin || now.Sub(e.curSince) < echoWindow) {
		// The clipboard holds what this machine already adopted. If that
		// came from a peer, this is our own write being reported back. If it
		// was copied here, it is a duplicate report unless enough time has
		// passed for it to be the user copying the same text again, which is
		// worth resending: a peer may have moved on to something that is not
		// synced, such as a password.
		return
	}
	e.clock = max(e.clock+1, uint64(now.UnixMilli()))
	clip := Clip{Stamp: e.clock, Origin: e.origin, Text: text}
	e.adoptLocked(clip, hash, now)
	e.stats.Sent++
	e.stats.LastSent = now
	e.log.Info("clipboard sent", "bytes", len(text))
	e.send(clip)
}

// Remote handles a clip sent by another machine. It becomes the clipboard
// content unless this machine already holds something newer.
func (e *Engine) Remote(ctx context.Context, clip Clip) {
	if clip.Text == "" || len(clip.Text) > clipboard.MaxBytes {
		return
	}
	hash := sha256.Sum256([]byte(clip.Text))

	// The lock is held across the write so that two clips arriving at once
	// reach the clipboard in the order they were judged.
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = max(e.clock, clip.Stamp)
	if !clip.newerThan(e.cur) {
		return
	}
	now := e.now()
	e.adoptLocked(clip, hash, now)
	e.stats.Received++
	e.stats.LastReceived = now
	// Written even when the text matches what was adopted before: the
	// clipboard may since have been replaced by content that is not watched.
	if err := e.cb.Write(ctx, clip.Text); err != nil {
		e.curHash = [32]byte{}
		e.log.Error("clipboard write failed", "err", err)
		return
	}
	e.log.Info("clipboard received", "bytes", len(clip.Text))
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
