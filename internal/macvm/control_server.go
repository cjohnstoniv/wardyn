// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// controlHandler is what the control server asks of the run loop.
type controlHandler interface {
	status() Status
	stop(graceful bool, timeout time.Duration) error
	netFD(f *os.File) error
	subscribe() (<-chan Event, func())
	// desktopPassword is the run's VNC password; ErrNoDesktopPassword without one.
	desktopPassword() (string, error)
}

// controlOwnerUID is the uid a desktop_password peer must have: the helper's own (runnerd's, which
// started it). Settable so a test can make the peer a stranger.
var controlOwnerUID = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(os.Getuid()))
	return &v
}()

// maxControlLine bounds one request line; ctl.sock is 0700 inside a 0700 run
// dir, so only the user's own processes reach it, but a runaway line still
// must not grow without limit.
const maxControlLine = 64 << 10

// serveControl accepts connections on l until it is closed.
func serveControl(l *net.UnixListener, h controlHandler) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		go serveControlConn(c, h)
	}
}

func serveControlConn(c *net.UnixConn, h controlHandler) {
	defer c.Close()
	var (
		pending []byte
		hello   bool
	)
	enc := json.NewEncoder(c)
	reply := func(r ControlReply) bool { return enc.Encode(r) == nil }
	cc := &controlConn{c: c, h: h, enc: enc, reply: reply}
	defer func() {
		for _, f := range cc.fds {
			_ = f.Close()
		}
	}()
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4*4))
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			cc.fds = append(cc.fds, parseRights(oob[:oobn])...)
		}
		if err != nil {
			return
		}
		pending = append(pending, buf[:n]...)
		if len(pending) > maxControlLine && bytes.IndexByte(pending, '\n') < 0 {
			reply(ControlReply{Error: "request too long"})
			return
		}
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := pending[:i]
			pending = pending[i+1:]
			var req ControlRequest
			if err := json.Unmarshal(line, &req); err != nil {
				reply(ControlReply{Error: "bad request"})
				return
			}
			if !hello {
				if req.Op != OpHello {
					reply(ControlReply{Error: "hello first", Proto: ControlProto})
					return
				}
				hello = true
				if !reply(ControlReply{OK: true, Proto: ControlProto}) || req.Proto != ControlProto {
					return
				}
				continue
			}
			if !cc.op(req) {
				return
			}
		}
	}
}

// controlConn is one ctl.sock connection after its hello.
type controlConn struct {
	c     *net.UnixConn
	h     controlHandler
	enc   *json.Encoder
	fds   []*os.File
	reply func(ControlReply) bool
}

// op answers one request and says whether the connection goes on.
func (cc *controlConn) op(req ControlRequest) bool {
	h, reply := cc.h, cc.reply
	switch req.Op {
	case OpStatus:
		st := h.status()
		return reply(ControlReply{OK: true, Status: &st})
	case OpStop:
		r := ControlReply{OK: true}
		if err := h.stop(req.Graceful, time.Duration(req.TimeoutMS)*time.Millisecond); err != nil {
			r = ControlReply{Error: err.Error()}
		}
		return reply(r)
	case OpNetFD:
		if len(cc.fds) == 0 {
			reply(ControlReply{Error: "net_fd without a descriptor"})
			return false
		}
		f := cc.fds[0]
		cc.fds = cc.fds[1:]
		r := ControlReply{OK: true}
		if err := h.netFD(f); err != nil {
			_ = f.Close()
			r = ControlReply{Error: err.Error()}
		}
		return reply(r)
	case OpDesktopPassword:
		// ctl.sock is 0700 already; the peer's uid is checked as well, because this one
		// answer is a secret.
		if uid, err := peerUID(cc.c); err != nil || int64(uid) != controlOwnerUID.Load() {
			reply(ControlReply{Error: "desktop_password: the peer is not this helper's user"})
			return false
		}
		r := ControlReply{OK: true}
		pw, err := h.desktopPassword()
		if err != nil {
			r = ControlReply{Error: err.Error()}
		}
		r.DesktopPassword = pw
		return reply(r)
	case OpWindow:
		return cc.window(req)
	case OpEvents:
		// Subscribe before the ack, so the client misses nothing
		// published after its Events call returns.
		ch, cancel := h.subscribe()
		if !reply(ControlReply{OK: true}) {
			cancel()
			return false
		}
		streamEvents(cc.c, cc.enc, ch, cancel)
		return false
	default:
		reply(ControlReply{Error: "unknown op " + req.Op})
		return false
	}
}

// windowHandler is what the run loop adds for OpWindow. A handler without it (the stage-2 VM) answers
// every window request with WindowUnavailableSentence.
type windowHandler interface {
	openWindow(grant, title string, going func() bool) (*windowSession, *WindowRefusal)
}

// windowSession is one open window, held by the connection that asked for it.
type windowSession struct {
	win Window
	// ready is the console check, asked every windowRecheck while the window is up.
	ready func() error
	// vmDone is closed when the VM has stopped.
	vmDone <-chan struct{}
	// end runs once when the window is gone: it frees the run's one window slot.
	end func(reason string, held time.Duration)
	at  time.Time
}

// windowRecheck is how often an open window asks whether the person is still at the Mac's screen.
var windowRecheck = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(2 * time.Second))
	return &v
}()

// windowReleaseWait bounds how long an open waits for a window whose holder's connection is ending.
var windowReleaseWait = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(2 * time.Second))
	return &v
}()

// windowReleaseHook runs once a window's hold has ended, before it is closed and its slot freed; tests hold it.
var windowReleaseHook atomic.Pointer[func()]

// windowWaitHook runs each time an open starts waiting for an ending window's slot; tests watch it.
var windowWaitHook atomic.Pointer[func()]

// windowWaitedHook runs when an open that waited has checked its own connection (and, if it left, given the
// slot back); tests watch it.
var windowWaitedHook atomic.Pointer[func()]

var windowGrantRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// windowTitleMax bounds the title a caller can set.
const windowTitleMax = 200

// cleanWindowTitle trims title to windowTitleMax characters and drops control characters.
func cleanWindowTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)
	if rs := []rune(title); len(rs) > windowTitleMax {
		title = string(rs[:windowTitleMax])
	}
	return strings.TrimSpace(title)
}

// window answers OpWindow: it checks the peer, asks the run loop for a window, and holds the connection
// until the window ends. It always ends the connection.
func (cc *controlConn) window(req ControlRequest) bool {
	refuse := func(code, sentence string) bool {
		cc.reply(ControlReply{Error: sentence, Code: code})
		return false
	}
	// Whatever the run loop thinks, only the helper's own user gets a window onto the VM.
	if uid, err := peerUID(cc.c); err != nil || int64(uid) != controlOwnerUID.Load() {
		return refuse(WindowCodeNotOwner, WindowNotOwnerSentence)
	}
	wh, ok := cc.h.(windowHandler)
	if !ok {
		return refuse(WindowCodeUnavailable, WindowUnavailableSentence)
	}
	if !windowGrantRE.MatchString(req.Grant) {
		return refuse(WindowCodeBadRequest, WindowBadRequestSentence)
	}
	// The window is going once it has ended here, or once its client has closed the connection: the
	// client closes before it opens again, so a reopen can arrive before this goroutine reads that close.
	var ended atomic.Bool
	going := func() bool { return ended.Load() || peerClosed(cc.c) }
	s, rf := wh.openWindow(req.Grant, cleanWindowTitle(req.Title), going)
	if rf != nil {
		return refuse(rf.Code, rf.Sentence)
	}
	reason := cc.holdWindow(s)
	if h := windowReleaseHook.Load(); h != nil {
		(*h)()
	}
	ended.Store(true)
	s.win.Close()
	s.end(reason, time.Since(s.at))
	if reason != "" {
		cc.reply(ControlReply{OK: true, Event: &Event{Time: time.Now().UTC(), Type: EventWindowClosed, Detail: reason}})
	}
	return false
}

// peerClosed reports whether c's client has closed its end: a read would return end of file now. It peeks
// without blocking and consumes nothing, so the connection's own reader is undisturbed.
func peerClosed(c *net.UnixConn) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return true
	}
	closed := false
	if err := rc.Control(func(fd uintptr) {
		n, _, rerr := unix.Recvfrom(int(fd), make([]byte, 1), unix.MSG_PEEK|unix.MSG_DONTWAIT)
		closed = (n == 0 && rerr == nil) || (rerr != nil && rerr != unix.EAGAIN && rerr != unix.EINTR)
	}); err != nil {
		return true
	}
	return closed
}

// holdWindow answers the open, then waits for the window to end and returns why. The reason is empty when the
// client went first: it needs no event.
func (cc *controlConn) holdWindow(s *windowSession) string {
	if !cc.reply(ControlReply{OK: true}) {
		return ""
	}
	gone := make(chan struct{})
	go func() {
		_, _ = cc.c.Read(make([]byte, 1)) // returns when the client closes (or writes: nothing else is allowed)
		close(gone)
	}()
	tick := time.NewTicker(time.Duration(windowRecheck.Load()))
	defer tick.Stop()
	for {
		select {
		case <-s.win.Locked():
			return WindowClosedHostLocked
		case <-s.win.Closed():
			select {
			case <-s.win.Locked(): // a lock tore the window down: it is the lock's close, not the person's
				return WindowClosedHostLocked
			default:
			}
			return WindowClosedPerson
		case <-gone:
			return ""
		case <-s.vmDone:
			return WindowClosedRunEnded
		case <-tick.C:
			if err := s.ready(); err != nil {
				// A lock that can no longer be told is a lock: the window fails closed.
				if errors.Is(err, ErrNotAtMac) || errors.Is(err, ErrLockUnknown) {
					return WindowClosedHostLocked
				}
				return WindowClosedFailed
			}
		}
	}
}

// streamEvents writes events until the subscription ends or the client goes.
func streamEvents(c net.Conn, enc *json.Encoder, ch <-chan Event, cancel func()) {
	defer cancel()
	gone := make(chan struct{})
	go func() {
		_, _ = c.Read(make([]byte, 1)) // returns when the client closes
		close(gone)
	}()
	for {
		select {
		case ev, ok := <-ch:
			if !ok || enc.Encode(ControlReply{OK: true, Event: &ev}) != nil {
				return
			}
		case <-gone:
			return
		}
	}
}

// parseRights returns the descriptors in an SCM_RIGHTS control message.
func parseRights(oob []byte) []*os.File {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var out []*os.File
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			out = append(out, os.NewFile(uintptr(fd), "net-fd"))
		}
	}
	return out
}

// eventHub fans events out to subscribers; a slow subscriber loses events
// rather than stalling the VM loop.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
	done bool
}

func (e *eventHub) publish(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (e *eventHub) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		close(ch)
		return ch, func() {}
	}
	if e.subs == nil {
		e.subs = map[chan Event]struct{}{}
	}
	e.subs[ch] = struct{}{}
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
	}
}

// close ends every subscription once the last events are queued.
func (e *eventHub) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done = true
	for ch := range e.subs {
		delete(e.subs, ch)
		close(ch)
	}
}

var errAlreadyAttached = errors.New("the network is already attached")
