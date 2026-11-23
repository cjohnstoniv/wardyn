// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

// Local window: the person's CLI on the runner's own computer shows a Linux VM run's display in a window
// of its own. The CLI asks the runner's socket for a window nonce (LocalHelloMsg.Kind LocalKindWindow);
// the display is dialled only when the organisation sends local_window naming that nonce. The runner
// splices the socket to the display through rfbfilter with no clipboard rules, so cut text is dropped both
// ways, and answers with a local_window stream that carries no display bytes, only the one end line.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// LocalReasonDisplayRefused is the LocalEnd reason of a window whose display filter refused the stream: the
// display did not speak RFB, or either side sent what the filter cannot follow.
const LocalReasonDisplayRefused = "display_refused"

// localWindowSession is one granted window: the CLI's connection, the filtered display and the org's
// stream. It ends once, at the first of the CLI, the display, the filter, the org or the link.
type localWindowSession struct {
	c       *localClient
	st      *runnerwire.Stream
	peer    *runnerwire.Peer
	display *rfbfilter.Conn
	cancel  context.CancelFunc
	once    sync.Once
	dropped atomic.Int64 // cut-text messages the filter dropped

	mu      sync.Mutex
	refusal string // guarded by mu: what the filter refused first, in its fixed words
}

// localWindow is MethodLocalWindow: the organisation's grant for the connection holding args.Nonce. The
// nonce is spent by this call whatever it answers.
func (s *Server) localWindow(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.LocalWindowArgs](raw)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c := s.nonces[args.Nonce]
	delete(s.nonces, args.Nonce)
	s.mu.Unlock()
	if c == nil {
		return nil, runnerwire.Refuse("unknown or expired local window nonce")
	}
	close(c.taken)
	if c.kind != LocalKindWindow { // a terminal's nonce never opens a window
		c.close(LocalReasonWrongKind)
		return nil, runnerwire.Refuse("the local window nonce is a terminal's, not a window's")
	}
	if c.ref != ref { // the run named on the socket must be the run the org's grant is for
		c.close(LocalReasonUnavailable)
		return nil, runnerwire.Refuse("the run on the local socket is not the run the organisation granted a window onto")
	}
	s.mu.Lock()
	resolve := s.attachRef
	s.mu.Unlock()
	if resolve == nil || resolve(c.runID) != ref { // the run may have become one this runner refuses since the hello
		c.close(LocalReasonNotHere)
		return nil, runnerwire.Refuse(LocalReasonNotHere)
	}
	if err := s.held(ref); err != nil {
		c.close(LocalReasonNotHere)
		return nil, err
	}
	dd, ok := s.sub.(runner.DesktopDialer)
	if !ok {
		c.close(LocalReasonAttach)
		return nil, runner.ErrDesktopUnsupported
	}
	life, cancel := context.WithCancel(s.life(peer))
	conn, err := dd.DialDesktop(life, ref)
	if err != nil {
		cancel()
		c.close(LocalReasonAttach)
		return nil, err
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindLocalWindow, Target: ref, Call: id})
	if err != nil {
		cancel()
		_ = conn.Close()
		c.close(LocalReasonAttach)
		return nil, err
	}
	w := &localWindowSession{c: c, st: st, peer: peer, cancel: cancel}
	w.display = rfbfilter.Wrap(&wsFramed{raw: conn, wsUnframer: wsUnframer{w: conn}}, rfbfilter.Rules{}, w.audit, w.refused)
	// The window's life is the link's: when the organisation link drops, the CLI is told and the socket
	// closes. end cancels life too, and then this finds the window already ended.
	context.AfterFunc(life, func() { w.end(LocalReasonLinkLost, "") })
	if err := c.writeJSON(LocalBound, LocalBoundMsg{}); err != nil {
		w.end(LocalReasonClosed, "")
		return nil, runnerwire.Refuse("the local window client went away")
	}
	go w.output()
	go w.input()
	go w.watchOrg()
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

// audit counts the cut-text messages the filter dropped; with no clipboard rules it drops every one.
func (w *localWindowSession) audit(ev rfbfilter.Event) {
	if ev.Input == "" && !ev.Allowed {
		w.dropped.Add(1)
	}
}

// refused records what the filter refused and ends the window. A refusal before the display spoke RFB
// carries no fixed words of the filter's, so it reads as a display that is not RFB.
func (w *localWindowSession) refused(err error) {
	kind := runnerwire.LocalWindowNotRFB
	var r *rfbfilter.Refusal
	if errors.As(err, &r) {
		kind = r.Kind
	}
	w.mu.Lock()
	if w.refusal == "" {
		w.refusal = kind
	}
	w.mu.Unlock()
	w.end(LocalReasonDisplayRefused, runnerwire.LocalWindowEndRefused)
}

// output carries the display's bytes, filtered, to the CLI. The display closing ends the window as the
// run's end; a refusal has already ended it.
func (w *localWindowSession) output() {
	out := &wsUnframer{w: localDataWriter{w.c}}
	buf := make([]byte, 32<<10)
	for {
		n, err := w.display.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				w.end(LocalReasonClosed, runnerwire.LocalWindowEndPerson)
				return
			}
		}
		if err != nil {
			w.end(LocalReasonRunEnded, runnerwire.LocalWindowEndRunEnded)
			return
		}
	}
}

// input carries the CLI's bytes to the display through the filter. The CLI closing its connection is the
// person closing the window; any frame but data breaks the protocol.
func (w *localWindowSession) input() {
	for f := range w.c.in {
		if f.typ != LocalData {
			w.end(LocalReasonProtocol, runnerwire.LocalWindowEndHelperFailed)
			return
		}
		if _, err := w.display.Write(appendWSBinary(nil, f.b)); err != nil {
			w.end(LocalReasonRunEnded, runnerwire.LocalWindowEndRunEnded)
			return
		}
	}
	w.end(LocalReasonClosed, runnerwire.LocalWindowEndPerson)
}

// watchOrg ends the window when the org resets or closes its stream, or the link carrying it drops.
func (w *localWindowSession) watchOrg() {
	_, _ = io.Copy(io.Discard, w.st) // the org never writes the stream
	reason := LocalReasonOrgEnded
	select {
	case <-w.peer.Done():
		reason = LocalReasonLinkLost
	default:
	}
	w.end(reason, "")
}

// end ends the window once: the display first, then the CLI with cliReason, then the stream. With line set
// the window ended first and the org is told why (LocalWindowEnd) before the stream is half-closed;
// otherwise the stream is closed (the org ended it, or its link is gone).
func (w *localWindowSession) end(cliReason, line string) {
	w.once.Do(func() {
		w.cancel()
		_ = w.display.Close()
		w.c.close(cliReason)
		if line == "" {
			_ = w.st.Close()
			return
		}
		w.mu.Lock()
		kind := w.refusal
		w.mu.Unlock()
		b, _ := json.Marshal(runnerwire.LocalWindowEnd{Reason: line, RefusalKind: kind, CutTextDropped: int(w.dropped.Load())})
		if _, err := w.st.Write(append(b, '\n')); err != nil {
			_ = w.st.Reset(runnerwire.ResetInternal)
			return
		}
		_ = w.st.CloseWrite()
	})
}

// localDataWriter writes the display's bytes to the CLI as LocalData frames of at most MaxLocalFrame.
type localDataWriter struct{ c *localClient }

func (d localDataWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += MaxLocalFrame {
		if err := d.c.write(LocalData, p[off:min(off+MaxLocalFrame, len(p))]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// rfbfilter follows RFB inside WebSocket frames, as websockify's socket carries it on the noVNC path. Both
// sides of a local window are raw RFB, so each chunk goes into the filter as one unmasked binary frame
// (wsFramed on the display's side, appendWSBinary on the CLI's) and comes out unframed (wsUnframer).
const wsFrameHeader = 10 // the longest unmasked header: 2 bytes and a 64-bit length

func appendWSBinary(dst, p []byte) []byte {
	switch n := len(p); {
	case n < 126:
		dst = append(dst, 0x82, byte(n))
	case n <= 0xffff:
		dst = binary.BigEndian.AppendUint16(append(dst, 0x82, 126), uint16(n))
	default:
		dst = binary.BigEndian.AppendUint64(append(dst, 0x82, 127), uint64(n))
	}
	return append(dst, p...)
}

// wsUnframer writes the payloads of the filter's frames. They are the unmasked frames the framers made,
// re-encoded by the filter, so no mask is ever set.
type wsUnframer struct {
	w       io.Writer
	pending []byte
}

func (u *wsUnframer) Write(p []byte) (int, error) {
	u.pending = append(u.pending, p...)
	for {
		b := u.pending
		if len(b) < 2 {
			return len(p), nil
		}
		size, off := uint64(b[1]&0x7f), 2
		switch size {
		case 126:
			if len(b) < 4 {
				return len(p), nil
			}
			size, off = uint64(binary.BigEndian.Uint16(b[2:])), 4
		case 127:
			if len(b) < 10 {
				return len(p), nil
			}
			size, off = binary.BigEndian.Uint64(b[2:]), 10
		}
		if uint64(len(b)-off) < size {
			return len(p), nil
		}
		end := off + int(size)
		if end > off {
			if _, err := u.w.Write(b[off:end]); err != nil {
				return 0, err
			}
		}
		u.pending = b[end:]
	}
}

// wsFramed is the display as the filter's backend: reads come framed, writes go out unframed.
type wsFramed struct {
	raw io.ReadWriteCloser
	wsUnframer
}

func (f *wsFramed) Read(p []byte) (int, error) {
	if len(p) <= wsFrameHeader {
		return 0, io.ErrShortBuffer
	}
	n, err := f.raw.Read(p[wsFrameHeader:])
	if n == 0 {
		return 0, err
	}
	return copy(p, appendWSBinary(nil, p[wsFrameHeader:wsFrameHeader+n])), err
}

func (f *wsFramed) Close() error { return f.raw.Close() }
