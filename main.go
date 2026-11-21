// osync shares the clipboard, text and copied files, between a person's Linux
// and macOS machines over their Tailscale network.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/iluxav/osync/internal/clipboard"
	"github.com/iluxav/osync/internal/daemon"
	"github.com/iluxav/osync/internal/tailnet"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `osync shares the clipboard between your machines over Tailscale.

Usage:
  osync daemon [--port N]   run the sync daemon (normally started by the service)
  osync status [--json]     show Tailscale state and connected machines
  osync version             print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "daemon":
		err = runDaemon(args)
	case "status":
		err = runStatus(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "osync:", err)
		os.Exit(1)
	}
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	port := fs.Int("port", daemon.DefaultPort, "TCP port to listen on; must be the same on every machine")
	verbose := fs.Bool("verbose", false, "log connection attempts")
	fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cb, err := clipboard.New(log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "version", version, "port", *port)
	return daemon.Run(ctx, daemon.Config{
		Version:   version,
		Port:      *port,
		Socket:    daemon.DefaultSocket(),
		Files:     daemon.DefaultFiles(),
		Tailnet:   &tailnet.CLI{},
		Clipboard: cb,
		Log:       log,
	})
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the raw status as JSON")
	fs.Parse(args)

	st, err := daemon.Query(context.Background(), daemon.DefaultSocket())
	if err != nil {
		// No socket, or one left behind by a daemon that died.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("the daemon is not running")
		}
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(os.Stdout, st, time.Now())
	return nil
}

func printStatus(out io.Writer, st daemon.Status, now time.Time) {
	fmt.Fprintf(out, "osync %s, running for %s\n\n", st.Version, age(now.Sub(st.Started)))

	m := st.Mesh
	switch {
	case m.Error != "":
		fmt.Fprintf(out, "Tailscale:  not available: %s\n", m.Error)
	case m.Tailscale != "Running":
		fmt.Fprintf(out, "Tailscale:  %s (waiting for it to connect)\n", m.Tailscale)
	default:
		fmt.Fprintf(out, "Tailscale:  connected as %s\n", m.Self.Name)
	}
	switch {
	case m.ListenError != "":
		fmt.Fprintf(out, "Listening:  failed: %s\n", m.ListenError)
	case len(m.Listening) > 0:
		fmt.Fprintf(out, "Listening:  %s\n", strings.Join(m.Listening, ", "))
	}
	c := st.Clipboard
	fmt.Fprintf(out, "Clipboard:  %d sent%s, %d received%s\n",
		c.Sent, ago(now, c.LastSent), c.Received, ago(now, c.LastReceived))
	// Copies of files are counted above too; this line is about them alone.
	if f := st.Files; f.Problem != "" {
		fmt.Fprintf(out, "Files:      %d sent, %d received; %s\n", f.Sent, f.Received, f.Problem)
	} else {
		fmt.Fprintf(out, "Files:      %d sent, %d received\n", f.Sent, f.Received)
	}

	fmt.Fprintln(out)
	if m.Note != "" {
		fmt.Fprintf(out, "No machines to sync with: %s.\n", m.Note)
		return
	}
	if len(m.Peers) == 0 {
		if m.Tailscale == "Running" {
			fmt.Fprintln(out, "No other Linux or macOS machines of yours are on this tailnet yet.")
		}
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tOS\tADDRESS\tSTATE")
	for _, p := range m.Peers {
		state := "offline"
		switch {
		case p.Connected:
			state = "connected for " + age(now.Sub(p.Since))
		case p.Online && p.Error != "":
			state = "not connected: " + p.Error
		case p.Online:
			state = "online, not connected yet"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.OS, p.Addr, state)
	}
	tw.Flush()
}

// ago renders ", last 5s ago" for a time, or nothing if it never happened.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " (last " + age(now.Sub(t)) + " ago)"
}

// age renders a duration the way a person would say it.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
