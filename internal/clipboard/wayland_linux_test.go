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
			text, ok, err := parseWatchLine(tt.line)
			if err != nil {
				t.Fatal(err)
			}
			if text != tt.wantText || ok != tt.wantOK {
				t.Errorf("got %d bytes, ok=%v; want %d bytes, ok=%v", len(text), ok, len(tt.wantText), tt.wantOK)
			}
		})
	}

	if _, _, err := parseWatchLine("unknown " + b64("x") + "\n"); !errors.Is(err, errTooOld) {
		t.Errorf("wl-clipboard without CLIPBOARD_STATE: err = %v, want errTooOld", err)
	}
	if _, _, err := parseWatchLine("garbage\n"); err == nil {
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

	// Put back what the user had selected, as far as text goes.
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
	reports := make(chan string, 16)
	go w.Watch(ctx, func(text string) { reports <- text })

	expect := func(what, want string) {
		t.Helper()
		select {
		case got := <-reports:
			if got != want {
				t.Fatalf("%s: reported %d bytes %.40q, want %d bytes %.40q", what, len(got), got, len(want), want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing reported", what)
		}
	}
	expectNothing := func(what string) {
		t.Helper()
		select {
		case got := <-reports:
			t.Fatalf("%s: reported %d bytes %.40q, want nothing", what, len(got), got)
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

	userCopy("\x89PNG\r\n\x1a\n not really an image", "--type", "image/png")
	expectNothing("an image")

	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	userCopy(big, "--type", "text/plain")
	expect("1 MiB of text", big)

	userCopy(strings.Repeat("x", MaxBytes+1), "--type", "text/plain")
	expectNothing("text over the size limit")

	// What the daemon writes can be read back unchanged, pastes as plain
	// text, and is reported like any other copy.
	written := "written by the daemon ✓\n"
	if err := w.Write(ctx, written); err != nil {
		t.Fatal(err)
	}
	expect("our own write", written)
	if got, err := w.Read(ctx); err != nil || got != written {
		t.Errorf("Read = %q, %v; want %q", got, err, written)
	}
	types, err := exec.Command("wl-paste", "--primary", "--list-types").Output()
	if err != nil || !strings.Contains(string(types), textType) {
		t.Errorf("types offered after Write = %q, %v; want %s among them", types, err, textType)
	}
}
