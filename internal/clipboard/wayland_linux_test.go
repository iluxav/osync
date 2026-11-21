//go:build linux

package clipboard

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseWatchLine(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name     string
		line     string
		wantText string
		wantOK   bool
	}{
		{"plain text", "data " + b64("hello\n") + "\n", "hello\n", true},
		{"password manager", "sensitive " + b64("hunter2") + "\n", "", false},
		{"cleared", "clear \n", "", false},
		{"empty clipboard", "nil \n", "", false},
		{"a state added by a later wl-clipboard", "novel " + b64("x") + "\n", "", false},
		{"zero bytes", "data \n", "", false},
		{"not text", "data " + b64("\x89PNG\xff\xfe") + "\n", "\x89PNG\xff\xfe", false},
		{"too large", "data " + b64(strings.Repeat("a", MaxBytes+1)) + "\n", strings.Repeat("a", MaxBytes+1), false},
		{"largest allowed", "data " + b64(strings.Repeat("a", MaxBytes)) + "\n", strings.Repeat("a", MaxBytes), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ok, err := parseWatchLine(tt.line, syncable)
			if err != nil {
				t.Fatal(err)
			}
			if text != tt.wantText || ok != tt.wantOK {
				t.Errorf("got %d bytes, ok=%v; want %d bytes, ok=%v", len(text), ok, len(tt.wantText), tt.wantOK)
			}
		})
	}

	if _, _, err := parseWatchLine("unknown "+b64("x")+"\n", syncable); !errors.Is(err, errTooOld) {
		t.Errorf("wl-clipboard without CLIPBOARD_STATE: err = %v, want errTooOld", err)
	}
	if _, _, err := parseWatchLine("garbage\n", syncable); err == nil {
		t.Error("line without a state: want an error")
	}
}

// TestWaylandSession drives the real wl-clipboard tools in the running
// desktop session. It uses the primary selection (the middle-click buffer),
// so the clipboard itself is left alone. Opt in with OMARCHY_SYNC_TEST_WAYLAND=1.
func TestWaylandSession(t *testing.T) {
	if os.Getenv("OMARCHY_SYNC_TEST_WAYLAND") == "" {
		t.Skip("set OMARCHY_SYNC_TEST_WAYLAND=1 to test against the running Wayland session")
	}
	w := &wayland{log: slog.New(slog.NewTextHandler(io.Discard, nil)), primary: true}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Put back what the user had selected, as far as text and files go.
	if before, err := w.Read(ctx); err == nil {
		defer w.Write(context.Background(), before)
	}

	// userCopy acts as some other application taking the selection.
	userCopy := func(content string, args ...string) {
		t.Helper()
		cmd := exec.Command("wl-copy", append([]string{"--primary"}, args...)...)
		cmd.Stdin = strings.NewReader(content)
		if err := cmd.Run(); err != nil {
			t.Fatalf("wl-copy %v: %v", args, err)
		}
	}

	userCopy("there before the watcher started", "--type", "text/plain")
	reports := make(chan Content, 16)
	go w.Watch(ctx, func(c Content) { reports <- c })

	expect := func(what, want string) {
		t.Helper()
		select {
		case c := <-reports:
			if got := c.Text; got != want || c.Files != nil {
				t.Fatalf("%s: reported %d bytes %.40q and files %q, want %d bytes %.40q", what, len(got), got, c.Files, len(want), want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing reported", what)
		}
	}
	expectFiles := func(what string, want ...string) {
		t.Helper()
		select {
		case c := <-reports:
			if c.Text != "" || !slices.Equal(c.Files, want) {
				t.Fatalf("%s: reported text %.40q and files %q, want files %q", what, c.Text, c.Files, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing reported", what)
		}
	}
	expectNothing := func(what string) {
		t.Helper()
		select {
		case c := <-reports:
			t.Fatalf("%s: reported %d bytes %.40q and files %q, want nothing", what, len(c.Text), c.Text, c.Files)
		case <-time.After(700 * time.Millisecond):
		}
	}

	expectNothing("content that predates the watcher")

	userCopy("plain text", "--type", "text/plain")
	expect("plain text", "plain text")

	exact := "two lines, ünïcödé ✓, trailing newline\n\tand a tab\n"
	userCopy(exact, "--type", "text/plain")
	expect("text must arrive byte for byte", exact)

	userCopy("hunter2", "--sensitive", "--type", "text/plain")
	expectNothing("a password manager's copy")

	userCopy("\xff\xd8\xff\xe0 a JPEG", "--type", "image/jpeg")
	expectNothing("a picture in a form that is not synced")

	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	userCopy(big, "--type", "text/plain")
	expect("1 MiB of text", big)

	userCopy(strings.Repeat("x", MaxBytes+1), "--type", "text/plain")
	expectNothing("text over the size limit")

	// What the daemon writes can be read back unchanged, pastes as plain
	// text, and is reported like any other copy.
	written := "written by the daemon ✓\n"
	if err := w.Write(ctx, Content{Text: written}); err != nil {
		t.Fatal(err)
	}
	expect("our own write", written)
	if got, err := w.Read(ctx); err != nil || got.Text != written || got.Files != nil {
		t.Errorf("Read = %+v, %v; want the text %q", got, err, written)
	}
	types, err := exec.Command("wl-paste", "--primary", "--list-types").Output()
	if err != nil || !strings.Contains(string(types), textType) {
		t.Errorf("types offered after Write = %q, %v; want %s among them", types, err, textType)
	}

	// Files, the way a file manager copies them. It offers them as text as
	// well; the copy must be reported once, as files.
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one file.txt"), filepath.Join(dir, "ünïcödé ✓")
	userCopy(uriList([]string{one, two}), "--type", filesType)
	expectFiles("files copied in a file manager", one, two)
	expectNothing("the text form of copied files")
	if got, err := w.Read(ctx); err != nil || got.Text != "" || !slices.Equal(got.Files, []string{one, two}) {
		t.Errorf("Read = %+v, %v; want the two files", got, err)
	}

	userCopy("https://example.com/a\r\nhttps://example.com/b\r\n", "--type", filesType)
	expect("links are text, not files", "https://example.com/a\r\nhttps://example.com/b\r\n")

	// Files the daemon writes can be read back, are reported like any other
	// copy, and are offered the way file managers look for them.
	if err := w.Write(ctx, Content{Files: []string{two, one}}); err != nil {
		t.Fatal(err)
	}
	expectFiles("our own write of files", two, one)
	expectNothing("the text form of written files")
	if got, err := w.Read(ctx); err != nil || !slices.Equal(got.Files, []string{two, one}) {
		t.Errorf("Read = %+v, %v; want the files that were written", got, err)
	}
	types, err = exec.Command("wl-paste", "--primary", "--list-types").Output()
	if err != nil || !slices.Contains(strings.Fields(string(types)), filesType) {
		t.Errorf("types offered after writing files = %q, %v; want %s among them", types, err, filesType)
	}

	userCopy("text again", "--type", "text/plain")
	expect("text after files", "text again")

	// A screenshot: a picture and nothing else.
	shot := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\xff pixels \n", 5000)
	expectImage := func(what, want string) {
		t.Helper()
		select {
		case c := <-reports:
			if c.Text != "" || c.Files != nil || string(c.Image) != want {
				t.Fatalf("%s: reported text %.40q, files %q and a picture of %d bytes; want only a picture of %d bytes", what, c.Text, c.Files, len(c.Image), len(want))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing reported", what)
		}
	}
	userCopy(shot, "--type", imageType)
	expectImage("a screenshot", shot)
	if got, err := w.Read(ctx); err != nil || string(got.Image) != shot || got.Text != "" {
		t.Errorf("Read = text %.40q and a picture of %d bytes, %v; want the screenshot", got.Text, len(got.Image), err)
	}
	// The same screenshot taken again is a new copy, and is reported again.
	userCopy(shot, "--type", imageType)
	expectImage("the same picture copied again", shot)

	userCopy("\xff\xd8\xff\xe0 a JPEG under the wrong name", "--type", imageType)
	expectNothing("a picture that is not a PNG")

	// A picture the daemon writes is reported, read back unchanged, and
	// offered as a picture.
	other := "\x89PNG\r\n\x1a\n" + strings.Repeat("other pixels", 300)
	if err := w.Write(ctx, Content{Image: []byte(other)}); err != nil {
		t.Fatal(err)
	}
	expectImage("our own write of a picture", other)
	if got, err := w.Read(ctx); err != nil || string(got.Image) != other {
		t.Errorf("Read = a picture of %d bytes, %v; want the one that was written", len(got.Image), err)
	}
	types, err = exec.Command("wl-paste", "--primary", "--list-types").Output()
	if err != nil || strings.TrimSpace(string(types)) != imageType {
		t.Errorf("types offered after writing a picture = %q, %v; want only %s", types, err, imageType)
	}

	userCopy("text after a picture", "--type", "text/plain")
	expect("text after a picture", "text after a picture")
}
