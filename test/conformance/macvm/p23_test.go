// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// ---- a small RFB client ----

// rfbClient is the viewer side of RFB 3.8 with security None, which is what the helper's desktop.sock
// presents (internal/macvm/rfbshim.go: rfbPresentNone). It frames only what it asks for: Raw rectangles.
type rfbClient struct {
	c    net.Conn
	r    *bufio.Reader
	w, h int // the framebuffer, from ServerInit
	bpp  int // bytes per pixel
}

// rfbMaxRect bounds one Raw rectangle a server may send.
const rfbMaxRect = 16 << 20

// rfbDial connects to a desktop socket and completes the handshake and ServerInit within wait.
func rfbDial(path string, wait time.Duration) (*rfbClient, error) {
	c, err := net.DialTimeout("unix", path, wait)
	if err != nil {
		return nil, err
	}
	return newRFBClient(c, wait)
}

// newRFBClient runs the handshake on an open connection, closing it when that fails.
func newRFBClient(c net.Conn, wait time.Duration) (*rfbClient, error) {
	x := &rfbClient{c: c, r: bufio.NewReader(c)}
	if err := x.handshake(wait); err != nil {
		_ = c.Close()
		return nil, err
	}
	return x, nil
}

func (x *rfbClient) handshake(wait time.Duration) error {
	_ = x.c.SetDeadline(time.Now().Add(wait))
	defer func() { _ = x.c.SetDeadline(time.Time{}) }()
	v := make([]byte, 12)
	if _, err := io.ReadFull(x.r, v); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	if string(v) != "RFB 003.008\n" {
		return fmt.Errorf("the server speaks %q, not RFB 3.8", v)
	}
	if _, err := io.WriteString(x.c, "RFB 003.008\n"); err != nil {
		return err
	}
	n, err := x.r.ReadByte()
	if err != nil {
		return fmt.Errorf("security types: %w", err)
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(x.r, types); err != nil || n != 1 || types[0] != 1 {
		return fmt.Errorf("security types %v, want only None", types)
	}
	if _, err := x.c.Write([]byte{1}); err != nil {
		return err
	}
	var res [4]byte
	if _, err := io.ReadFull(x.r, res[:]); err != nil || binary.BigEndian.Uint32(res[:]) != 0 {
		return fmt.Errorf("security result %v: %v", res, err)
	}
	if _, err := x.c.Write([]byte{1}); err != nil { // ClientInit: shared
		return err
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(x.r, si); err != nil {
		return fmt.Errorf("ServerInit: %w", err)
	}
	x.w, x.h, x.bpp = int(binary.BigEndian.Uint16(si[0:])), int(binary.BigEndian.Uint16(si[2:])), int(si[4]/8)
	if x.bpp < 1 || x.bpp > 4 {
		return fmt.Errorf("pixel format of %d bits per pixel", si[4])
	}
	name := binary.BigEndian.Uint32(si[20:])
	if name > 4<<10 {
		return fmt.Errorf("a desktop name of %d bytes", name)
	}
	_, err = io.CopyN(io.Discard, x.r, int64(name))
	return err
}

func (x *rfbClient) Close() error { return x.c.Close() }

// setRaw asks for Raw rectangles only, so every update is framed by this client.
func (x *rfbClient) setRaw() error {
	_, err := x.c.Write([]byte{2, 0, 0, 1, 0, 0, 0, 0})
	return err
}

// request asks for the w x h region at x0,y0, whole (not incremental).
func (x *rfbClient) request(x0, y0, w, h int) error {
	b := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(x0))
	binary.BigEndian.PutUint16(b[4:], uint16(y0))
	binary.BigEndian.PutUint16(b[6:], uint16(w))
	binary.BigEndian.PutUint16(b[8:], uint16(h))
	_, err := x.c.Write(b)
	return err
}

// pointer sends one PointerEvent.
func (x *rfbClient) pointer(buttons byte, px, py int) error {
	b := []byte{5, buttons, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(px))
	binary.BigEndian.PutUint16(b[4:], uint16(py))
	_, err := x.c.Write(b)
	return err
}

// rfbFrame is the first FramebufferUpdate that came back.
type rfbFrame struct {
	Rects, Bytes int
	Distinct     int // distinct pixel values in the Raw rectangles, capped at 2
}

// frame reads server messages until a FramebufferUpdate arrives or wait passes. Bell and cut text are
// skipped; any other message, or a rectangle that is not Raw, is an error.
func (x *rfbClient) frame(wait time.Duration) (rfbFrame, error) {
	_ = x.c.SetDeadline(time.Now().Add(wait))
	defer func() { _ = x.c.SetDeadline(time.Time{}) }()
	for {
		t, err := x.r.ReadByte()
		if err != nil {
			return rfbFrame{}, err
		}
		switch t {
		case 0:
			h := make([]byte, 3)
			if _, err := io.ReadFull(x.r, h); err != nil {
				return rfbFrame{}, err
			}
			var f rfbFrame
			f.Rects = int(binary.BigEndian.Uint16(h[1:]))
			first := make([]byte, x.bpp)
			seen := false
			for range f.Rects {
				rh := make([]byte, 12)
				if _, err := io.ReadFull(x.r, rh); err != nil {
					return f, err
				}
				w, hh, enc := int(binary.BigEndian.Uint16(rh[4:])), int(binary.BigEndian.Uint16(rh[6:])), int32(binary.BigEndian.Uint32(rh[8:]))
				n := w * hh * x.bpp
				if enc != 0 || n > rfbMaxRect {
					return f, fmt.Errorf("a rectangle of encoding %d and %d bytes: this client asked for Raw only", enc, n)
				}
				px := make([]byte, n)
				if _, err := io.ReadFull(x.r, px); err != nil {
					return f, err
				}
				f.Bytes += n
				for i := 0; i+x.bpp <= n && f.Distinct < 2; i += x.bpp {
					if !seen {
						copy(first, px[:x.bpp])
						seen, f.Distinct = true, 1
					} else if !bytes.Equal(first, px[i:i+x.bpp]) {
						f.Distinct = 2
					}
				}
			}
			return f, nil
		case 2: // SetColourMapEntries
			h := make([]byte, 5)
			if _, err := io.ReadFull(x.r, h); err != nil {
				return rfbFrame{}, err
			}
			if _, err := io.CopyN(io.Discard, x.r, int64(binary.BigEndian.Uint16(h[3:]))*6); err != nil {
				return rfbFrame{}, err
			}
		case 3: // Bell
		case 4: // ServerCutText
			h := make([]byte, 7)
			if _, err := io.ReadFull(x.r, h); err != nil {
				return rfbFrame{}, err
			}
			if _, err := io.CopyN(io.Discard, x.r, int64(binary.BigEndian.Uint32(h[3:]))); err != nil {
				return rfbFrame{}, err
			}
		default:
			return rfbFrame{}, fmt.Errorf("server message type %d", t)
		}
	}
}

// ---- the helper log's counter and the guest's pointer ----

var (
	sessionEnded = regexp.MustCompile(`desktop session ended.*`)
	droppedCount = regexp.MustCompile(`input_dropped"?[=:]\s*(\d+)`)
)

// inputDropped is the input_dropped counter of the last "desktop session ended" line in a helper log.
func inputDropped(log []byte) (n int, ok bool) {
	for _, l := range sessionEnded.FindAll(log, -1) {
		if m := droppedCount.FindSubmatch(l); m != nil {
			n, _ = strconv.Atoi(string(m[1]))
			ok = true
		}
	}
	return n, ok
}

// guestPointer is the pointer's position as the guest reads it.
type guestPointer struct{ X, Y float64 }

const (
	pointerRead = `osascript -l JavaScript -e 'ObjC.import("AppKit"); JSON.stringify($.NSEvent.mouseLocation)'`
	// pointerWarp is the guest moving its own pointer, the control that shows the reading can change.
	pointerWarp = `osascript -l JavaScript -e 'ObjC.import("CoreGraphics"); $.CGWarpMouseCursorPosition($.CGPointMake(%d, %d)); "ok"'`
)

func parsePointer(out string) (guestPointer, bool) {
	var p struct{ X, Y *float64 }
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &p) != nil || p.X == nil || p.Y == nil {
		return guestPointer{}, false
	}
	return guestPointer{*p.X, *p.Y}, true
}

func (p guestPointer) near(q guestPointer) bool {
	return math.Abs(p.X-q.X) <= 2 && math.Abs(p.Y-q.Y) <= 2
}

// readPointer reads the guest's pointer with a command, false when it gives no position.
func readPointer(t *testing.T, id string) (guestPointer, bool) {
	t.Helper()
	r := guest(t, id, pointerRead, time.Minute)
	if r.code != 0 {
		return guestPointer{}, false
	}
	return parsePointer(r.out)
}

// pointerReadable moves the guest's own pointer to two places and checks the reading follows both: a reading
// that cannot change proves nothing about input that did not move it. It returns the pointer at the second.
func pointerReadable(t *testing.T, id string) (guestPointer, bool) {
	t.Helper()
	for _, to := range []guestPointer{{300, 300}, {120, 200}} {
		guest(t, id, fmt.Sprintf(pointerWarp, int(to.X), int(to.Y)), time.Minute)
		got, ok := readPointer(t, id)
		if !ok || !got.near(to) {
			return guestPointer{}, false
		}
	}
	return guestPointer{120, 200}, true
}

// p23Verdict decides the row from its three facts. dropped is the helper's input_dropped, -1 when its log
// gave none; sent is the pointer events the client sent. The counter alone never passes: with the guest's
// pointer unreadable the row is not run, however many events were counted.
func p23Verdict(readable, moved bool, dropped, sent int) (status, why string) {
	switch {
	case readable && moved:
		return "fail", "the pointer moved in the guest although the desktop is view only"
	case dropped >= 0 && dropped != sent:
		return "fail", fmt.Sprintf("the helper dropped %d of the %d pointer events sent", dropped, sent)
	case !readable:
		return "notrun", "the guest's pointer position cannot be read and set by a command (no GUI session for exec), so the counter alone cannot pass"
	case dropped < 0:
		return "notrun", "the helper log has no desktop-session-ended line with input_dropped"
	}
	return "pass", fmt.Sprintf("the pointer did not move and the helper dropped all %d pointer events", sent)
}

// TestP23_DesktopObserve: on a view-only (observe) base, a viewer on the helper's desktop.sock gets a frame and
// its pointer input never moves the guest. This is the helper layer only: desktop.sock bypasses the gateway, so
// the gateway's own view-only filter is witnessed only by P9 in the console.
func TestP23_DesktopObserve(t *testing.T) {
	live(t)
	block(t)
	id := start(t, spec{})
	if !macvm.BaseViewOnly(runDir(t, id)) {
		notRun(t, "this run's base has desktop control, so pointer input is meant to reach the guest; the drop proof is for a view-only base")
	}
	sock := filepath.Join(macvm.SockDir("", id), macvm.DesktopSock)
	if _, err := os.Stat(sock); err != nil {
		notRun(t, "the run has no desktop.sock: %v", err)
	}
	begun := time.Now()
	c, err := rfbDial(sock, 30*time.Second)
	if err != nil {
		notRun(t, "desktop.sock refused an RFB session (%s): run with the gateway viewer closed", clean(err.Error()))
	}
	defer c.Close()
	t.Log("EVIDENCE: helper layer only (desktop.sock); the gateway's view-only filter is P9's")
	t.Run("frame", func(t *testing.T) {
		if err := c.setRaw(); err != nil {
			t.Fatal(err)
		}
		if err := c.request(0, 0, min(64, c.w), min(64, c.h)); err != nil {
			t.Fatal(err)
		}
		f, err := c.frame(30 * time.Second)
		if err != nil {
			t.Fatalf("no frame from desktop.sock: %v", err)
		}
		record(t, "first_frame_ms", time.Since(begun).Milliseconds())
		record(t, "framebuffer", fmt.Sprintf("%dx%d", c.w, c.h))
		record(t, "corner_uniform", f.Distinct < 2)
		t.Logf("EVIDENCE: a frame of %d bytes arrived", f.Bytes)
	})
	readable := true
	var before guestPointer
	if before, readable = pointerReadable(t, id); !readable {
		t.Log("the guest's pointer could not be read and set from a command")
	}
	sent := 0
	move := func(buttons byte, px, py int) {
		if err := c.pointer(buttons, px, py); err != nil {
			t.Fatalf("sending a pointer event: %v", err)
		}
		sent++
	}
	for i := range 10 {
		move(0, 100+i*90, 80+i*60)
	}
	move(1, 900, 700)
	move(0, 900, 700)
	// A round trip after the events: the helper handles a viewer's messages in order.
	if err := c.request(0, 0, 8, 8); err == nil {
		_, _ = c.frame(30 * time.Second)
	}
	time.Sleep(2 * time.Second)
	moved := false
	if readable {
		after, ok := readPointer(t, id)
		moved = !ok || !after.near(before)
	}
	_ = c.Close()
	dropped := -1
	log := filepath.Join(runDir(t, id), macvm.HelperLog)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if b, err := os.ReadFile(log); err == nil {
			if n, ok := inputDropped(b); ok {
				dropped = n
				break
			}
		}
	}
	record(t, "pointer_events_sent", sent)
	record(t, "input_dropped", dropped)
	record(t, "guest_pointer_readable", readable)
	status, why := p23Verdict(readable, moved, dropped, sent)
	switch status {
	case "fail":
		t.Fatal(why)
	case "notrun":
		notRun(t, "%s", why)
	}
	t.Logf("EVIDENCE: %s (helper layer only)", why)
}

// ---- self-tests: these run everywhere, with no Mac ----

func TestDesktopVerdict(t *testing.T) {
	for name, c := range map[string]struct {
		readable, moved bool
		dropped, sent   int
		want            string
	}{
		"all dropped, pointer still":          {true, false, 12, 12, "pass"},
		"pointer moved, counter fine":         {true, true, 12, 12, "fail"},
		"counter zero, pointer still":         {true, false, 0, 12, "fail"},
		"some got through":                    {true, false, 5, 12, "fail"},
		"counter fine, pointer unreadable":    {false, false, 12, 12, "notrun"},
		"counter zero, pointer unreadable":    {false, false, 0, 12, "fail"},
		"pointer fine, no counter line":       {true, false, -1, 12, "notrun"},
		"pointer unreadable, no counter line": {false, false, -1, 12, "notrun"},
		"pointer moved, counter missing":      {true, true, -1, 12, "fail"},
	} {
		if got, why := p23Verdict(c.readable, c.moved, c.dropped, c.sent); got != c.want {
			t.Errorf("%s: %s (%s), want %s", name, got, why, c.want)
		}
	}
}

func TestInputDroppedAndPointer(t *testing.T) {
	for name, c := range map[string]struct {
		log  string
		n    int
		want bool
	}{
		"text":      {"time=x level=INFO msg=\"macvm: desktop session ended\" err=\"\" client_cut_texts=0 server_cut_texts=0 input_dropped=12\n", 12, true},
		"json":      {`{"msg":"macvm: desktop session ended","input_dropped":3}` + "\n", 3, true},
		"last wins": {"desktop session ended input_dropped=1\ndesktop session ended input_dropped=4\n", 4, true},
		"no line":   {"desktop refused\n", 0, false},
		"other key": {"msg=other input_dropped=9\n", 0, false},
	} {
		if n, ok := inputDropped([]byte(c.log)); n != c.n || ok != c.want {
			t.Errorf("%s: %d %v, want %d %v", name, n, ok, c.n, c.want)
		}
	}
	if p, ok := parsePointer(`{"x":300,"y":412.5}`); !ok || !p.near(guestPointer{301, 411}) || p.near(guestPointer{310, 412}) {
		t.Errorf("parsePointer: %v %v", p, ok)
	}
	for _, bad := range []string{"", "{}", `{"x":1}`, "0,0", "undefined"} {
		if _, ok := parsePointer(bad); ok {
			t.Errorf("parsePointer(%q) read a position", bad)
		}
	}
}

// tcpPair is two connected conns.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	defer l.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := l.Accept(); ch <- res{c, err} }()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return a, r.c
}

// fakeScreenSharing is the guest's Screen Sharing: VncAuth in, a 128x96 desktop out, one Raw update per
// request, and a count of the pointer events that reach it.
func fakeScreenSharing(c net.Conn, pointers *atomic.Int64) {
	defer c.Close()
	r := bufio.NewReader(c)
	c.Write([]byte("RFB 003.008\n"))
	io.CopyN(io.Discard, r, 12)
	c.Write([]byte{1, 2})
	io.CopyN(io.Discard, r, 1)
	c.Write(make([]byte, 16))
	io.CopyN(io.Discard, r, 16)
	c.Write([]byte{0, 0, 0, 0})
	io.CopyN(io.Discard, r, 1)
	si := make([]byte, 24)
	binary.BigEndian.PutUint16(si[0:], 128)
	binary.BigEndian.PutUint16(si[2:], 96)
	copy(si[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.BigEndian.PutUint32(si[20:], 4)
	c.Write(append(si, "fake"...))
	for {
		t, err := r.ReadByte()
		if err != nil {
			return
		}
		var skip int64
		switch t {
		case 0:
			skip = 19
		case 2:
			h := make([]byte, 3)
			io.ReadFull(r, h)
			skip = 4 * int64(binary.BigEndian.Uint16(h[1:]))
		case 3:
			req := make([]byte, 9)
			if _, err := io.ReadFull(r, req); err != nil {
				return
			}
			w, h := int(binary.BigEndian.Uint16(req[5:])), int(binary.BigEndian.Uint16(req[7:]))
			msg := []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint16(msg[8:], uint16(w))
			binary.BigEndian.PutUint16(msg[10:], uint16(h))
			px := make([]byte, w*h*4)
			for i := range px {
				px[i] = byte(i)
			}
			c.Write(append(msg, px...))
		case 4:
			skip = 7
		case 5:
			pointers.Add(1)
			skip = 5
		default:
			return
		}
		io.CopyN(io.Discard, r, skip)
	}
}

// lockedBuffer is a log sink two goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b.Bytes()...)
}

// TestRFBClientAgainstShim runs this client against the real RFB shim (internal/macvm) in front of a fake
// Screen Sharing, as a view-only desktop and as one with control. The view-only shim passes the row's facts;
// the one that lets input through is the planted failure the row must catch.
func TestRFBClientAgainstShim(t *testing.T) {
	for _, viewOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("view_only=%v", viewOnly), func(t *testing.T) {
			viewer, shimViewer := tcpPair(t)
			shimGuest, apple := tcpPair(t)
			var pointers atomic.Int64
			go fakeScreenSharing(apple, &pointers)
			var log lockedBuffer
			shim := &macvm.RFBShim{ViewOnly: viewOnly, Log: slog.New(slog.NewTextHandler(&log, nil)),
				Credential: func(context.Context) (string, error) { return "pw123456", nil }}
			done := make(chan struct{})
			go func() { shim.ServeDesktop(shimViewer, shimGuest); close(done) }()

			c, err := newRFBClient(viewer, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.setRaw(); err != nil {
				t.Fatal(err)
			}
			if err := c.request(0, 0, 64, 64); err != nil {
				t.Fatal(err)
			}
			f, err := c.frame(5 * time.Second)
			if err != nil || f.Bytes != 64*64*4 || f.Distinct != 2 {
				t.Fatalf("frame %+v, %v: want a 64x64 Raw frame with more than one pixel value", f, err)
			}
			const sent = 6
			for i := range sent {
				if err := c.pointer(0, 10+i, 20+i); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.request(0, 0, 8, 8); err != nil {
				t.Fatal(err)
			}
			if _, err := c.frame(5 * time.Second); err != nil {
				t.Fatal(err)
			}
			c.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the shim did not end the session")
			}
			dropped, ok := inputDropped(log.Bytes())
			if !ok {
				t.Fatalf("no input_dropped in the shim's log:\n%s", log.Bytes())
			}
			reached := int(pointers.Load())
			if viewOnly && (dropped != sent || reached != 0) {
				t.Errorf("view only: dropped %d, reached the guest %d, want %d and 0", dropped, reached, sent)
			}
			if !viewOnly && (dropped != 0 || reached != sent) {
				t.Errorf("control: dropped %d, reached the guest %d, want 0 and %d", dropped, reached, sent)
			}
			// What the row would conclude, with the guest's pointer following what reached it.
			status, _ := p23Verdict(true, reached > 0, dropped, sent)
			if want := map[bool]string{true: "pass", false: "fail"}[viewOnly]; status != want {
				t.Errorf("verdict %s, want %s", status, want)
			}
		})
	}
}
