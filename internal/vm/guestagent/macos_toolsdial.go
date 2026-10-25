// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// How the agent's last dial of VsockToolsPort ended (ToolsDial.Class). On the rented Mac, MAC-PROOF P5
// reads a stage-2 boot's (connected, the positive control) beside a run boot's (refused, the negative
// one); any other class is indeterminate.
const (
	DialConnected        = "connected"         // the host answered and sent its hello
	DialRefused          = "refused"           // ECONNREFUSED or ECONNRESET: nothing listens there
	DialUnsupported      = "unsupported"       // no vsock socket: EAFNOSUPPORT or EPROTONOSUPPORT
	DialTimeout          = "timeout"           // no answer within toolsDialTimeout
	DialTransportFailure = "transport failure" // anything else, or a session that ended before its hello
)

// toolsDialTimeout is how long one dial of the tools port may take before it counts as a timeout.
const toolsDialTimeout = 5 * time.Second

// maxDialError bounds the error text a ToolsDial carries.
const maxDialError = 256

// ClassifyDial is the class of a failed dial's error: DialHost's socket and connect errors keep their
// errno, and a dial its context cut short is a timeout.
func ClassifyDial(err error) string {
	switch {
	case errors.Is(err, unix.ECONNREFUSED), errors.Is(err, unix.ECONNRESET):
		return DialRefused
	case errors.Is(err, unix.EAFNOSUPPORT), errors.Is(err, unix.EPROTONOSUPPORT):
		return DialUnsupported
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, unix.ETIMEDOUT):
		return DialTimeout
	default:
		return DialTransportFailure
	}
}

// ToolsDial is the agent's report of its tools-port dial, served on the control session as
// GET /v1/dial/tools. Class is "" until a dial has ended one way or another.
type ToolsDial struct {
	Class string `json:"class,omitempty"`
	// Error is the dial's error text (at most maxDialError bytes) for a class other than connected.
	Error string `json:"error,omitempty"`
	// Dials counts the dials made this boot.
	Dials int `json:"dials"`
}

// ToolsDialRecord holds the latest ToolsDial; the zero value is ready.
type ToolsDialRecord struct {
	mu   sync.Mutex
	last ToolsDial
}

func (r *ToolsDialRecord) dialed() {
	r.mu.Lock()
	r.last.Dials++
	r.mu.Unlock()
}

func (r *ToolsDialRecord) set(class string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last.Class, r.last.Error = class, ""
	if err != nil {
		msg := err.Error()
		r.last.Error = msg[:min(len(msg), maxDialError)]
	}
}

// Get is the latest ToolsDial.
func (r *ToolsDialRecord) Get() ToolsDial {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// ServeHTTP answers GET /v1/dial/tools on the control session.
func (r *ToolsDialRecord) ServeHTTP(w http.ResponseWriter, _ *http.Request) { writeJSONAny(w, r.Get()) }

// ServeTools serves h (Tools.ToolsHandler) on the tools port as ServeVsock serves the control port, and
// records how each dial ends in rec: each dial and its hello get toolsDialTimeout, a session the host
// opens with its hello is connected, an open one with no hello in time is a timeout, and one that ends
// before the hello is a transport failure.
func ServeTools(ctx context.Context, dial DialFunc, h http.Handler, rec *ToolsDialRecord) error {
	return serveTools(ctx, dial, h, rec, redialWait, toolsDialTimeout)
}

func serveTools(ctx context.Context, dial DialFunc, h http.Handler, rec *ToolsDialRecord, wait, timeout time.Duration) error {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	for ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(dctx)
		cancel()
		rec.dialed()
		if ctx.Err() != nil {
			if conn != nil {
				_ = conn.Close()
			}
			break
		}
		if err != nil {
			rec.set(ClassifyDial(err), err)
			sleep(ctx, wait)
			continue
		}
		// Connected, so no class until the hello (or its lack) decides; the hello gets the dial's timeout.
		rec.set("", nil)
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		helloed := false
		herr := serveSession(ctx, conn, h, &protocols, func() {
			_ = conn.SetReadDeadline(time.Time{})
			helloed = true
			rec.set(DialConnected, nil)
		})
		switch {
		case helloed || ctx.Err() != nil:
		case errors.Is(herr, os.ErrDeadlineExceeded):
			rec.set(DialTimeout, errors.New("no hello from the host within the dial's timeout"))
		default:
			rec.set(DialTransportFailure, errors.New("the tools session ended before the host's hello"))
		}
		sleep(ctx, wait)
	}
	return nil
}
