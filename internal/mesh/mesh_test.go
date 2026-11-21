package mesh

import (
	"bytes"
	"context"
	"errors"
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

	mu     sync.Mutex
	got    []string // messages, each prefixed with the sender's name
	pieces [][]byte // pieces of streams, as they arrived
	events []string // messages, "piece" for each piece, and "closed", in order
}

func (mb *member) received() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return append([]string(nil), mb.got...)
}

// streamed returns the bytes of all pieces received so far.
func (mb *member) streamed() []byte {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return bytes.Join(mb.pieces, nil)
}

func (mb *member) history() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return append([]string(nil), mb.events...)
}

// conn is the Handler for one of a member's connections.
type conn struct {
	mb   *member
	from tailnet.Node
}

func (c conn) Message(payload []byte) {
	c.mb.mu.Lock()
	defer c.mb.mu.Unlock()
	c.mb.got = append(c.mb.got, c.from.Name+":"+string(payload))
	c.mb.events = append(c.mb.events, string(payload))
}

func (c conn) Piece(data []byte) {
	c.mb.mu.Lock()
	defer c.mb.mu.Unlock()
	c.mb.pieces = append(c.mb.pieces, bytes.Clone(data))
	c.mb.events = append(c.mb.events, "piece")
}

func (c conn) Closed() {
	c.mb.mu.Lock()
	defer c.mb.mu.Unlock()
	c.mb.events = append(c.mb.events, "closed")
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
		Connect: func(from tailnet.Node) Handler { return conn{mb: mb, from: from} },
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

// pair starts two connected machines.
func pair(t *testing.T) (a, b *member) {
	t.Helper()
	nodes, port := tailnetOf(t, 2)
	a = start(t, port, nodes[0], nodes[1])
	b = start(t, port, nodes[1], nodes[0])
	eventually(t, "a and b to connect", func() bool { return connected(a) == 1 && connected(b) == 1 })
	return a, b
}

// streamOf returns a Stream carrying data, with messages named after label.
func streamOf(label string, data []byte) *Stream {
	return &Stream{
		Head:  []byte(label + " head"),
		Open:  func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		Tail:  []byte(label + " tail"),
		Abort: []byte(label + " abort"),
	}
}

func TestStreamArrivesWholeBetweenItsHeadAndTail(t *testing.T) {
	a, b := pair(t)
	// A few pieces and a last one that is not full.
	data := bytes.Repeat([]byte("0123456789abcdef"), 3*pieceSize/16+100)
	if n := a.mesh.Stream(streamOf("s", data)); n != 1 {
		t.Fatalf("Stream reached %d peers, want 1", n)
	}
	eventually(t, "b to receive the tail", func() bool {
		h := b.history()
		return len(h) > 0 && h[len(h)-1] == "s tail"
	})
	if got := b.streamed(); !bytes.Equal(got, data) {
		t.Errorf("b received %d bytes, want the %d that were sent, unchanged", len(got), len(data))
	}
	h := b.history()
	if h[0] != "s head" || len(h) != 6 {
		t.Errorf("b saw %q, want the head, four pieces and the tail", h)
	}
	if got := a.streamed(); len(got) != 0 {
		t.Errorf("a received %d bytes of its own stream", len(got))
	}
}

// slowReader hands out its data a piece at a time, each after a pause, the
// way a stream behaves on a slow link or a slow disk.
type slowReader struct {
	left   int
	closed chan struct{}
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	select {
	case <-r.closed:
		return 0, errors.New("read after close")
	case <-time.After(2 * time.Millisecond):
	}
	n := min(len(p), pieceSize, r.left)
	r.left -= n
	return n, nil
}

func (r *slowReader) Close() error {
	close(r.closed)
	return nil
}

func TestMessagesAreNotHeldUpBehindAStream(t *testing.T) {
	a, b := pair(t)
	slow := &slowReader{left: 200 * pieceSize, closed: make(chan struct{})}
	st := streamOf("s", nil)
	st.Open = func() (io.ReadCloser, error) { return slow, nil }
	a.mesh.Stream(st)
	eventually(t, "the stream to get going", func() bool { return len(b.history()) > 3 })

	a.mesh.Broadcast([]byte("urgent"))
	eventually(t, "b to receive the message", func() bool { return len(b.received()) == 1 })
	// The stream takes 400ms at the least; the message must not have waited
	// for it.
	h := b.history()
	if last := h[len(h)-1]; last == "s tail" || len(h) > 150 {
		t.Errorf("the message arrived after %d pieces, at the end of the stream", len(h))
	}
	eventually(t, "the rest of the stream to arrive", func() bool {
		h := b.history()
		return h[len(h)-1] == "s tail"
	})
	if got := len(b.streamed()); got != 200*pieceSize {
		t.Errorf("b received %d bytes, want %d", got, 200*pieceSize)
	}
}

func TestNewerStreamReplacesTheOneOnItsWay(t *testing.T) {
	a, b := pair(t)
	slow := &slowReader{left: 1000 * pieceSize, closed: make(chan struct{})}
	old := streamOf("old", nil)
	old.Open = func() (io.ReadCloser, error) { return slow, nil }
	a.mesh.Stream(old)
	eventually(t, "the old stream to get going", func() bool { return len(b.history()) > 3 })

	a.mesh.Stream(streamOf("new", []byte("replacement")))
	eventually(t, "b to receive the new stream", func() bool {
		h := b.history()
		return h[len(h)-1] == "new tail"
	})
	h := b.history()
	tail := h[len(h)-4:]
	if want := []string{"old abort", "new head", "piece", "new tail"}; fmt.Sprint(tail) != fmt.Sprint(want) {
		t.Errorf("b saw ...%q, want ...%q", tail, want)
	}
	select {
	case <-slow.closed:
	case <-time.After(time.Second):
		t.Error("the data of the abandoned stream was never closed")
	}
}

func TestStreamCanBeAbandonedAndAFailingOneIsReportedAsCutShort(t *testing.T) {
	a, b := pair(t)
	slow := &slowReader{left: 1000 * pieceSize, closed: make(chan struct{})}
	st := streamOf("s", nil)
	st.Open = func() (io.ReadCloser, error) { return slow, nil }
	a.mesh.Stream(st)
	eventually(t, "the stream to get going", func() bool { return len(b.history()) > 3 })
	a.mesh.Stream(nil)
	eventually(t, "b to hear the stream was abandoned", func() bool {
		h := b.history()
		return h[len(h)-1] == "s abort"
	})

	// A stream whose data fails part way.
	failing := streamOf("f", nil)
	failing.Open = func() (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(make([]byte, pieceSize+10)), errReader{})), nil
	}
	a.mesh.Stream(failing)
	eventually(t, "b to hear the stream was cut short", func() bool {
		h := b.history()
		return h[len(h)-1] == "f abort"
	})

	// One that cannot even be opened is never announced.
	before := len(b.history())
	unopened := streamOf("u", nil)
	unopened.Open = func() (io.ReadCloser, error) { return nil, errors.New("gone") }
	a.mesh.Stream(unopened)
	a.mesh.Broadcast([]byte("after"))
	eventually(t, "b to receive the message sent after", func() bool { return len(b.history()) > before })
	if h := b.history(); h[len(h)-1] != "after" || len(h) != before+1 {
		t.Errorf("b saw %q after the unopenable stream, want only the message", h[before:])
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestHandlerIsClosedWhenTheConnectionEnds(t *testing.T) {
	a, b := pair(t)
	a.mesh.Broadcast([]byte("hello"))
	eventually(t, "b to receive", func() bool { return len(b.received()) == 1 })
	// a's Tailscale goes down, which drops the connection on both sides.
	a.tn.Set(tailnet.State{Backend: "Stopped", Self: a.self}, nil)
	eventually(t, "b's handler to be closed", func() bool {
		h := b.history()
		return h[len(h)-1] == "closed"
	})
}

// An older build says nothing about streams in its hello. It is sent
// messages, and never the pieces it would not know what to do with.
func TestPeerFromBeforeStreamsIsSentMessagesOnly(t *testing.T) {
	nodes, port := tailnetOf(t, 2)
	n1, n2 := nodes[0], nodes[1]
	// n2 is played by hand so that it can speak like an old build; n1 < n2,
	// so it has to be the one that is dialled.
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr(2), port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type frame struct {
		typ  websocket.MessageType
		data string
	}
	frames := make(chan frame, 16)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		if _, _, err := ws.Read(ctx); err != nil { // the new build's hello
			return
		}
		ws.Write(ctx, websocket.MessageText, []byte(`{"proto":1,"node":"n2"}`))
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			frames <- frame{typ, string(data)}
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	a := start(t, port, n1, n2)
	eventually(t, "a to connect to the old build", func() bool { return connected(a) == 1 })
	if n := a.mesh.Stream(streamOf("s", []byte("data"))); n != 0 {
		t.Errorf("Stream reached %d peers, want 0", n)
	}
	a.mesh.Broadcast([]byte("text"))
	select {
	case f := <-frames:
		if f.typ != websocket.MessageText || f.data != "text" {
			t.Errorf("the old build received %v %q, want only the message", f.typ, f.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old build received nothing")
	}
}
