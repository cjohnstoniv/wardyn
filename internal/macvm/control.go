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
)

// ControlRequest is one JSON line from runnerd to the helper.
type ControlRequest struct {
	Op        string `json:"op"`
	Proto     int    `json:"proto,omitempty"`
	Graceful  bool   `json:"graceful,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// ControlReply is one JSON line from the helper.
type ControlReply struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Proto  int     `json:"proto,omitempty"`
	Status *Status `json:"status,omitempty"`
	Event  *Event  `json:"event,omitempty"`
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
	Error           string  `json:"error,omitempty"`
}

// Event types.
const (
	EventState      = "state"
	EventNetwork    = "network"
	EventGuestAgent = "guest_agent"
)

// Event is one line of the events stream.
type Event struct {
	Time   time.Time    `json:"time"`
	Type   string       `json:"type"`
	State  MachineState `json:"state,omitempty"`
	Detail string       `json:"detail,omitempty"`
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
