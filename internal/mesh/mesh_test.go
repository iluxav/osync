package mesh

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iluxav/osync/internal/tailnet"
	"github.com/iluxav/osync/internal/tailnet/tailnettest"
)

// A test tailnet lives on loopback; see tailnettest.Loopback.
const subnet = 1

// tailnetOf returns count machines and the port they share.
func tailnetOf(t *testing.T, count int) ([]tailnet.Node, int) {
	t.Helper()
	return tailnettest.Loopback(t, subnet, count)
}

func addr(i int) string { return tailnettest.LoopbackAddr(subnet, i).String() }

// member is one running Mesh and what it has received.
type member struct {
	self tailnet.Node
	tn   *tailnettest.Fake
	mesh *Mesh

	mu  sync.Mutex
	got []string
}

func (mb *member) received() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return append([]string(nil), mb.got...)
}

func running(peers ...tailnet.Node) func(self tailnet.Node) tailnet.State {
	return func(self tailnet.Node) tailnet.State {
		return tailnet.State{Backend: "Running", Self: self, Peers: peers}
	}
}

// start runs a Mesh for self that sees the given peers.
func start(t *testing.T, port int, self tailnet.Node, peers ...tailnet.Node) *member {
	t.Helper()
	mb := &member{self: self, tn: tailnettest.New(running(peers...)(self))}
	mb.mesh = New(Config{
		Port:    port,
		Tailnet: mb.tn,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Poll:    20 * time.Millisecond,
		Receive: func(from tailnet.Node, payload []byte) {
			mb.mu.Lock()
			mb.got = append(mb.got, from.Name+":"+string(payload))
			mb.mu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mb.mesh.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Mesh.Run did not return after cancel")
		}
	})
	return mb
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

func connected(mb *member) int {
	n := 0
	for _, p := range mb.mesh.Status().Peers {
		if p.Connected {
			n++
		}
	}
	return n
}

func TestThreeMachinesFormAFullMesh(t *testing.T) {
	nodes, port := tailnetOf(t, 3)
	n1, n2, n3 := nodes[0], nodes[1], nodes[2]
	a := start(t, port, n1, n2, n3)
	b := start(t, port, n2, n1, n3)
	c := start(t, port, n3, n1, n2)
	for _, mb := range []*member{a, b, c} {
		eventually(t, mb.self.Name+" to connect to both peers", func() bool { return connected(mb) == 2 })
	}

	b.mesh.Broadcast([]byte(`"from b"`))
	eventually(t, "a and c to receive b's message", func() bool {
		return len(a.received()) == 1 && len(c.received()) == 1
	})
	if got := a.received()[0]; got != `machine2:"from b"` {
		t.Errorf("a received %q", got)
	}
	if got := b.received(); len(got) != 0 {
		t.Errorf("b received its own broadcast: %q", got)
	}

	st := a.mesh.Status()
	if st.Tailscale != "Running" || len(st.Listening) != 1 || !strings.HasPrefix(st.Listening[0], addr(1)+":") {
		t.Errorf("status = %+v", st)
	}
}

// dialAs opens a WebSocket to target from the given loopback address.
func dialAs(t *testing.T, from string, port int, target string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	tr := &http.Transport{DialContext: d.DialContext}
	t.Cleanup(tr.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, fmt.Sprintf("ws://%s:%d/ws", target, port), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: tr},
		HTTPHeader: header,
	})
}

func TestStrangersAndBrowsersAreRefused(t *testing.T) {
	nodes, port := tailnetOf(t, 2)
	n1, n2 := nodes[0], nodes[1]
	a := start(t, port, n1, n2)
	eventually(t, "a to listen", func() bool { return len(a.mesh.Status().Listening) == 1 })

	// The third address is on the "tailnet" but is not one of a's peers.
	_, resp, err := dialAs(t, addr(3), port, addr(1), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("stranger: err = %v, resp = %v; want 403", err, resp)
	}

	// A web page on a real peer machine: right address, but browsers send Origin.
	_, resp, err = dialAs(t, addr(2), port, addr(1), http.Header{"Origin": {"http://" + addr(1)}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("browser: err = %v, resp = %v; want 403", err, resp)
	}

	if n := connected(a); n != 0 {
		t.Errorf("a has %d connections, want 0", n)
	}
}

func TestPeerWithUnexpectedIdentityIsRejected(t *testing.T) {
	nodes, port := tailnetOf(t, 2)
	n1, n2 := nodes[0], nodes[1]
	// a expects n2 at its address, but the machine answering there says it
	// is someone else.
	impostor := n2
	impostor.ID = "n2-other"
	a := start(t, port, n1, n2)
	start(t, port, impostor, n1)

	eventually(t, "a to report the mismatch", func() bool {
		ps := a.mesh.Status().Peers
		return len(ps) == 1 && strings.Contains(ps[0].Error, "expected node n2")
	})
	if n := connected(a); n != 0 {
		t.Errorf("a has %d connections, want 0", n)
	}
}

func TestTailscaleGoingDownDropsEverythingAndRecovers(t *testing.T) {
	nodes, port := tailnetOf(t, 2)
	n1, n2 := nodes[0], nodes[1]
	a := start(t, port, n1, n2)
	b := start(t, port, n2, n1)
	eventually(t, "a and b to connect", func() bool { return connected(a) == 1 && connected(b) == 1 })

	a.tn.Set(tailnet.State{Backend: "Stopped", Self: n1}, nil)
	eventually(t, "a to stop listening and b to notice", func() bool {
		return len(a.mesh.Status().Listening) == 0 && connected(a) == 0 && connected(b) == 0
	})
	// Nothing answers on a's address while Tailscale is down.
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", addr(1), port), time.Second); err == nil {
		conn.Close()
		t.Error("a still accepts connections with Tailscale stopped")
	}

	a.tn.Set(running(n2)(n1), nil)
	eventually(t, "a and b to reconnect", func() bool { return connected(a) == 1 && connected(b) == 1 })
	a.mesh.Broadcast([]byte(`"back"`))
	eventually(t, "b to receive after the reconnect", func() bool { return len(b.received()) == 1 })
}

func TestPeerRemovedFromTailnetIsDisconnected(t *testing.T) {
	nodes, port := tailnetOf(t, 2)
	n1, n2 := nodes[0], nodes[1]
	a := start(t, port, n1, n2)
	b := start(t, port, n2, n1)
	eventually(t, "a and b to connect", func() bool { return connected(a) == 1 && connected(b) == 1 })

	// n2 is removed from the user's devices: a no longer lists it.
	a.tn.Set(running()(n1), nil)
	eventually(t, "a to drop n2", func() bool { return len(a.mesh.Status().Peers) == 0 && connected(b) == 0 })
}
