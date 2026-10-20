// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
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

// serverStream is a sandbox side: a static file on a kept-alive connection, the upgrade, an RFB
// ProtocolVersion, a framebuffer update split across a continuation, a ping, and a large frame.
func serverStream() []byte {
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\nvnc.html...")
	b.WriteString(upgrade)
	b.Write(wsFrame(true, 2, []byte("RFB 003.008\n"), nil))
	b.Write(wsFrame(false, 2, []byte{0, 0, 0, 1}, nil))
	b.Write(wsFrame(true, 9, []byte("ping"), nil))
	b.Write(wsFrame(true, 0, []byte{0, 0, 0, 0}, nil))
	b.Write(wsFrame(true, 2, bytes.Repeat([]byte{7}, 300), nil))
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
		doc, had := r.Playback()
		if !had {
			t.Fatal("no frames kept")
		}
		var pb RFBPlayback
		if err := json.Unmarshal(doc, &pb); err != nil || len(pb.Frames) != 3 {
			t.Fatalf("chunk %d: %d frames (%v): %s", chunk, len(pb.Frames), err, doc)
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

// TestRFBRecorderPauseGap: while paused no frame is kept, and the gap names its start, end and reason.
func TestRFBRecorderPauseGap(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}
	r := NewRFBRecorder(c.now)
	_, _ = r.Write([]byte(upgrade))
	_, _ = r.Write(wsFrame(true, 2, []byte("before"), nil))
	if !r.Pause("you took over to sign in") || r.Pause("again") {
		t.Fatal("Pause should report true once")
	}
	_, _ = r.Write(wsFrame(true, 2, []byte("secret screen"), nil))
	if !r.Resume() || r.Resume() {
		t.Fatal("Resume should report true once")
	}
	_, _ = r.Write(wsFrame(true, 2, []byte("after"), nil))
	doc, _ := r.Playback()
	var pb RFBPlayback
	if err := json.Unmarshal(doc, &pb); err != nil {
		t.Fatal(err)
	}
	if len(pb.Frames) != 2 || strings.Contains(string(doc), "c2VjcmV0IHNjcmVlbg") {
		t.Fatalf("a paused frame was kept: %s", doc)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != "you took over to sign in" || pb.Gaps[0].EndMS <= pb.Gaps[0].StartMS {
		t.Fatalf("gap = %+v", pb.Gaps)
	}
}

// TestRFBRecorderTruncates: past MaxRFBBytes the head is kept and the playback says it was cut.
func TestRFBRecorderTruncates(t *testing.T) {
	r := NewRFBRecorder(time.Now)
	_, _ = r.Write([]byte(upgrade))
	big := bytes.Repeat([]byte{1}, 1<<20)
	for range MaxRFBBytes/len(big) + 2 {
		_, _ = r.Write(wsFrame(true, 2, big, nil))
	}
	doc, _ := r.Playback()
	var pb RFBPlayback
	_ = json.Unmarshal(doc, &pb)
	if !pb.Truncated || len(pb.Frames) == 0 || len(pb.Frames) > MaxRFBBytes/len(big) {
		t.Fatalf("truncated=%v frames=%d", pb.Truncated, len(pb.Frames))
	}
}

// TestReadSideConnNeverRecordsKeystrokes: the browser writes RFB KeyEvents (and a ClientCutText)
// through the relayed connection; the playback holds only what the sandbox sent. A recorder fed the
// written side as well fails this test.
func TestReadSideConnNeverRecordsKeystrokes(t *testing.T) {
	sandbox, relay := net.Pipe()
	r := NewRFBRecorder(time.Now)
	closed := make(chan struct{})
	conn := ReadSideConn(relay, r, func() { close(closed) })

	key := []byte{4, 1, 0, 0, 0, 0, 0, 0x61} // KeyEvent down 'a'
	cut := append([]byte{6, 0, 0, 0, 0, 0, 0, 9}, []byte("hunter2!!")...)
	mask := []byte{1, 2, 3, 4}
	go func() {
		_, _ = sandbox.Write([]byte(upgrade))
		_, _ = sandbox.Write(wsFrame(true, 2, []byte("RFB 003.008\n"), nil))
		_, _ = io.Copy(io.Discard, sandbox) // the sandbox reads what the browser typed
	}()
	buf := make([]byte, 4096)
	got := 0
	for got < len(upgrade)+14 {
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
	doc, _ := r.Playback()
	var pb RFBPlayback
	if err := json.Unmarshal(doc, &pb); err != nil {
		t.Fatal(err)
	}
	if len(pb.Frames) != 1 || strings.Contains(string(doc), "BAEAAAAAAGE") || strings.Contains(string(doc), "aHVudGVy") {
		t.Fatalf("the browser's side reached the recording: %s", doc)
	}
	for _, f := range pb.Frames {
		if strings.HasPrefix(f, "}") {
			t.Fatalf("a client frame was recorded: %q", f)
		}
	}
}
