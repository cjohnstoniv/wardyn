// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// openRawVMDesktop is openVMDesktop that also hands back the viewer's TCP connection, to send frames the
// WebSocket client never would.
func openRawVMDesktop(t *testing.T, ctx context.Context, h *uiHarness, dr *desktopFakeRunner) (*desktopViewer, net.Conn) {
	t.Helper()
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	conns := make(chan net.Conn, 1)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			conns <- conn
		}
		return conn, err
	}}}
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	v := &desktopViewer{t: t, ctx: ctx, ws: ws}
	<-dr.dialled
	v.handshake()
	return v, <-conns
}

// viewerFrame is a masked frame from the viewer with b0 for its first byte.
func viewerFrame(b0 byte, payload []byte) []byte {
	f := wsFrame(payload, true)
	f[0] = b0
	return f
}

// TestVMDesktopRecording_AFrameTheLibraryWouldRefuseEndsWithAStopMark: on a recorded VM desktop, each frame the
// door's WebSocket library would refuse with an error like a dropped connection's is refused in front of it
// (rfbfilter.ClientFrames): the viewer's socket closes, the frame never reaches the display, and the stored
// recording ends with a stop mark saying the relay refused a WebSocket frame from the browser. A connection that
// drops inside a frame ends the recording with no mark.
func TestVMDesktopRecording_AFrameTheLibraryWouldRefuseEndsWithAStopMark(t *testing.T) {
	const refused = "Wardyn refused a WebSocket frame from the browser that it cannot read"
	for name, tc := range map[string]struct {
		frames []byte
		reason string // "" is the connection dropping: no mark
	}{
		"reserved bits":                      {viewerFrame(0xC2, vmKeyEvent), refused},
		"an unmasked frame":                  {wsFrame(vmKeyEvent, false), refused},
		"an unknown opcode":                  {viewerFrame(0x83, vmKeyEvent), refused},
		"a control frame over 125 bytes":     {viewerFrame(0x89, make([]byte, 126)), refused},
		"a fragmented control frame":         {viewerFrame(0x09, nil), refused},
		"an invalid close payload":           {viewerFrame(0x88, []byte{0x03, 0xED}), refused}, // 1005, never sent
		"a continuation outside a message":   {viewerFrame(0x80, vmKeyEvent), refused},
		"a new message inside one":           {append(viewerFrame(0x02, nil), viewerFrame(0x82, vmKeyEvent)...), refused},
		"a 64-bit length with its top bit":   {[]byte{0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 5}, refused},
		"the connection dropping in a frame": {viewerFrame(0x82, vmKeyEvent)[:7], ""},
	} {
		t.Run(name, func(t *testing.T) {
			h, dr, send, got := vmDesktopRun(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v, raw := openRawVMDesktop(t, ctx, h, dr)
			if _, err := raw.Write(tc.frames); err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				_ = raw.Close()
			}
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			_, _, err := v.ws.Read(rctx)
			open := rctx.Err() != nil
			rcancel()
			if err == nil || open {
				t.Fatalf("the viewer's socket stayed open after %x: %v", tc.frames, err)
			}
			endVMDesktop(t, h, v, send)
			write, pb, _ := storedVMRecording(t, h)
			if tc.reason == "" { // what of the frame arrived may have been relayed, as before
				if _, ok := write["stopped_ms"]; ok || pb.StoppedMS != 0 || pb.StopReason != "" {
					t.Fatalf("a dropped connection marked a stop (row %v, stopped at %d, %q)", write["stopped_ms"], pb.StoppedMS, pb.StopReason)
				}
				return
			}
			select {
			case b := <-got:
				t.Fatalf("the VM read %x: the refused frame reached it", b)
			default:
			}
			stoppedWhereRefused(t, write, pb, tc.reason)
		})
	}
}

// TestDesktopAppRecording_AFrameThatBreaksTheWebSocketProtocolEndsWithAStopMark is the same for a container's
// noVNC app, whose relay hands the browser's own frames to the desktop's websockify: each frame that breaks the
// protocol is refused in front of the filter (rfbfilter.CheckBrowserFrames), closes the browser's socket, never
// reaches the desktop, and ends the stored recording with its stop mark. A connection that drops inside a frame,
// a clean close and the desktop going away end it with no mark.
func TestDesktopAppRecording_AFrameThatBreaksTheWebSocketProtocolEndsWithAStopMark(t *testing.T) {
	const refused = "Wardyn refused a WebSocket frame from the browser that it cannot read"
	for name, tc := range map[string]struct {
		frames []byte
		reason string // "" is no mark
		end    string // how a row with no mark ends: the browser "drop"s, "close"s, or the "desktop" goes away
	}{
		"reserved bits":                      {viewerFrame(0xC2, vmKeyEvent), refused, ""},
		"an unmasked frame":                  {wsFrame(vmKeyEvent, false), refused, ""},
		"an unknown data opcode":             {viewerFrame(0x83, vmKeyEvent), refused, ""},
		"an unknown control opcode":          {viewerFrame(0x8B, nil), refused, ""},
		"a control frame over 125 bytes":     {viewerFrame(0x89, make([]byte, 126)), refused, ""},
		"a fragmented control frame":         {viewerFrame(0x09, nil), refused, ""},
		"an invalid close payload":           {viewerFrame(0x88, []byte{0x03, 0xED}), refused, ""}, // 1005, never sent
		"a continuation outside a message":   {viewerFrame(0x80, vmKeyEvent), refused, ""},
		"a new message inside one":           {append(viewerFrame(0x02, nil), viewerFrame(0x82, vmKeyEvent)...), refused, ""},
		"a 64-bit length with its top bit":   {[]byte{0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 5}, refused, ""},
		"the connection dropping in a frame": {viewerFrame(0x82, vmKeyEvent)[:7], "", "drop"},
		"a clean close":                      {viewerFrame(0x88, []byte{0x03, 0xE8}), "", "close"},
		"the desktop going away":             {viewerFrame(0x82, vmKeyEvent), "", "desktop"},
	} {
		t.Run(name, func(t *testing.T) {
			offered, pasted := make(chan string, 1), make(chan []byte, 1)
			h := desktopHarness(t, rfbDesktopBackend(t, offered, pasted, false), nil)
			h.run.RecordingLevel = types.RecordingScreenActions
			h.store.putRun(h.run)
			h.srv.cfg.RecordingStore = fsRecordingStore(t)
			ds := openDesktop(t, h, desktopApp)
			<-offered
			if next, err := readWSFrame(ds.br); err != nil || !bytes.Equal(next, []byte{2}) {
				t.Fatalf("after ServerInit the browser got %q (err %v), want the Bell", next, err)
			}
			if _, err := ds.conn.Write(tc.frames); err != nil {
				t.Fatalf("%x write: %v", tc.frames, err)
			}
			if tc.end == "drop" {
				_ = ds.conn.Close()
			} else if tc.end != "" {
				select {
				case b := <-pasted: // the desktop read the frame and closed
					if !bytes.Equal(viewerFrame(tc.frames[0], b), tc.frames) {
						t.Fatalf("the desktop got %x, want the payload of the browser's %x", b, tc.frames)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("the browser's %x never reached the desktop", tc.frames)
				}
				for i := 0; tc.end == "desktop" && i < 500; i++ { // until the relay's write to the desktop fails
					if _, err := ds.conn.Write(viewerFrame(0x82, vmKeyEvent)); err != nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				_ = ds.conn.Close()
			} else {
				var ne net.Error
				if _, err := readWSFrame(ds.br); err == nil || errors.As(err, &ne) && ne.Timeout() {
					t.Fatalf("the browser's socket stayed open after %x on a recorded desktop: %v", tc.frames, err)
				}
			}
			write, pb, _ := storedVMRecording(t, h) // the relay closed and stored its recording
			if tc.reason == "" {
				if _, ok := write["stopped_ms"]; ok || pb.StoppedMS != 0 || pb.StopReason != "" {
					t.Fatalf("no refusal marked a stop (row %v, stopped at %d, %q)", write["stopped_ms"], pb.StoppedMS, pb.StopReason)
				}
				return
			}
			select {
			case b := <-pasted:
				t.Fatalf("the desktop read %x: the refused frame reached it", b)
			default:
			}
			stoppedWhereRefused(t, write, pb, tc.reason)
		})
	}
}
