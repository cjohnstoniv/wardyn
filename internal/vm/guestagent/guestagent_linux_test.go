// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePort stands in for a guest's virtio-serial port: it reads end-of-file while no host is
// connected, and each connect is a new host session on the same port.
type fakePort struct {
	mu  sync.Mutex
	cur net.Conn
}

func (p *fakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c == nil {
		return 0, io.EOF
	}
	n, err := c.Read(b)
	if err != nil {
		p.mu.Lock()
		if p.cur == c {
			p.cur = nil
		}
		p.mu.Unlock()
		return n, io.EOF
	}
	return n, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c == nil {
		return len(b), nil // no host: the bytes go nowhere, as on a real port
	}
	return c.Write(b)
}

func (p *fakePort) connect() net.Conn {
	host, guest := net.Pipe()
	p.mu.Lock()
	p.cur = guest
	p.mu.Unlock()
	return host
}

type rig struct {
	port *fakePort
	srv  *Server
	ctx  context.Context
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{port: &fakePort{}, ctx: ctx, srv: &Server{Version: "test", Home: t.TempDir(), OutDir: t.TempDir()}}
	l := newPortListener(ctx, r.port, 5*time.Millisecond)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: r.srv.Handler(), Protocols: &protocols}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	return r
}

// client is one host session: every dial of it is the same connection, as the QEMU chardev accepts
// only one.
func (r *rig) client(t *testing.T) *Client {
	t.Helper()
	conn := r.port.connect()
	once := false
	c := NewClient(func(context.Context) (net.Conn, error) {
		if once {
			return nil, errors.New("the chardev accepts one connection")
		}
		once = true
		return conn, nil
	})
	t.Cleanup(c.Close)
	return c
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestReadyAndInitOnce(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	if got, err := c.Ready(ctxT(t)); err != nil || got.Version != "test" {
		t.Fatalf("ready = %+v, %v", got, err)
	}
	dir := t.TempDir()
	in := Init{Env: map[string]string{"A": "1", "SHADOW": "init"}, SecretEnv: map[string]string{"B": "2"},
		Files: []File{{Path: filepath.Join(dir, "etc", "managed.json"), Mode: 0o644, Content: []byte(`{"x":1}`)}}}
	if err := c.Init(ctxT(t), in); err != nil {
		t.Fatal(err)
	}
	if err := c.Init(ctxT(t), in); err == nil || !strings.Contains(err.Error(), "already initialised") {
		t.Fatalf("second init = %v, want refused", err)
	}
	st, err := os.Stat(filepath.Join(dir, "etc", "managed.json"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("managed file = %v, %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc", "managed.json")); string(b) != `{"x":1}` {
		t.Fatalf("managed file body = %q", b)
	}
	s, err := c.Exec(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `printf %s "$A$B$SHADOW"`}, Env: []string{"SHADOW=exec"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s.Stdout)
	if code, err := s.Wait(); err != nil || code != 0 || string(out) != "12exec" {
		t.Fatalf("env exec = %q, %d, %v: init env and secret env reach the exec, the exec's own env wins", out, code, err)
	}
}

func TestManagedFilePathsAreCleanAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"etc/managed.json", dir + "/x/../managed.json", dir + "/a..b"} {
		if err := (&Server{}).writeFile(File{Path: p, Mode: 0o644}); err == nil {
			t.Errorf("managed file %q was written", p)
		}
	}
}

func TestAttachedExecPipes(t *testing.T) {
	c := newRig(t).client(t)
	s, err := c.Exec(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `read x; echo "out:$x"; echo err >&2; exit 3`}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(s.Stdin, "hi\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, s.Stdout) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errb, s.Stderr) }()
	code, err := s.Wait()
	wg.Wait()
	if err != nil || code != 3 || out.String() != "out:hi\n" || errb.String() != "err\n" {
		t.Fatalf("exec = code %d err %v stdout %q stderr %q", code, err, out.String(), errb.String())
	}
}

func TestAttachedExecTTY(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	c := newRig(t).client(t)
	s, err := c.Exec(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `stty size; [ -t 0 ] && echo istty`}, TTY: true, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s.Stdout)
	code, err := s.Wait()
	if err != nil || code != 0 || !strings.Contains(string(out), "30 100") || !strings.Contains(string(out), "istty") {
		t.Fatalf("tty exec = %q, %d, %v", out, code, err)
	}
}

func TestClosingASessionKillsItsProcess(t *testing.T) {
	c := newRig(t).client(t)
	s, err := c.Exec(ctxT(t), ExecRequest{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("closing the session did not end it")
	}
}

// The harness's exec outlives the host connection (a runner restart): a new session finds it by id,
// reads its output from any offset and its exit code.
func TestDetachedExecSurvivesAHostReconnect(t *testing.T) {
	r := newRig(t)
	c1 := r.client(t)
	id, err := c1.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `echo one; sleep 0.3; echo two; exit 7`}})
	if err != nil {
		t.Fatal(err)
	}
	c1.Close() // the host goes away; the port reads end-of-file
	c2 := r.client(t)
	var st ExecState
	for st, err = c2.Agent(ctxT(t)); err == nil && st.Running; st, err = c2.Agent(ctxT(t)) {
		if st.ID != id {
			t.Fatalf("latest exec = %q, want %q", st.ID, id)
		}
		out, err := c2.Output(ctxT(t), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(out) // follows until the exec exits
		out.Close()
		if string(b) != "one\ntwo\n" {
			t.Fatalf("followed output = %q", b)
		}
	}
	if err != nil || st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	out, err := c2.Output(ctxT(t), id, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out)
	out.Close()
	if string(b) != "two\n" {
		t.Fatalf("output from offset 4 = %q", b)
	}
	if _, err := c2.State(ctxT(t), "nope"); !errors.Is(err, ErrNoExec) {
		t.Fatalf("unknown exec = %v", err)
	}
}

func TestFramesAreBounded(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, FrameStdout, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized write = %v", err)
	}
	b.Write([]byte{FrameStdout, 0x7f, 0xff, 0xff, 0xff})
	if _, _, err := ReadFrame(&b); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized read = %v: a peer must not make the other allocate what it names", err)
	}
}
