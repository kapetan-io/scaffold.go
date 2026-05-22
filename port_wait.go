package scaffold

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/kapetan-io/tackle/clock"
)

// waitForListener probes addr by dialing until a TCP connection succeeds,
// ctx is cancelled, or timeout expires. Unspecified listen addresses
// (0.0.0.0, [::]) are translated to their loopback equivalents because
// dialing the unspecified address is non-portable (fails on macOS/BSD).
//
// Default timeout for scaffold call sites is 5 seconds; the parameter is
// present for signature completeness and is not exposed or configurable.
func waitForListener(ctx context.Context, addr net.Addr, timeout time.Duration, clk *clock.Provider) error {
	dialAddr := translateDialAddr(addr)
	deadline := clk.Now().Add(timeout)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", dialAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if clk.Now().After(deadline) {
			return fmt.Errorf("scaffold: timed out waiting for listener at %s", addr.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(10 * time.Millisecond):
		}
	}
}

// translateDialAddr returns an address suitable for net.Dial for addr.
// Unspecified TCP addresses are rewritten to loopback; other addresses are
// returned as their String() form.
func translateDialAddr(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsUnspecified() {
		return addr.String()
	}
	if tcp.IP.To4() != nil {
		return fmt.Sprintf("127.0.0.1:%d", tcp.Port)
	}
	return fmt.Sprintf("[::1]:%d", tcp.Port)
}
