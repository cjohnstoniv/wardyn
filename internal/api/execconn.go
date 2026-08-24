// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// execConn adapts ONE runner.ExecSession to net.Conn: Read pulls the exec's
// stdout, Write pushes the exec's stdin, Close tears down only that exec
// stream. Paired with `socat - TCP:127.0.0.1:<port>` inside the sandbox (the
// SAME dial handleSSHDirectTCPIP already uses for `ssh -L`), it turns the
// exec lane into an http.Transport DialContext — which is the whole of the UI
// gateway's transport: no pod-IP/container-IP dial, no new network path out
// of the sandbox, no substrate change (invariant 3).
//
// Stderr is drained to io.Discard on construction. That is NOT tidiness: the
// ExecSession streaming contract makes Stdout and Stderr unbuffered io.Pipes
// fed by ONE demux goroutine, so a single undrained stderr byte blocks Stdout
// AND Wait (drainExecStderr's doc, sshgateway_channels.go). ponytail: the
// drained bytes are dropped rather than surfaced — socat's connect error
// reaches the caller as a dial/EOF failure instead of its own text; the
// launcher probe (uiEnsureApp) is what produces the honest "app is not
// listening" message, so nothing needs the socat text.
type execConn struct {
	sess *runner.ExecSession
	addr execAddr

	closeOnce sync.Once
	closeErr  error
}

// errExecConnDeadline is what every SetDeadline variant returns. execConn
// rides io.Pipes with no deadline machinery underneath, so a nil return would
// be a LIE — a caller that set a deadline would get none and hang forever.
// net/http's Transport and ReverseProxy never call these on a dialed
// connection (the plain-HTTP path uses timers, not conn deadlines), so this
// is a refusal nothing in the gateway's own path trips.
var errExecConnDeadline = errors.New("execConn: deadlines are not supported on an exec-lane connection (bound the runner.ExecStream context instead)")

// execAddr names one exec-lane endpoint for net.Conn's addressing. The dial
// target is always the sandbox's OWN loopback, so the string is
// "<sandbox-ref>:<port>", never a routable address.
type execAddr string

func (a execAddr) Network() string { return "wardyn-exec" }
func (a execAddr) String() string  { return string(a) }

// newExecConn wraps sess. addr is the human-readable dial target used for
// both LocalAddr and RemoteAddr (there is no second endpoint to name).
func newExecConn(sess *runner.ExecSession, addr string) *execConn {
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	return &execConn{sess: sess, addr: execAddr(addr)}
}

func (c *execConn) Read(p []byte) (int, error) {
	if c.sess.Stdout == nil {
		return 0, io.EOF
	}
	return c.sess.Stdout.Read(p)
}

func (c *execConn) Write(p []byte) (int, error) {
	if c.sess.Stdin == nil {
		return 0, io.ErrClosedPipe
	}
	return c.sess.Stdin.Write(p)
}

// Close tears down the exec stream ONLY — never the sandbox, the agent, or a
// sidecar (ExecSession.Close's contract). Idempotent: net/http closes a
// connection from both its read and write loops.
func (c *execConn) Close() error {
	c.closeOnce.Do(func() {
		if c.sess.Stdin != nil {
			_ = c.sess.Stdin.Close()
		}
		if c.sess.Close != nil {
			c.closeErr = c.sess.Close()
		}
	})
	return c.closeErr
}

func (c *execConn) LocalAddr() net.Addr              { return c.addr }
func (c *execConn) RemoteAddr() net.Addr             { return c.addr }
func (c *execConn) SetDeadline(time.Time) error      { return errExecConnDeadline }
func (c *execConn) SetReadDeadline(time.Time) error  { return errExecConnDeadline }
func (c *execConn) SetWriteDeadline(time.Time) error { return errExecConnDeadline }

var _ net.Conn = (*execConn)(nil)
