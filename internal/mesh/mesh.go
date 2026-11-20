// Package mesh keeps one WebSocket open to every peer on the tailnet and
// carries messages between them.
//
// Every machine listens on its Tailscale addresses only. For each pair of
// machines exactly one side dials: the one whose node ID sorts first.
package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/iluxav/osync/internal/tailnet"
)

// protoVersion is bumped when peers running different builds could no longer
// understand each other.
const protoVersion = 1

const (
	defaultPoll      = 5 * time.Second
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 5 * time.Second
	writeTimeout     = 10 * time.Second
	pingInterval     = 15 * time.Second
	pingTimeout      = 10 * time.Second
	readLimit        = 32 << 20
)

// Config configures a Mesh.
type Config struct {
	Port    int
	Tailnet tailnet.Source
	// Receive is called with every message a peer sends. Calls for one peer
	// are sequential; calls for different peers may overlap.
	Receive func(from tailnet.Node, payload []byte)
	Log     *slog.Logger
	// Poll is how often the tailnet state is re-read. Zero means 5 seconds.
	Poll time.Duration
}

// Mesh is the set of connections to the other machines.
type Mesh struct {
	cfg Config
	srv *http.Server

	mu        sync.Mutex
	runCtx    context.Context
	closed    bool
	polled    bool // the tailnet state has been read at least once
	state     tailnet.State
	stateErr  error
	listenErr error
	listeners map[netip.Addr]net.Listener
	peers     map[string]*peer // by node ID
	wg        sync.WaitGroup
}

type peer struct {
	node    tailnet.Node
	link    *link
	dialing bool
	backoff time.Duration
	retryAt time.Time
	lastErr string
}

// link is one established connection to a peer.
type link struct {
	ws     *websocket.Conn
	out    chan []byte
	since  time.Time
	cancel context.CancelFunc
}

// hello is the first message each side sends on a new connection.
type hello struct {
	Proto int    `json:"proto"`
	Node  string `json:"node"`
}

// New returns a Mesh. It does nothing until Run is called.
func New(cfg Config) *Mesh {
	if cfg.Poll == 0 {
		cfg.Poll = defaultPoll
	}
	m := &Mesh{
		cfg:       cfg,
		listeners: make(map[netip.Addr]net.Listener),
		peers:     make(map[string]*peer),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", m.accept)
	m.srv = &http.Server{Handler: mux, ReadHeaderTimeout: handshakeTimeout}
	return m
}

// Run keeps the mesh in line with the tailnet until ctx is done: it listens
// while Tailscale is up, connects to peers as they come online, and drops
// everything when Tailscale goes down.
func (m *Mesh) Run(ctx context.Context) error {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()

	tick := time.NewTicker(m.cfg.Poll)
	defer tick.Stop()
	for ctx.Err() == nil {
		st, err := m.cfg.Tailnet.State(ctx)
		if ctx.Err() != nil {
			break
		}
		m.reconcile(st, err)
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}

	m.mu.Lock()
	m.closed = true
	m.dropAllLocked()
	m.mu.Unlock()
	m.srv.Close()
	m.wg.Wait()
	return ctx.Err()
}

// reconcile brings listeners and connections in line with st.
func (m *Mesh) reconcile(st tailnet.State, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wasUp := m.stateErr == nil && m.state.Running()
	first := !m.polled
	m.polled = true
	m.state, m.stateErr = st, err
	if err != nil || !st.Running() {
		switch {
		case wasUp:
			m.cfg.Log.Warn("Tailscale went down, dropping connections", "state", st.Backend, "err", err)
		case first:
			m.cfg.Log.Info("waiting for Tailscale", "state", st.Backend, "err", err)
		}
		m.dropAllLocked()
		return
	}
	if !wasUp {
		m.cfg.Log.Info("Tailscale is up", "self", st.Self.Name, "peers", len(st.Peers))
	}

	for addr, ln := range m.listeners {
		if !slices.Contains(st.Self.Addrs, addr) {
			ln.Close()
			delete(m.listeners, addr)
		}
	}
	m.listenErr = nil
	for _, addr := range st.Self.Addrs {
		if m.listeners[addr] != nil {
			continue
		}
		ln, err := net.Listen("tcp", netip.AddrPortFrom(addr, uint16(m.cfg.Port)).String())
		if err != nil {
			m.listenErr = err
			continue
		}
		m.listeners[addr] = ln
		m.wg.Go(func() { m.srv.Serve(ln) })
	}

	now := time.Now()
	current := make(map[string]bool, len(st.Peers))
	for _, n := range st.Peers {
		current[n.ID] = true
		p := m.peers[n.ID]
		if p == nil {
			p = &peer{}
			m.peers[n.ID] = p
		}
		p.node = n
		if p.link == nil && !p.dialing && n.Online && st.Self.ID < n.ID && !now.Before(p.retryAt) {
			p.dialing = true
			m.wg.Go(func() { m.dial(st.Self, n) })
		}
	}
	for id, p := range m.peers {
		if !current[id] {
			if p.link != nil {
				p.link.cancel()
			}
			delete(m.peers, id)
		}
	}
}

// dropAllLocked closes every listener and connection.
func (m *Mesh) dropAllLocked() {
	for addr, ln := range m.listeners {
		ln.Close()
		delete(m.listeners, addr)
	}
	for id, p := range m.peers {
		if p.link != nil {
			p.link.cancel()
		}
		delete(m.peers, id)
	}
}

// dial connects to n and records the outcome.
func (m *Mesh) dial(self, n tailnet.Node) {
	ws, err := m.connect(self, n)

	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[n.ID]
	if p == nil || m.closed {
		// The peer left the tailnet, or Tailscale went down, meanwhile.
		if ws != nil {
			ws.CloseNow()
		}
		return
	}
	p.dialing = false
	if err != nil {
		// Retry after one poll, then two, then every three: a peer that is
		// online but not answering has usually just not started its daemon
		// yet, and it cannot dial us, so keep trying at a steady pace.
		p.backoff = min(p.backoff+m.cfg.Poll, 3*m.cfg.Poll)
		p.retryAt = time.Now().Add(p.backoff)
		p.lastErr = err.Error()
		m.cfg.Log.Debug("dial failed", "peer", n.Name, "err", err)
		return
	}
	m.attachLocked(p, ws)
}

func (m *Mesh) connect(self, n tailnet.Node) (*websocket.Conn, error) {
	to, ok := n.Addr()
	if !ok {
		return nil, errors.New("peer has no address")
	}
	from, ok := self.AddrLike(to)
	if !ok {
		return nil, fmt.Errorf("no local Tailscale address to reach %s from", to)
	}
	ctx, cancel := context.WithTimeout(m.runCtx, dialTimeout)
	defer cancel()

	// Binding to our own Tailscale address keeps the connection on the
	// tailnet. If Tailscale is down that address is gone and the dial fails,
	// rather than the peer's 100.x address being routed over whatever network
	// is left, where a stranger could answer.
	dialer := &net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(from, 0))}
	transport := &http.Transport{DialContext: dialer.DialContext}
	defer transport.CloseIdleConnections()

	url := "ws://" + netip.AddrPortFrom(to, uint16(m.cfg.Port)).String() + "/ws"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return nil, err
	}
	if err := handshake(ctx, ws, self.ID, n.ID); err != nil {
		ws.CloseNow()
		return nil, err
	}
	return ws, nil
}

// accept handles a peer dialing in.
func (m *Mesh) accept(w http.ResponseWriter, r *http.Request) {
	// Browsers always send Origin with a WebSocket handshake and peers never
	// do. Refusing it keeps a web page open on one of the user's machines
	// from talking to the mesh, whatever name it used to reach this address.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "bad remote address", http.StatusBadRequest)
		return
	}
	from := remote.Addr().Unmap()

	m.mu.Lock()
	self := m.state.Self
	n, ok := m.peerAtLocked(from)
	m.mu.Unlock()
	if !ok {
		m.cfg.Log.Warn("refused connection from a device that is not a peer", "from", from)
		http.Error(w, "not a peer", http.StatusForbidden)
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	if err := handshake(r.Context(), ws, self.ID, n.ID); err != nil {
		m.cfg.Log.Warn("handshake failed", "peer", n.Name, "err", err)
		ws.CloseNow()
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[n.ID]
	if p == nil || m.closed {
		ws.CloseNow()
		return
	}
	m.attachLocked(p, ws)
}

func (m *Mesh) peerAtLocked(addr netip.Addr) (tailnet.Node, bool) {
	for _, p := range m.peers {
		if slices.Contains(p.node.Addrs, addr) {
			return p.node, true
		}
	}
	return tailnet.Node{}, false
}

// handshake swaps hellos and checks the other side is who it should be.
func handshake(ctx context.Context, ws *websocket.Conn, selfID, peerID string) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := wsjson.Write(ctx, ws, hello{Proto: protoVersion, Node: selfID}); err != nil {
		return err
	}
	var h hello
	if err := wsjson.Read(ctx, ws, &h); err != nil {
		return err
	}
	if h.Proto != protoVersion {
		return fmt.Errorf("peer speaks protocol %d and this build speaks %d: update both machines", h.Proto, protoVersion)
	}
	if h.Node != peerID {
		return fmt.Errorf("expected node %s, got %s", peerID, h.Node)
	}
	return nil
}

// attachLocked makes ws the connection to p, replacing any earlier one.
func (m *Mesh) attachLocked(p *peer, ws *websocket.Conn) {
	if p.link != nil {
		// The peer reconnected before the old connection was noticed dead.
		p.link.cancel()
	}
	ctx, cancel := context.WithCancel(m.runCtx)
	l := &link{ws: ws, out: make(chan []byte, 32), since: time.Now(), cancel: cancel}
	ws.SetReadLimit(readLimit)
	p.link, p.lastErr, p.backoff, p.retryAt = l, "", 0, time.Time{}
	node := p.node
	m.wg.Go(func() { m.runLink(ctx, node, l) })
	m.cfg.Log.Info("peer connected", "peer", node.Name)
}

// runLink pumps one connection until it fails or is cancelled.
func (m *Mesh) runLink(ctx context.Context, node tailnet.Node, l *link) {
	errs := make(chan error, 2)
	go func() { errs <- m.readLoop(ctx, node, l) }()
	go func() { errs <- writeLoop(ctx, l) }()
	err := <-errs
	l.cancel()
	l.ws.CloseNow()
	<-errs

	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peers[node.ID]; p != nil && p.link == l {
		p.link = nil
		p.lastErr = "connection lost: " + err.Error()
		m.cfg.Log.Info("peer disconnected", "peer", node.Name, "err", err)
	}
}

func (m *Mesh) readLoop(ctx context.Context, node tailnet.Node, l *link) error {
	for {
		typ, data, err := l.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			m.cfg.Receive(node, data)
		}
	}
}

func writeLoop(ctx context.Context, l *link) error {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case payload := <-l.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := l.ws.Write(wctx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return err
			}
		case <-ping.C:
			// A sleeping laptop leaves its connection looking alive. The ping
			// finds out, so the peer shows as disconnected and gets redialled.
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := l.ws.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
	}
}

// Broadcast sends payload to every connected peer.
func (m *Mesh) Broadcast(payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.peers {
		if p.link == nil {
			continue
		}
		select {
		case p.link.out <- payload:
		default:
			// The peer stopped reading. Reconnecting later costs less than
			// queueing clips that will be stale by the time they arrive.
			p.link.cancel()
		}
	}
}

// PeerStatus describes one peer for display.
type PeerStatus struct {
	Name      string    `json:"name"`
	OS        string    `json:"os"`
	Addr      string    `json:"addr"`
	Online    bool      `json:"online"` // as reported by Tailscale
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Status describes the mesh for display.
type Status struct {
	Tailscale   string       `json:"tailscale"` // Tailscale backend state
	Error       string       `json:"error,omitempty"`
	Note        string       `json:"note,omitempty"`
	Self        tailnet.Node `json:"self"`
	Listening   []string     `json:"listening"`
	ListenError string       `json:"listenError,omitempty"`
	Peers       []PeerStatus `json:"peers"`
}

// Status returns a snapshot of the mesh.
func (m *Mesh) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Tailscale: m.state.Backend, Note: m.state.Note, Self: m.state.Self}
	if m.stateErr != nil {
		s.Error = m.stateErr.Error()
	}
	if m.listenErr != nil {
		s.ListenError = m.listenErr.Error()
	}
	for addr := range m.listeners {
		s.Listening = append(s.Listening, netip.AddrPortFrom(addr, uint16(m.cfg.Port)).String())
	}
	slices.Sort(s.Listening)
	for _, p := range m.peers {
		ps := PeerStatus{Name: p.node.Name, OS: p.node.OS, Online: p.node.Online, Error: p.lastErr}
		if a, ok := p.node.Addr(); ok {
			ps.Addr = a.String()
		}
		if p.link != nil {
			ps.Connected, ps.Since = true, p.link.since
		}
		s.Peers = append(s.Peers, ps)
	}
	slices.SortFunc(s.Peers, func(a, b PeerStatus) int { return strings.Compare(a.Name, b.Name) })
	return s
}
