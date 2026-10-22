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
// connected, and each connect is a new host session on the same port. A host that dials is
// connected at the agent's next read, after it has seen the previous host go: the byte stream does
// not say which host is which, so the agent cannot keep apart a host that comes back faster.
// A write made while no host is connected, or that the host left in the middle of, is held, as the
// kernel holds it, and goes to the next host; only a past write deadline ends it.
type fakePort struct {
	mu      sync.Mutex
	cur     net.Conn      // the connected host's end
	next    net.Conn      // a host that dialled, connected at the agent's next read
	dead    bool          // the write deadline has passed
	changed chan struct{} // closed and replaced when cur or dead changes
	held    chan struct{} // if set, told when a write is held
}

func (p *fakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	if p.cur == nil && p.next != nil {
		p.cur, p.next = p.next, nil
		p.signal()
	}
	c := p.cur
	p.mu.Unlock()
	if c == nil {
		return 0, io.EOF
	}
	n, err := c.Read(b)
	if err != nil {
		p.mu.Lock()
		p.cur = nil
		p.signal()
		p.mu.Unlock()
		return n, io.EOF
	}
	return n, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	var left net.Conn // a host that went away in the middle of this write
	for n := 0; ; {
		p.mu.Lock()
		c, dead, changed := p.cur, p.dead, p.wait()
		p.mu.Unlock()
		if dead {
			return n, os.ErrDeadlineExceeded
		}
		if c != nil && c != left {
			m, err := c.Write(b[n:])
			if n += m; err == nil {
				return n, nil
			}
			left = c
			continue
		}
		if p.held != nil {
			select {
			case p.held <- struct{}{}:
			default:
			}
		}
		<-changed
	}
}

// SetWriteDeadline takes what the agent sets: a past deadline, or none.
func (p *fakePort) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	p.dead = !t.IsZero() && t.Before(time.Now())
	p.signal()
	p.mu.Unlock()
	return nil
}

func (p *fakePort) wait() chan struct{} {
	if p.changed == nil {
		p.changed = make(chan struct{})
	}
	return p.changed
}

func (p *fakePort) signal() {
	close(p.wait())
	p.changed = make(chan struct{})
}

func (p *fakePort) connect() net.Conn {
	host, guest := net.Pipe()
	p.mu.Lock()
	p.next = guest
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
	return newRigAt(t, t.TempDir())
}

// newRigAt is a fresh agent whose Init marker lives in markerDir: two rigs on one directory are one
// boot's agent before and after a restart.
func newRigAt(t *testing.T, markerDir string) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{port: &fakePort{}, ctx: ctx, srv: &Server{Version: "test", Home: t.TempDir(), OutDir: t.TempDir(), MarkerDir: markerDir}}
	l := newPortListener(ctx, r.port, 5*time.Millisecond)
	h, err := r.srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: h, Protocols: &protocols}
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

func TestInitRefusedAfterAnAgentRestart(t *testing.T) {
	boot := t.TempDir()
	in := Init{Files: []File{{Path: filepath.Join(t.TempDir(), "managed.json"), Mode: 0o644}}}
	if err := newRigAt(t, boot).client(t).Init(ctxT(t), in); err != nil {
		t.Fatalf("first init on an empty marker dir = %v", err)
	}
	if err := newRigAt(t, boot).client(t).Init(ctxT(t), in); err == nil || !strings.Contains(err.Error(), `409 Conflict: "the sandbox is already initialised"`) {
		t.Fatalf("init after a restart = %v, want refused", err)
	}
	if st, err := os.Stat(filepath.Join(boot, "wardyn-guest-init")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("init marker = %v, %v", st, err)
	}
}

func TestPartWayFailedInitStaysRefused(t *testing.T) {
	boot := t.TempDir()
	bad := Init{Files: []File{{Path: "relative/managed.json", Mode: 0o644}}}
	if err := newRigAt(t, boot).client(t).Init(ctxT(t), bad); err == nil || strings.Contains(err.Error(), "already initialised") {
		t.Fatalf("init with a bad file path = %v, want it to fail after the marker", err)
	}
	good := Init{Files: []File{{Path: filepath.Join(t.TempDir(), "managed.json"), Mode: 0o644}}}
	if err := newRigAt(t, boot).client(t).Init(ctxT(t), good); err == nil || !strings.Contains(err.Error(), `409 Conflict: "the sandbox is already initialised"`) {
		t.Fatalf("init after a part-way failure = %v, want refused", err)
	}
}

func TestHandlerRefusesAnEmptyMarkerDir(t *testing.T) {
	if _, err := (&Server{Version: "test", Home: t.TempDir(), OutDir: t.TempDir()}).Handler(); err == nil {
		t.Fatal("a Server with no MarkerDir got a handler")
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

// nextHost connects a host and returns it with what it reads first.
func nextHost(port *fakePort) (net.Conn, <-chan string) {
	h := port.connect()
	first := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := h.Read(b)
		first <- string(b[:n])
	}()
	return h, first
}

// handOver ends session c and starts host h's, whose first write is "hello"; it fails unless that is
// the first thing h reads.
func handOver(ctx context.Context, t *testing.T, l *portListener, c, h net.Conn, first <-chan string) {
	t.Helper()
	go func() { _, _ = h.Write([]byte("two")) }()
	_ = c.Close()
	next, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = next.Write([]byte("hello")) }()
	select {
	case got := <-first:
		if got != "hello" {
			t.Fatalf("the next host read %q first, not its own session's %q", got, "hello")
		}
	case <-ctx.Done():
		t.Fatal("the next host read nothing")
	}
}

// The HTTP/2 server writes frames from its own goroutine, so a session can write after its host has
// gone and before the server closes it. The next host reads any such bytes before its own session's
// first frame, as a protocol error.
func TestALateWriteNeverReachesTheNextHost(t *testing.T) {
	ctx := ctxT(t)
	port := &fakePort{}
	l := newPortListener(ctx, port, time.Millisecond)
	h1 := port.connect()
	go func() { _, _ = h1.Write([]byte("one")) }()
	c1, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = h1.Close()
	if b, err := io.ReadAll(c1); err != nil || string(b) != "one" {
		t.Fatalf("first session read %q, %v", b, err)
	}
	h2, first := nextHost(port)
	if n, err := c1.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a write after the host went = %d, %v; want net.ErrClosed", n, err)
	}
	handOver(ctx, t, l, c1, h2, first)
}

// A write the host left in the middle of is held by the port for the next host; it ends when the
// agent sees the host go.
func TestAWriteThePortHoldsEndsWithItsHost(t *testing.T) {
	ctx := ctxT(t)
	port := &fakePort{held: make(chan struct{}, 1)}
	l := newPortListener(ctx, port, time.Millisecond)
	h1 := port.connect()
	go func() { _, _ = h1.Write([]byte("one")) }()
	c1, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The reader keeps "x" until c1 reads it, so it sees h1 go only after the write is held.
	if _, err := h1.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	late := make(chan error, 1)
	go func() { _, err := c1.Write([]byte("late")); late <- err }()
	_ = h1.Close()
	<-port.held
	read := make(chan string, 1)
	go func() { b, _ := io.ReadAll(c1); read <- string(b) }()
	select {
	case err := <-late:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the held write = %v; want it ended by the deadline", err)
		}
	case <-ctx.Done():
		t.Fatal("the held write outlived its host, and the reader waits on it")
	}
	if b := <-read; b != "onex" {
		t.Fatalf("first session read %q", b)
	}
	h2, first := nextHost(port)
	handOver(ctx, t, l, c1, h2, first)
}

// A Read on a closed session takes no chunk: the next one is the next session's, or the end-of-file
// Accept drains to. Both select cases are ready here, so 64 rounds catch a Read that can pick either.
func TestAClosedSessionTakesNoChunk(t *testing.T) {
	l := &portListener{ctx: ctxT(t), chunks: make(chan chunk, 1), done: make(chan struct{})}
	for i := range 64 {
		c := &portConn{l: l, closed: make(chan struct{})}
		_ = c.Close()
		next := chunk{data: []byte("next")}
		if i%2 == 1 {
			next = chunk{eof: true}
		}
		l.chunks <- next
		if n, err := c.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("round %d: a closed session's Read = %d, %v; want net.ErrClosed", i, n, err)
		}
		select {
		case <-l.chunks:
		default:
			t.Fatalf("round %d: the closed session took the next chunk", i)
		}
	}
}

// The HTTP/2 server closes a session from its own goroutine while its reader may be taking the
// end-of-file; Accept must see that, or it drains the next session's bytes waiting for another.
// Under -race this also checks the two goroutines' sawEOF accesses are ordered.
func TestAcceptSeesTheEOFAClosingSessionTook(t *testing.T) {
	ctx := ctxT(t)
	l := &portListener{ctx: ctx, chunks: make(chan chunk), done: make(chan struct{})}
	go func() { l.chunks <- chunk{data: []byte("one")} }()
	c1, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	if n, err := c1.Read(b); err != nil || string(b[:n]) != "one" {
		t.Fatalf("first session read %q, %v", b[:n], err)
	}
	read := make(chan error, 1)
	go func() { _, err := c1.Read(make([]byte, 8)); read <- err }()
	next := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		next <- c
	}()
	l.chunks <- chunk{eof: true} // only c1's Read takes it: Accept waits for c1 to close
	_ = c1.Close()               // unordered with that Read setting sawEOF
	if err := <-read; err != io.EOF {
		t.Fatalf("c1 read %v, want io.EOF", err)
	}
	select {
	case l.chunks <- chunk{data: []byte("two")}:
	case <-ctx.Done():
		t.Fatal("Accept took no chunk")
	}
	select {
	case c2 := <-next:
		if b, _ := io.ReadAll(io.LimitReader(c2, 3)); string(b) != "two" {
			t.Fatalf("next session read %q", b)
		}
	case <-ctx.Done():
		t.Fatal("Accept dropped the next session's first chunk, waiting for an end-of-file c1 took")
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
