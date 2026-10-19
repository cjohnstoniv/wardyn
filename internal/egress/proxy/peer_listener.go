// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"
)

// peerListener serves only connections from allowed: any other is closed on
// accept, before a byte is read. It bounds which callers reach the proxy, not
// how the proxy is addressed (Config.AllowedPeers).
type peerListener struct {
	net.Listener
	allowed  []netip.Prefix
	lastWarn atomic.Int64 // unix nanos of the last refusal warning
}

// peerWarnEvery spaces the refusal warnings, so a flood writes a line a minute.
const peerWarnEvery = time.Minute

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ap, perr := netip.ParseAddrPort(c.RemoteAddr().String()); perr == nil &&
			slices.ContainsFunc(l.allowed, func(p netip.Prefix) bool { return p.Contains(ap.Addr().Unmap()) }) {
			return c, nil
		}
		now, last := time.Now().UnixNano(), l.lastWarn.Load()
		if now-last >= int64(peerWarnEvery) && l.lastWarn.CompareAndSwap(last, now) {
			slog.Warn("wardyn-proxy: refused a connection from outside the run's network", slog.String("peer", c.RemoteAddr().String()))
		}
		_ = c.Close()
	}
}
