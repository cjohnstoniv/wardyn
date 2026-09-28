// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Stream activity (long-holds design rev 4, §3.3, RL-7). A decision is emitted
// once per CONNECT, so a long download or a model stream in I/O wait looks like
// nobody is there. The proxy therefore notes any byte moved on a tunnel or a
// MITM stream and, at most once per activityReportEvery, tells the control
// plane, which moves the run's presence clock so the run is not paused under
// it. The local routes (the toolgate's approval poll among them) are plain
// requests, not streams, and never count.

// activityReportEvery is how often the proxy may report stream activity.
const activityReportEvery = 60 * time.Second

// activityConn marks moved whenever a byte crosses it, either way.
type activityConn struct {
	net.Conn
	moved *atomic.Bool
}

func (c activityConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.moved.Store(true)
	}
	return n, err
}

func (c activityConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.moved.Store(true)
	}
	return n, err
}

// CloseWrite keeps tunnel's half-close working through the wrapper.
func (c activityConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// countActivity wraps a sandbox-side stream connection so the bytes it moves
// count as the run being in use.
func (p *Proxy) countActivity(c net.Conn) net.Conn {
	return activityConn{Conn: c, moved: &p.streamMoved}
}

// runActivityReporter reports stream activity until ctx ends. A failed report
// is dropped: presence is best-effort, and the next minute's bytes report again.
func runActivityReporter(ctx context.Context, moved *atomic.Bool, ts *tokenSource, base string, client *http.Client, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if moved.Swap(false) {
				reportActivity(ctx, ts, base, client)
			}
		}
	}
}

func reportActivity(ctx context.Context, ts *tokenSource, base string, client *http.Client) {
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/api/v1/internal/activity", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+ts.Get())
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
