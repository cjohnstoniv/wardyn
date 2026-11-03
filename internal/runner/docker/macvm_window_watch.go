// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A window is meant to open only through OpenWindow. The helper says on its events stream whenever one
// opens, and a window whose grant this runner did not issue is someone using ctl.sock on the Mac
// directly: it is reported (MacVMConfig.OnUnexpectedWindow), never closed.

// macWindowIssued holds the grants OpenWindow has issued and not yet seen end, as "run/grant". Run ids
// are unique, so one table serves every driver in the process.
var macWindowIssued sync.Map

func issueMacWindow(runID uuid.UUID, grant string) {
	macWindowIssued.Store(runID.String()+"/"+grant, struct{}{})
}
func forgetMacWindow(runID uuid.UUID, grant string) {
	macWindowIssued.Delete(runID.String() + "/" + grant)
}
func issuedMacWindow(runID uuid.UUID, grant string) bool {
	_, ok := macWindowIssued.Load(runID.String() + "/" + grant)
	return ok
}

// The helper's window events on the events stream (macvm.Event with Type window).
const (
	macWindowEventType    = "window"
	macWindowEventOpen    = "open"
	macWindowWatchBackoff = 30 * time.Second
)

// watchWindows keeps one events connection to runID's helper until ctx ends, redialling after a loss
// (the helper may not be up yet, or restarted) with a growing wait.
func (c *MacVMConfig) watchWindows(ctx context.Context, runID uuid.UUID) {
	wait := time.Second
	for ctx.Err() == nil {
		if path, err := c.sockPath(runID, macvm.ControlSock); err == nil {
			var nd net.Dialer
			if conn, err := nd.DialContext(ctx, "unix", path); err == nil {
				if watchWindowEvents(ctx, conn, runID, c.OnUnexpectedWindow) == nil {
					wait = time.Second
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(wait*2, macWindowWatchBackoff)
	}
}

// watchWindowEvents subscribes on conn to the helper's events and reports each window open whose grant
// was not issued here. It closes conn and returns when the stream ends or ctx does; nil means the
// subscription was made, so the caller may redial at once.
func watchWindowEvents(ctx context.Context, conn net.Conn, runID uuid.UUID, report func(uuid.UUID, string, time.Time)) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	rd := bufio.NewReaderSize(conn, 4096)
	var r macWindowReply
	if err := writeLine(conn, macvm.ControlRequest{Op: macvm.OpHello, Proto: macvm.ControlProto}); err != nil {
		return err
	}
	if err := readLine(rd, &r); err != nil {
		return err
	}
	if !r.OK || r.Proto != macvm.ControlProto {
		return fmt.Errorf("docker: the macOS VM helper speaks another control protocol")
	}
	if err := writeLine(conn, macvm.ControlRequest{Op: macvm.OpEvents}); err != nil {
		return err
	}
	if err := readLine(rd, &r); err != nil || !r.OK {
		return fmt.Errorf("docker: the macOS VM helper did not start its events: %v", err)
	}
	for {
		r = macWindowReply{}
		if err := readLine(rd, &r); err != nil {
			return nil
		}
		if e := r.Event; e != nil && e.Type == macWindowEventType && e.Detail == macWindowEventOpen && !issuedMacWindow(runID, e.Grant) && report != nil {
			report(runID, e.Grant, time.Now())
		}
	}
}
