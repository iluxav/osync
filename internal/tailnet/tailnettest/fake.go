// Package tailnettest provides a tailnet.Source for tests.
package tailnettest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/iluxav/osync/internal/tailnet"
)

// Fake is a tailnet.Source whose state is set by the test.
type Fake struct {
	mu  sync.Mutex
	st  tailnet.State
	err error
}

// New returns a Fake reporting st.
func New(st tailnet.State) *Fake { return &Fake{st: st} }

// Set replaces the reported state. A non-nil err is returned by State instead.
func (f *Fake) Set(st tailnet.State, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st, f.err = st, err
}

// State implements tailnet.Source.
func (f *Fake) State(context.Context) (tailnet.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, f.err
}

// Loopback lays out a tailnet of count machines on the loopback addresses
// 127.0.subnet.1 and up, and returns them with a port that is free on every
// one of them and on the next address after the last, which tests use as a
// stranger. As on a real tailnet, all machines share the port.
//
// Each test package passes its own subnet, so packages tested in parallel
// never compete for an address. Linux routes all of 127.0.0.0/8 to loopback;
// where that is not so, the test is skipped.
func Loopback(t testing.TB, subnet, count int) ([]tailnet.Node, int) {
	t.Helper()
	nodes := make([]tailnet.Node, count)
	for i := range nodes {
		nodes[i] = tailnet.Node{
			ID:     fmt.Sprintf("n%d", i+1),
			Name:   fmt.Sprintf("machine%d", i+1),
			OS:     "linux",
			Addrs:  []netip.Addr{LoopbackAddr(subnet, i+1)},
			Online: true,
		}
	}
	var lastErr error
	for range 20 {
		first, err := net.Listen("tcp", netip.AddrPortFrom(LoopbackAddr(subnet, 1), 0).String())
		if err != nil {
			t.Skipf("cannot listen on %s (%v): loopback tailnet tests need Linux", LoopbackAddr(subnet, 1), err)
		}
		port := first.Addr().(*net.TCPAddr).Port
		held := []net.Listener{first}
		lastErr = nil
		for i := 2; i <= count+1; i++ {
			ln, err := net.Listen("tcp", netip.AddrPortFrom(LoopbackAddr(subnet, i), uint16(port)).String())
			if err != nil {
				lastErr = err
				break
			}
			held = append(held, ln)
		}
		for _, ln := range held {
			ln.Close()
		}
		if lastErr == nil {
			return nodes, port
		}
	}
	t.Fatalf("no port free on all loopback addresses: %v", lastErr)
	return nil, 0
}

// LoopbackAddr returns the address of machine i (counting from 1) on subnet.
func LoopbackAddr(subnet, i int) netip.Addr {
	return netip.AddrFrom4([4]byte{127, 0, byte(subnet), byte(i)})
}
