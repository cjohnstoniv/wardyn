// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A desktop recording is the sandbox-to-browser half of one relayed noVNC connection: the RFB
// server's messages, as noVNC's playback reads them ("{<ms>{<base64>", tests/playback.js), plus the
// gaps a pause left. The browser-to-sandbox half carries the person's keys, pointer and clipboard and
// is never read: ReadSideConn tees Read only, and RFBRecorder has no other input. So keystrokes are
// never recorded, by construction rather than by filtering. The sandbox's half carries the desktop's
// clipboard too (ServerCutText, which can hold a password copied inside the desktop): rfbStream follows
// the messages so the recording leaves those out, and counts them.

// MaxRFBBytes bounds one desktop recording's RFB payload. Past it the recording keeps its head and
// says it was truncated, like a console session's cast.
const MaxRFBBytes = 32 << 20

// maxHTTPHeader bounds the HTTP response header read before the WebSocket starts.
const maxHTTPHeader = 16 << 10

// RFBGap is a stretch with no frames: a pause, from its start to its end, in ms since the recording
// started. EndMS is 0 while the pause lasts.
type RFBGap struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms,omitempty"`
	Reason  string `json:"reason"`
}

// RFBPlayback is the stored document: noVNC playback frames plus the gaps.
type RFBPlayback struct {
	Version   int       `json:"version"`
	Format    string    `json:"format"`
	Encoding  string    `json:"encoding"`
	StartedAt time.Time `json:"started_at"`
	Frames    []string  `json:"frames"`
	Gaps      []RFBGap  `json:"gaps,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	// ClipboardOmitted counts the desktop's clipboard messages (ServerCutText) left out of the frames;
	// the browser still received them.
	ClipboardOmitted int `json:"clipboard_omitted,omitempty"`
	// StoppedMS is when the stream stopped being one the recorder could follow; nothing after it is kept.
	StoppedMS int64 `json:"stopped_ms,omitempty"`
}

type rfbState int

const (
	rfbHeader rfbState = iota // reading an HTTP response header
	rfbBody                   // skipping a sized HTTP body
	rfbFrames                 // reading WebSocket frames after a 101
	rfbScan                   // a body of unknown length: looking for the next upgrade response
)

// RFBRecorder turns the sandbox side of a relayed noVNC connection into an RFBPlayback. Write never
// fails and never blocks on anything but its own mutex: the relay's bytes go on whatever it does.
type RFBRecorder struct {
	mu    sync.Mutex
	now   func() time.Time
	start time.Time
	state rfbState
	hdr   []byte
	skip  int64

	fh      []byte // the WebSocket frame header being read
	left    uint64 // payload bytes left in the current frame
	inFrame bool
	opcode  byte // the current message's opcode (a continuation keeps the first frame's)
	ctl     bool // the current frame is a control frame, whose payload is not the message's
	fin     bool
	mask    [4]byte
	masked  bool
	maskAt  int
	msg     []byte

	stream  *rfbStream // the RFB stream after a 101
	kept    []byte
	stopped int64
	seen    int64 // every byte Write was given

	frames    []string
	size      int
	truncated bool
	gaps      []RFBGap
	paused    bool
	// tainted: the message being read holds bytes that arrived while paused (or the stream carries a
	// field begun then), so it is not kept even if it ends after Resume. The parser still follows it.
	tainted bool
}

// NewRFBRecorder starts a recording at now().
func NewRFBRecorder(now func() time.Time) *RFBRecorder {
	return &RFBRecorder{now: now, start: now()}
}

func (r *RFBRecorder) ms() int64 { return r.now().Sub(r.start).Milliseconds() }

// Write takes bytes read from the sandbox. It always reports them all taken.
func (r *RFBRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	r.seen += int64(n)
	for len(p) > 0 {
		switch r.state {
		case rfbHeader:
			p = r.readHeader(p)
		case rfbBody:
			k := min(int64(len(p)), r.skip)
			r.skip -= k
			p = p[k:]
			if r.skip == 0 {
				r.state = rfbHeader
			}
		case rfbFrames:
			p = r.readFrames(p)
		default:
			p = r.scan(p)
		}
	}
	return n, nil
}

// upgradeStatus starts the response a recording waits for after a body it cannot measure.
var upgradeStatus = []byte("HTTP/1.1 101")

// scan looks for the next upgrade response after a body of unknown length (chunked, or a header past
// maxHTTPHeader), so an upgrade on a reused connection is still recorded. A body that happens to hold
// the same bytes starts a stream the RFB reader then refuses, which keeps nothing.
func (r *RFBRecorder) scan(p []byte) []byte {
	buf := append(r.hdr, p...)
	if i := bytes.Index(buf, upgradeStatus); i >= 0 {
		r.state, r.hdr = rfbHeader, nil
		return buf[i:]
	}
	r.hdr = append([]byte(nil), buf[max(0, len(buf)-len(upgradeStatus)+1):]...)
	return nil
}

// readHeader collects one HTTP response header. A 101 switches to frames; a sized body is skipped to
// the next response on a kept-alive connection; any other body is scanned for the next upgrade.
func (r *RFBRecorder) readHeader(p []byte) []byte {
	r.hdr = append(r.hdr, p...)
	end := bytes.Index(r.hdr, []byte("\r\n\r\n"))
	if end < 0 {
		if len(r.hdr) > maxHTTPHeader {
			r.state, r.hdr = rfbScan, nil
		}
		return nil
	}
	rest := r.hdr[end+4:]
	head := string(r.hdr[:end])
	r.hdr = nil
	status, fields, _ := strings.Cut(head, "\r\n")
	if parts := strings.Fields(status); len(parts) >= 2 && parts[1] == "101" {
		r.state, r.stream = rfbFrames, newRFBStream()
		return rest
	}
	r.state = rfbScan
	for line := range strings.SplitSeq(fields, "\r\n") {
		k, v, _ := strings.Cut(line, ":")
		if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
				r.state, r.skip = rfbBody, n
			}
		}
		if strings.EqualFold(strings.TrimSpace(k), "Transfer-Encoding") {
			r.state = rfbScan
			break
		}
	}
	if r.state == rfbBody && r.skip == 0 {
		r.state = rfbHeader
	}
	return rest
}

// readFrames consumes WebSocket frames, keeping each complete data message.
func (r *RFBRecorder) readFrames(p []byte) []byte {
	for len(p) > 0 {
		if !r.inFrame {
			r.fh = append(r.fh, p[0])
			p = p[1:]
			if !r.frameHeader() {
				continue
			}
			if r.left == 0 {
				r.endFrame()
			}
			continue
		}
		k := min(uint64(len(p)), r.left)
		chunk := p[:k]
		p = p[k:]
		r.left -= k
		if !r.ctl && !r.truncated {
			for _, b := range chunk {
				if r.masked {
					b ^= r.mask[r.maskAt%4]
					r.maskAt++
				}
				r.msg = append(r.msg, b)
			}
			r.tainted = r.tainted || r.paused
			if r.size+len(r.msg) > MaxRFBBytes {
				r.truncated, r.msg = true, nil
			}
		}
		if r.left == 0 {
			r.endFrame()
		}
	}
	return nil
}

// frameHeader parses r.fh once it holds a whole frame header, and reports whether it did.
func (r *RFBRecorder) frameHeader() bool {
	if len(r.fh) < 2 {
		return false
	}
	need := 2
	l := uint64(r.fh[1] & 0x7f)
	switch l {
	case 126:
		need += 2
	case 127:
		need += 8
	}
	masked := r.fh[1]&0x80 != 0
	if masked {
		need += 4
	}
	if len(r.fh) < need {
		return false
	}
	switch l {
	case 126:
		l = uint64(binary.BigEndian.Uint16(r.fh[2:4]))
	case 127:
		l = binary.BigEndian.Uint64(r.fh[2:10])
	}
	op := r.fh[0] & 0x0f
	r.fin = r.fh[0]&0x80 != 0
	r.ctl = op >= 8
	if op != 0 && !r.ctl {
		r.opcode = op
	}
	r.masked = masked
	if masked {
		copy(r.mask[:], r.fh[need-4:need])
	}
	r.maskAt, r.left, r.inFrame, r.fh = 0, l, true, r.fh[:0]
	return true
}

// endFrame closes a frame; a final data frame ends its message. The RFB reader keeps the message's
// bytes except ServerCutText, and they are a frame unless paused.
func (r *RFBRecorder) endFrame() {
	r.inFrame = false
	if r.ctl || !r.fin {
		return
	}
	payload := r.msg
	if r.opcode == 1 { // websockify's base64 subprotocol sends base64 text
		dec, err := base64.StdEncoding.DecodeString(string(r.msg))
		if err != nil {
			r.stream.fail()
		}
		payload = dec
	}
	r.kept = r.kept[:0]
	r.stream.feed(payload, func(b []byte) { r.kept = append(r.kept, b...) })
	if r.stream.failed && r.stopped == 0 {
		r.stopped = max(r.ms(), 1)
	}
	if len(r.kept) > 0 && !r.paused && !r.tainted && !r.truncated {
		r.frames = append(r.frames, "{"+strconv.FormatInt(r.ms(), 10)+"{"+base64.StdEncoding.EncodeToString(r.kept))
		r.size += len(r.kept)
	}
	r.msg, r.opcode = r.msg[:0], 0
	// A paused field the stream still carries keeps the next message tainted too.
	r.tainted = r.tainted && len(r.stream.buf) > 0
}

// ClipboardOmitted is how many clipboard messages the recording left out so far.
func (r *RFBRecorder) ClipboardOmitted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stream == nil {
		return 0
	}
	return r.stream.cutTexts
}

// Pause stops keeping frames and opens a gap; it reports false when already paused.
func (r *RFBRecorder) Pause(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return false
	}
	r.paused = true
	r.gaps = append(r.gaps, RFBGap{StartMS: r.ms(), Reason: reason})
	return true
}

// Resume keeps frames again and closes the open gap; it reports false when not paused.
func (r *RFBRecorder) Resume() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		return false
	}
	r.paused = false
	// A message or RFB field already begun holds paused bytes: the message that ends it is not kept.
	r.tainted = r.tainted || r.inFrame || len(r.msg) > 0 || (r.stream != nil && len(r.stream.buf) > 0)
	r.gaps[len(r.gaps)-1].EndMS = max(r.ms(), 1)
	return true
}

// Playback is the recording so far. HadFrames is false when nothing worth storing was seen.
func (r *RFBRecorder) Playback() (doc []byte, hadFrames bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pb := RFBPlayback{Version: 1, Format: "novnc-playback", Encoding: "base64", StartedAt: r.start.UTC(),
		Frames: append([]string{}, r.frames...), Gaps: append([]RFBGap(nil), r.gaps...), Truncated: r.truncated,
		StoppedMS: r.stopped}
	if r.stream != nil {
		pb.ClipboardOmitted = r.stream.cutTexts
	}
	if r.paused {
		pb.Gaps[len(pb.Gaps)-1].EndMS = max(r.ms(), 1)
	}
	b, _ := json.Marshal(pb)
	return b, len(r.frames) > 0
}

// ReadSideConn tees every byte read from c (the sandbox side) into rec, and runs onClose once when
// the connection closes. Write is c's own: what the browser sends is never seen here.
func ReadSideConn(c net.Conn, rec *RFBRecorder, onClose func()) net.Conn {
	return &readSideConn{Conn: c, rec: rec, onClose: onClose}
}

type readSideConn struct {
	net.Conn
	rec     *RFBRecorder
	once    sync.Once
	onClose func()
}

func (c *readSideConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		_, _ = c.rec.Write(p[:n])
	}
	return n, err
}

func (c *readSideConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}
