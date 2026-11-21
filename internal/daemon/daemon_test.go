package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iluxav/osync/internal/bundle"
	"github.com/iluxav/osync/internal/clipboard/clipboardtest"
	"github.com/iluxav/osync/internal/tailnet"
	"github.com/iluxav/osync/internal/tailnet/tailnettest"
)

// These tests run whole daemons against each other. Each has an in-memory
// clipboard and sits on a loopback "tailnet"; see tailnettest.Loopback.

type machine struct {
	name   string
	board  *clipboardtest.Fake
	socket string
	inbox  string // where files from the other machines end up
}

func startMachines(t *testing.T, count int) []*machine {
	t.Helper()
	nodes, port := tailnettest.Loopback(t, 2, count)

	dir := t.TempDir()
	machines := make([]*machine, count)
	for i, self := range nodes {
		var peers []tailnet.Node
		for j, n := range nodes {
			if j != i {
				peers = append(peers, n)
			}
		}
		mc := &machine{
			name:   self.Name,
			board:  clipboardtest.New(),
			socket: filepath.Join(dir, self.Name+".sock"),
			inbox:  filepath.Join(dir, self.Name+"-inbox"),
		}
		machines[i] = mc
		cfg := Config{
			Version:   "test",
			Port:      port,
			Socket:    mc.socket,
			Files:     mc.inbox,
			Tailnet:   tailnettest.New(tailnet.State{Backend: "Running", Self: self, Peers: peers}),
			Clipboard: mc.board,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Poll:      20 * time.Millisecond,
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s: Run returned %v", cfg.Socket, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s: Run did not return after cancel", cfg.Socket)
			}
		})
	}

	for _, mc := range machines {
		eventually(t, mc.name+" to connect to every peer", func() bool {
			st, err := Query(context.Background(), mc.socket)
			if err != nil {
				return false
			}
			n := 0
			for _, p := range st.Mesh.Peers {
				if p.Connected {
					n++
				}
			}
			return n == count-1
		})
	}
	return machines
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func status(t *testing.T, mc *machine) Status {
	t.Helper()
	st, err := Query(context.Background(), mc.socket)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCopyReachesEveryMachineOnce(t *testing.T) {
	ms := startMachines(t, 3)
	a, b, c := ms[0], ms[1], ms[2]

	a.board.Copy("hello from a")
	eventually(t, "b and c to receive the text", func() bool {
		return b.board.Text() == "hello from a" && c.board.Text() == "hello from a"
	})

	// Let any ping-pong show itself, then check there was none: one clip
	// sent, each receiver wrote it once, and the sender's clipboard was left
	// alone.
	time.Sleep(200 * time.Millisecond)
	if a.board.Writes() != 0 || b.board.Writes() != 1 || c.board.Writes() != 1 {
		t.Errorf("writes a/b/c = %d/%d/%d, want 0/1/1", a.board.Writes(), b.board.Writes(), c.board.Writes())
	}
	sa, sb, sc := status(t, a).Clipboard, status(t, b).Clipboard, status(t, c).Clipboard
	if sa.Sent != 1 || sb.Sent != 0 || sc.Sent != 0 {
		t.Errorf("sent a/b/c = %d/%d/%d, want 1/0/0", sa.Sent, sb.Sent, sc.Sent)
	}
	if sa.Received != 0 || sb.Received != 1 || sc.Received != 1 {
		t.Errorf("received a/b/c = %d/%d/%d, want 0/1/1", sa.Received, sb.Received, sc.Received)
	}
}

func TestCopiesGoBothWays(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]

	a.board.Copy("one")
	eventually(t, "b to have 'one'", func() bool { return b.board.Text() == "one" })
	b.board.Copy("two\nwith a second line and ünïcödé ✓\n")
	eventually(t, "a to have b's text, byte for byte", func() bool {
		return a.board.Text() == "two\nwith a second line and ünïcödé ✓\n"
	})
}

func TestBackToBackCopiesSettleOnTheLast(t *testing.T) {
	ms := startMachines(t, 3)
	a := ms[0]
	for i := range 50 {
		a.board.Copy(fmt.Sprintf("copy %d", i))
	}
	for _, mc := range ms {
		eventually(t, mc.name+" to hold the last copy", func() bool { return mc.board.Text() == "copy 49" })
	}
	// Receivers report their own writes late, after newer clips are in.
	// None of that may be sent back.
	time.Sleep(200 * time.Millisecond)
	for _, mc := range ms[1:] {
		if sent := status(t, mc).Clipboard.Sent; sent != 0 {
			t.Errorf("%s sent %d clips, want 0", mc.name, sent)
		}
	}
	for _, mc := range ms {
		if got := mc.board.Text(); got != "copy 49" {
			t.Errorf("%s ended with %q, want 'copy 49'", mc.name, got)
		}
	}
}

func TestCopiesAtTheSameMomentConverge(t *testing.T) {
	ms := startMachines(t, 3)
	for range 20 {
		for i, mc := range ms {
			go mc.board.Copy(fmt.Sprintf("racing copy from %d", i))
		}
		eventually(t, "all machines to agree", func() bool {
			return ms[0].board.Text() == ms[1].board.Text() && ms[1].board.Text() == ms[2].board.Text()
		})
		time.Sleep(20 * time.Millisecond)
		if x, y, z := ms[0].board.Text(), ms[1].board.Text(), ms[2].board.Text(); x != y || y != z {
			t.Fatalf("machines disagree after settling: %q / %q / %q", x, y, z)
		}
	}
}

func TestPrivateCopyStaysOnItsMachine(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]

	a.board.Copy("public")
	eventually(t, "b to have 'public'", func() bool { return b.board.Text() == "public" })
	a.board.CopyPrivate("hunter2")
	time.Sleep(200 * time.Millisecond)
	if got := b.board.Text(); got != "public" {
		t.Errorf("b has %q, want it to still hold 'public'", got)
	}
}

func TestStatusAndSingleInstance(t *testing.T) {
	ms := startMachines(t, 2)
	st := status(t, ms[0])
	if st.Version != "test" || st.PID == 0 || st.Mesh.Self.Name != "machine1" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Mesh.Peers) != 1 || st.Mesh.Peers[0].Name != "machine2" || !st.Mesh.Peers[0].Connected {
		t.Errorf("peers = %+v", st.Mesh.Peers)
	}

	// A second daemon for the same user must refuse to start.
	err := Run(context.Background(), Config{Socket: ms[0].socket})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second daemon: err = %v, want 'already running'", err)
	}
	// ...and must not have taken the socket away from the first.
	if _, err := Query(context.Background(), ms[0].socket); err != nil {
		t.Errorf("first daemon unreachable after the refused start: %v", err)
	}
}

// write creates a file with the given content under a new folder and returns
// its path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// holds reports whether mc's clipboard has exactly one file or folder with
// the given name, sitting in mc's inbox.
func holds(mc *machine, name string) bool {
	files := mc.board.Files()
	return len(files) == 1 && filepath.Base(files[0]) == name && strings.HasPrefix(files[0], mc.inbox+string(filepath.Separator))
}

func TestCopiedFileCanBePastedOnEveryMachine(t *testing.T) {
	ms := startMachines(t, 3)
	a, b, c := ms[0], ms[1], ms[2]
	// Several pieces long, so that it does not travel in one go.
	content := strings.Repeat("all work and no play makes jack a dull boy\n", 8000)
	src := write(t, t.TempDir(), "report.txt", content)

	a.board.CopyFiles(src)
	eventually(t, "b and c to hold the file", func() bool { return holds(b, "report.txt") && holds(c, "report.txt") })
	for _, mc := range []*machine{b, c} {
		got, err := os.ReadFile(mc.board.Files()[0])
		if err != nil || string(got) != content {
			t.Errorf("%s: the file has %d bytes, %v; want the %d that were copied", mc.name, len(got), err, len(content))
		}
		if mc.board.Text() != "" {
			t.Errorf("%s: the clipboard also holds text %q", mc.name, mc.board.Text())
		}
	}

	// Receivers report their own write of the files. Nothing may come of
	// it: no file is sent back, and the sender's clipboard is left alone.
	time.Sleep(200 * time.Millisecond)
	if got := a.board.Files(); len(got) != 1 || got[0] != src {
		t.Errorf("a's clipboard = %q, want the file it copied", got)
	}
	if a.board.Writes() != 0 || b.board.Writes() != 1 || c.board.Writes() != 1 {
		t.Errorf("writes a/b/c = %d/%d/%d, want 0/1/1", a.board.Writes(), b.board.Writes(), c.board.Writes())
	}
	sa, sb, sc := status(t, a), status(t, b), status(t, c)
	if sa.Files.Sent != 1 || sb.Files.Sent != 0 || sc.Files.Sent != 0 {
		t.Errorf("files sent a/b/c = %d/%d/%d, want 1/0/0", sa.Files.Sent, sb.Files.Sent, sc.Files.Sent)
	}
	if sa.Files.Received != 0 || sb.Files.Received != 1 || sc.Files.Received != 1 {
		t.Errorf("files received a/b/c = %d/%d/%d, want 0/1/1", sa.Files.Received, sb.Files.Received, sc.Files.Received)
	}
	if sa.Clipboard.Sent != 1 || sb.Clipboard.Received != 1 {
		t.Errorf("a copy of files counts as a copy: a sent %d, b received %d, want 1 and 1", sa.Clipboard.Sent, sb.Clipboard.Received)
	}
}

func TestCopiedFolderArrivesWithEverythingInIt(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]
	dir := t.TempDir()
	write(t, dir, "project/readme.md", "# project\n")
	write(t, dir, "project/src/main.go", "package main\n")
	lone := write(t, dir, "ünïcödé ✓.txt", "alongside\n")

	a.board.CopyFiles(filepath.Join(dir, "project"), lone)
	eventually(t, "b to hold the folder and the file", func() bool { return len(b.board.Files()) == 2 })
	got := b.board.Files()
	if filepath.Base(got[0]) != "project" || filepath.Base(got[1]) != "ünïcödé ✓.txt" {
		t.Fatalf("b's clipboard = %q, want the folder then the file", got)
	}
	for name, want := range map[string]string{
		filepath.Join(got[0], "readme.md"):      "# project\n",
		filepath.Join(got[0], "src", "main.go"): "package main\n",
		got[1]:                                  "alongside\n",
	} {
		if data, err := os.ReadFile(name); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
}

func TestFilesAndTextReplaceEachOther(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]
	src := write(t, t.TempDir(), "notes.txt", "notes\n")

	a.board.CopyFiles(src)
	eventually(t, "b to hold the file", func() bool { return holds(b, "notes.txt") })
	b.board.Copy("text from b")
	eventually(t, "a to hold b's text", func() bool { return a.board.Text() == "text from b" && len(a.board.Files()) == 0 })
	b.board.CopyFiles(write(t, t.TempDir(), "back.txt", "from b\n"))
	eventually(t, "a to hold b's file", func() bool { return holds(a, "back.txt") && a.board.Text() == "" })
}

func TestTextCopiedRightAfterFilesWins(t *testing.T) {
	ms := startMachines(t, 3)
	a := ms[0]
	big := write(t, t.TempDir(), "big.bin", strings.Repeat("x", 8<<20))
	for i := range 5 {
		// The text is copied while the file is still on its way.
		a.board.CopyFiles(big)
		text := fmt.Sprintf("text %d", i)
		a.board.Copy(text)
		for _, mc := range ms {
			eventually(t, mc.name+" to hold the text", func() bool { return mc.board.Text() == text })
		}
		time.Sleep(50 * time.Millisecond)
		for _, mc := range ms {
			if mc.board.Text() != text || len(mc.board.Files()) != 0 {
				t.Fatalf("%s ended with text %q and files %q, want %q", mc.name, mc.board.Text(), mc.board.Files(), text)
			}
		}
	}
}

func TestFilesOverTheLimitStayPutAndSayWhy(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]
	a.board.Copy("before")
	eventually(t, "b to hold the text", func() bool { return b.board.Text() == "before" })

	huge := filepath.Join(t.TempDir(), "huge.iso")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(bundle.MaxBytes + 1); err != nil { // sparse: takes no disk
		t.Fatal(err)
	}
	f.Close()
	a.board.CopyFiles(huge)
	eventually(t, "a to report why the file was not sent", func() bool {
		return strings.Contains(status(t, a).Files.Problem, "huge.iso not sent")
	})
	st := status(t, a)
	if !strings.Contains(st.Files.Problem, "100 MB") || st.Files.Sent != 0 || st.Clipboard.Sent != 1 {
		t.Errorf("a's status = %+v and %+v, want the limit named and nothing counted as sent", st.Files, st.Clipboard)
	}
	if b.board.Text() != "before" {
		t.Errorf("b holds %q, want it left with 'before'", b.board.Text())
	}

	// The copy that went nowhere must not stand in the way of what comes next.
	b.board.Copy("after")
	eventually(t, "a to hold b's text", func() bool { return a.board.Text() == "after" })
	small := write(t, t.TempDir(), "small.txt", "fits\n")
	a.board.CopyFiles(small)
	eventually(t, "b to hold the small file", func() bool { return holds(b, "small.txt") })
	if p := status(t, a).Files.Problem; p != "" {
		t.Errorf("a still reports %q after a copy that went through", p)
	}
}

func TestInboxKeepsOnlyTheLatestFiles(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]
	dir := t.TempDir()
	for i := range 5 {
		name := fmt.Sprintf("file%d.txt", i)
		a.board.CopyFiles(write(t, dir, name, name))
		eventually(t, "b to hold "+name, func() bool { return holds(b, name) })
	}
	entries, err := os.ReadDir(b.inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != bundlesKept {
		t.Errorf("b's inbox holds %d bundles, want %d", len(entries), bundlesKept)
	}
	if data, err := os.ReadFile(b.board.Files()[0]); err != nil || string(data) != "file4.txt" {
		t.Errorf("the file on b's clipboard = %q, %v; want the last one copied", data, err)
	}
}

func TestFilesCopiedOutOfTheInboxAreNotSentBack(t *testing.T) {
	ms := startMachines(t, 2)
	a, b := ms[0], ms[1]
	a.board.CopyFiles(write(t, t.TempDir(), "once.txt", "once\n"))
	eventually(t, "b to hold the file", func() bool { return holds(b, "once.txt") })

	// Long after, something on b puts the very same file on the clipboard
	// again, spelled differently: as a clipboard manager might.
	received := b.board.Files()[0]
	b.board.CopyFiles(filepath.Dir(received) + "/./once.txt")
	time.Sleep(200 * time.Millisecond)
	if st := status(t, b); st.Files.Sent != 0 || st.Clipboard.Sent != 0 {
		t.Errorf("b sent %d copies, want 0: the file came from a", st.Clipboard.Sent)
	}
	if a.board.Writes() != 0 {
		t.Errorf("a's clipboard was written %d times, want 0", a.board.Writes())
	}
}
