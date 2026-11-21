//go:build darwin

package clipboard

import (
	"context"
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

// How often the pasteboard is checked. macOS has no change notification.
const pollInterval = time.Second

// Pasteboard types that apps set to keep content out of clipboard managers.
// See nspasteboard.org.
var privateTypes = []string{
	"org.nspasteboard.ConcealedType",
	"org.nspasteboard.TransientType",
}

// pasteboard drives the macOS clipboard: text through pbcopy and pbpaste,
// and what those cannot do (type names, files) through AppKit, reached with
// osascript.
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
	if snap, err := p.snapshot(ctx); err == nil && len(snap.Files) > 0 {
		return Content{Files: snap.Files}, nil
	}
	text, err := p.readText(ctx)
	if err != nil {
		return Content{}, err
	}
	return Content{Text: text}, nil
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
	if len(c.Files) > 0 {
		return p.writeFiles(ctx, c.Files)
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

func (p *pasteboard) Watch(ctx context.Context, onCopy func(Content)) error {
	// A copy shows as a change of the pasteboard's text: Finder puts a
	// file's name there next to the file itself. Copying a second file with
	// the same name straight after the first is the one copy this misses.
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
		// If the pasteboard cannot be inspected nothing is synced: failing
		// to sync is better than syncing a password.
		snap, err := p.snapshot(ctx)
		if err != nil {
			p.log.Warn("cannot inspect the pasteboard, not syncing this copy", "err", err)
			continue
		}
		switch {
		case snap.private():
		case len(snap.Files) > 0:
			onCopy(Content{Files: snap.Files})
		case syncable(text):
			onCopy(Content{Text: text})
		}
	}
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

// snapshotScript prints a snapshot as one line of JSON. The JSON is kept to
// ASCII so that no text encoding between here and osascript can damage a
// file name.
//
// Reading the files is fenced off with try: if it fails, the type names
// still come back, and with them the answer to whether text may be synced.
const snapshotScript = `ObjC.import("AppKit");
(function () {
  function missing(x) {
    return x === undefined || x === null || (typeof x.isNil === "function" && x.isNil());
  }
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
