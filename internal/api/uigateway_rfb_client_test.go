// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What a client that sends real RFB input puts on the socket, which the console's own viewer never does
// on a view-only desktop. rfbClientEncodings asks for raw, ZRLE and Extended Clipboard; the relay keeps
// the first two.
var (
	rfbClientPixelFormat   = []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	rfbClientEncodings     = []byte{2, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 16, 0xc0, 0xa1, 0xe5, 0xce}
	rfbClientEncodingsKept = []byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 16}
	rfbClientUpdate        = []byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}
	rfbClientLastUpdate    = []byte{3, 1, 0, 0, 0, 0, 7, 128, 4, 56}
	rfbClientKeyDown       = []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	rfbClientKeyUp         = []byte{4, 0, 0, 0, 0, 0, 0, 'a'}
	rfbClientPointer       = []byte{5, 1, 0, 10, 0, 20}
	rfbClientPointerMove   = []byte{5, 0, 0, 11, 0, 21}

	// The control messages a view-only relay ends the socket on.
	rfbClientXVP            = []byte{250, 0, 1, 2}
	rfbClientSetDesktopSize = []byte{251, 0, 7, 128, 4, 56, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7, 128, 4, 56, 0, 0, 0, 0}
	rfbClientQEMUKey        = []byte{255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 30}

	// One FramebufferUpdate of one raw 32-bit pixel: what the fake display sends the client.
	rfbClientFrame = []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0, 9, 9, 9, 9}
)

const rfbClientExtendedText = "SENTINEL-extended-paste-7be1"

// rfbClientExtendedCut is a ClientCutText in the extended clipboard form: a negative length.
func rfbClientExtendedCut(text string) []byte {
	n := int32(len(text))
	return append(binary.BigEndian.AppendUint32([]byte{6, 0, 0, 0}, uint32(-n)), text...)
}

// rfbClientScript is the client's whole session after the handshake: what it sends, message by message;
// what a view-only display may read of it; and what a display with control reads, which is everything
// but the encoding the relay always cuts.
func rfbClientScript() (sent [][]byte, viewOnly, control []byte) {
	read := func(out, in []byte) {
		sent = append(sent, out)
		viewOnly, control = append(viewOnly, in...), append(control, in...)
	}
	input := func(msg []byte) {
		sent = append(sent, msg)
		control = append(control, msg...)
	}
	read(rfbClientPixelFormat, rfbClientPixelFormat)
	read(rfbClientEncodings, rfbClientEncodingsKept)
	read(rfbClientUpdate, rfbClientUpdate)
	input(rfbClientKeyDown)
	input(rfbClientPointer)
	input(rfbCut(6, pasteSentinel))
	read(rfbClientUpdate, rfbClientUpdate)
	input(rfbClientKeyUp)
	input(rfbClientPointerMove)
	input(rfbClientExtendedCut(rfbClientExtendedText))
	read(rfbClientLastUpdate, rfbClientLastUpdate)
	return sent, viewOnly, control
}

// rfbClientDisplay is the VM's end of one stream: it records every byte after the handshake, and sends
// the client one frame so the stream is shown still carrying the picture.
type rfbClientDisplay struct {
	mu    sync.Mutex
	got   []byte
	ended chan struct{}
}

func (d *rfbClientDisplay) serve(b net.Conn) {
	defer close(d.ended)
	go func() { _, _ = b.Write(rfbClientFrame) }()
	buf := make([]byte, 4096)
	for {
		n, err := b.Read(buf)
		d.mu.Lock()
		d.got = append(d.got, buf[:n]...)
		d.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (d *rfbClientDisplay) recorded() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.got...)
}

// waitFor returns once the recording ends in end.
func (d *rfbClientDisplay) waitFor(ctx context.Context, t *testing.T, end []byte) {
	t.Helper()
	for !bytes.HasSuffix(d.recorded(), end) {
		select {
		case <-ctx.Done():
			t.Fatalf("the display never read %v; it read %v", end, d.recorded())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// all waits for the stream to end and returns the whole recording.
func (d *rfbClientDisplay) all(ctx context.Context, t *testing.T) []byte {
	t.Helper()
	select {
	case <-d.ended:
	case <-ctx.Done():
		t.Fatalf("the display's stream never ended; it read %v", d.recorded())
	}
	return d.recorded()
}

// rfbClientRig is a macOS VM run behind the real gateway, over a real WebSocket, with a recording
// display. Each open is one stream under the same session cookie.
type rfbClientRig struct {
	h        *uiHarness
	dr       *desktopFakeRunner
	url      string
	cookie   *http.Cookie
	displays chan *rfbClientDisplay
}

// rfbClientNewRig puts the run on a live Mac whose caps offer desktop, or on no runner when it is "".
func rfbClientNewRig(t *testing.T, desktop string) *rfbClientRig {
	t.Helper()
	h, dr := newDesktopHarness(t)
	h.srv.cfg.Store = &desktopSiteStore{desktopClosedStore: h.srv.cfg.Store.(*desktopClosedStore)}
	h.run.Agent, h.run.MachineKind = "claude-code", types.MachineMacOSVM
	if desktop != "" {
		caps := laptopCaps(types.CC1)
		caps.MacOSVM = readyMac()
		caps.MacOSVM.Desktop = desktop
		data, err := json.Marshal(caps)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
		sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
		h.srv.runnerStreams.subsMu.Lock()
		if h.srv.runnerStreams.subs == nil {
			h.srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
		}
		h.srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
		h.srv.runnerStreams.subsMu.Unlock()
		h.run.RunnerID = &id
	}
	h.store.putRun(h.run)
	displays := make(chan *rfbClientDisplay, 4)
	dr.after = func(b net.Conn) {
		d := &rfbClientDisplay{ended: make(chan struct{})}
		displays <- d
		d.serve(b)
	}
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	return &rfbClientRig{
		h: h, dr: dr, url: srv.URL, displays: displays,
		cookie: desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))),
	}
}

// open is one stream through the RFB 3.8 handshake.
func (r *rfbClientRig) open(ctx context.Context, t *testing.T) (*desktopViewer, *rfbClientDisplay) {
	t.Helper()
	v := dialDesktop(t, ctx, r.h, r.url, r.cookie)
	select {
	case <-r.dr.dialled:
	case <-ctx.Done():
		t.Fatal("the display was never dialled")
	}
	v.handshake()
	select {
	case d := <-r.displays:
		return v, d
	case <-ctx.Done():
		t.Fatal("the display never finished its handshake")
	}
	return nil, nil
}

// play runs the whole script over one stream and returns every byte the display read. chunks splits the
// client's bytes into WebSocket messages.
func (r *rfbClientRig) play(ctx context.Context, t *testing.T, chunks func([][]byte) [][]byte) []byte {
	t.Helper()
	v, d := r.open(ctx, t)
	sent, _, _ := rfbClientScript()
	for _, chunk := range chunks(sent) {
		v.write(chunk)
	}
	d.waitFor(ctx, t, rfbClientLastUpdate)
	if got := v.read(len(rfbClientFrame)); !bytes.Equal(got, rfbClientFrame) {
		t.Fatalf("the client got %v, want the display's frame %v", got, rfbClientFrame)
	}
	_ = v.ws.CloseNow()
	return d.all(ctx, t)
}

// waitClosed returns once n streams have written ui.close, and with it every row of theirs.
func (r *rfbClientRig) waitClosed(ctx context.Context, t *testing.T, n int) []types.AuditEvent {
	t.Helper()
	for {
		rows := r.h.audit.auditRows("ui.close", "success")
		if len(rows) >= n {
			return rows
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ui.close rows = %d, want %d", len(rows), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func rfbClientWhole(sent [][]byte) [][]byte { return [][]byte{bytes.Join(sent, nil)} }

func rfbClientPerMessage(sent [][]byte) [][]byte { return sent }

func rfbClientPerByte(sent [][]byte) [][]byte {
	var out [][]byte
	for _, b := range bytes.Join(sent, nil) {
		out = append(out, []byte{b})
	}
	return out
}

// rfbClientCheckViewOnlyRows checks the audit trail of sessions view-only streams of the script: one
// ui.input.drop each for key and pointer per stream, naming the kind and nothing of the input, and a
// separate ui.clipboard.transfer denied for each paste, by size and hash, never as a drop and never with
// its text. The run's desktop rules allow paste in, so view only is what denies it.
func rfbClientCheckViewOnlyRows(ctx context.Context, t *testing.T, r *rfbClientRig, sessions int) {
	t.Helper()
	for _, row := range r.waitClosed(ctx, t, sessions) {
		if !strings.Contains(string(row.Data), `"view_only":true`) {
			t.Errorf("ui.close %s, want view_only", row.Data)
		}
	}
	for _, action := range []string{"ui.authorize", "ui.open"} {
		rows := r.h.audit.auditRows(action, "success")
		if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"view_only":true`) {
			t.Errorf("%s rows = %+v, want view_only", action, rows)
		}
	}

	drops := r.h.audit.auditRows("ui.input.drop", "denied")
	if len(drops) != 2*sessions {
		t.Fatalf("ui.input.drop rows = %+v, want one each for key and pointer in each of %d sessions", drops, sessions)
	}
	if rows := r.h.audit.auditRows("ui.input.drop", "success"); len(rows) != 0 {
		t.Errorf("ui.input.drop rows that are not denied: %+v", rows)
	}
	for i, row := range drops {
		want := []string{"key", "pointer"}[i%2]
		var d map[string]any
		if err := json.Unmarshal(row.Data, &d); err != nil {
			t.Fatal(err)
		}
		if row.Target != want || d["input"] != want || d["app"] != uiDesktopAppName || len(d) != 2 {
			t.Errorf("ui.input.drop row %d = target %q %s, want the kind %q and the app only", i, row.Target, row.Data, want)
		}
	}

	rows := r.h.audit.auditRows("ui.clipboard.transfer", "denied")
	if len(rows) != 2*sessions {
		t.Fatalf("denied ui.clipboard.transfer rows = %+v, want the plain and extended paste in each of %d sessions", rows, sessions)
	}
	if ok := r.h.audit.auditRows("ui.clipboard.transfer", "success"); len(ok) != 0 {
		t.Errorf("a paste on a view-only desktop was allowed: %+v", ok)
	}
	rfbClientCheckPasteRows(t, rows)
}

// rfbClientCheckPasteRows checks each row is one of the script's two pastes, by size only.
func rfbClientCheckPasteRows(t *testing.T, rows []types.AuditEvent) {
	t.Helper()
	seen := map[int]int{}
	for _, row := range rows {
		var d struct {
			App       string `json:"app"`
			Direction string `json:"direction"`
			Bytes     int    `json:"bytes"`
		}
		if err := json.Unmarshal(row.Data, &d); err != nil {
			t.Fatal(err)
		}
		if (d.Bytes != len(pasteSentinel) && d.Bytes != len(rfbClientExtendedText)) || d.Direction != "paste_in" || row.Target != "paste_in" || d.App != uiDesktopAppName {
			t.Errorf("ui.clipboard.transfer row target %q %s is not one of the two pastes", row.Target, row.Data)
		}
		seen[d.Bytes]++
		if strings.Contains(string(row.Data), pasteSentinel) || strings.Contains(string(row.Data), rfbClientExtendedText) || strings.Contains(string(row.Data), "sha256") {
			t.Errorf("a ui.clipboard.transfer row carries the text or a digest of it: %s", row.Data)
		}
	}
	if len(seen) != 2 || seen[len(pasteSentinel)] != seen[len(rfbClientExtendedText)] {
		t.Errorf("paste rows by size = %v, want the plain and the extended paste equally", seen)
	}
}

// A client that speaks RFB, not the console's viewer, on a view-only macOS desktop: after the handshake
// the display reads only the pixel format, the encodings and the update requests, however the input is
// split into WebSocket messages, and the picture still reaches the client. Caps that say observe and no
// caps at all are the same.
func TestRFBClientOnAViewOnlyMacSendsTheDisplayOnlyReadRequests(t *testing.T) {
	for _, desktop := range []struct{ name, caps string }{
		{"caps say observe", runnerwire.MacVMDesktopObserve},
		{"no runner to ask", ""},
	} {
		for _, split := range []struct {
			name   string
			chunks func([][]byte) [][]byte
		}{
			{"one WebSocket message", rfbClientWhole},
			{"one per RFB message", rfbClientPerMessage},
			{"one per byte", rfbClientPerByte},
		} {
			t.Run(desktop.name+", "+split.name, func(t *testing.T) {
				r := rfbClientNewRig(t, desktop.caps)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_, want, _ := rfbClientScript()
				if got := r.play(ctx, t, split.chunks); !bytes.Equal(got, want) {
					t.Fatalf("the display read %v, want only %v", got, want)
				}
				rfbClientCheckViewOnlyRows(ctx, t, r, 1)
			})
		}
	}
}

// Input is reported once per kind per stream, not once a run: a second stream reports its own.
func TestRFBClientDropsAreAuditedPerSession(t *testing.T) {
	r := rfbClientNewRig(t, runnerwire.MacVMDesktopObserve)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, want, _ := rfbClientScript()
	for session := 1; session <= 2; session++ {
		if got := r.play(ctx, t, rfbClientPerMessage); !bytes.Equal(got, want) {
			t.Fatalf("session %d: the display read %v, want only %v", session, got, want)
		}
		rfbClientCheckViewOnlyRows(ctx, t, r, session)
	}
}

// The same client on a Mac whose caps say control: every message reaches the display, and the socket
// stays open for the ones a view-only relay ends it on. This is what the recording shows when input
// leaks, so the view-only tests above can fail.
func TestRFBClientWithControlReachesTheDisplayWhole(t *testing.T) {
	r := rfbClientNewRig(t, runnerwire.MacVMDesktopControl)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sent, _, want := rfbClientScript()
	enders := bytes.Join([][]byte{rfbClientXVP, rfbClientSetDesktopSize, rfbClientQEMUKey}, nil)
	last := len(sent) - 1
	want = bytes.Join([][]byte{want[:len(want)-len(sent[last])], enders, sent[last]}, nil)
	got := r.play(ctx, t, func(sent [][]byte) [][]byte {
		return [][]byte{bytes.Join(sent[:last], nil), enders, sent[last]}
	})
	if !bytes.Equal(got, want) {
		t.Fatalf("the display read %v, want all of %v", got, want)
	}
	r.waitClosed(ctx, t, 1)
	if rows := r.h.audit.auditRows("ui.input.drop", "denied"); len(rows) != 0 {
		t.Errorf("ui.input.drop rows on a desktop with control: %+v", rows)
	}
	rows := r.h.audit.auditRows("ui.clipboard.transfer", "success")
	if len(rows) != 2 {
		t.Fatalf("ui.clipboard.transfer rows = %+v, want the two pastes allowed", rows)
	}
	rfbClientCheckPasteRows(t, rows)
}

// xvp, SetDesktopSize and QEMU's extended key are control the relay cannot drop and carry on: each ends
// the stream, and nothing of it or after it reaches the display.
func TestRFBClientViewOnlyEndsTheStreamOnControlMessages(t *testing.T) {
	for name, msg := range map[string][]byte{
		"xvp":               rfbClientXVP,
		"SetDesktopSize":    rfbClientSetDesktopSize,
		"QEMU extended key": rfbClientQEMUKey,
	} {
		t.Run(name, func(t *testing.T) {
			r := rfbClientNewRig(t, runnerwire.MacVMDesktopObserve)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v, d := r.open(ctx, t)
			v.write(bytes.Join([][]byte{rfbClientPixelFormat, rfbClientEncodings, rfbClientUpdate}, nil))
			d.waitFor(ctx, t, rfbClientUpdate)
			v.write(bytes.Join([][]byte{msg, rfbClientKeyDown, rfbClientLastUpdate}, nil))
			var err error
			for err == nil {
				_, _, err = v.ws.Read(ctx)
			}
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != desktopCloseUnreachable {
				t.Fatalf("the stream ended with %v, want close %d", err, desktopCloseUnreachable)
			}
			want := bytes.Join([][]byte{rfbClientPixelFormat, rfbClientEncodingsKept, rfbClientUpdate}, nil)
			if got := d.all(ctx, t); !bytes.Equal(got, want) {
				t.Fatalf("the display read %v, want only %v", got, want)
			}
			r.waitClosed(ctx, t, 1)
		})
	}
}
