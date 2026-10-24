// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
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
		fds     []*os.File
		hello   bool
	)
	defer func() {
		for _, f := range fds {
			_ = f.Close()
		}
	}()
	enc := json.NewEncoder(c)
	reply := func(r ControlReply) bool { return enc.Encode(r) == nil }
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4*4))
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			fds = append(fds, parseRights(oob[:oobn])...)
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
			switch req.Op {
			case OpStatus:
				st := h.status()
				if !reply(ControlReply{OK: true, Status: &st}) {
					return
				}
			case OpStop:
				r := ControlReply{OK: true}
				if err := h.stop(req.Graceful, time.Duration(req.TimeoutMS)*time.Millisecond); err != nil {
					r = ControlReply{Error: err.Error()}
				}
				if !reply(r) {
					return
				}
			case OpNetFD:
				if len(fds) == 0 {
					reply(ControlReply{Error: "net_fd without a descriptor"})
					return
				}
				f := fds[0]
				fds = fds[1:]
				r := ControlReply{OK: true}
				if err := h.netFD(f); err != nil {
					_ = f.Close()
					r = ControlReply{Error: err.Error()}
				}
				if !reply(r) {
					return
				}
			case OpDesktopPassword:
				// ctl.sock is 0700 already; the peer's uid is checked as well, because this one
				// answer is a secret.
				if uid, err := peerUID(c); err != nil || int64(uid) != controlOwnerUID.Load() {
					reply(ControlReply{Error: "desktop_password: the peer is not this helper's user"})
					return
				}
				r := ControlReply{OK: true}
				pw, err := h.desktopPassword()
				if err != nil {
					r = ControlReply{Error: err.Error()}
				}
				r.DesktopPassword = pw
				if !reply(r) {
					return
				}
			case OpEvents:
				// Subscribe before the ack, so the client misses nothing
				// published after its Events call returns.
				ch, cancel := h.subscribe()
				if !reply(ControlReply{OK: true}) {
					cancel()
					return
				}
				streamEvents(c, enc, ch, cancel)
				return
			default:
				reply(ControlReply{Error: "unknown op " + req.Op})
				return
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
