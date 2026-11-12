// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/des"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A fake of Apple's Screen Sharing: RFB 3.889, VncAuth among other security types, a 1920 × 1080
// 32-bit display.

const shimPassword = "Abcd2345"

func appleServerInit() []byte {
	b := binary.BigEndian.AppendUint16(nil, 1920)
	b = binary.BigEndian.AppendUint16(b, 1080)
	b = append(b, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0) // pixel format
	b = binary.BigEndian.AppendUint32(b, 3)
	return append(b, "mac"...)
}

// reverseBits is VncAuth's key byte order, written out rather than taken from the shim.
func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = r<<1 | b>>i&1
	}
	return r
}

type appleFake struct {
	version  string
	offered  []byte
	password string // what the fake accepts
}

func (f appleFake) serve(t *testing.T, c net.Conn) {
	t.Helper()
	read := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			return nil
		}
		return b
	}
	if _, err := io.WriteString(c, f.version); err != nil {
		return
	}
	if v := read(12); string(v) != "RFB 003.008\n" {
		return
	}
	_, _ = c.Write(append([]byte{byte(len(f.offered))}, f.offered...))
	if choice := read(1); choice == nil || choice[0] != 2 {
		return
	}
	challenge := []byte("0123456789abcdef")
	_, _ = c.Write(challenge)
	resp := read(16)
	var key [8]byte
	copy(key[:], f.password)
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	ci, _ := des.NewCipher(key[:])
	got := make([]byte, 16)
	if resp != nil {
		ci.Decrypt(got[:8], resp[:8])
		ci.Decrypt(got[8:], resp[8:])
	}
	if !bytes.Equal(got, challenge) {
		_, _ = c.Write([]byte{0, 0, 0, 1, 0, 0, 0, 6, 'f', 'a', 'i', 'l', 'e', 'd'})
		return
	}
	_, _ = c.Write([]byte{0, 0, 0, 0})
	if shared := read(1); shared == nil || shared[0] != 1 {
		return
	}
	_, _ = c.Write(appleServerInit())
}

type shimRig struct {
	viewer, apple net.Conn
	done          chan struct{}
}

func newShimRig(t *testing.T, cred func(context.Context) (string, error)) *shimRig {
	t.Helper()
	return newShim(t, &macvm.RFBShim{Credential: cred})
}

func newShim(t *testing.T, shim *macvm.RFBShim) *shimRig {
	t.Helper()
	viewer, client := net.Pipe()
	guest, apple := net.Pipe()
	r := &shimRig{viewer: viewer, apple: apple, done: make(chan struct{})}
	deadline := time.Now().Add(10 * time.Second)
	_ = viewer.SetDeadline(deadline)
	_ = apple.SetDeadline(deadline)
	go func() {
		shim.ServeDesktop(client, guest)
		close(r.done)
	}()
	t.Cleanup(func() {
		_ = viewer.Close()
		_ = apple.Close()
		<-r.done
	})
	return r
}

func password(context.Context) (string, error) { return shimPassword, nil }

func readFull(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return b
}

// viewerHandshake is runnerd's side: RFB 3.8, security None, then ServerInit.
func (r *shimRig) viewerHandshake(t *testing.T) {
	t.Helper()
	if v := readFull(t, r.viewer, 12); string(v) != "RFB 003.008\n" {
		t.Fatalf("the shim's version = %q", v)
	}
	_, _ = io.WriteString(r.viewer, "RFB 003.008\n")
	if sec := readFull(t, r.viewer, 2); !bytes.Equal(sec, []byte{1, 1}) {
		t.Fatalf("security types = %v, want only None", sec)
	}
	_, _ = r.viewer.Write([]byte{1})
	if res := readFull(t, r.viewer, 4); !bytes.Equal(res, []byte{0, 0, 0, 0}) {
		t.Fatalf("SecurityResult = %v", res)
	}
	_, _ = r.viewer.Write([]byte{0}) // ClientInit, not shared: the shim chose shared upstream already
	want := appleServerInit()
	if got := readFull(t, r.viewer, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("ServerInit = %v, want Apple's %v", got, want)
	}
}

func (r *shimRig) handshake(t *testing.T) {
	t.Helper()
	go appleFake{version: "RFB 003.889\n", offered: []byte{30, 2}, password: shimPassword}.serve(t, r.apple)
	r.viewerHandshake(t)
}

// ended says the shim closed c having sent it nothing.
func ended(t *testing.T, c net.Conn, what string) {
	t.Helper()
	if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
		t.Fatalf("%s got %d bytes (%q), err %v; want the session closed with nothing sent", what, len(b), b, err)
	}
}

func be32(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }

// The shim logs in to Screen Sharing with the run's password and presents runnerd plain RFB 3.8 with
// security None and Apple's ServerInit. Then it carries the standard messages both ways unchanged,
// except SetEncodings, which it cuts to the encodings it frames.
func TestRFBShimLogsInAndRelaysPlainRFB(t *testing.T) {
	r := newShimRig(t, password)
	r.handshake(t)

	// SetEncodings: ZRLE, ExtendedDesktopSize, Raw, Extended Clipboard, QEMU key, Cursor, JPEG quality 6.
	enc := []byte{2, 0, 0, 7}
	for _, e := range []int32{16, -308, 0, -1063131698, -258, -239, -26} {
		enc = append(enc, be32(e)...)
	}
	_, _ = r.viewer.Write(enc)
	want := []byte{2, 0, 0, 4}
	for _, e := range []int32{16, 0, -239, -26} {
		want = append(want, be32(e)...)
	}
	if got := readFull(t, r.apple, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("SetEncodings reached Screen Sharing as %v, want %v", got, want)
	}
	for _, msg := range [][]byte{
		{0, 0, 0, 0, 16, 16, 0, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0}, // SetPixelFormat, 16 bits
		{3, 1, 0, 0, 0, 0, 7, 128, 4, 56},                                  // FramebufferUpdateRequest
		{4, 1, 0, 0, 0, 0, 0, 0x61},                                        // KeyEvent
		{5, 1, 0, 10, 0, 20},                                               // PointerEvent
		{6, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'},                                 // ClientCutText
	} {
		_, _ = r.viewer.Write(msg)
		if got := readFull(t, r.apple, len(msg)); !bytes.Equal(got, msg) {
			t.Fatalf("viewer message %d reached Screen Sharing as %v", msg[0], got)
		}
	}

	// FramebufferUpdate in the viewer's 16-bit format: Raw 2×1, CopyRect, ZRLE, Cursor 1×1,
	// DesktopSize, then LastRect ends it early; then Bell, SetColourMapEntries and ServerCutText.
	rect := func(w, h uint16, e int32) []byte {
		b := []byte{0, 0, 0, 0}
		b = binary.BigEndian.AppendUint16(b, w)
		b = binary.BigEndian.AppendUint16(b, h)
		return append(b, be32(e)...)
	}
	up := []byte{0, 0, 0, 9}
	up = append(append(up, rect(2, 1, 0)...), 1, 2, 3, 4)
	up = append(append(up, rect(4, 4, 1)...), 0, 1, 0, 2)
	up = append(append(up, rect(8, 8, 16)...), 0, 0, 0, 3, 'z', 'z', 'z')
	up = append(append(up, rect(1, 1, -239)...), 9, 9, 0x80)
	up = append(up, rect(1920, 1080, -223)...)
	up = append(up, rect(0, 0, -224)...)
	for _, msg := range [][]byte{up, {2}, {1, 0, 0, 0, 0, 1, 1, 2, 3, 4, 5, 6}, {3, 0, 0, 0, 0, 0, 0, 3, 'c', 'u', 't'}} {
		_, _ = r.apple.Write(msg)
		if got := readFull(t, r.viewer, len(msg)); !bytes.Equal(got, msg) {
			t.Fatalf("server message %d reached the viewer as %v, want %v", msg[0], got, msg)
		}
	}
}

// A view-only shim keeps key, pointer and paste messages from Screen Sharing, and goes on relaying
// the rest: the update request behind them is the first thing Apple's server reads.
func TestRFBShimViewOnlyKeepsInputFromTheGuest(t *testing.T) {
	r := newShim(t, &macvm.RFBShim{Credential: password, ViewOnly: true})
	r.handshake(t)
	go func() {
		for _, msg := range [][]byte{
			{4, 1, 0, 0, 0, 0, 0, 0x61},        // KeyEvent
			{5, 1, 0, 10, 0, 20},               // PointerEvent
			{6, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'}, // ClientCutText
		} {
			_, _ = r.viewer.Write(msg)
		}
		_, _ = r.viewer.Write([]byte{3, 1, 0, 0, 0, 0, 7, 128, 4, 56})
	}()
	if got := readFull(t, r.apple, 10); got[0] != 3 {
		t.Fatalf("Screen Sharing read %v first, want the update request", got)
	}
}

func TestBaseViewOnly(t *testing.T) {
	for _, tc := range []struct {
		manifest string
		want     bool
	}{
		{`{"format":FMT,"state":"ready","desktop":"control"}`, false},
		{`{"format":FMT,"state":"ready","desktop":"observe"}`, true},
		{`{"format":FMT,"state":"ready","desktop":"none"}`, true},
		{`{"format":FMT,"state":"ready"}`, true},
		{`not json`, true},
		{"", true}, // no manifest
	} {
		const runID = "0a1b2c3d-4e5f-4061-8273-8495a6b7c8d9"
		base, run := t.TempDir(), macvm.Layout{Root: t.TempDir()}.RunDir(runID)
		if tc.manifest != "" {
			if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(strings.ReplaceAll(tc.manifest, "FMT", strconv.Itoa(macvm.BaseFormat))), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := macvm.WriteRunConfig(run, macvm.RunConfig{RunID: runID, BaseDir: base}); err != nil {
			t.Fatal(err)
		}
		if got := macvm.BaseViewOnly(run); got != tc.want {
			t.Errorf("manifest %q: view only %v, want %v", tc.manifest, got, tc.want)
		}
	}
	if !macvm.BaseViewOnly(t.TempDir()) {
		t.Error("a run directory with no run.json is not view only")
	}
}

// Nothing reaches runnerd until the shim holds the run's credential and Apple's server has accepted
// it: a run without a credential (its desktop is off), a server older than RFB 3.8, one that offers no
// VncAuth, and a refused password each close the viewer with no handshake at all.
func TestRFBShimRefusesBeforeTheViewersHandshake(t *testing.T) {
	for name, tc := range map[string]struct {
		cred  func(context.Context) (string, error)
		apple *appleFake
	}{
		"no credential":    {cred: func(context.Context) (string, error) { return "", errors.New("404 Not Found") }},
		"empty credential": {cred: func(context.Context) (string, error) { return "", nil }},
		"RFB 3.3 server":   {cred: password, apple: &appleFake{version: "RFB 003.003\n", offered: []byte{2}, password: shimPassword}},
		"no VncAuth":       {cred: password, apple: &appleFake{version: "RFB 003.889\n", offered: []byte{30, 35}, password: shimPassword}},
		"wrong password":   {cred: password, apple: &appleFake{version: "RFB 003.889\n", offered: []byte{2}, password: "Zzzz9999"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShimRig(t, tc.cred)
			if tc.apple == nil {
				ended(t, r.apple, "Screen Sharing") // the shim never spoke to it
			} else {
				go tc.apple.serve(t, r.apple)
			}
			ended(t, r.viewer, "the viewer")
		})
	}
}

// A message type the shim does not frame ends the session, from either side, and none of it crosses:
// a server message Apple's server might add, and a viewer's SetDesktopSize.
func TestRFBShimEndsTheSessionOnAMessageItDoesNotKnow(t *testing.T) {
	r := newShimRig(t, password)
	r.handshake(t)
	_, _ = r.apple.Write([]byte{0x7f, 1, 2, 3})
	ended(t, r.viewer, "the viewer")

	r = newShimRig(t, password)
	r.handshake(t)
	_, _ = r.viewer.Write([]byte{251, 0, 7, 128, 4, 56, 1, 0})
	ended(t, r.apple, "Screen Sharing")

	// A rectangle in an encoding the viewer was never let ask for ends it too, before any of its update.
	r = newShimRig(t, password)
	r.handshake(t)
	_, _ = r.apple.Write(append([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1}, be32(-308)...))
	ended(t, r.viewer, "the viewer")
}

// Each run's password is 8 characters (all VncAuth keys on) from crypto/rand, and the secret prints
// redacted however it is formatted.
func TestNewDesktopSecret(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		s, err := macvm.NewDesktopSecret()
		if err != nil {
			t.Fatal(err)
		}
		pw, err := s.Credential(context.Background())
		if err != nil || len(pw) != 8 {
			t.Fatalf("password of %d characters, %v", len(pw), err)
		}
		for _, c := range pw {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '2' && c <= '9') {
				t.Fatalf("password has %q, outside the printable alphabet", c)
			}
		}
		for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%+v %#v %s", s, s, s)} {
			if strings.Contains(out, pw) {
				t.Fatalf("the secret printed as %q", out)
			}
		}
		if b, _ := json.Marshal(s); strings.Contains(string(b), pw) {
			t.Fatalf("the secret marshalled as %s", b)
		}
		seen[pw] = true
	}
	if len(seen) < 49 {
		t.Errorf("50 passwords made %d distinct", len(seen))
	}
}

// A full RFB client against a view-only shim with no gateway in front: every byte Screen Sharing reads
// after the handshake is a pixel format, encodings or an update request, whether the messages come whole
// or a byte at a time. The same client against a shim that is not view only delivers all of it, so this
// recording would show a leak.
func TestRFBClientViewOnlyShimPassesOnlyReadRequests(t *testing.T) {
	pixelFormat := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	encodings := []byte{2, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 16, 0xc0, 0xa1, 0xe5, 0xce} // raw, ZRLE, Extended Clipboard
	kept := []byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 16}
	update := []byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}
	input := [][]byte{
		{4, 1, 0, 0, 0, 0, 0, 'a'},
		{5, 1, 0, 10, 0, 20},
		{6, 0, 0, 0, 0, 0, 0, 5, 'p', 'a', 's', 't', 'e'},
		{4, 0, 0, 0, 0, 0, 0, 'a'},
		{5, 0, 0, 11, 0, 21},
	}
	lastUpdate := []byte{3, 1, 0, 0, 0, 0, 7, 128, 4, 56}
	var sent, read, all []byte
	sent = append(append(append(sent, pixelFormat...), encodings...), update...)
	read = append(append(append(read, pixelFormat...), kept...), update...)
	all = append(all, read...)
	for _, msg := range input {
		sent, all = append(sent, msg...), append(all, msg...)
	}
	sent = append(sent, lastUpdate...)
	read, all = append(read, lastUpdate...), append(all, lastUpdate...)

	for _, tc := range []struct {
		name     string
		viewOnly bool
		chunk    int
		want     []byte
	}{
		{"view only, whole", true, len(sent), read},
		{"view only, a byte at a time", true, 1, read},
		{"with control, whole", false, len(sent), all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newShim(t, &macvm.RFBShim{Credential: password, ViewOnly: tc.viewOnly})
			r.handshake(t)
			got := rfbClientRecordGuest(r)
			for rest := sent; len(rest) > 0; rest = rest[min(tc.chunk, len(rest)):] {
				if _, err := r.viewer.Write(rest[:min(tc.chunk, len(rest))]); err != nil {
					t.Fatal(err)
				}
			}
			_ = r.viewer.Close()
			if b := <-got; !bytes.Equal(b, tc.want) {
				t.Fatalf("Screen Sharing read %v, want %v", b, tc.want)
			}
		})
	}
}

// A view-only shim ends the session on a control message it never relays, and on the extended clipboard
// form, which a plain text paste is not: nothing of it or after it reaches Screen Sharing.
func TestRFBClientViewOnlyShimEndsTheSessionOnAnythingElse(t *testing.T) {
	update := []byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}
	for name, msg := range map[string][]byte{
		"xvp":                {250, 0, 1, 2},
		"SetDesktopSize":     {251, 0, 7, 128, 4, 56, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7, 128, 4, 56, 0, 0, 0, 0},
		"QEMU extended key":  {255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 30},
		"extended clipboard": {6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfb, 'p', 'a', 's', 't', 'e'},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShim(t, &macvm.RFBShim{Credential: password, ViewOnly: true})
			r.handshake(t)
			got := rfbClientRecordGuest(r)
			if _, err := r.viewer.Write(update); err != nil {
				t.Fatal(err)
			}
			go func() { _, _ = r.viewer.Write(append(append([]byte(nil), msg...), update...)) }()
			if b := <-got; !bytes.Equal(b, update) {
				t.Fatalf("Screen Sharing read %v, want only the first update request", b)
			}
			ended(t, r.viewer, "the viewer")
		})
	}
}

// rfbClientRecordGuest records every byte Screen Sharing reads from here on, and hands them over when
// the shim closes its side.
func rfbClientRecordGuest(r *shimRig) <-chan []byte {
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r.apple)
		got <- b
	}()
	return got
}

// A forced session drops the guest's clipboard on its way to the viewer and carries what follows it. A
// run on a view-only base that isn't forced keeps its cut text, as it always has.
func TestRFBShimForcedKeepsCutTextFromTheViewer(t *testing.T) {
	cut := []byte{3, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'}
	for name, shim := range map[string]*macvm.RFBShim{
		"forced":    {Credential: password, ViewOnly: true, ForceViewOnly: true},
		"view only": {Credential: password, ViewOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShim(t, shim)
			r.handshake(t)
			go func() {
				_, _ = r.apple.Write(cut)
				_, _ = r.apple.Write([]byte{2}) // Bell
			}()
			if shim.ForceViewOnly {
				if got := readFull(t, r.viewer, 1); got[0] != 2 {
					t.Fatalf("the viewer read %v, want the bell with no cut text before it", got)
				}
				return
			}
			if got := readFull(t, r.viewer, len(cut)+1); !bytes.Equal(got, append(cut[:len(cut):len(cut)], 2)) {
				t.Fatalf("the viewer read %v, want the cut text and the bell", got)
			}
		})
	}
}

// The report is what a session's filter does: both halves, so a base that is merely view only, or a
// setting the input filter didn't take, isn't forced.
func TestRFBShimDesktopForcedViewOnly(t *testing.T) {
	for _, c := range []struct{ viewOnly, force, want bool }{
		{true, true, true},
		{true, false, false},
		{false, true, false},
		{false, false, false},
	} {
		s := &macvm.RFBShim{ViewOnly: c.viewOnly, ForceViewOnly: c.force}
		if got := s.DesktopForcedViewOnly(); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}
