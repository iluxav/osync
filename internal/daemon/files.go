package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"

	"github.com/iluxav/osync/internal/bundle"
	"github.com/iluxav/osync/internal/clipboard"
	"github.com/iluxav/osync/internal/clipsync"
	"github.com/iluxav/osync/internal/mesh"
	"github.com/iluxav/osync/internal/tailnet"
)

// How many received bundles stay in the inbox. The newest is what the
// clipboard points at; the one before may still be in the middle of being
// pasted somewhere.
const bundlesKept = 2

// FileStats counts the copies of files that were synced since the daemon
// started.
type FileStats struct {
	Sent     int `json:"sent"`
	Received int `json:"received"`
	// Problem says why the latest copy of files did not go out or come in.
	// The next one that does clears it.
	Problem string `json:"problem,omitempty"`
}

// fileSync sends what is too large for a message, copied files and pictures,
// to the other machines, and keeps the files they send.
//
// Files travel as a "files" message announcing the clip and what it holds,
// the bundle as a stream, and a "files-end" message; "files-abort" takes the
// place of the last when the bundle stops short. A picture travels the same
// way under "image", "image-end" and "image-abort", its stream being the PNG
// itself.
type fileSync struct {
	inbox *bundle.Inbox // nil when files are not synced
	mesh  *mesh.Mesh
	log   *slog.Logger

	mu    sync.Mutex
	stats FileStats
}

// send starts sending the files at paths to every peer as clip, and reports
// whether they are on their way.
func (f *fileSync) send(clip clipsync.Clip, paths []string) bool {
	if f.inbox == nil {
		return false
	}
	// What is in the inbox came from a peer and is on the clipboard because
	// this daemon put it there. It is never sent back, even if the clipboard
	// should name the files in a way the engine does not recognize as its
	// own write.
	if !slices.ContainsFunc(paths, func(p string) bool { return !f.inbox.Contains(p) }) {
		return false
	}
	sum, err := bundle.Scan(paths)
	if err != nil {
		f.problem(fmt.Sprintf("%s not sent: %v", describe(paths), err))
		return false
	}
	announce := func(typ string, sum bundle.Summary) []byte {
		payload, _ := json.Marshal(message{Type: typ, Clip: clip, Summary: sum})
		return payload
	}
	peers := f.mesh.Stream(&mesh.Stream{
		Head:  announce("files", sum),
		Open:  func() (io.ReadCloser, error) { return bundle.Open(paths), nil },
		Tail:  announce("files-end", bundle.Summary{}),
		Abort: announce("files-abort", bundle.Summary{}),
	})
	if peers == 0 && f.connected() > 0 {
		f.problem(fmt.Sprintf("%s not sent: the other machines run an osync from before it could send files; update them", describe(paths)))
		return false
	}
	f.mu.Lock()
	f.stats.Sent++
	f.stats.Problem = ""
	f.mu.Unlock()
	f.log.Info("files sent", "files", describe(paths), "bytes", sum.Bytes, "peers", peers)
	return true
}

// connected returns how many peers there is a connection to.
func (f *fileSync) connected() int {
	n := 0
	for _, p := range f.mesh.Status().Peers {
		if p.Connected {
			n++
		}
	}
	return n
}

// sendImage starts sending a picture to every peer as clip.
func (f *fileSync) sendImage(clip clipsync.Clip, png []byte) bool {
	announce := func(typ string, size int) []byte {
		payload, _ := json.Marshal(message{Type: typ, Clip: clip, Summary: bundle.Summary{Bytes: int64(size)}})
		return payload
	}
	peers := f.mesh.Stream(&mesh.Stream{
		Head:  announce("image", len(png)),
		Open:  func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(png)), nil },
		Tail:  announce("image-end", 0),
		Abort: announce("image-abort", 0),
	})
	f.log.Info("picture sent", "bytes", len(png), "peers", peers)
	return true
}

// superseded is called when this machine has taken up a newer clip. Files
// still on their way out are older than it and would lose everywhere.
func (f *fileSync) superseded() {
	f.mesh.Stream(nil)
}

func (f *fileSync) problem(what string) {
	f.log.Warn(what)
	f.mu.Lock()
	f.stats.Problem = what
	f.mu.Unlock()
}

func (f *fileSync) snapshot() FileStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

// describe names files for a person: the first, and how many more.
func describe(paths []string) string {
	if len(paths) == 0 {
		return "files"
	}
	first := filepath.Base(paths[0])
	if len(paths) == 1 {
		return first
	}
	return fmt.Sprintf("%s and %d more", first, len(paths)-1)
}

// session receives what one connection to a peer delivers.
type session struct {
	ctx    context.Context
	peer   tailnet.Node
	engine *clipsync.Engine
	files  *fileSync
	log    *slog.Logger

	// What the peer is in the middle of sending, if anything.
	arriving *arrival
}

// arrival is files or a picture on their way from a peer.
type arrival struct {
	clip clipsync.Clip
	sum  bundle.Summary
	end  string // the message that will say everything is here
	// Where the pieces go. Both are nil when the pieces are not wanted:
	// they still arrive, and are dropped.
	recv  *bundle.Receiver // for files
	image []byte           // for a picture; non-nil even while empty
}

// Message implements mesh.Handler.
func (s *session) Message(payload []byte) {
	var msg message
	if err := json.Unmarshal(payload, &msg); err != nil {
		s.log.Warn("unreadable message", "peer", s.peer.Name, "err", err)
		return
	}
	// Unknown types are ignored so that newer builds can add some.
	switch msg.Type {
	case "clip":
		if s.engine.Remote(s.ctx, msg.Clip) {
			s.files.superseded()
		}
	case "files":
		s.beginFiles(msg)
	case "image":
		s.beginImage(msg)
	case "files-end", "image-end":
		s.finish(msg.Type, msg.Clip)
	case "files-abort", "image-abort":
		s.abandon()
	}
}

// beginFiles handles the announcement of files: their pieces come next.
func (s *session) beginFiles(msg message) {
	s.abandon() // the peer has moved on to these
	a := &arrival{clip: msg.Clip, sum: msg.Summary, end: "files-end"}
	s.arriving = a
	f := s.files
	if f.inbox == nil {
		return
	}
	if msg.Bytes > bundle.MaxBytes || msg.Entries > bundle.MaxEntries {
		f.problem(fmt.Sprintf("%s from %s not received: over the limit of %s or %d files",
			describe(msg.Names), s.peer.Name, bundle.Size(bundle.MaxBytes), bundle.MaxEntries))
		return
	}
	// If something newer is held already these files would be thrown away
	// on arrival, so they are not kept in the first place.
	if !s.engine.Expect(msg.Clip) {
		return
	}
	recv, err := f.inbox.Receive()
	if err != nil {
		f.problem(fmt.Sprintf("%s from %s not received: %v", describe(msg.Names), s.peer.Name, err))
		return
	}
	a.recv = recv
}

// beginImage handles the announcement of a picture.
func (s *session) beginImage(msg message) {
	s.abandon()
	a := &arrival{clip: msg.Clip, sum: msg.Summary, end: "image-end"}
	s.arriving = a
	if msg.Bytes <= 0 || msg.Bytes > clipboard.MaxImageBytes {
		s.log.Warn("picture not received: over the size limit", "peer", s.peer.Name, "bytes", msg.Bytes)
		return
	}
	if !s.engine.Expect(msg.Clip) {
		return
	}
	a.image = make([]byte, 0, msg.Bytes)
}

// Piece implements mesh.Handler.
func (s *session) Piece(data []byte) {
	a := s.arriving
	switch {
	case a == nil:
	case a.recv != nil:
		if _, err := a.recv.Write(data); err != nil {
			a.recv.Abort()
			a.recv = nil
			s.files.problem(fmt.Sprintf("%s from %s not received: %v", describe(a.sum.Names), s.peer.Name, err))
		}
	case a.image != nil:
		if int64(len(a.image)+len(data)) > a.sum.Bytes {
			s.log.Warn("picture not received: more arrived than was announced", "peer", s.peer.Name)
			a.image = nil
			return
		}
		a.image = append(a.image, data...)
	}
}

// finish handles the message that ends a stream: everything is here.
func (s *session) finish(end string, clip clipsync.Clip) {
	a := s.arriving
	if a == nil || a.end != end || a.clip != clip {
		s.abandon()
		return
	}
	s.arriving = nil
	switch {
	case a.recv != nil:
		s.finishFiles(a)
	case a.image != nil:
		s.finishImage(a)
	}
}

func (s *session) finishFiles(a *arrival) {
	f := s.files
	paths, err := a.recv.Finish(a.sum)
	if err != nil {
		f.problem(fmt.Sprintf("%s from %s not received: %v", describe(a.sum.Names), s.peer.Name, err))
		return
	}
	if !s.engine.RemoteFiles(s.ctx, a.clip, paths) {
		// Something newer was copied while these were on their way.
		a.recv.Drop()
		return
	}
	f.superseded()
	a.recv.Keep(bundlesKept)
	f.mu.Lock()
	f.stats.Received++
	f.stats.Problem = ""
	f.mu.Unlock()
	s.log.Info("files received", "files", describe(paths), "bytes", a.sum.Bytes, "peer", s.peer.Name)
}

func (s *session) finishImage(a *arrival) {
	if int64(len(a.image)) != a.sum.Bytes || !clipboard.ValidImage(a.image) {
		s.log.Warn("picture not received: it is cut short or not a PNG", "peer", s.peer.Name, "bytes", len(a.image))
		return
	}
	if !s.engine.RemoteImage(s.ctx, a.clip, a.image) {
		return
	}
	s.files.superseded()
	s.log.Info("picture received", "bytes", len(a.image), "peer", s.peer.Name)
}

// abandon gives up on what has not fully arrived.
func (s *session) abandon() {
	if a := s.arriving; a != nil && a.recv != nil {
		a.recv.Abort()
	}
	s.arriving = nil
}

// Closed implements mesh.Handler.
func (s *session) Closed() { s.abandon() }
