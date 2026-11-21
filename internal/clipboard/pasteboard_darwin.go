//go:build darwin

package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	// How often the pasteboard's text is checked when its change count
	// cannot be watched. See Watch.
	pollInterval = time.Second
	// How long the change watcher gets to say it is running.
	watcherStart = 15 * time.Second
)

// Pasteboard types that apps set to keep content out of clipboard managers.
// See nspasteboard.org.
var privateTypes = []string{
	"org.nspasteboard.ConcealedType",
	"org.nspasteboard.TransientType",
}

// Pasteboard types that hold a picture.
var imageTypes = []string{"public.png", "public.tiff"}

// pasteboard drives the macOS clipboard: text through pbcopy and pbpaste,
// and what those cannot do (noticing a change, type names, files, pictures)
// through AppKit, reached with osascript.
type pasteboard struct {
	log *slog.Logger
	env []string
}

// New returns the clipboard of the current macOS login session.
func New(log *slog.Logger) (Clipboard, error) {
	// pbcopy and pbpaste pick the text encoding from the locale. launchd
	// agents run without one, which would mangle anything outside ASCII.
	return &pasteboard{log: log, env: append(os.Environ(), "LC_ALL=en_US.UTF-8")}, nil
}

func (p *pasteboard) Read(ctx context.Context) (Content, error) {
	text, textErr := p.readText(ctx)
	snap, snapErr := p.snapshot(ctx)
	switch {
	case snapErr == nil && len(snap.Files) > 0:
		return Content{Files: snap.Files}, nil
	case textErr == nil && text != "":
		return Content{Text: text}, nil
	case snapErr == nil && snap.image():
		image, err := p.readImage(ctx)
		if err != nil {
			return Content{}, err
		}
		return Content{Image: image}, nil
	case textErr != nil:
		return Content{}, textErr
	}
	return Content{}, nil
}

func (p *pasteboard) readText(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pbpaste")
	cmd.Env = p.env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pbpaste: %w", err)
	}
	return string(out), nil
}

func (p *pasteboard) Write(ctx context.Context, c Content) error {
	switch {
	case len(c.Files) > 0:
		return p.writeFiles(ctx, c.Files)
	case len(c.Image) > 0:
		return p.writeImage(ctx, c.Image)
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pbcopy")
	cmd.Env = p.env
	cmd.Stdin = strings.NewReader(c.Text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pbcopy: %w", err)
	}
	return nil
}

// missingJS defines missing(x): whether a call into AppKit came back empty.
const missingJS = `
  function missing(x) {
    return x === undefined || x === null || (typeof x.isNil === "function" && x.isNil());
  }`

// writeFilesScript puts files on the general pasteboard the way Finder does,
// as file URLs, so that they paste into Finder and into apps that take files.
// %s is the list of paths as a JSON array, which is also valid JavaScript.
const writeFilesScript = `ObjC.import("AppKit");
(function (paths) {
  var urls = $.NSMutableArray.alloc.init;
  for (var i = 0; i < paths.length; i++) {
    urls.addObject($.NSURL.fileURLWithPath(paths[i]));
  }
  var pb = $.NSPasteboard.generalPasteboard;
  pb.clearContents;
  if (!pb.writeObjects(urls)) {
    throw new Error("the pasteboard did not take the files");
  }
  return "ok";
})(%s)`

func (p *pasteboard) writeFiles(ctx context.Context, paths []string) error {
	// json.Marshal escapes the characters that JSON allows and JavaScript
	// source does not.
	list, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	if _, err := p.script(ctx, fmt.Sprintf(writeFilesScript, list)); err != nil {
		return fmt.Errorf("putting files on the pasteboard: %w", err)
	}
	return nil
}

// writeImageScript puts the PNG file at %s (a JSON string) on the general
// pasteboard as a picture. It adds a TIFF of it for the apps that only look
// for that; if making one fails the PNG is still there.
const writeImageScript = `ObjC.import("AppKit");
(function (path) {` + missingJS + `
  var png = $.NSData.dataWithContentsOfFile(path);
  if (missing(png)) {
    throw new Error("cannot read " + path);
  }
  var pb = $.NSPasteboard.generalPasteboard;
  pb.clearContents;
  if (!pb.setDataForType(png, "public.png")) {
    throw new Error("the pasteboard did not take the picture");
  }
  try {
    var image = $.NSImage.alloc.initWithData(png);
    var tiff = missing(image) ? null : image.TIFFRepresentation;
    if (!missing(tiff)) {
      pb.setDataForType(tiff, "public.tiff");
    }
  } catch (e) {}
  return "ok";
})(%s)`

func (p *pasteboard) writeImage(ctx context.Context, png []byte) error {
	// A picture is too large to hand to osascript on its command line.
	f, err := os.CreateTemp("", "osync-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(png)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	path, err := json.Marshal(f.Name())
	if err != nil {
		return err
	}
	if _, err := p.script(ctx, fmt.Sprintf(writeImageScript, path)); err != nil {
		return fmt.Errorf("putting a picture on the pasteboard: %w", err)
	}
	return nil
}

// readImageScript prints the picture on the general pasteboard as a PNG in
// base64, or nothing when there is none or it is over the size limit (%d).
// Apps that copy a picture as TIFF only get it converted.
const readImageScript = `ObjC.import("AppKit");
(function (limit) {` + missingJS + `
  var pb = $.NSPasteboard.generalPasteboard;
  var png = pb.dataForType("public.png");
  if (missing(png)) {
    var tiff = pb.dataForType("public.tiff");
    if (missing(tiff)) return "";
    var bitmap = $.NSBitmapImageRep.imageRepWithData(tiff);
    if (missing(bitmap)) return "";
    // 4 is NSBitmapImageFileTypePNG.
    png = bitmap.representationUsingTypeProperties(4, $.NSDictionary.dictionary);
    if (missing(png)) return "";
  }
  if (png.length > limit) return "";
  return ObjC.unwrap(png.base64EncodedStringWithOptions(0));
})(%d)`

func (p *pasteboard) readImage(ctx context.Context) ([]byte, error) {
	out, err := p.script(ctx, fmt.Sprintf(readImageScript, MaxImageBytes))
	if err != nil {
		return nil, fmt.Errorf("reading a picture from the pasteboard: %w", err)
	}
	image, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("reading a picture from the pasteboard: %w", err)
	}
	if len(image) == 0 {
		return nil, errors.New("the pasteboard holds no picture that can be synced")
	}
	return image, nil
}

func (p *pasteboard) Watch(ctx context.Context, onCopy func(Content)) error {
	changed := func() {
		if c, ok := p.copied(ctx); ok {
			onCopy(c)
		}
	}
	for {
		started := time.Now()
		err := p.watchChanges(ctx, changed)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) < time.Minute {
			// It does not work on this Mac, and trying again will not make
			// it. Watching the text is the older, lesser way: it notices
			// every copy that changes the text, and so misses a second
			// screenshot, or a second file of the same name, in a row.
			p.log.Error("cannot watch the pasteboard for changes, watching its text instead", "err", err)
			return p.watchText(ctx, changed)
		}
		p.log.Warn("pasteboard watcher stopped, restarting", "err", err)
	}
}

// watchScript runs until it is killed. It prints "ready" once it is going
// and "change" each time the general pasteboard's change count moves, which
// it does for every copy, whatever is copied. Nothing about the content is
// read here: that is done afresh for every change, by copied.
const watchScript = `ObjC.import("AppKit");
(function () {
  var pb = $.NSPasteboard.generalPasteboard;
  var out = $.NSFileHandle.fileHandleWithStandardOutput;
  function say(line) {
    // 4 is NSUTF8StringEncoding.
    out.writeData($(line + "\n").dataUsingEncoding(4));
  }
  var last = pb.changeCount;
  say("ready");
  while (true) {
    delay(0.5);
    var now = pb.changeCount;
    if (now !== last) {
      last = now;
      say("change");
    }
  }
})()`

// watchChanges calls changed for every change of the pasteboard, until the
// watcher stops, which it only does by failing.
func (p *pasteboard) watchChanges(ctx context.Context, changed func()) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", watchScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("osascript: %w", err)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		for sc := bufio.NewScanner(stdout); sc.Scan(); {
			lines <- sc.Text()
		}
	}()
	// stop ends the watcher and reports why it had to be ended.
	stop := func(why string) error {
		cmd.Process.Kill()
		for range lines {
		}
		cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			why += ": " + msg
		}
		return errors.New(why)
	}

	ready := false
	deadline := time.NewTimer(watcherStart)
	defer deadline.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return stop("the pasteboard watcher exited")
			}
			switch line {
			case "ready":
				ready = true
			case "change":
				changed()
			}
		case <-deadline.C:
			// A watcher that cannot say "ready" could not say "change"
			// either, and would sit there looking as if nothing was copied.
			if !ready {
				return stop("the pasteboard watcher did not start")
			}
		}
	}
}

// watchText calls changed each time the text of the pasteboard changes.
func (p *pasteboard) watchText(ctx context.Context, changed func()) error {
	last, _ := p.readText(ctx)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		text, err := p.readText(ctx)
		if err != nil || text == last {
			continue
		}
		last = text
		changed()
	}
}

// copied returns what was just copied, if it is something to sync.
func (p *pasteboard) copied(ctx context.Context) (Content, bool) {
	// The text is read first and the type names after. If a password is
	// copied in between, the names are the password's and say to drop it.
	// The other way round, the password could pass under the names of the
	// harmless copy before it.
	text, textErr := p.readText(ctx)
	// If the pasteboard cannot be inspected nothing is synced: failing to
	// sync is better than syncing a password.
	snap, err := p.snapshot(ctx)
	if err != nil {
		p.log.Warn("cannot inspect the pasteboard, not syncing this copy", "err", err)
		return Content{}, false
	}
	switch {
	case snap.private():
	case len(snap.Files) > 0:
		return Content{Files: snap.Files}, true
	case textErr == nil && text != "":
		return Content{Text: text}, syncable(text)
	case snap.image():
		image, err := p.readImage(ctx)
		if err != nil {
			p.log.Warn("picture not synced", "err", err)
			return Content{}, false
		}
		return Content{Image: image}, ValidImage(image)
	}
	return Content{}, false
}

// snapshot is what is on the general pasteboard besides its text.
type snapshot struct {
	Types []string `json:"types"`
	Files []string `json:"files"` // paths, when files were copied
	Error string   `json:"error"` // why Files could not be read
}

// private reports whether the content is marked as not to be recorded.
func (s snapshot) private() bool {
	return slices.ContainsFunc(s.Types, func(t string) bool { return slices.Contains(privateTypes, t) })
}

// image reports whether the content can be had as a picture.
func (s snapshot) image() bool {
	return slices.ContainsFunc(s.Types, func(t string) bool { return slices.Contains(imageTypes, t) })
}

// snapshotScript prints a snapshot as one line of JSON. The JSON is kept to
// ASCII so that no text encoding between here and osascript can damage a
// file name.
//
// Reading the files is fenced off with try: if it fails, the type names
// still come back, and with them the answer to whether text may be synced.
const snapshotScript = `ObjC.import("AppKit");
(function () {` + missingJS + `
  var pb = $.NSPasteboard.generalPasteboard;
  var snap = {types: ObjC.deepUnwrap(pb.types) || [], files: []};
  try {
    var items = pb.pasteboardItems;
    var count = missing(items) ? 0 : items.count;
    for (var i = 0; i < count; i++) {
      var raw = items.objectAtIndex(i).stringForType("public.file-url");
      if (missing(raw)) continue;
      var url = $.NSURL.URLWithString(raw);
      if (missing(url)) continue;
      // Finder can hand out references (file:///.file/id=...) instead of paths.
      var byPath = url.filePathURL;
      var path = ObjC.unwrap(missing(byPath) ? url.path : byPath.path);
      if (path) snap.files.push(path);
    }
  } catch (e) {
    snap.files = [];
    snap.error = String(e);
  }
  return JSON.stringify(snap).replace(/[\u007f-￿]/g, function (c) {
    return "\\u" + ("0000" + c.charCodeAt(0).toString(16)).slice(-4);
  });
})()`

func (p *pasteboard) snapshot(ctx context.Context) (snapshot, error) {
	out, err := p.script(ctx, snapshotScript)
	if err != nil {
		return snapshot{}, err
	}
	var snap snapshot
	if err := json.Unmarshal(out, &snap); err != nil {
		return snapshot{}, fmt.Errorf("unreadable pasteboard snapshot %.80q: %w", out, err)
	}
	if snap.Error != "" {
		p.log.Warn("cannot read files from the pasteboard", "err", snap.Error)
	}
	return snap, nil
}

// script runs JavaScript with access to AppKit and returns what it printed.
func (p *pasteboard) script(ctx context.Context, source string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", source).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if msg := strings.TrimSpace(string(exit.Stderr)); msg != "" {
				return nil, fmt.Errorf("osascript: %s", msg)
			}
		}
		return nil, fmt.Errorf("osascript: %w", err)
	}
	return out, nil
}
