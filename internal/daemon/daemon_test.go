package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		}
		machines[i] = mc
		cfg := Config{
			Version:   "test",
			Port:      port,
			Socket:    mc.socket,
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
