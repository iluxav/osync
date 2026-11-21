package clipsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iluxav/osync/internal/clipboard"
)

// board is a clipboard that records writes and can be made to fail.
type board struct {
	text   string
	files  []string
	image  []byte
	writes []string // what was written; files as their paths joined by commas, a picture as its bytes
	fail   bool
}

func (b *board) Read(context.Context) (clipboard.Content, error) {
	return clipboard.Content{Text: b.text, Files: b.files, Image: b.image}, nil
}

func (b *board) Write(_ context.Context, c clipboard.Content) error {
	if b.fail {
		return errors.New("no display")
	}
	b.text, b.files, b.image = c.Text, c.Files, c.Image
	b.writes = append(b.writes, c.Text+strings.Join(c.Files, ",")+string(c.Image))
	return nil
}

func (b *board) Watch(ctx context.Context, _ func(clipboard.Content)) error {
	<-ctx.Done()
	return ctx.Err()
}

// rig is an Engine with a clock the test moves and a record of what it sent.
type rig struct {
	*Engine
	board      *board
	sent       []Clip
	sentFiles  [][]string // the files each sent clip stood for; nil for text
	unsendable bool       // send refuses, as it does for files over the limit
	clock      time.Time
}

func newRig(origin string) *rig {
	r := &rig{board: &board{}, clock: time.UnixMilli(1_000_000)}
	send := func(c Clip, content clipboard.Content) bool {
		if r.unsendable {
			return false
		}
		r.sent = append(r.sent, c)
		r.sentFiles = append(r.sentFiles, content.Files)
		return true
	}
	r.Engine = New(r.board, send, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Engine.origin = origin
	r.Engine.now = func() time.Time { return r.clock }
	return r
}

func (r *rig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// copy acts as the user copying text: it lands on the clipboard and the
// watcher reports it.
func (r *rig) copy(text string) {
	r.board.text, r.board.files, r.board.image = text, nil, nil
	r.report(text)
}

// report acts as the watcher reporting text, which may or may not still be
// what the clipboard holds.
func (r *rig) report(text string) { r.local(ctx, clipboard.Content{Text: text}) }

// copyFiles acts as the user copying files in a file manager.
func (r *rig) copyFiles(paths ...string) {
	r.board.text, r.board.files, r.board.image = "", paths, nil
	r.reportFiles(paths...)
}

// copyImage acts as the user taking a screenshot to the clipboard.
func (r *rig) copyImage(png string) {
	r.board.text, r.board.files, r.board.image = "", nil, []byte(png)
	r.local(ctx, clipboard.Content{Image: []byte(png)})
}

// A PNG as far as its signature goes, which is as far as anything here looks.
func png(pixels string) string { return "\x89PNG\r\n\x1a\n" + pixels }

func (r *rig) reportFiles(paths ...string) { r.local(ctx, clipboard.Content{Files: paths}) }

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

func TestCopiedFilesAreSentWithoutText(t *testing.T) {
	r := newRig("a")
	r.copyFiles("/home/me/report.pdf", "/home/me/photos")
	if len(r.sent) != 1 || r.sent[0].Text != "" || r.sent[0].Origin != "a" {
		t.Fatalf("sent = %+v, want one clip without text", r.sent)
	}
	if want := []string{"/home/me/report.pdf", "/home/me/photos"}; !slices.Equal(r.sentFiles[0], want) {
		t.Errorf("the clip stood for %q, want %q", r.sentFiles[0], want)
	}
}

func TestReceivedFilesAreWrittenAndTheirEchoIsNotSentBack(t *testing.T) {
	r := newRig("b")
	if !r.RemoteFiles(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []string{"/inbox/1/report.pdf"}) {
		t.Fatal("RemoteFiles = false, want the clip adopted")
	}
	if len(r.board.writes) != 1 || r.board.writes[0] != "/inbox/1/report.pdf" {
		t.Fatalf("writes = %q, want the received file", r.board.writes)
	}
	r.reportFiles("/inbox/1/report.pdf")
	r.advance(time.Hour)
	r.reportFiles("/inbox/1/report.pdf")
	if len(r.sent) != 0 {
		t.Errorf("sent = %+v, want nothing: these files came from a peer", r.sent)
	}
}

func TestFilesAndTextFollowTheSameOrder(t *testing.T) {
	r := newRig("b")
	r.copy("mine") // stamp 1_000_000
	if r.RemoteFiles(ctx, Clip{Stamp: 999_999, Origin: "a"}, []string{"/inbox/1/old.txt"}) {
		t.Error("files older than the local copy were adopted")
	}
	if !r.RemoteFiles(ctx, Clip{Stamp: 1_000_001, Origin: "a"}, []string{"/inbox/2/new.txt"}) {
		t.Error("files newer than the local copy were not adopted")
	}
	if !r.Remote(ctx, Clip{Stamp: 1_000_002, Origin: "a", Text: "text after files"}) {
		t.Error("text newer than the files was not adopted")
	}
	if want := []string{"/inbox/2/new.txt", "text after files"}; !slices.Equal(r.board.writes, want) {
		t.Errorf("writes = %q, want %q", r.board.writes, want)
	}
}

// Files take time to arrive. A copy made here while they are on their way
// is the newer one, and must stay ahead of them.
func TestCopyMadeWhileFilesArriveWins(t *testing.T) {
	r := newRig("b")
	// A peer whose clock runs an hour ahead starts sending files.
	future := Clip{Stamp: uint64(r.clock.Add(time.Hour).UnixMilli()), Origin: "a"}
	if !r.Expect(future) {
		t.Fatal("Expect = false, want the files wanted: nothing newer is held")
	}
	r.copy("copied meanwhile")
	if len(r.sent) != 1 || r.sent[0].Stamp <= future.Stamp {
		t.Fatalf("sent = %+v, want a stamp past the files' %d", r.sent, future.Stamp)
	}
	if r.RemoteFiles(ctx, future, []string{"/inbox/1/late.bin"}) {
		t.Error("the files were adopted over the newer local copy")
	}
	if r.Expect(future) {
		t.Error("Expect = true for a clip older than what is held")
	}
}

func TestCopyThatCannotBeSentLeavesNoTrace(t *testing.T) {
	r := newRig("b")
	r.unsendable = true
	r.copyFiles("/home/me/huge.iso")
	if s := r.Stats(); s.Sent != 0 {
		t.Errorf("stats = %+v, want nothing counted as sent", s)
	}
	// A peer whose clock is a little behind copies next. Had the unsent
	// copy been adopted, it would outrank this clip here and nowhere else.
	if !r.Remote(ctx, Clip{Stamp: 999_000, Origin: "a", Text: "from a"}) {
		t.Error("a peer's clip lost to a copy that was never sent")
	}
}

func TestCopyingTheSameFilesAgain(t *testing.T) {
	r := newRig("a")
	r.copyFiles("/home/me/draft.txt")
	r.reportFiles("/home/me/draft.txt") // re-announced moments later
	if len(r.sent) != 1 {
		t.Fatalf("sent %d clips, want 1: the repeat is a duplicate event", len(r.sent))
	}
	// The user edits the file and copies it again: same path, new content.
	r.advance(echoWindow + time.Second)
	r.copyFiles("/home/me/draft.txt")
	if len(r.sent) != 2 {
		t.Fatalf("sent %d clips, want 2: the file may have changed", len(r.sent))
	}
}

func TestTextThatSpellsAPathIsNotMistakenForTheFile(t *testing.T) {
	r := newRig("b")
	r.RemoteFiles(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []string{"/inbox/1/a.txt"})
	// The user copies the path as text. That is a new copy, not an echo.
	r.copy("/inbox/1/a.txt")
	if len(r.sent) != 1 || r.sent[0].Text != "/inbox/1/a.txt" {
		t.Errorf("sent = %+v, want the text sent", r.sent)
	}
}

func TestScreenshotIsSentAndTheNextOneToo(t *testing.T) {
	r := newRig("a")
	r.copyImage(png("first"))
	if len(r.sent) != 1 || r.sent[0].Text != "" {
		t.Fatalf("sent = %+v, want one clip without text", r.sent)
	}
	// A clipboard manager takes the picture over and announces it again.
	r.local(ctx, clipboard.Content{Image: []byte(png("first"))})
	if len(r.sent) != 1 {
		t.Fatalf("sent %d clips, want 1: the repeat is a duplicate event", len(r.sent))
	}
	r.copyImage(png("second"))
	if len(r.sent) != 2 || !r.sent[1].newerThan(r.sent[0]) {
		t.Fatalf("sent = %+v, want a second, newer clip for the next screenshot", r.sent)
	}
}

func TestReceivedPictureIsWrittenAndItsEchoIsNotSentBack(t *testing.T) {
	r := newRig("b")
	if !r.RemoteImage(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []byte(png("from a"))) {
		t.Fatal("RemoteImage = false, want the clip adopted")
	}
	if len(r.board.writes) != 1 || r.board.writes[0] != png("from a") {
		t.Fatalf("wrote %d things, want the picture", len(r.board.writes))
	}
	r.local(ctx, clipboard.Content{Image: []byte(png("from a"))})
	r.advance(time.Hour)
	r.local(ctx, clipboard.Content{Image: []byte(png("from a"))})
	if len(r.sent) != 0 {
		t.Errorf("sent = %+v, want nothing: this picture came from a peer", r.sent)
	}
}

func TestPictureThatIsNotOneIsDropped(t *testing.T) {
	r := newRig("b")
	if r.RemoteImage(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []byte("#!/bin/sh")) {
		t.Error("something that is not a PNG was adopted as a picture")
	}
	if r.RemoteImage(ctx, Clip{Stamp: 5_000_001, Origin: "a"}, nil) {
		t.Error("an empty picture was adopted")
	}
	if len(r.board.writes) != 0 {
		t.Errorf("wrote %d things, want none", len(r.board.writes))
	}
}

func TestPicturesTextAndFilesReplaceEachOtherInOrder(t *testing.T) {
	r := newRig("b")
	r.RemoteImage(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []byte(png("one")))
	r.Remote(ctx, Clip{Stamp: 5_000_001, Origin: "a", Text: "two"})
	r.RemoteImage(ctx, Clip{Stamp: 4_000_000, Origin: "a"}, []byte(png("stale")))
	r.RemoteFiles(ctx, Clip{Stamp: 5_000_002, Origin: "a"}, []string{"/inbox/1/three.txt"})
	if want := []string{png("one"), "two", "/inbox/1/three.txt"}; !slices.Equal(r.board.writes, want) {
		t.Errorf("writes = %q, want %q", r.board.writes, want)
	}
}

func TestTextThatSpellsAPictureIsNotMistakenForIt(t *testing.T) {
	r := newRig("b")
	r.RemoteImage(ctx, Clip{Stamp: 5_000_000, Origin: "a"}, []byte(png("pixels")))
	r.copy(png("pixels"))
	if len(r.sent) != 1 {
		t.Errorf("sent = %+v, want the text sent: it is a new copy, not an echo", r.sent)
	}
}
