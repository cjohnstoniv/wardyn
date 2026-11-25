// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// One keyboard at a time on a Linux VM run: a browser desktop with control yields to a local window that opens,
// and one opened while the window is open only watches, so no key or pointer move reaches the display.
func TestDesktopViewOnlyWhileLocalWindow(t *testing.T) {
	t.Run("a stream with control yields to the window", func(t *testing.T) {
		h, dr := newDesktopHarness(t)
		dr.after = func(b net.Conn) { _, _ = io.Copy(io.Discard, b) }
		srv := httptest.NewServer(h.gateway)
		t.Cleanup(srv.Close)
		c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, h, srv.URL, c)
		<-dr.dialled
		v.handshake()
		w := &localWindow{runID: h.run.ID}
		if !h.srv.localWindows.reserve(w) {
			t.Fatal("the window's slot was already held")
		}
		t.Cleanup(func() { h.srv.localWindows.release(w) })
		if !h.srv.macWindowYieldControl(ctx, h.run.ID) {
			t.Fatal("the stream with control never yielded to the window")
		}
		for {
			_, _, err := v.ws.Read(ctx)
			if err == nil {
				continue
			}
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != desktopCloseWindowOpen || ce.Reason != desktopWindowOpenMsg {
				t.Fatalf("stream = %v, want close %d %q", err, desktopCloseWindowOpen, desktopWindowOpenMsg)
			}
			return
		}
	})

	t.Run("a stream opened while the window is open only watches", func(t *testing.T) {
		h, dr := newDesktopHarness(t)
		w := &localWindow{runID: h.run.ID}
		if !h.srv.localWindows.reserve(w) {
			t.Fatal("the window's slot was already held")
		}
		t.Cleanup(func() { h.srv.localWindows.release(w) })
		request := []byte{3, 1, 0, 0, 0, 0, 7, 128, 4, 56}
		reached := make(chan []byte, 1)
		dr.after = func(b net.Conn) {
			got := make([]byte, len(request))
			if _, err := io.ReadFull(b, got); err == nil {
				reached <- got
			}
			_, _ = io.Copy(io.Discard, b)
		}
		srv := httptest.NewServer(h.gateway)
		t.Cleanup(srv.Close)
		c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, h, srv.URL, c)
		<-dr.dialled
		v.handshake()
		var input []byte
		input = append(input, 4, 1, 0, 0, 0, 0, 0, 'a')
		input = append(input, 5, 1, 0, 10, 0, 20)
		v.write(append(input, request...))
		select {
		case got := <-reached:
			if string(got) != string(request) {
				t.Fatalf("the VM got %q, want only the update request", got)
			}
		case <-ctx.Done():
			t.Fatal("the update request never reached the VM")
		}
		if left := h.srv.desktopControl.end(h.run.ID); left != 0 {
			t.Fatalf("%d streams hold control beside the window, want none", left)
		}
		rows := h.audit.auditRows("ui.open", "success")
		if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"view_only":true`) {
			t.Fatalf("ui.open rows = %+v, want view_only", rows)
		}
	})
}

// A WebSocket close reason holds at most 123 bytes (the close frame's 125 less its 2-byte code); a longer one
// makes Close drop the connection with no close frame, so the viewer would never see 4006.
func TestDesktopWindowOpenMsgFitsACloseFrame(t *testing.T) {
	if n := len(desktopWindowOpenMsg); n > 123 {
		t.Fatalf("desktopWindowOpenMsg is %d bytes, want at most 123 to fit a close frame", n)
	}
}
