// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ControlProto is the ctl.sock protocol version. A runnerd that cannot speak
// the helper's version ends that run cleanly rather than guessing.
const ControlProto = 1

// Control operations. The first request on every connection is hello.
const (
	OpHello  = "hello"
	OpStatus = "status"
	OpStop   = "stop"
	// OpNetFD carries one file descriptor (SCM_RIGHTS): the hijacked attach
	// connection of the run's vmnet container. The VM starts once it arrives.
	OpNetFD = "net_fd"
	// OpEvents turns the connection into a one-way stream of events.
	OpEvents = "events"
	// OpDesktopPassword reads the run's VNC password (ControlReply.DesktopPassword), which runnerd
	// sends in the guest agent's Init. Only a peer of the helper's own uid is answered.
	OpDesktopPassword = "desktop_password"
	// OpWindow draws a close-only window on the live VM and binds it to the connection: the reply
	// {"ok":true} comes once the window is up, one window_closed event (Event.Detail is the reason, a
	// WindowClosed* value) follows when it ends and the helper closes the connection. Closing the
	// connection closes the window. Closing the window never stops the VM or the helper.
	OpWindow = "window"
)

// Detail values of an EventWindow.
const (
	EventWindowOpen         = "open"
	EventWindowClosedDetail = "closed"
)

// Reasons a window ends (Event.Detail of the EventWindowClosed event). A client that closes the
// connection first gets no event: it knows why.
const (
	WindowClosedPerson     = "person"      // the person closed the window
	WindowClosedHostLocked = "host_locked" // the Mac's screen locked, its lock can no longer be told, or its console session changed
	WindowClosedRunEnded   = "run_ended"   // the VM stopped
	WindowClosedFailed     = "failed"      // the console check could not be read, so the window closed
)

// WindowClosedConnection is the reason ControlClient.Window delivers when the connection ended with no
// event (the helper went away). It is never sent on the wire.
const WindowClosedConnection = "connection"

// Codes of a refused window (ControlReply.Code), each with its sentence (ControlReply.Error). The
// sentences are what the person reads; the codes are what a caller branches on.
const (
	WindowCodeNotRunning  = "window_not_running"
	WindowCodeNotOwner    = "window_not_owner"
	WindowCodeOpen        = "window_open"
	WindowCodeNotAtMac    = "window_not_at_mac"
	WindowCodeLockUnknown = "window_lock_unknown"
	WindowCodeUnavailable = "window_unavailable"
	WindowCodeBadRequest  = "window_bad_request"
	WindowCodeFailed      = "window_failed"
)

const (
	// WindowAlreadyOpenSentence is the one-window-per-run refusal. The helper, the runner and the
	// control plane all use these words, so a person reads the same sentence from any of them.
	WindowAlreadyOpenSentence = "This run's window is already open."
	// WindowNotRunningSentence answers a window asked of a VM that is not running.
	WindowNotRunningSentence = "The window can't open yet: this run's macOS VM isn't running."
	// WindowNotOwnerSentence answers a window asked by a process that is not this helper's user.
	WindowNotOwnerSentence = "window: the peer is not this helper's user"
	// WindowNotAtMacSentence answers a screen that is locked or whose console is not this person's.
	WindowNotAtMacSentence = "The window didn't open: you aren't signed in at this Mac's screen, or it is locked. Unlock it and open the window again."
	// WindowUnreadableSentence answers a console check that could not be read; the window refuses.
	WindowUnreadableSentence = "The window didn't open: Wardyn couldn't tell whether you are at this Mac's screen. Unlock it and open the window again."
	// WindowLockUnknownSentence answers a screen whose lock state can't be told; the window fails closed.
	WindowLockUnknownSentence = "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed."
	// WindowUnavailableSentence answers a helper or VM that cannot draw a window.
	WindowUnavailableSentence = "This Mac's Wardyn helper can't open a window onto this run's VM. Update Wardyn on this Mac."
	// WindowBadRequestSentence answers a window request with no usable grant id.
	WindowBadRequestSentence = "window: the request needs a grant id of letters, digits and . _ : - (at most 128)"
	// WindowFailedSentence starts the sentence for a window that could not be made; the cause follows.
	WindowFailedSentence = "The window didn't open: "
)

// WindowRefusal is a refused window: a ControlReply's Code and Error.
type WindowRefusal struct{ Code, Sentence string }

func (e *WindowRefusal) Error() string { return e.Sentence }

// ControlRequest is one JSON line from runnerd to the helper.
type ControlRequest struct {
	Op        string `json:"op"`
	Proto     int    `json:"proto,omitempty"`
	Graceful  bool   `json:"graceful,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
	// Grant and Title are OpWindow's. Grant names the open in helper.log (letters, digits and . _ : -,
	// at most 128). Title is the window's title, trimmed to 200 characters; empty is a default title.
	Grant string `json:"grant,omitempty"`
	Title string `json:"title,omitempty"`
}

// ControlReply is one JSON line from the helper.
type ControlReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Code names a refused OpWindow (a WindowCode* value); other refusals leave it out.
	Code   string  `json:"code,omitempty"`
	Proto  int     `json:"proto,omitempty"`
	Status *Status `json:"status,omitempty"`
	Event  *Event  `json:"event,omitempty"`
	// DesktopPassword answers OpDesktopPassword. Never log a reply that carries it.
	DesktopPassword string `json:"desktop_password,omitempty"`
}

// Network states in Status.
const (
	NetWaiting  = "waiting"
	NetAttached = "attached"
	NetLost     = "lost"
)

// Status is the helper's answer to status.
type Status struct {
	Proto     int          `json:"proto"`
	RunID     string       `json:"run_id"`
	State     MachineState `json:"state"`
	HelperPID int          `json:"helper_pid"`
	StartedAt time.Time    `json:"started_at"`
	// Identity and MachineID are the strategy and the identifier this run's VM
	// was actually configured with (base64 of its data representation).
	Identity  IdentityStrategy `json:"identity"`
	MachineID string           `json:"machine_id"`
	// MachineIDSHA256 hashes the configured identifier; ReadBackSHA256
	// hashes the one read back after start (empty before). The read-back
	// comes from the helper's own configuration object, so a match proves
	// nothing about what the guest was given; it is logged, not evidence.
	MachineIDSHA256 string  `json:"machine_id_sha256"`
	ReadBackSHA256  string  `json:"machine_id_readback_sha256,omitempty"`
	CPUs            int     `json:"cpus"`
	MemoryMiB       uint64  `json:"memory_mib"`
	Display         Display `json:"display"`
	Network         string  `json:"network"`
	// Listeners are the vsock ports the helper registered for this VM: a
	// run's are 1024 and 1025 only; runnerd ends a run that shows another.
	Listeners []uint32 `json:"vsock_listeners"`
	// DesktopForcedViewOnly comes from the shim serving this boot (RFBShim.DesktopForcedViewOnly), never
	// from run.json. An older helper leaves it out, which reads as not forced.
	DesktopForcedViewOnly bool   `json:"desktop_forced_view_only,omitempty"`
	Error                 string `json:"error,omitempty"`
}

// Event types.
const (
	EventState      = "state"
	EventNetwork    = "network"
	EventGuestAgent = "guest_agent"
	// EventWindow is published on the OpEvents stream when a run window opens or closes (Detail is
	// EventWindowOpen or EventWindowClosedDetail), so runnerd can tell a window it did not ask for.
	EventWindow = "window"
	// EventWindowClosed ends an OpWindow connection; Detail is a WindowClosed* reason.
	EventWindowClosed = "window_closed"
)

// Event is one line of the events stream.
type Event struct {
	Time   time.Time    `json:"time"`
	Type   string       `json:"type"`
	State  MachineState `json:"state,omitempty"`
	Detail string       `json:"detail,omitempty"`
	// Grant and Reason are an EventWindow's: the grant id the window was opened with, and, when it closed,
	// why (a WindowClosed* value).
	Grant  string `json:"grant,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ErrProto is returned by DialControl when the helper speaks another version.
type ErrProto struct{ Helper int }

func (e *ErrProto) Error() string {
	return fmt.Sprintf("macvm: helper speaks control protocol %d, this runner speaks %d", e.Helper, ControlProto)
}

// ControlClient is runnerd's side of ctl.sock. One request at a time.
type ControlClient struct {
	c  *net.UnixConn
	rd *bufio.Reader
}

// DialControl connects to a helper's ctl.sock and says hello.
func DialControl(ctx context.Context, path string) (*ControlClient, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	cc := &ControlClient{c: c.(*net.UnixConn), rd: bufio.NewReader(c)}
	r, err := cc.call(ctx, ControlRequest{Op: OpHello, Proto: ControlProto}, nil)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if r.Proto != ControlProto {
		_ = c.Close()
		return nil, &ErrProto{Helper: r.Proto}
	}
	return cc, nil
}

func (cc *ControlClient) Close() error { return cc.c.Close() }

func (cc *ControlClient) call(ctx context.Context, req ControlRequest, f *os.File) (ControlReply, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = cc.c.SetDeadline(dl)
		defer cc.c.SetDeadline(time.Time{})
	}
	b, err := json.Marshal(req)
	if err != nil {
		return ControlReply{}, err
	}
	b = append(b, '\n')
	var oob []byte
	if f != nil {
		oob = unix.UnixRights(int(f.Fd()))
	}
	if _, _, err := cc.c.WriteMsgUnix(b, oob, nil); err != nil {
		return ControlReply{}, err
	}
	var r ControlReply
	line, err := cc.rd.ReadBytes('\n')
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return r, fmt.Errorf("macvm: control reply: %w", err)
	}
	if !r.OK {
		return r, errors.New(r.Error)
	}
	return r, nil
}

// Status asks for the helper's status.
func (cc *ControlClient) Status(ctx context.Context) (Status, error) {
	r, err := cc.call(ctx, ControlRequest{Op: OpStatus}, nil)
	if err != nil {
		return Status{}, err
	}
	if r.Status == nil {
		return Status{}, errors.New("macvm: status reply without a status")
	}
	return *r.Status, nil
}

// DesktopPassword reads the run's VNC password, for the guest agent's Init (Init.DesktopPassword).
// It fails for a run with no desktop. Never log or store the value.
func (cc *ControlClient) DesktopPassword(ctx context.Context) (string, error) {
	r, err := cc.call(ctx, ControlRequest{Op: OpDesktopPassword}, nil)
	if err != nil {
		return "", err
	}
	if r.DesktopPassword == "" {
		return "", errors.New("macvm: desktop_password reply without a password")
	}
	return r.DesktopPassword, nil
}

// Stop asks the helper to stop the VM (gracefully first when graceful, for at
// most timeout, then hard) and returns once it has stopped.
func (cc *ControlClient) Stop(ctx context.Context, graceful bool, timeout time.Duration) error {
	_, err := cc.call(ctx, ControlRequest{Op: OpStop, Graceful: graceful, TimeoutMS: timeout.Milliseconds()}, nil)
	return err
}

// SendNetFD hands the helper the attach connection's descriptor. The caller
// may close its own copy afterwards.
func (cc *ControlClient) SendNetFD(ctx context.Context, f *os.File) error {
	_, err := cc.call(ctx, ControlRequest{Op: OpNetFD}, f)
	return err
}

// Events turns this connection into the helper's event stream. The channel
// closes when the stream ends; use another client for requests.
func (cc *ControlClient) Events(ctx context.Context) (<-chan Event, error) {
	if _, err := cc.call(ctx, ControlRequest{Op: OpEvents}, nil); err != nil {
		return nil, err
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		for {
			line, err := cc.rd.ReadBytes('\n')
			if err != nil {
				return
			}
			var r ControlReply
			if json.Unmarshal(line, &r) != nil || r.Event == nil {
				return
			}
			select {
			case ch <- *r.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// Window asks the helper for a close-only window on the run's VM. It returns once the window is up (a
// *WindowRefusal when the helper refuses). The channel then delivers the reason the window ended (a
// WindowClosed* value) and closes. Closing cc closes the window and ends the channel with
// WindowClosedConnection. Like Events, it takes over the connection: use another client for requests.
func (cc *ControlClient) Window(ctx context.Context, grant, title string) (<-chan string, error) {
	r, err := cc.call(ctx, ControlRequest{Op: OpWindow, Grant: grant, Title: title}, nil)
	if err != nil {
		if r.Code != "" {
			return nil, &WindowRefusal{Code: r.Code, Sentence: r.Error}
		}
		return nil, err
	}
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		reason := WindowClosedConnection
		if line, err := cc.rd.ReadBytes('\n'); err == nil {
			var ev ControlReply
			if json.Unmarshal(line, &ev) == nil && ev.Event != nil && ev.Event.Type == EventWindowClosed {
				reason = ev.Event.Detail
			}
		}
		ch <- reason
	}()
	return ch, nil
}
