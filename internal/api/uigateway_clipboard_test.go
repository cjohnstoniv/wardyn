// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func wsFrame(payload []byte, masked bool) []byte {
	b1 := byte(0)
	if masked {
		b1 = 0x80
	}
	out := []byte{0x82}
	if n := len(payload); n < 126 {
		out = append(out, b1|byte(n))
	} else {
		out = binary.BigEndian.AppendUint16(append(out, b1|126), uint16(n))
	}
	if !masked {
		return append(out, payload...)
	}
	key := []byte{1, 2, 3, 4}
	out = append(out, key...)
	for i, c := range payload {
		out = append(out, c^key[i&3])
	}
	return out
}

func readWSFrame(r *bufio.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var l [2]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint16(l[:]))
	}
	var key [4]byte
	if h[1]&0x80 != 0 {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return nil, err
		}
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	if h[1]&0x80 != 0 {
		for i := range p {
			p[i] ^= key[i&3]
		}
	}
	return p, nil
}

func rfbCut(typ byte, text string) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{typ, 0, 0, 0}, uint32(len(text))), text...)
}

func rfbServerInit() []byte {
	b := binary.BigEndian.AppendUint16(nil, 1280)
	b = binary.BigEndian.AppendUint16(b, 800)
	b = append(b, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0)
	return append(binary.BigEndian.AppendUint32(b, 4), "desk"...)
}

const (
	copySentinel  = "SENTINEL-copy-out-41c2"
	pasteSentinel = "SENTINEL-paste-in-93d0"
)

// rfbDesktopBackend is a desktop in miniature: websockify's 101, an RFB 3.8 handshake, then one
// ServerCutText and a Bell. It reports the upgrade request's extension offer and what the relay
// let through as the browser's paste.
func rfbDesktopBackend(t *testing.T, extensionOffered chan<- string, pasted chan<- []byte, answerExtension bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extensionOffered <- r.Header.Get("Sec-WebSocket-Extensions")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		head := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
		if answerExtension {
			head += "Sec-WebSocket-Extensions: permessage-deflate\r\n"
		}
		if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
			return
		}
		br := bufio.NewReader(conn)
		for _, msg := range [][]byte{[]byte("RFB 003.008\n"), {1, 1}, {0, 0, 0, 0}} {
			if _, err := conn.Write(wsFrame(msg, false)); err != nil {
				return
			}
			if _, err := readWSFrame(br); err != nil {
				return
			}
		}
		_, _ = conn.Write(append(append(wsFrame(rfbServerInit(), false), wsFrame(rfbCut(3, copySentinel), false)...), wsFrame([]byte{2}, false)...))
		p, err := readWSFrame(br)
		if err == nil {
			pasted <- p
		}
	})
}

type desktopSession struct {
	br   *bufio.Reader
	conn net.Conn
	resp *http.Response
}

// openDesktop runs the browser's half: the upgrade (offering compression, as browsers do) and the
// RFB handshake. It stops at the first normal-phase frame for the test to read.
func openDesktop(t *testing.T, h *uiHarness) desktopSession {
	t.Helper()
	gw := httptest.NewServer(h.gateway)
	t.Cleanup(gw.Close)
	cookie := h.openSession()
	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := http.NewRequest(http.MethodGet, gw.URL+uiRunPrefix+h.run.ID.String()+"/code/websockify", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits")
	req.AddCookie(cookie)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	ds := desktopSession{br: br, conn: conn, resp: resp}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return ds
	}
	for _, reply := range [][]byte{[]byte("RFB 003.008\n"), {1}, {1}} {
		if _, err := readWSFrame(br); err != nil {
			t.Fatalf("handshake read: %v", err)
		}
		if _, err := conn.Write(wsFrame(reply, true)); err != nil {
			t.Fatalf("handshake write: %v", err)
		}
	}
	if init, err := readWSFrame(br); err != nil || !bytes.Equal(init, rfbServerInit()) {
		t.Fatalf("ServerInit through the relay: %x %v", init, err)
	}
	return ds
}

func clipboardRows(t *testing.T, h *uiHarness, n int) []types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := append(h.audit.auditRows("ui.clipboard", "denied"), h.audit.auditRows("ui.clipboard", "success")...)
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func digest(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }

// The relay under the default rules: the desktop's copy never reaches the browser, the browser's
// paste reaches the desktop, the extension offer never reaches the sandbox, and both transfers are
// audited by direction, size and hash with neither text in any audit row.
func TestUIGateway_ClipboardDefaultsBlockCopyOutAndAuditBoth(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := newUIHarness(t, rfbDesktopBackend(t, offered, pasted, false))
	ds := openDesktop(t, h)
	if got := <-offered; got != "" {
		t.Fatalf("the sandbox saw a WebSocket extension offer %q", got)
	}
	if next, err := readWSFrame(ds.br); err != nil || !bytes.Equal(next, []byte{2}) {
		t.Fatalf("after ServerInit the browser got %q (err %v), want only the Bell: the copy out must be dropped", next, err)
	}
	paste := rfbCut(6, pasteSentinel)
	if _, err := ds.conn.Write(wsFrame(paste, true)); err != nil {
		t.Fatalf("paste write: %v", err)
	}
	select {
	case got := <-pasted:
		if !bytes.Equal(got, paste) {
			t.Fatalf("the desktop got %x, want the paste %x", got, paste)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an allowed paste never reached the desktop")
	}
	rows := clipboardRows(t, h, 2)
	want := map[string]struct {
		outcome string
		bytes   int
		sum     string
	}{
		"copy_out": {"denied", len(copySentinel), digest(copySentinel)},
		"paste_in": {"success", len(pasteSentinel), digest(pasteSentinel)},
	}
	if len(rows) != 2 {
		t.Fatalf("ui.clipboard rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		var d struct {
			Direction string `json:"direction"`
			Bytes     int    `json:"bytes"`
			SHA256    string `json:"sha256"`
		}
		_ = json.Unmarshal(row.Data, &d)
		w := want[d.Direction]
		if row.Outcome != w.outcome || d.Bytes != w.bytes || d.SHA256 != w.sum || row.Target != d.Direction {
			t.Errorf("ui.clipboard row %s/%s %s, want %+v", row.Outcome, row.Target, row.Data, w)
		}
	}
	h.audit.mu.Lock()
	all, _ := json.Marshal(h.audit.events)
	h.audit.mu.Unlock()
	if bytes.Contains(all, []byte(copySentinel)) || bytes.Contains(all, []byte(pasteSentinel)) {
		t.Fatal("clipboard content reached an audit row")
	}
}

// The knob is what the relay reads: a run whose policy opens copy out gets the desktop's copy.
func TestUIGateway_ClipboardFollowsTheRunsDesktopRules(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := newUIHarness(t, rfbDesktopBackend(t, offered, pasted, false))
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		UIApps:              []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}},
		Desktop:             &types.DesktopControls{CopyOut: types.TransferAllowed, PasteIn: types.TransferBlocked},
	})
	ds := openDesktop(t, h)
	<-offered
	if next, err := readWSFrame(ds.br); err != nil || !strings.Contains(string(next), copySentinel) {
		t.Fatalf("an opened copy out did not reach the browser: %q %v", next, err)
	}
	if rows := clipboardRows(t, h, 1); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("ui.clipboard rows = %+v", rows)
	}
}

// A sandbox that answers with an extension the relay never offered is refused before any byte flows.
func TestUIGateway_ClipboardRefusesANegotiatedExtension(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := newUIHarness(t, rfbDesktopBackend(t, offered, pasted, true))
	ds := openDesktop(t, h)
	<-offered
	if ds.resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upgrade answered with an extension: %d, want 502", ds.resp.StatusCode)
	}
}
