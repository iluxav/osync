package tailnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shaped like real `tailscale status --json` output, trimmed to the fields
// that matter plus a few that must be ignored.
const statusJSON = `{
  "Version": "1.104.2",
  "BackendState": "Running",
  "TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"],
  "Self": {
    "ID": "nSELF", "HostName": "Home Desk", "DNSName": "home-desk.tail1234.ts.net.",
    "OS": "linux", "UserID": 1001, "TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"], "Online": true
  },
  "Peer": {
    "nodekey:aa": {"ID": "nOFFICE", "HostName": "office", "DNSName": "office.tail1234.ts.net.",
      "OS": "linux", "UserID": 1001, "TailscaleIPs": ["100.64.0.2"], "Online": true},
    "nodekey:bb": {"ID": "nMAC", "HostName": "MacBook Pro", "DNSName": "macbook.tail1234.ts.net.",
      "OS": "macOS", "UserID": 1001, "TailscaleIPs": ["100.64.0.3"], "Online": false},
    "nodekey:cc": {"ID": "nPHONE", "HostName": "iPhone", "DNSName": "iphone.tail1234.ts.net.",
      "OS": "iOS", "UserID": 1001, "TailscaleIPs": ["100.64.0.4"], "Online": true},
    "nodekey:dd": {"ID": "nOTHER", "HostName": "colleague", "DNSName": "colleague.tail1234.ts.net.",
      "OS": "linux", "UserID": 2002, "TailscaleIPs": ["100.64.0.5"], "Online": true},
    "nodekey:ee": {"ID": "nSERVER", "HostName": "server", "DNSName": "server.tail1234.ts.net.",
      "OS": "linux", "UserID": 1001, "Tags": ["tag:server"], "TailscaleIPs": ["100.64.0.6"], "Online": true},
    "nodekey:ff": {"ID": "nSHARED", "HostName": "shared", "DNSName": "shared.tail1234.ts.net.",
      "OS": "linux", "UserID": 1001, "ShareeNode": true, "TailscaleIPs": ["100.64.0.7"], "Online": true}
  }
}`

func TestParseKeepsOnlyOwnSupportedDevices(t *testing.T) {
	st, err := Parse([]byte(statusJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running() {
		t.Errorf("Running() = false, want true")
	}
	if st.Self.ID != "nSELF" || st.Self.Name != "home-desk" {
		t.Errorf("Self = %+v, want ID nSELF named home-desk", st.Self)
	}
	if a, ok := st.Self.Addr(); !ok || a.String() != "100.64.0.1" {
		t.Errorf("Self.Addr() = %v, %v, want 100.64.0.1", a, ok)
	}

	var got []string
	for _, p := range st.Peers {
		got = append(got, p.Name)
	}
	// The phone (unsupported OS), the colleague (other user), the tagged
	// server and the shared node must all be gone.
	if want := "macbook,office"; strings.Join(got, ",") != want {
		t.Errorf("peers = %v, want %s", got, want)
	}
	if st.Peers[0].Online || !st.Peers[1].Online {
		t.Errorf("online flags wrong: %+v", st.Peers)
	}
}

func TestParseTaggedSelfHasNoPeers(t *testing.T) {
	tagged := strings.Replace(statusJSON, `"OS": "linux", "UserID": 1001, "TailscaleIPs": ["100.64.0.1"`,
		`"OS": "linux", "UserID": 1001, "Tags": ["tag:desk"], "TailscaleIPs": ["100.64.0.1"`, 1)
	if tagged == statusJSON {
		t.Fatal("fixture replacement did not apply")
	}
	st, err := Parse([]byte(tagged))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Peers) != 0 || st.Note == "" {
		t.Errorf("tagged self: peers = %v, note = %q; want no peers and a note", st.Peers, st.Note)
	}
}

func TestParseStoppedAndGarbage(t *testing.T) {
	st, err := Parse([]byte(`{"BackendState": "Stopped", "Self": null, "Peer": null}`))
	if err != nil || st.Running() || st.Backend != "Stopped" {
		t.Errorf("stopped: st = %+v, err = %v", st, err)
	}
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("garbage input: want an error")
	}
}

// fakeCLI writes an executable named tailscale that runs script.
func fakeCLI(t *testing.T, script string) *CLI {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &CLI{path: path}
}

func TestCLIReadsStatus(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(fixture, []byte(statusJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// The script fails unless called exactly as `tailscale status --json`.
	c := fakeCLI(t, `[ "$1 $2" = "status --json" ] || exit 9; cat '`+fixture+`'`)
	st, err := c.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Self.Name != "home-desk" || len(st.Peers) != 2 {
		t.Errorf("state = %+v", st)
	}
}

func TestCLIReportsWhyTailscaleIsDown(t *testing.T) {
	c := fakeCLI(t, `echo "failed to connect to local tailscaled; it doesn't appear to be running" >&2; echo "second line" >&2; exit 1`)
	_, err := c.State(context.Background())
	if err == nil || err.Error() != "failed to connect to local tailscaled; it doesn't appear to be running" {
		t.Errorf("err = %v, want the first stderr line", err)
	}
}
