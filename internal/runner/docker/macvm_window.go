// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// An attended window onto a macOS VM run is one connection to the run's helper on its ctl.sock: the
// helper opens the window on the live VM when it answers the window request, keeps it up while the
// connection stays open, and closes the connection when the window is over. Closing the connection from
// here closes the window. The connection is dialled here (dialUnix, which checks the socket directory)
// and held as a net.Conn, so the control client's Window, which dials its own and keeps its connection
// to itself, is not used.

// macWindowAsk is the open request's wait: the hello and the helper's answer, which comes once the
// window is up.
const macWindowAsk = 15 * time.Second

// OpenWindow implements runner.MacWindower: it asks the run's helper, on the ctl.sock of its checked
// socket directory, to open a window, and holds the connection as the window. A sandbox that is no macOS
// VM is refused; a helper that does not know the window op (it started before this runner was upgraded)
// fails closed with runner.ErrMacWindowHelperOld.
func (d *Driver) OpenWindow(ctx context.Context, ref string, req runner.MacWindowRequest) (runner.MacWindow, error) {
	v := d.vmFor(ctx, ref)
	c := d.macVMConfig()
	if v == nil || !v.mac || c == nil {
		return nil, runner.ErrMacWindowUnsupported
	}
	path, err := c.sockPath(v.runID, macvm.ControlSock)
	if err != nil {
		return nil, fmt.Errorf("docker: macOS VM window: %w", err)
	}
	conn, err := d.dialUnix(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("docker: macOS VM window: %w", err)
	}
	// The grant is issued before the helper hears of it, so its open event is never read as a stranger's.
	issueMacWindow(v.runID, req.GrantID)
	w, err := startMacWindow(ctx, conn, req)
	if err != nil {
		c.forgetMacWindowAfter(v.runID, req.GrantID, err)
		return nil, err
	}
	if c.OnUnexpectedWindow == nil { // no watcher to forget it on the helper's closed event
		go func() { <-w.Ended(); forgetMacWindow(v.runID, req.GrantID) }()
	}
	return w, nil
}

// macWindowGrace is how long a grant stays issued after an open that failed without the helper refusing
// it: the helper may have opened the window just as the open gave up, and its open event must not read as a
// stranger's. The helper's closed event (the connection is gone) forgets it sooner.
const macWindowGrace = time.Minute

// forgetMacWindowAfter forgets the grant of a failed open: at once when the helper refused it (no window
// was opened), after macWindowGrace when it may have opened one and a watcher will hear of it.
func (c *MacVMConfig) forgetMacWindowAfter(runID uuid.UUID, grant string, err error) {
	var refusal *runner.MacWindowRefusal
	if c.OnUnexpectedWindow == nil || errors.As(err, &refusal) || errors.Is(err, runner.ErrMacWindowOpen) || errors.Is(err, runner.ErrMacWindowHelperOld) {
		forgetMacWindow(runID, grant)
		return
	}
	time.AfterFunc(macWindowGrace, func() { forgetMacWindow(runID, grant) })
}

// startMacWindow asks the helper on conn for a window and, once it is up, holds conn as the window. conn
// is closed on every failure. A helper that never answers is given macWindowAsk.
func startMacWindow(ctx context.Context, conn net.Conn, req runner.MacWindowRequest) (runner.MacWindow, error) {
	ctx, cancel := context.WithTimeout(ctx, macWindowAsk)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // a stuck helper does not hold the open
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	rd := bufio.NewReaderSize(conn, 4096)
	w, err := askMacWindow(conn, rd, req)
	if !stop() || err != nil { // the context ended the open, or the helper refused it
		_ = conn.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	w.conn, w.ended = conn, make(chan struct{})
	go w.watch(rd)
	return w, nil
}

// askMacWindow says hello, sends the window request and reads the helper's answer.
func askMacWindow(conn net.Conn, rd *bufio.Reader, req runner.MacWindowRequest) (*macWindow, error) {
	if err := writeLine(conn, macvm.ControlRequest{Op: macvm.OpHello, Proto: macvm.ControlProto}); err != nil {
		return nil, err
	}
	var hello macvm.ControlReply
	if err := readLine(rd, &hello); err != nil {
		return nil, err
	}
	if !hello.OK || hello.Proto != macvm.ControlProto {
		return nil, fmt.Errorf("%w: %w", runner.ErrMacWindowHelperOld, &macvm.ErrProto{Helper: hello.Proto})
	}
	if err := writeLine(conn, macvm.ControlRequest{Op: macvm.OpWindow, Grant: req.GrantID, Title: req.Title}); err != nil {
		return nil, err
	}
	var r macvm.ControlReply
	if err := readLine(rd, &r); err != nil {
		return nil, err
	}
	if r.OK {
		return &macWindow{}, nil
	}
	switch {
	case strings.HasPrefix(r.Error, "unknown op"): // a helper older than the window op
		return nil, runner.ErrMacWindowHelperOld
	case r.Code == macvm.WindowCodeOpen:
		return nil, runner.ErrMacWindowOpen
	case r.Code != "":
		return nil, &runner.MacWindowRefusal{Code: r.Code, Sentence: r.Error}
	}
	return nil, fmt.Errorf("docker: the macOS VM helper did not open the window: %s", r.Error)
}

func writeLine(conn net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(b, '\n'))
	return err
}

func readLine(rd *bufio.Reader, v any) error {
	line, err := rd.ReadSlice('\n')
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return errors.New("docker: the macOS VM helper's window reply was not understood")
	}
	return nil
}

// macWindow is runner.MacWindow over the helper's connection.
type macWindow struct {
	conn   net.Conn
	ended  chan struct{}
	closed atomic.Bool // Close ran: the connection's end is ours, not the helper's
	reason string      // written before ended closes
	once   sync.Once
}

// watch reads the helper's connection until it ends and records why. A close of our own records nothing.
func (w *macWindow) watch(rd *bufio.Reader) {
	reason := runner.MacWindowEndHelperLost
	var r macvm.ControlReply
	if err := readLine(rd, &r); err == nil && r.Event != nil && r.Event.Type == macvm.EventWindowClosed {
		switch r.Event.Detail {
		case macvm.WindowClosedPerson:
			reason = runner.MacWindowEndPerson
		case macvm.WindowClosedHostLocked:
			reason = runner.MacWindowEndHostLocked
		case macvm.WindowClosedRunEnded:
			reason = runner.MacWindowEndRunEnded
		default: // "failed", or a reason a newer helper has
			reason = runner.MacWindowEndHelperFailed
		}
	}
	if w.closed.Load() {
		reason = ""
	}
	w.reason = reason
	_ = w.conn.Close()
	close(w.ended)
}

func (w *macWindow) Ended() <-chan struct{} { return w.ended }
func (w *macWindow) Reason() string         { return w.reason }

// Close closes the helper's connection, which closes the window. The watch then ends the window with no
// reason.
func (w *macWindow) Close() error {
	w.once.Do(func() {
		w.closed.Store(true)
		_ = w.conn.Close()
	})
	return nil
}
