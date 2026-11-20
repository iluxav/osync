package clipsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// board is a clipboard that records writes and can be made to fail.
type board struct {
	text   string
	writes []string
	fail   bool
}

func (b *board) Read(context.Context) (string, error) { return b.text, nil }

func (b *board) Write(_ context.Context, text string) error {
	if b.fail {
		return errors.New("no display")
	}
	b.text = text
	b.writes = append(b.writes, text)
	return nil
}

func (b *board) Watch(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}

// rig is an Engine with a clock the test moves and a record of what it sent.
type rig struct {
	*Engine
	board *board
	sent  []Clip
	clock time.Time
}

func newRig(origin string) *rig {
	r := &rig{board: &board{}, clock: time.UnixMilli(1_000_000)}
	r.Engine = New(r.board, func(c Clip) { r.sent = append(r.sent, c) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Engine.origin = origin
	r.Engine.now = func() time.Time { return r.clock }
	return r
}

func (r *rig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// copy acts as the user copying text: it lands on the clipboard and the
// watcher reports it.
func (r *rig) copy(text string) {
	r.board.text = text
	r.report(text)
}

// report acts as the watcher reporting text, which may or may not still be
// what the clipboard holds.
func (r *rig) report(text string) { r.local(ctx, text) }

var ctx = context.Background()

func TestLocalCopyIsSent(t *testing.T) {
	r := newRig("a")
	r.copy("hello")
	if len(r.sent) != 1 || r.sent[0].Text != "hello" || r.sent[0].Origin != "a" || r.sent[0].Stamp != 1_000_000 {
		t.Fatalf("sent = %+v, want one clip 'hello' from a stamped with the clock", r.sent)
	}
	if s := r.Stats(); s.Sent != 1 || s.Received != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestRemoteClipIsWrittenAndItsEchoIsNotSentBack(t *testing.T) {
	r := newRig("b")
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "from a"})
	if len(r.board.writes) != 1 || r.board.writes[0] != "from a" {
		t.Fatalf("writes = %q, want [from a]", r.board.writes)
	}
	// The watcher reports the write, and keeps doing so for as long as a
	// clipboard manager cares to re-announce it.
	r.report("from a")
	r.advance(time.Hour)
	r.report("from a")
	if len(r.sent) != 0 {
		t.Errorf("sent = %+v, want nothing: this text came from a peer", r.sent)
	}
}

func TestOlderRemoteClipLoses(t *testing.T) {
	r := newRig("b")
	r.copy("mine") // stamp 1_000_000
	r.Remote(ctx, Clip{Stamp: 999_999, Origin: "a", Text: "stale"})
	if len(r.board.writes) != 0 {
		t.Errorf("writes = %q, want none: the remote clip is older", r.board.writes)
	}
	r.Remote(ctx, Clip{Stamp: 1_000_001, Origin: "a", Text: "fresh"})
	if len(r.board.writes) != 1 || r.board.writes[0] != "fresh" {
		t.Errorf("writes = %q, want [fresh]", r.board.writes)
	}
}

func TestSimultaneousCopiesConverge(t *testing.T) {
	a, b := newRig("a"), newRig("b")
	a.copy("from a")
	b.copy("from b")
	// Same stamp on both; each receives the other's clip.
	a.Remote(ctx, b.sent[0])
	b.Remote(ctx, a.sent[0])
	// The higher origin wins on both machines.
	if len(a.board.writes) != 1 || a.board.writes[0] != "from b" {
		t.Errorf("a wrote %q, want [from b]", a.board.writes)
	}
	if len(b.board.writes) != 0 {
		t.Errorf("b wrote %q, want nothing", b.board.writes)
	}
}

func TestDuplicateRemoteClipIsIgnored(t *testing.T) {
	r := newRig("b")
	clip := Clip{Stamp: 5_000_000, Origin: "a", Text: "once"}
	r.Remote(ctx, clip)
	r.Remote(ctx, clip)
	if len(r.board.writes) != 1 {
		t.Errorf("writes = %q, want exactly one", r.board.writes)
	}
}

func TestLocalCopyWinsOverPeerWithFastClock(t *testing.T) {
	r := newRig("b")
	// A peer whose clock runs an hour ahead.
	future := uint64(r.clock.Add(time.Hour).UnixMilli())
	r.Remote(ctx, Clip{Stamp: future, Origin: "a", Text: "from the future"})
	r.advance(time.Minute)
	r.copy("copied later")
	if len(r.sent) != 1 || r.sent[0].Stamp <= future {
		t.Fatalf("sent = %+v, want a stamp past %d so the later copy wins", r.sent, future)
	}
}

func TestCopyingTheSameTextAgain(t *testing.T) {
	r := newRig("a")
	r.copy("bob")
	r.report("bob") // a clipboard manager re-announcing, moments later
	if len(r.sent) != 1 {
		t.Fatalf("sent %d clips, want 1: the repeat is a duplicate event", len(r.sent))
	}
	// Later the user copies it again. A peer may have copied a password in
	// between, which this machine never hears about, so it must go out.
	r.advance(echoWindow + time.Second)
	r.copy("bob")
	if len(r.sent) != 2 || !r.sent[1].newerThan(r.sent[0]) {
		t.Fatalf("sent = %+v, want a second, newer clip", r.sent)
	}
}

func TestRemoteClipWithSameTextIsWrittenAgain(t *testing.T) {
	r := newRig("b")
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "bob"})
	// Here the user copies a password: the engine is not told.
	r.Remote(ctx, Clip{Stamp: 6_000_000, Origin: "a", Text: "bob"})
	if len(r.board.writes) != 2 {
		t.Errorf("writes = %q, want bob twice: the clipboard may have held something else", r.board.writes)
	}
}

func TestFailedWriteDoesNotHideALaterCopy(t *testing.T) {
	r := newRig("b")
	r.board.fail = true
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "lost"})
	// The text never reached the clipboard, so seeing it there later means
	// the user copied it.
	r.copy("lost")
	if len(r.sent) != 1 {
		t.Errorf("sent = %+v, want the local copy to go out", r.sent)
	}
}

func TestRemoteClipsThatCannotBeSyncedAreDropped(t *testing.T) {
	r := newRig("b")
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: ""})
	r.Remote(ctx, Clip{Stamp: 5_000_001, Origin: "a", Text: string(make([]byte, 4<<20+1))})
	if len(r.board.writes) != 0 {
		t.Errorf("wrote %d clips, want none", len(r.board.writes))
	}
}

func TestLateReportsDoNotResurrectOldText(t *testing.T) {
	r := newRig("b")
	// Two clips arrive back to back; both are written before the watcher
	// gets round to reporting the first.
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "first"})
	r.Remote(ctx, Clip{Stamp: 5_000_001, Origin: "a", Text: "second"})
	r.report("first")
	r.report("second")
	if len(r.sent) != 0 {
		t.Errorf("sent = %+v, want nothing: sending 'first' back would undo 'second' everywhere", r.sent)
	}
}

func TestCopyOverwrittenBeforeItWasSeenIsDropped(t *testing.T) {
	r := newRig("b")
	// The user copies, and a peer's clip lands on the clipboard before the
	// watcher reports the copy.
	r.board.text = "local"
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "remote"})
	r.report("local")
	r.report("remote")
	if len(r.sent) != 0 {
		t.Errorf("sent = %+v, want nothing: the clipboard holds the peer's text", r.sent)
	}
}

func TestCopyRightAfterARemoteWriteIsSent(t *testing.T) {
	r := newRig("b")
	r.Remote(ctx, Clip{Stamp: 5_000_000, Origin: "a", Text: "remote"})
	r.board.text = "local" // copied before the write was reported
	r.report("remote")
	r.report("local")
	if len(r.sent) != 1 || r.sent[0].Text != "local" || r.sent[0].Stamp <= 5_000_000 {
		t.Errorf("sent = %+v, want 'local' stamped after the remote clip", r.sent)
	}
}
