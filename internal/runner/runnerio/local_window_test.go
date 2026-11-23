// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// displaySub is a substrate whose sandboxes have a display the test plays: each dial hands the test the
// display's end of a pipe.
type displaySub struct {
	contextSubstrate
	dialled chan net.Conn
}

func (d displaySub) DialDesktop(_ context.Context, _ string) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	d.dialled <- b
	return a, nil
}

type lwRig struct {
	server *Server
	sub    displaySub
	org    *runnerwire.Peer
	rp     *runnerwire.Peer
	runs   []uuid.UUID // two runs the runner holds
	refs   []string    // their sandboxes, as the org names them
}

func newLWRig(t *testing.T, times ...func(*localTimes)) *lwRig {
	t.Helper()
	fake := runnertest.NewFake()
	r := &lwRig{sub: displaySub{contextSubstrate{fake}, make(chan net.Conn, 4)}}
	byRun := map[uuid.UUID]string{}
	for range 2 {
		id := uuid.New()
		sb, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: id})
		if err != nil {
			t.Fatal(err)
		}
		byRun[id] = sb.Ref
		r.runs, r.refs = append(r.runs, id), append(r.refs, placement.RunnerRefPrefix("laptop")+sb.Ref)
	}
	server, err := New(r.sub, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range times {
		f(&server.times)
	}
	r.server = server
	org, run := runnerwire.Loopback()
	r.org = runnerwire.NewPeer(org, runnerwire.PeerConfig{Org: true})
	go func() { _ = r.org.Run(t.Context()) }()
	r.rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return server.Call(ctx, r.rp, id, method, args)
	}})
	go func() { _ = r.rp.Run(t.Context()) }()
	var mu sync.Mutex
	server.HoldSandboxes(func(string) bool { return true })
	server.AttachRuns(func(id uuid.UUID) string { mu.Lock(); defer mu.Unlock(); return byRun[id] })
	server.Online(r.rp)
	t.Cleanup(func() { r.rp.Close(); r.org.Close(); server.Offline(r.rp) })
	return r
}

// grant is the org's local_window call; the stream is nil when the call is refused.
func (r *lwRig) grant(t *testing.T, ref, nonce string) (*runnerwire.Stream, error) {
	t.Helper()
	var res runnerwire.StreamResult
	if err := r.org.Call(ctx5(t), runnerwire.MethodLocalWindow, runnerwire.LocalWindowArgs{Ref: ref, Nonce: nonce, GrantID: uuid.New(), Title: "Run, for Pat"}, &res); err != nil {
		return nil, err
	}
	st, err := r.org.TakeStream(res.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind() != runnerwire.KindLocalWindow {
		t.Fatalf("the window's stream is %q", st.Kind())
	}
	return st, nil
}

// display is the display the runner dialled.
func (r *lwRig) display(t *testing.T) net.Conn {
	t.Helper()
	select {
	case d := <-r.sub.dialled:
		t.Cleanup(func() { _ = d.Close() })
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never dialled the display")
		return nil
	}
}

func (r *lwRig) notDialled(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.sub.dialled:
		t.Fatalf("the display was dialled: %s", what)
	default:
	}
}

// windowHello asks the socket for a window nonce for run.
func (c *localCLI) windowHello(t *testing.T, run uuid.UUID) string {
	t.Helper()
	c.send(t, LocalHello, LocalHelloMsg{V: LocalProtocol, RunID: run, Kind: LocalKindWindow})
	var ok LocalHelloOKMsg
	if err := json.Unmarshal(c.expect(t, LocalHelloOK).b, &ok); err != nil || len(ok.Nonce) < 40 {
		t.Fatalf("hello_ok = %+v, %v", ok, err)
	}
	return ok.Nonce
}

func (c *localCLI) expectRefuse(t *testing.T, reason string) {
	t.Helper()
	f := c.expect(t, LocalRefuse)
	var r LocalReasonMsg
	if err := json.Unmarshal(f.b, &r); err != nil || r.Reason != reason {
		t.Fatalf("refusal %s, want %q", f.b, reason)
	}
}

// open is a granted window: the CLI bound, the display dialled and the org's stream.
func (r *lwRig) open(t *testing.T) (*localCLI, net.Conn, *runnerwire.Stream) {
	t.Helper()
	cli := dialLocal(t, r.server)
	st, err := r.grant(t, r.refs[0], cli.windowHello(t, r.runs[0]))
	if err != nil {
		t.Fatal(err)
	}
	cli.expect(t, LocalBound)
	return cli, r.display(t), st
}

// toCLI is the display sending msg; the CLI must read it unchanged.
func toCLI(t *testing.T, d net.Conn, cli *localCLI, msg []byte) {
	t.Helper()
	if _, err := d.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(len(msg), 5*time.Second); !bytes.Equal(got, msg) {
		t.Fatalf("the CLI read %x, want %x", got, msg)
	}
}

// fromCLI is the CLI sending msg; the display reads want.
func fromCLI(t *testing.T, d net.Conn, cli *localCLI, msg, want []byte) {
	t.Helper()
	cli.send(t, LocalData, msg)
	got := make([]byte, len(want))
	_ = d.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(d, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the display read %x (%v), want %x", got, err, want)
	}
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// handshake runs RFB 3.8 with security None up to the normal phase, across the window.
func handshake(t *testing.T, d net.Conn, cli *localCLI) {
	t.Helper()
	pf := []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	init := append(append(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, 1280), 800), pf...), append(be32(4), "desk"...)...)
	toCLI(t, d, cli, []byte("RFB 003.008\n"))
	fromCLI(t, d, cli, []byte("RFB 003.008\n"), []byte("RFB 003.008\n"))
	toCLI(t, d, cli, []byte{1, 1})
	fromCLI(t, d, cli, []byte{1}, []byte{1})
	toCLI(t, d, cli, be32(0))
	fromCLI(t, d, cli, []byte{1}, []byte{1})
	toCLI(t, d, cli, init)
}

func cutText(typ byte, text string) []byte {
	return append(append([]byte{typ, 0, 0, 0}, be32(uint32(len(text)))...), text...)
}

// endLine reads the org's stream to its end: the one LocalWindowEnd line.
func endLine(t *testing.T, st *runnerwire.Stream) runnerwire.LocalWindowEnd {
	t.Helper()
	b, err := io.ReadAll(st)
	var end runnerwire.LocalWindowEnd
	if err != nil || json.Unmarshal(b, &end) != nil || len(b) > 512 {
		t.Fatalf("the org read %q, %v", b, err)
	}
	return end
}

func displayClosed(t *testing.T, d net.Conn) {
	t.Helper()
	_ = d.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("the display stayed open: %v", err)
	}
}

// A window nonce is spent by the first grant whatever it answers: a second grant of it, and a nonce the
// runner never handed out, are refused.
func TestLocalWindowNonceSpentOnce(t *testing.T) {
	r := newLWRig(t)
	if _, err := r.grant(t, r.refs[0], "not-a-nonce"); err == nil {
		t.Fatal("an unknown nonce was granted")
	}
	cli := dialLocal(t, r.server)
	nonce := cli.windowHello(t, r.runs[0])
	st, err := r.grant(t, r.refs[0], nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cli.expect(t, LocalBound)
	r.display(t)
	if _, err := r.grant(t, r.refs[0], nonce); err == nil {
		t.Fatal("a spent nonce was granted again")
	}
	r.notDialled(t, "a spent nonce")
}

// A window nonce the org never grants expires with the connection, and a grant after that is refused.
func TestLocalWindowNonceExpires(t *testing.T) {
	r := newLWRig(t, func(lt *localTimes) { lt.nonceTTL = 300 * time.Millisecond })
	cli := dialLocal(t, r.server)
	nonce := cli.windowHello(t, r.runs[0])
	cli.expectRefuse(t, LocalReasonExpired)
	if _, err := r.grant(t, r.refs[0], nonce); err == nil {
		t.Fatal("an expired nonce was granted")
	}
	r.notDialled(t, "an expired nonce")
}

// A nonce grants only its own kind: a terminal's never opens a window, a window's never opens a terminal,
// and a hello of any other kind is refused before a nonce exists.
func TestLocalWindowNonceKindBound(t *testing.T) {
	r := newLWRig(t)
	term := dialLocal(t, r.server)
	term.send(t, LocalHello, LocalHelloMsg{V: LocalProtocol, RunID: r.runs[0]})
	var ok LocalHelloOKMsg
	if err := json.Unmarshal(term.expect(t, LocalHelloOK).b, &ok); err != nil {
		t.Fatal(err)
	}
	if _, err := r.grant(t, r.refs[0], ok.Nonce); err == nil {
		t.Fatal("a terminal's nonce opened a window")
	}
	term.expectRefuse(t, LocalReasonWrongKind)
	r.notDialled(t, "a terminal's nonce")

	win := dialLocal(t, r.server)
	nonce := win.windowHello(t, r.runs[0])
	if err := r.org.Call(ctx5(t), runnerwire.MethodLocalAttach, runnerwire.LocalAttachArgs{Ref: r.refs[0], Nonce: nonce}, nil); err == nil {
		t.Fatal("a window's nonce opened a terminal")
	}
	win.expectRefuse(t, LocalReasonWrongKind)
	if _, err := r.grant(t, r.refs[0], nonce); err == nil {
		t.Fatal("a nonce spent by a refused terminal grant opened a window")
	}

	odd := dialLocal(t, r.server)
	odd.send(t, LocalHello, LocalHelloMsg{V: LocalProtocol, RunID: r.runs[0], Kind: "screen"})
	odd.expectRefuse(t, LocalReasonProtocol)
}

// The socket grants nothing by itself: no display is dialled and no display byte reaches the CLI before
// the org's local_window; after it the display's bytes flow.
func TestLocalWindowNoBytesBeforeGrant(t *testing.T) {
	r := newLWRig(t)
	cli := dialLocal(t, r.server)
	nonce := cli.windowHello(t, r.runs[0])
	if f, ok := cli.next(300 * time.Millisecond); ok {
		t.Fatalf("the CLI read %q %s before the grant", f.typ, f.b)
	}
	r.notDialled(t, "before the grant")
	st, err := r.grant(t, r.refs[0], nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cli.expect(t, LocalBound)
	toCLI(t, r.display(t), cli, []byte("RFB 003.008\n"))
}

// A grant for another run's sandbox than the one the socket named is refused, and nothing is dialled.
func TestLocalWindowRefMismatch(t *testing.T) {
	r := newLWRig(t)
	cli := dialLocal(t, r.server)
	if _, err := r.grant(t, r.refs[1], cli.windowHello(t, r.runs[0])); err == nil {
		t.Fatal("a grant for another run's sandbox opened a window")
	}
	cli.expectRefuse(t, LocalReasonUnavailable)
	r.notDialled(t, "a grant for another run")
}

// No cut text crosses either way: the display's ServerCutText and the CLI's ClientCutText are dropped and
// counted, and everything around them passes. The person closing the window tells the org so.
func TestLocalWindowFilterNoClipboard(t *testing.T) {
	r := newLWRig(t)
	cli, d, st := r.open(t)
	handshake(t, d, cli)
	if _, err := d.Write(append(cutText(3, "TOP-SECRET-COPY"), 2)); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(1, 5*time.Second); !bytes.Equal(got, []byte{2}) {
		t.Fatalf("the CLI read %x, want only the bell", got)
	}
	key := append([]byte{4, 1, 0, 0}, be32(0x61)...)
	fromCLI(t, d, cli, append(cutText(6, "do not paste"), key...), key)
	_ = cli.conn.Close()
	if end := endLine(t, st); end != (runnerwire.LocalWindowEnd{Reason: runnerwire.LocalWindowEndPerson, CutTextDropped: 2}) {
		t.Fatalf("end line = %+v", end)
	}
	displayClosed(t, d)
}

// Each way a window ends first tells the org why in one end line and the CLI in LocalEnd; an org's end
// closes the display and tells the CLI, with no end line.
func TestLocalWindowEndLine(t *testing.T) {
	t.Run("not_rfb", func(t *testing.T) {
		r := newLWRig(t)
		cli, d, st := r.open(t)
		if _, err := d.Write([]byte("HTTP/1.1 200")); err != nil {
			t.Fatal(err)
		}
		cli.expectEnd(t, 5*time.Second, LocalReasonDisplayRefused)
		if end := endLine(t, st); end != (runnerwire.LocalWindowEnd{Reason: runnerwire.LocalWindowEndRefused, RefusalKind: runnerwire.LocalWindowNotRFB}) {
			t.Fatalf("end line = %+v", end)
		}
	})
	t.Run("refused", func(t *testing.T) {
		r := newLWRig(t)
		cli, d, st := r.open(t)
		handshake(t, d, cli)
		cli.send(t, LocalData, []byte{0xee}) // no client message has this type
		cli.expectEnd(t, 5*time.Second, LocalReasonDisplayRefused)
		end := endLine(t, st)
		if end.Reason != runnerwire.LocalWindowEndRefused || end.RefusalKind == "" || end.RefusalKind == runnerwire.LocalWindowNotRFB {
			t.Fatalf("end line = %+v", end)
		}
		displayClosed(t, d)
	})
	t.Run("run_ended", func(t *testing.T) {
		r := newLWRig(t)
		cli, d, st := r.open(t)
		handshake(t, d, cli)
		_ = d.Close()
		cli.expectEnd(t, 5*time.Second, LocalReasonRunEnded)
		if end := endLine(t, st); end != (runnerwire.LocalWindowEnd{Reason: runnerwire.LocalWindowEndRunEnded}) {
			t.Fatalf("end line = %+v", end)
		}
	})
	t.Run("helper_failed", func(t *testing.T) {
		r := newLWRig(t)
		cli, d, st := r.open(t)
		cli.send(t, LocalResize, LocalSizeMsg{Cols: 80, Rows: 24})
		cli.expectEnd(t, 5*time.Second, LocalReasonProtocol)
		if end := endLine(t, st); end != (runnerwire.LocalWindowEnd{Reason: runnerwire.LocalWindowEndHelperFailed}) {
			t.Fatalf("end line = %+v", end)
		}
		displayClosed(t, d)
	})
	t.Run("org_ended", func(t *testing.T) {
		r := newLWRig(t)
		cli, d, st := r.open(t)
		_ = st.Reset(runnerwire.ResetCancelled)
		cli.expectEnd(t, 5*time.Second, LocalReasonOrgEnded)
		displayClosed(t, d)
	})
}

// The window's life is the organisation link's: when it drops, the CLI reads the link lost and its
// socket closes, and the display closes.
func TestLocalWindowEndsWhenPeerDrops(t *testing.T) {
	r := newLWRig(t)
	cli, d, _ := r.open(t)
	r.rp.Close()
	cli.expectEnd(t, 5*time.Second, LocalReasonLinkLost)
	if _, ok := cli.next(time.Second); ok {
		t.Fatal("the CLI's connection stayed open after the link dropped")
	}
	displayClosed(t, d)
}
