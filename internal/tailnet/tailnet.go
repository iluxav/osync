// Package tailnet reports where this machine sits in the user's Tailscale
// network: whether Tailscale is up, this machine's addresses, and which other
// devices belong to the same person.
package tailnet

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Node is one device on the tailnet.
type Node struct {
	ID     string       `json:"id"`   // Tailscale stable node ID
	Name   string       `json:"name"` // short machine name
	OS     string       `json:"os"`
	Addrs  []netip.Addr `json:"addrs"` // Tailscale IPs
	Online bool         `json:"online"`
}

// Addr returns the address to reach the node on: IPv4 when it has one.
func (n Node) Addr() (netip.Addr, bool) {
	for _, a := range n.Addrs {
		if a.Is4() {
			return a, true
		}
	}
	if len(n.Addrs) > 0 {
		return n.Addrs[0], true
	}
	return netip.Addr{}, false
}

// AddrLike returns the node's address in the same family as other.
func (n Node) AddrLike(other netip.Addr) (netip.Addr, bool) {
	for _, a := range n.Addrs {
		if a.Is4() == other.Is4() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// State is a snapshot of the local Tailscale client.
type State struct {
	Backend string // Tailscale's backend state; "Running" once connected
	Self    Node
	Peers   []Node // the devices to sync with, sorted by name
	Note    string // why Peers was deliberately left empty, if it was
}

// Running reports whether Tailscale is connected.
func (s State) Running() bool { return s.Backend == "Running" }

// Source provides the current State.
type Source interface {
	State(ctx context.Context) (State, error)
}

// The subset of `tailscale status --json` (ipnstate.Status) that is used.
type rawStatus struct {
	BackendState string
	Self         *rawPeer
	Peer         map[string]*rawPeer
}

type rawPeer struct {
	ID           string
	HostName     string
	DNSName      string
	OS           string
	UserID       int64
	TailscaleIPs []netip.Addr
	Tags         []string
	Online       bool
	ShareeNode   bool
}

func (p *rawPeer) node() Node {
	name, _, _ := strings.Cut(p.DNSName, ".")
	if name == "" {
		name = p.HostName
	}
	return Node{ID: p.ID, Name: name, OS: p.OS, Addrs: p.TailscaleIPs, Online: p.Online}
}

// Parse turns the output of `tailscale status --json` into a State.
//
// Peers are the devices that receive this machine's clipboard, so the choice
// is conservative: only devices owned by the same Tailscale user. Devices of
// other users, devices shared into the tailnet, and tagged devices (which have
// no owner) are left out, as are operating systems the daemon does not run on.
func Parse(statusJSON []byte) (State, error) {
	var raw rawStatus
	if err := json.Unmarshal(statusJSON, &raw); err != nil {
		// Show what arrived instead: the client prints plain sentences when
		// it cannot do its job, and those say more than a JSON syntax error.
		return State{}, fmt.Errorf("parse tailscale status: %w (it printed %q)", err, firstLine(statusJSON))
	}
	st := State{Backend: raw.BackendState}
	if raw.Self == nil {
		return st, nil
	}
	st.Self = raw.Self.node()
	if len(raw.Self.Tags) > 0 {
		st.Note = "this device is tagged in Tailscale, so it has no owner to match other devices against"
		return st, nil
	}
	for _, p := range raw.Peer {
		if p == nil || p.ID == "" || len(p.TailscaleIPs) == 0 {
			continue
		}
		if p.UserID != raw.Self.UserID || len(p.Tags) > 0 || p.ShareeNode || !supportedOS(p.OS) {
			continue
		}
		st.Peers = append(st.Peers, p.node())
	}
	slices.SortFunc(st.Peers, func(a, b Node) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return st, nil
}

func supportedOS(os string) bool {
	switch strings.ToLower(os) {
	case "linux", "macos", "darwin":
		return true
	}
	return false
}

// CLI is a Source backed by the tailscale command-line client.
type CLI struct {
	mu   sync.Mutex
	path string
}

// Where the tailscale client lives when it is not on PATH. launchd agents get
// a minimal PATH, and the macOS app only ships the client inside its bundle.
var cliPaths = []string{
	"/usr/bin/tailscale",
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

func (c *CLI) binary() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "" {
		return c.path, nil
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		c.path = p
		return p, nil
	}
	for _, p := range cliPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			c.path = p
			return p, nil
		}
	}
	return "", errors.New("Tailscale is not installed (no tailscale command found)")
}

// State runs `tailscale status --json`.
func (c *CLI) State(ctx context.Context) (State, error) {
	path, err := c.binary()
	if err != nil {
		return State{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "status", "--json")
	// On macOS the client and the app are one binary, which picks its role
	// from the terminal variables around it. A launchd agent has none, so
	// without this the binary acts as the app and prints no status.
	cmd.Env = append(os.Environ(), "TAILSCALE_BE_CLI=1")
	out, err := cmd.Output()
	if err == nil {
		return Parse(out)
	}
	if errors.Is(err, fs.ErrNotExist) {
		// Uninstalled or moved since it was found; look again next time.
		c.mu.Lock()
		c.path = ""
		c.mu.Unlock()
	}
	// The client can exit non-zero and still print a usable status.
	if st, perr := Parse(out); perr == nil && st.Backend != "" {
		return st, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if line := firstLine(exit.Stderr); line != "" {
			return State{}, errors.New(line)
		}
	}
	return State{}, fmt.Errorf("tailscale status: %w", err)
}

// firstLine returns the first line of what a command printed, cut short enough
// to sit in an error message.
func firstLine(output []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 200 {
		line = string(r[:200]) + "…"
	}
	return line
}
