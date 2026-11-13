// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
