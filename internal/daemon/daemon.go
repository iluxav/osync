// Package daemon wires the pieces into the running service: the mesh of peer
// connections, the clipboard sync engine, and the control socket that the
// status command reads.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iluxav/osync/internal/clipboard"
	"github.com/iluxav/osync/internal/clipsync"
	"github.com/iluxav/osync/internal/mesh"
	"github.com/iluxav/osync/internal/tailnet"
)

// DefaultPort is the TCP port every machine listens on, on its Tailscale
// addresses. All machines must use the same one.
const DefaultPort = 24817

// Config configures the daemon.
type Config struct {
	Version   string
	Port      int
	Socket    string // path of the control socket
	Tailnet   tailnet.Source
	Clipboard clipboard.Clipboard
	Log       *slog.Logger
	Poll      time.Duration // see mesh.Config.Poll
}

// Status is what the control socket reports.
type Status struct {
	Version   string         `json:"version"`
	PID       int            `json:"pid"`
	Started   time.Time      `json:"started"`
	Mesh      mesh.Status    `json:"mesh"`
	Clipboard clipsync.Stats `json:"clipboard"`
}

// message is what peers exchange. Type names the kind; unknown kinds are
// ignored so that newer builds can add some.
type message struct {
	Type string `json:"type"`
	clipsync.Clip
}

// DefaultSocket returns where the control socket lives for the current user.
func DefaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "osync.sock")
	}
	// macOS: TMPDIR is already private to the user.
	return filepath.Join(os.TempDir(), fmt.Sprintf("osync-%d.sock", os.Getuid()))
}

// Run runs the daemon until ctx is done or a part of it fails.
func Run(ctx context.Context, cfg Config) error {
	ctl, err := listenControl(cfg.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Socket)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var engine *clipsync.Engine
	m := mesh.New(mesh.Config{
		Port:    cfg.Port,
		Tailnet: cfg.Tailnet,
		Log:     cfg.Log,
		Poll:    cfg.Poll,
		Receive: func(from tailnet.Node, payload []byte) {
			var msg message
			if err := json.Unmarshal(payload, &msg); err != nil {
				cfg.Log.Warn("unreadable message", "peer", from.Name, "err", err)
				return
			}
			if msg.Type == "clip" {
				engine.Remote(ctx, msg.Clip)
			}
		},
	})
	engine = clipsync.New(cfg.Clipboard, func(clip clipsync.Clip) {
		payload, err := json.Marshal(message{Type: "clip", Clip: clip})
		if err != nil {
			cfg.Log.Error("cannot encode clip", "err", err)
			return
		}
		m.Broadcast(payload)
	}, cfg.Log)

	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Status{
			Version:   cfg.Version,
			PID:       os.Getpid(),
			Started:   started,
			Mesh:      m.Status(),
			Clipboard: engine.Stats(),
		})
	})
	srv := &http.Server{Handler: mux}

	// The first part to stop takes the others down with it.
	var (
		wg    sync.WaitGroup
		once  sync.Once
		cause error
	)
	part := func(name string, run func() error) {
		wg.Go(func() {
			err := run()
			once.Do(func() {
				if ctx.Err() == nil {
					cause = fmt.Errorf("%s stopped: %w", name, err)
				}
				cancel()
			})
		})
	}
	part("mesh", func() error { return m.Run(ctx) })
	part("clipboard", func() error { return engine.Run(ctx) })
	part("control socket", func() error { return srv.Serve(ctl) })

	<-ctx.Done()
	srv.Close()
	wg.Wait()
	return cause
}

// listenControl opens the control socket, refusing if a daemon already
// answers on it.
func listenControl(path string) (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("already running (control socket %s is in use)", path)
	}
	// Whatever is at the path now is left over from a daemon that died.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Query asks the daemon listening on socket for its status.
func Query(ctx context.Context, socket string) (Status, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://osync/status", nil)
	if err != nil {
		return Status{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("unreadable status: %w", err)
	}
	return st, nil
}
