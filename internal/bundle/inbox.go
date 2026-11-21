package bundle

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// unfinished marks the folder of a bundle that has not fully arrived.
const unfinished = "arriving-"

// Inbox is the folder that bundles from other machines are unpacked into,
// each into a subfolder of its own. The clipboard then points at the files
// there, and pasting copies them to where the user wants them.
type Inbox struct {
	dir string
}

// OpenInbox returns the Inbox at dir, creating the folder if needed and
// clearing out what a daemon that died left half-received.
func OpenInbox(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Paths handed to the clipboard should be the ones it hands back.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), unfinished) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return &Inbox{dir: dir}, nil
}

// Contains reports whether path lies inside the inbox.
func (in *Inbox) Contains(path string) bool {
	rel, err := filepath.Rel(in.dir, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// Keep trims the inbox to its newest count bundles. The bundle r received
// is always one of them, whatever arrived while it was being finished: it is
// the one the clipboard is about to point at.
func (r *Receiver) Keep(count int) {
	entries, err := os.ReadDir(r.in.dir)
	if err != nil {
		return
	}
	var others []string
	for _, e := range entries {
		if name := e.Name(); name != r.folder && !strings.HasPrefix(name, unfinished) {
			others = append(others, name)
		}
	}
	// Folders are named by when their bundle started to arrive, so name
	// order is age order.
	slices.Sort(others)
	for i := 0; i < len(others)-(count-1); i++ {
		os.RemoveAll(filepath.Join(r.in.dir, others[i]))
	}
}

// Receiver unpacks one bundle as its pieces arrive.
type Receiver struct {
	in     *Inbox
	folder string // where the bundle ends up, within the inbox
	pieces *io.PipeWriter
	done   chan struct{} // closed when unpacking has stopped
	got    Summary
	err    error
}

var errAborted = errors.New("the transfer was abandoned")

// Receive starts unpacking a bundle that arrives piece by piece. The caller
// must end it with Finish or Abort.
func (in *Inbox) Receive() (*Receiver, error) {
	// Named by arrival time; Keep relies on that.
	folder := fmt.Sprintf("%020d", time.Now().UnixNano())
	if err := os.Mkdir(filepath.Join(in.dir, unfinished+folder), 0o700); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	r := &Receiver{in: in, folder: folder, pieces: pw, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.got, r.err = Read(pr, r.tmp())
		if r.err == nil {
			io.Copy(io.Discard, pr) // any padding after the end of the bundle
		}
		// From here on Write reports why nothing more is being taken.
		pr.CloseWithError(r.err)
	}()
	return r, nil
}

func (r *Receiver) tmp() string { return filepath.Join(r.in.dir, unfinished+r.folder) }

// Write takes the next piece of the bundle. It fails once the bundle has
// turned out to be unusable.
func (r *Receiver) Write(piece []byte) (int, error) {
	return r.pieces.Write(piece)
}

// Finish is called when the last piece has been written. If what arrived is
// a whole bundle holding exactly what was announced, it returns the paths of
// the files and folders that were copied, now inside the inbox. Otherwise
// nothing is kept.
func (r *Receiver) Finish(announced Summary) ([]string, error) {
	r.pieces.Close()
	<-r.done
	err := r.err
	if err == nil && !r.got.equal(announced) {
		// Also how a bundle cut short between two files shows.
		err = errors.New("the bundle is not what was announced")
	}
	final := filepath.Join(r.in.dir, r.folder)
	if err == nil {
		err = os.Rename(r.tmp(), final)
	}
	if err != nil {
		os.RemoveAll(r.tmp())
		return nil, err
	}
	paths := make([]string, len(r.got.Names))
	for i, name := range r.got.Names {
		paths[i] = filepath.Join(final, name)
	}
	return paths, nil
}

// Abort gives up on the bundle and removes what had arrived.
func (r *Receiver) Abort() {
	r.pieces.CloseWithError(errAborted)
	<-r.done
	os.RemoveAll(r.tmp())
}

// Drop removes a bundle that Finish returned but that is not wanted after
// all.
func (r *Receiver) Drop() {
	os.RemoveAll(filepath.Join(r.in.dir, r.folder))
}
