// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// relayRunner connects a runner over loopback to a substrate whose relay sink
// is sink (nil: no relay served).
func relayRunner(t *testing.T, sink func(string, net.Conn) error) *runnertest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	link := &remote.Link{}
	remote.New("r1", link, remote.Options{Relay: sink})
	org, run := runnerwire.Loopback()
	peer := link.Attach(ctx, org)
	srv := runnertest.Serve(ctx, run, "r1", runnertest.NewFake())
	t.Cleanup(func() { peer.Close(); srv.Peer.Close() })
	return srv
}

func TestRelay_OpenWithTargetOrNoSinkRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan net.Conn, 2)
	srv := relayRunner(t, func(id string, c net.Conn) error {
		if id != "r1" {
			t.Errorf("relay tagged with %q, want the session's runner r1", id)
		}
		got <- c
		return nil
	})
	if _, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay, Target: "10.0.0.1:443"}); err == nil {
		t.Fatal("a relay naming a target was accepted: the runner chose where the org dials")
	}
	if len(got) != 0 {
		t.Fatal("a refused relay reached the sink")
	}

	st, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
	if err != nil {
		t.Fatalf("relay with no target: %v", err)
	}
	c := <-got
	if c.RemoteAddr().String() != "runner:r1" {
		t.Fatalf("relay conn remote = %q", c.RemoteAddr())
	}
	if _, err := st.Write([]byte("client hello")); err != nil {
		t.Fatal(err)
	}
	_ = st.CloseWrite()
	if b, err := io.ReadAll(c); err != nil || string(b) != "client hello" {
		t.Fatalf("org read %q, %v", b, err)
	}
	if _, err := c.Write([]byte("server hello")); err != nil {
		t.Fatal(err)
	}
	_ = c.(interface{ CloseWrite() error }).CloseWrite()
	if b, err := io.ReadAll(st); err != nil || string(b) != "server hello" {
		t.Fatalf("runner read %q, %v", b, err)
	}

	none := relayRunner(t, nil)
	if _, err := none.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay}); err == nil {
		t.Fatal("a relay was accepted where the org serves none")
	}
}

// TestRunnerOpenedInteractiveStreamsAreRefused: a runner opens only a relay or
// an output copy toward the org. A PTY or exec stream it opens on its own,
// with no org call in flight, is refused, so a runner cannot start a terminal
// the org did not issue.
func TestRunnerOpenedInteractiveStreamsAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := relayRunner(t, func(string, net.Conn) error { t.Error("an interactive stream reached the relay sink"); return nil })
	for _, kind := range []string{runnerwire.KindPTY, runnerwire.KindExec, runnerwire.KindExecStderr} {
		if _, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: kind}); err == nil {
			t.Errorf("the org accepted a runner-opened %s stream", kind)
		}
		if _, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: kind, Call: 999999}); err == nil {
			t.Errorf("the org accepted a runner-opened %s stream for a call it never made", kind)
		}
	}
}

// relayPair opens one relay and returns the org's end and the runner's, which never reads.
func relayPair(t *testing.T) (org net.Conn, runner *runnerwire.Stream, srv *runnertest.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan net.Conn, 1)
	srv = relayRunner(t, func(_ string, c net.Conn) error { got <- c; return nil })
	runner, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
	if err != nil {
		t.Fatal(err)
	}
	return <-got, runner, srv
}

// stalledWrite starts a Write that blocks: far more than one stream window, to a peer that never reads.
func stalledWrite(c net.Conn) <-chan error {
	res := make(chan error, 1)
	go func() {
		_, err := c.Write(make([]byte, 8*runnerwire.StreamWindow))
		res <- err
	}()
	return res
}

func waitErr(t *testing.T, res <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the call stayed blocked", what)
		return nil
	}
}

// A write blocked on a peer that stopped reading returns once its deadline passes, and a deadline
// already past fails the next call at once.
func TestRelayConn_WriteDeadlineUnblocksAStalledPeer(t *testing.T) {
	c, _, _ := relayPair(t)
	res := stalledWrite(c)
	time.Sleep(100 * time.Millisecond) // the write is blocked on credit by now
	if err := c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	err := waitErr(t, res, "a write past its deadline")
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("blocked write after its deadline = %v, want a timeout satisfying os.ErrDeadlineExceeded", err)
	}

	c2, _, _ := relayPair(t)
	if err := c2.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write with a deadline already past = %v, want os.ErrDeadlineExceeded", err)
	}
}

// A read deadline ends the Read waiting on it, not the relay: cleared, the same conn reads on.
func TestRelayConn_ReadDeadlineUnblocksARead(t *testing.T) {
	c, runner, _ := relayPair(t)
	res := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); res <- err }()
	if err := c.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, res, "a read past its deadline"); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("blocked read after its deadline = %v, want os.ErrDeadlineExceeded", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if _, err := runner.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "late" {
		t.Fatalf("read after the deadline was cleared = %q, %v", buf, err)
	}
}

// A zero time clears a deadline, and re-arming replaces the earlier one.
func TestRelayConn_ZeroClearsAndRearmReplaces(t *testing.T) {
	c, _, _ := relayPair(t)
	for _, set := range []func(time.Time) error{c.SetWriteDeadline, c.SetReadDeadline, c.SetDeadline} {
		_ = set(time.Now().Add(time.Second))
		_ = set(time.Time{})
	}
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(time.Hour))
	time.Sleep(1500 * time.Millisecond)
	if _, err := c.Write([]byte("still open")); err != nil {
		t.Fatalf("write after cleared and replaced deadlines = %v", err)
	}
}

// The runner's session ending resets a stalled relay stream, so no deadline is needed for that end.
func TestRelayConn_RunnerSessionEndUnblocksAStalledWrite(t *testing.T) {
	c, _, srv := relayPair(t)
	res := stalledWrite(c)
	time.Sleep(100 * time.Millisecond)
	srv.Peer.Close()
	if err := waitErr(t, res, "a write after the runner session ended"); err == nil {
		t.Fatal("stalled write returned nil after the session ended")
	}
}

// TestRelayConn_CloseKeepsTheResponseTheOrgJustWrote: the org's server writes its answer and closes the
// connection at once (an error answer that ends the exchange). The runner reads the answer to its end;
// closing the org's end must not reset the stream under bytes the runner has not read yet.
func TestRelayConn_CloseKeepsTheResponseTheOrgJustWrote(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const answer = "HTTP/1.1 500 Internal Server Error\r\nConnection: close\r\n\r\nfailed"
	srv := relayRunner(t, func(_ string, c net.Conn) error {
		go func() {
			_, _ = c.Write([]byte(answer))
			_ = c.Close()
		}()
		return nil
	})
	st, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the answer and the close both arrived before the first read
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(st); err != nil || string(b) != answer {
		t.Fatalf("runner read %q, %v; want the whole answer then EOF, not a reset", b, err)
	}
	_ = st.CloseWrite()
	select {
	case <-st.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not finish once the runner closed its end too")
	}
}

// TestRelayConn_CloseUnblocksAStalledWrite: net.Conn requires Close to release a Write waiting on a peer
// that stopped reading, and tls.Conn.Close relies on it to break a stalled close_notify.
func TestRelayConn_CloseUnblocksAStalledWrite(t *testing.T) {
	c, _, _ := relayPair(t)
	res := stalledWrite(c)
	time.Sleep(100 * time.Millisecond) // the Write is waiting for credit
	_ = c.Close()
	select {
	case <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not release an in-flight Write")
	}
}

// TestRelayConn_CloseFreesTheLinkWindowFromUnreadUploads: runners keep sending to relays the org already
// closed (a request body the org answered early). The bytes already buffered, and the ones that arrive
// afterwards, are discarded and returned at once; they must not hold the runner link's shared window,
// which every other stream on that link waits on.
func TestRelayConn_CloseFreesTheLinkWindowFromUnreadUploads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const (
		chunk = runnerwire.StreamWindow / 2
		fill  = runnerwire.ConnWindow / chunk // streams whose first chunk together hold the whole link window
	)
	got := make(chan net.Conn, 64)
	srv := relayRunner(t, func(_ string, c net.Conn) error { got <- c; return nil })
	open := func() (*runnerwire.Stream, net.Conn) {
		st, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
		if err != nil {
			t.Fatal(err)
		}
		return st, <-got
	}
	write := func(st *runnerwire.Stream, n int) <-chan error {
		res := make(chan error, 1)
		go func() { _, err := st.Write(make([]byte, n)); res <- err }()
		return res
	}
	var runners []*runnerwire.Stream
	var orgs []net.Conn
	for range fill {
		st, c := open()
		if err := waitErr(t, write(st, chunk), "an upload that fits the link window"); err != nil {
			t.Fatal(err)
		}
		runners, orgs = append(runners, st), append(orgs, c)
	}
	blocked, _ := open()
	res := write(blocked, chunk)
	select {
	case <-res:
		t.Fatal("a write went through a full link window: the setup does not fill it")
	case <-time.After(200 * time.Millisecond):
	}

	for _, c := range orgs {
		_ = c.Close()
	}
	if err := waitErr(t, res, "a write on another stream after the relays were closed"); err != nil {
		t.Fatalf("write on another stream: %v", err)
	}
	// The runners go on sending to the relays the org closed; those bytes are discarded and returned too.
	for _, st := range runners {
		if err := waitErr(t, write(st, chunk-1), "an upload to a closed relay"); err != nil {
			t.Fatal(err)
		}
	}
	for range 8 { // nearly half the window again, in streams the closed relays left room for
		st, _ := open()
		if err := waitErr(t, write(st, runnerwire.StreamWindow), "a write after the discarded uploads"); err != nil {
			t.Fatal(err)
		}
	}
}
