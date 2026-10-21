// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// wsFrame builds one WebSocket frame; masked frames are what a browser sends.
func wsFrame(fin bool, op byte, payload []byte, mask []byte) []byte {
	var b bytes.Buffer
	first := op
	if fin {
		first |= 0x80
	}
	b.WriteByte(first)
	m := byte(0)
	if mask != nil {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b.WriteByte(m | byte(n))
	case n <= 0xffff:
		b.WriteByte(m | 126)
		_ = binary.Write(&b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(m | 127)
		_ = binary.Write(&b, binary.BigEndian, uint64(n))
	}
	if mask == nil {
		b.Write(payload)
		return b.Bytes()
	}
	b.Write(mask)
	for i, c := range payload {
		b.WriteByte(c ^ mask[i%4])
	}
	return b.Bytes()
}

// clock steps 100 ms per read, so frame times are fixed for the golden.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(100 * time.Millisecond); return c.t }

const upgrade = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// rfbHello is the server's half of an RFB handshake: the version, the one security type None and its
// result, and ServerInit for a 1024x768 desktop named "wd".
func rfbHello() []byte {
	b := append([]byte(rfbVersion8), 1, 1, 0, 0, 0, 0)
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:], 1024)
	binary.BigEndian.PutUint16(init[2:], 768)
	init[4], init[5], init[7] = 32, 24, 1
	binary.BigEndian.PutUint32(init[20:], 2)
	return append(append(b, init...), "wd"...)
}

// rect is one FramebufferUpdate rectangle header.
func rect(w, h uint16, enc int32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[4:], w)
	binary.BigEndian.PutUint16(b[6:], h)
	binary.BigEndian.PutUint32(b[8:], uint32(enc))
	return b
}

// fbu is a FramebufferUpdate of the given rectangles (header and data each).
func fbu(n uint16, rects ...[]byte) []byte {
	b := []byte{0, 0, byte(n >> 8), byte(n)}
	for _, r := range rects {
		b = append(b, r...)
	}
	return b
}

func rawRect(w, h uint16, fill byte) []byte {
	return append(rect(w, h, 0), bytes.Repeat([]byte{fill}, int(w)*int(h)*rfbBPP)...)
}

// cutText is a ServerCutText; a negative n writes an extended clipboard message of -n bytes.
func cutText(text string, n int32) []byte {
	b := []byte{rfbCutText, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[4:], uint32(n))
	return append(b, text...)
}

var bell = []byte{2}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// kept is the bytes a playback's frames carry, in order.
func kept(t *testing.T, pb RFBPlayback) []byte {
	t.Helper()
	var out []byte
	for _, f := range pb.Frames {
		_, data, _ := strings.Cut(f[1:], "{")
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

func playback(t *testing.T, r *RFBRecorder) RFBPlayback {
	t.Helper()
	doc, _ := r.Playback()
	var pb RFBPlayback
	if err := json.Unmarshal(doc, &pb); err != nil {
		t.Fatal(err)
	}
	return pb
}

// serverStream is a sandbox side: a static file on a kept-alive connection, the upgrade, the RFB
// handshake, a framebuffer update split across a continuation with a ping inside, a message with the
// desktop's clipboard between two bells, and a large update.
func serverStream() []byte {
	update := fbu(1, rawRect(1, 1, 9))
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\nvnc.html...")
	b.WriteString(upgrade)
	b.Write(wsFrame(true, 2, rfbHello(), nil))
	b.Write(wsFrame(false, 2, update[:10], nil))
	b.Write(wsFrame(true, 9, []byte("ping"), nil))
	b.Write(wsFrame(true, 0, update[10:], nil))
	b.Write(wsFrame(true, 2, cat(bell, cutText("hunter2", 7), bell), nil))
	b.Write(wsFrame(true, 2, fbu(1, rawRect(75, 1, 7)), nil))
	return b.Bytes()
}

// TestRFBRecorderReplayGolden: the stored playback for a fixed stream, fed one byte at a time and in
// one write, is testdata/rfb-playback.golden.json. WARDYN_UPDATE_GOLDEN=1 rewrites it.
func TestRFBRecorderReplayGolden(t *testing.T) {
	var docs [][]byte
	for _, chunk := range []int{1, 1 << 20} {
		c := &clock{t: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
		r := NewRFBRecorder(c.now)
		s := serverStream()
		for len(s) > 0 {
			n := min(chunk, len(s))
			_, _ = r.Write(s[:n])
			s = s[n:]
		}
		pb := playback(t, r)
		if len(pb.Frames) != 4 || pb.ClipboardOmitted != 1 || pb.StoppedMS != 0 {
			t.Fatalf("chunk %d: %d frames, %d clipboard omitted, stopped %d", chunk, len(pb.Frames), pb.ClipboardOmitted, pb.StoppedMS)
		}
		for i := range pb.Frames { // frame times depend on how often the clock is read
			_, data, _ := strings.Cut(pb.Frames[i][1:], "{")
			pb.Frames[i] = "{" + data
		}
		norm, _ := json.MarshalIndent(pb, "", "  ")
		docs = append(docs, norm)
	}
	if !bytes.Equal(docs[0], docs[1]) {
		t.Fatalf("byte-at-a-time and whole-stream playbacks differ:\n%s\n%s", docs[0], docs[1])
	}
	const golden = "testdata/rfb-playback.golden.json"
	if os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, append(docs[0], '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), docs[0]) {
		t.Fatalf("playback is not the golden:\n%s", docs[0])
	}
}

// TestRFBRecorderOmitsServerCutText: the desktop's clipboard (ServerCutText, plain and extended, one
// split across two WebSocket messages) never reaches the stored playback, and every other byte does,
// through an update with each encoding noVNC asks for, so the reader stays in step.
func TestRFBRecorderOmitsServerCutText(t *testing.T) {
	update := fbu(12,
		append(rect(4, 4, 7), 0x80, 1, 2, 3),                                              // Tight fill
		append(rect(4, 4, 7), append([]byte{0x90, 0xc8, 0x01}, make([]byte, 200)...)...),  // Tight JPEG, 2-byte length
		append(rect(8, 2, 7), 0x40, 1, 1, 1, 2, 3, 4, 5, 6, 0xaa, 0x55),                   // Tight palette of 2, raw rows
		append(rect(4, 4, 7), append([]byte{0x00, 5}, make([]byte, 5)...)...),             // Tight copy, zlib
		append(rect(16, 16, 5), 2|8|16, 1, 2, 3, 4, 1, 9, 9, 9, 9, 0, 0x11),               // Hextile, one coloured subrect
		append(rect(2, 2, 2), 0, 0, 0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 0, 0, 0, 0, 0, 1, 0, 1), // RRE, one subrect
		append(rect(2, 2, 16), 0, 0, 0, 3, 1, 2, 3),                                       // ZRLE
		append(rect(2, 2, 1), 0, 0, 0, 0),                                                 // CopyRect
		append(rect(2, 2, -239), append(make([]byte, 16), 0xc0, 0xc0)...),                 // Cursor
		append(rect(0, 0, -308), append([]byte{1, 0, 0, 0}, make([]byte, 16)...)...),      // ExtendedDesktopSize
		append(rect(0, 0, -307), 0, 0, 0, 1, 'x'),                                         // DesktopName
		rect(0, 0, -224), // LastRect
	)
	cut := cutText("hunter2", 7)
	r := NewRFBRecorder(time.Now)
	_, _ = r.Write([]byte(upgrade))
	for _, m := range [][]byte{rfbHello(), update, cat(bell, cut[:5]), cat(cut[5:], cutText("{\"secret\":1}", -12)), bell} {
		_, _ = r.Write(wsFrame(true, 2, m, nil))
	}
	_, _ = r.Write(wsFrame(true, 1, []byte(base64.StdEncoding.EncodeToString(bell)), nil)) // the base64 subprotocol
	pb := playback(t, r)
	got := kept(t, pb)
	if want := cat(rfbHello(), update, bell, bell, bell); !bytes.Equal(got, want) {
		t.Fatalf("kept %d bytes, want %d (stopped at %d)", len(got), len(want), pb.StoppedMS)
	}
	if pb.ClipboardOmitted != 2 || r.ClipboardOmitted() != 2 || bytes.Contains(got, []byte("hunter2")) || bytes.Contains(got, []byte("secret")) {
		t.Fatalf("clipboard omitted = %d, kept the clipboard: %v", pb.ClipboardOmitted, bytes.Contains(got, []byte("hunter2")))
	}
}

// TestRFBRecorderStopsOnAStreamItCannotFollow: an unknown server message stops the recording there,
// so nothing it could not place (a clipboard among it) is kept.
func TestRFBRecorderStopsOnAStreamItCannotFollow(t *testing.T) {
	r := NewRFBRecorder(time.Now)
	_, _ = r.Write([]byte(upgrade))
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	_, _ = r.Write(wsFrame(true, 2, cat([]byte{99}, cutText("hunter2", 7)), nil))
	_, _ = r.Write(wsFrame(true, 2, bell, nil))
	pb := playback(t, r)
	if pb.StoppedMS == 0 || !bytes.Equal(kept(t, pb), rfbHello()) {
		t.Fatalf("stopped %d, kept %q", pb.StoppedMS, kept(t, pb))
	}
}

// TestRFBRecorderFindsAnUpgradeAfterAChunkedBody: on a reused connection, an upgrade that follows a
// body of unknown length is still recorded.
func TestRFBRecorderFindsAnUpgradeAfterAChunkedBody(t *testing.T) {
	r := NewRFBRecorder(time.Now)
	_, _ = r.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"))
	_, _ = r.Write([]byte(upgrade[:6]))
	_, _ = r.Write([]byte(upgrade[6:]))
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	if pb := playback(t, r); !bytes.Equal(kept(t, pb), rfbHello()) {
		t.Fatalf("the upgrade after a chunked body was not recorded: %d frames", len(pb.Frames))
	}
}

// TestRFBRecorderPauseGap: while paused no frame is kept, and the gap names its start, end and reason.
func TestRFBRecorderPauseGap(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	r := NewRFBRecorder(c.now)
	_, _ = r.Write([]byte(upgrade))
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	if !r.Pause("you took over to sign in") || r.Pause("again") {
		t.Fatal("Pause should report true once")
	}
	_, _ = r.Write(wsFrame(true, 2, fbu(1, append(rect(3, 1, 0), "secretscreen"...)), nil))
	if !r.Resume() || r.Resume() {
		t.Fatal("Resume should report true once")
	}
	_, _ = r.Write(wsFrame(true, 2, bell, nil))
	pb := playback(t, r)
	if len(pb.Frames) != 2 || bytes.Contains(kept(t, pb), []byte("secretscreen")) {
		t.Fatalf("a paused frame was kept: %+v", pb)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != "you took over to sign in" || pb.Gaps[0].EndMS <= pb.Gaps[0].StartMS {
		t.Fatalf("gap = %+v", pb.Gaps)
	}
}

// pausedRecorder is a recorder past its hello, paused for a takeover.
func pausedRecorder(t *testing.T) *RFBRecorder {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	r := NewRFBRecorder(c.now)
	_, _ = r.Write([]byte(upgrade))
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	if !r.Pause("takeover") {
		t.Fatal("Pause")
	}
	return r
}

// TestRFBRecorderPauseMessageStraddlesResume: a message whose bytes arrive while paused but whose end
// arrives after Resume is not kept; a message wholly after Resume is.
func TestRFBRecorderPauseMessageStraddlesResume(t *testing.T) {
	r := pausedRecorder(t)
	frame := wsFrame(true, 2, fbu(1, append(rect(3, 1, 0), "secretscreen"...)), nil)
	_, _ = r.Write(frame[:len(frame)-1])
	if !r.Resume() {
		t.Fatal("Resume")
	}
	_, _ = r.Write(frame[len(frame)-1:])
	_, _ = r.Write(wsFrame(true, 2, fbu(1, append(rect(3, 1, 0), "afterresume!"...)), nil))
	k := kept(t, playback(t, r))
	if bytes.Contains(k, []byte("secretscreen")) {
		t.Fatal("bytes received while paused are in the recording")
	}
	if !bytes.Contains(k, []byte("afterresume!")) {
		t.Fatal("a frame wholly after Resume was not kept")
	}
}

// TestRFBRecorderPauseFieldStraddlesResume: an RFB field begun in a paused message and finished after
// Resume (here a rect header, over one message or two) is not kept, and the stream stays in step: the
// next whole frame is kept.
func TestRFBRecorderPauseFieldStraddlesResume(t *testing.T) {
	hdr := []byte{0x12, 0x34, 0x56, 0x78, 0, 3, 0, 1, 0, 0, 0, 0} // x=0x1234 y=0x5678 w=3 h=1 raw
	for name, after := range map[string][][]byte{
		"one message":  {append(append([]byte{}, hdr[6:]...), "secretscreen"...)},
		"two messages": {hdr[6:9], append(append([]byte{}, hdr[9:]...), "secretscreen"...)},
	} {
		r := pausedRecorder(t)
		_, _ = r.Write(wsFrame(true, 2, append([]byte{0, 0, 0, 1}, hdr[:6]...), nil))
		if !r.Resume() {
			t.Fatal("Resume")
		}
		for _, m := range after {
			_, _ = r.Write(wsFrame(true, 2, m, nil))
		}
		_, _ = r.Write(wsFrame(true, 2, fbu(1, append(rect(3, 1, 0), "afterresume!"...)), nil))
		k := kept(t, playback(t, r))
		if bytes.Contains(k, []byte{0x12, 0x34, 0x56, 0x78}) || bytes.Contains(k, []byte("secretscreen")) {
			t.Errorf("%s: bytes received while paused are in the recording: %x", name, k)
		}
		if !bytes.Contains(k, []byte("afterresume!")) {
			t.Errorf("%s: a frame wholly after Resume was not kept", name)
		}
	}
}

// TestRFBRecorderTruncates: past MaxRFBBytes the head is kept and the playback says it was cut.
func TestRFBRecorderTruncates(t *testing.T) {
	r := NewRFBRecorder(time.Now)
	_, _ = r.Write([]byte(upgrade))
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	big := fbu(1, rawRect(512, 512, 1))
	for range MaxRFBBytes/len(big) + 2 {
		_, _ = r.Write(wsFrame(true, 2, big, nil))
	}
	pb := playback(t, r)
	if !pb.Truncated || len(pb.Frames) == 0 || len(pb.Frames) > MaxRFBBytes/len(big)+1 {
		t.Fatalf("truncated=%v frames=%d", pb.Truncated, len(pb.Frames))
	}
}

// TestReadSideConnNeverRecordsKeystrokes: the browser writes RFB KeyEvents (and a ClientCutText)
// through the relayed connection; the recorder is given only what the sandbox sent. A recorder fed the
// written side as well fails this test.
func TestReadSideConnNeverRecordsKeystrokes(t *testing.T) {
	sandbox, relay := net.Pipe()
	r := NewRFBRecorder(time.Now)
	closed := make(chan struct{})
	conn := ReadSideConn(relay, r, func() { close(closed) })

	key := []byte{4, 1, 0, 0, 0, 0, 0, 0x61} // KeyEvent down 'a'
	cut := append([]byte{6, 0, 0, 0, 0, 0, 0, 9}, []byte("hunter2!!")...)
	mask := []byte{1, 2, 3, 4}
	hello := wsFrame(true, 2, rfbHello(), nil)
	go func() {
		_, _ = sandbox.Write([]byte(upgrade))
		_, _ = sandbox.Write(hello)
		_, _ = io.Copy(io.Discard, sandbox) // the sandbox reads what the browser typed
	}()
	buf := make([]byte, 4096)
	got := 0
	for got < len(upgrade)+len(hello) {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got += n
	}
	for _, msg := range [][]byte{wsFrame(true, 2, key, mask), wsFrame(true, 2, cut, mask)} {
		if _, err := conn.Write(msg); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	<-closed
	pb := playback(t, r)
	r.mu.Lock()
	seen := r.seen
	r.mu.Unlock()
	if seen != int64(len(upgrade)+len(hello)) || len(pb.Frames) != 1 || bytes.Contains(kept(t, pb), []byte("hunter2")) {
		t.Fatalf("the browser's side reached the recorder: it was given %d bytes, the sandbox sent %d", seen, len(upgrade)+len(hello))
	}
}
