// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// muxFrame builds one stdcopy-multiplexed frame (the docker exec-attach wire
// format for a non-TTY stream): a 1-byte stream type, 3 zero bytes, a
// big-endian uint32 payload length, then the payload. streamType 1 = stdout,
// 2 = stderr (see github.com/moby/moby/api/pkg/stdcopy).
func muxFrame(streamType byte, payload string) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

// bufConn is a net.Conn whose Read serves a canned byte sequence once (then
// EOF) and whose Write/CloseWrite/Close are recorded, so a test can drive
// ExecStream's demux + half-close wiring without a real daemon. Unlike the
// package's fakeConn (fixed instant-EOF stub used everywhere else), this type
// is scoped to exec-stream tests via fakeDocker.execAttachConn.
type bufConn struct {
	r          *bytes.Reader
	writes     [][]byte
	closeWrite bool
	closed     bool
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *bufConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (c *bufConn) CloseWrite() error                { c.closeWrite = true; return nil }
func (c *bufConn) Close() error                     { c.closed = true; return nil }
func (c *bufConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *bufConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *bufConn) SetDeadline(time.Time) error      { return nil }
func (c *bufConn) SetReadDeadline(time.Time) error  { return nil }
func (c *bufConn) SetWriteDeadline(time.Time) error { return nil }

var _ net.Conn = (*bufConn)(nil)

// execStreamSandbox creates a running sandbox on f and returns its ref, the
// shared setup every ExecStream test starts from.
func execStreamSandbox(t *testing.T, f *fakeDocker) (*Driver, string) {
	t.Helper()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return d, sb.Ref
}

func TestExecStream_RejectsEmptyArgv(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	if _, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{}); err == nil {
		t.Error("ExecStream with empty argv must error")
	}
}

// TestExecStream_NonTTY_SeparatesStdoutStderr proves the non-TTY contract:
// ExecCreate/ExecAttach are called with the right argv/env/TTY=false, and the
// stdcopy-multiplexed hijack stream is demultiplexed into SEPARATE Stdout and
// Stderr readers (a stderr write must never land on Stdout).
func TestExecStream_NonTTY_SeparatesStdoutStderr(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	f.execAttachConn = &bufConn{r: bytes.NewReader(append(
		muxFrame(1, "hello-stdout"),
		muxFrame(2, "hello-stderr")...,
	))}

	spec := runner.ExecSpec{Argv: []string{"sh", "-c", "whatever"}, Env: []string{"X=1"}}
	sess, err := d.ExecStream(context.Background(), ref, spec)
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer sess.Close()

	if got := f.lastExecCmd; !slices.Equal(got, spec.Argv) {
		t.Errorf("exec argv = %v, want %v", got, spec.Argv)
	}
	if f.lastExecOpts.TTY {
		t.Error("ExecCreateOptions.TTY = true, want false for a non-TTY spec")
	}
	if len(f.lastExecOpts.Env) != 1 || f.lastExecOpts.Env[0] != "X=1" {
		t.Errorf("ExecCreateOptions.Env = %v, want [X=1]", f.lastExecOpts.Env)
	}

	// Drain concurrently: the demux goroutine writes both streams over
	// unbuffered pipes, so — exactly as a real caller must — Stdout and
	// Stderr have to be read in parallel or the writer of whichever stream
	// isn't being read blocks the other (this is the streaming contract, not
	// a workaround).
	stdoutCh := make(chan []byte, 1)
	stderrCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(sess.Stdout); stdoutCh <- b }()
	go func() { b, _ := io.ReadAll(sess.Stderr); stderrCh <- b }()

	var stdout, stderr []byte
	select {
	case stdout = <-stdoutCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading Stdout")
	}
	select {
	case stderr = <-stderrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading Stderr")
	}
	if string(stdout) != "hello-stdout" {
		t.Errorf("Stdout = %q, want %q", stdout, "hello-stdout")
	}
	if string(stderr) != "hello-stderr" {
		t.Errorf("Stderr = %q, want %q", stderr, "hello-stderr")
	}
}

// TestExecStream_TTY_MergesOntoStdout proves the TTY contract: the raw
// (non-multiplexed) PTY stream passes straight through as Stdout, and Stderr
// — with no separate channel to read — is present but yields io.EOF
// immediately rather than being nil.
func TestExecStream_TTY_MergesOntoStdout(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("hello-tty"))}

	sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"sh"}, TTY: true})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer sess.Close()

	if !f.lastExecOpts.TTY {
		t.Error("ExecCreateOptions.TTY = false, want true for a TTY spec")
	}
	if sess.Stderr == nil {
		t.Fatal("Stderr must not be nil under TTY (callers read it uniformly)")
	}
	if n, err := sess.Stderr.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("TTY Stderr.Read = (%d, %v), want (0, io.EOF)", n, err)
	}

	stdout, err := io.ReadAll(sess.Stdout)
	if err != nil {
		t.Fatalf("read Stdout: %v", err)
	}
	if string(stdout) != "hello-tty" {
		t.Errorf("Stdout = %q, want %q (raw PTY passthrough)", stdout, "hello-tty")
	}
}

// TestExecStream_StdinHalfClose proves Stdin.Write reaches the hijacked
// connection and Stdin.Close HALF-closes it (CloseWrite) without tearing down
// the whole hijack — the underlying conn's Close must NOT be called.
func TestExecStream_StdinHalfClose(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	conn := &bufConn{r: bytes.NewReader(nil)}
	f.execAttachConn = conn

	sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	if _, err := sess.Stdin.Write([]byte("ping")); err != nil {
		t.Fatalf("Stdin.Write: %v", err)
	}
	if len(conn.writes) != 1 || string(conn.writes[0]) != "ping" {
		t.Errorf("conn writes = %v, want one write of %q", conn.writes, "ping")
	}

	if err := sess.Stdin.Close(); err != nil {
		t.Fatalf("Stdin.Close: %v", err)
	}
	if !conn.closeWrite {
		t.Error("Stdin.Close must half-close via CloseWrite")
	}
	if conn.closed {
		t.Error("Stdin.Close must NOT fully close the hijacked connection (Stdout/Stderr may still be flowing)")
	}

	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !conn.closed {
		t.Error("ExecSession.Close must close the hijacked connection")
	}
}

// TestExecStream_WaitAndResize proves Wait polls ExecInspect for the STREAMED
// exec (not the tracked agent exec) via the same pollExecExit contract Wait
// uses, and Resize drives ExecResize while ignoring degenerate sizes.
func TestExecStream_WaitAndResize(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	f.execAttachConn = &bufConn{r: bytes.NewReader(nil)}

	sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer sess.Close()

	// Degenerate resize must no-op (matches the attach Session's contract).
	if err := sess.Resize(0, 0); err != nil {
		t.Errorf("Resize(0,0): %v", err)
	}
	if f.lastResize != nil {
		t.Error("Resize(0,0) must not call ExecResize")
	}

	if err := sess.Resize(120, 40); err != nil {
		t.Errorf("Resize(120,40): %v", err)
	}
	if f.lastResize == nil || f.lastResize.Height != 40 || f.lastResize.Width != 120 {
		t.Errorf("lastResize = %+v, want Height=40 Width=120", f.lastResize)
	}

	f.execExited = true
	f.execExitCode = 7
	code, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 7 {
		t.Errorf("Wait exit code = %d, want 7", code)
	}
}
