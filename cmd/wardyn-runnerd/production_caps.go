// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// errHostCaps wraps the error of a caps send that could not build the machine's part of the caps (Docker
// does not answer).
var errHostCaps = errors.New("the host's caps could not be built")

// resendMacOSCaps sends the caps again after a macOS VM change. The change is marked unsent first (a send
// takes the mark; with no session the next one sends). While Docker is down the send fails before it
// builds the host's caps, and the caps the org last acked go again with only macos_vm replaced, so the
// drawer keeps showing a Mac's macOS VM state (preparing, waiting for Docker) whatever Docker does. Every
// other field is what the org already holds. The mark stays set after such a send, because its host part
// is stale.
func resendMacOSCaps(ctx context.Context, peer *runnerwire.Peer, dirty *atomic.Bool, last *atomic.Pointer[runnerwire.Caps],
	send func(context.Context, *runnerwire.Peer, bool, bool) error, ack func(context.Context, *runnerwire.Peer, runnerwire.Caps) error,
	current func() *runnerwire.MacVMCaps) {
	dirty.Store(true)
	if peer == nil {
		return
	}
	err := send(ctx, peer, false, true)
	if prev := last.Load(); errors.Is(err, errHostCaps) && prev != nil {
		c := *prev
		c.MacOSVM = current()
		err = ack(ctx, peer, c)
	}
	if err != nil {
		slog.WarnContext(ctx, "runner caps after a macOS VM change failed", "error", err)
	}
}

// capsReporter sends the runner's caps to its organisation: on a fresh session, after a resume that has
// something new, and when a macOS VM or the relay changes.
type capsReporter struct {
	sub        substrate.Substrate
	extra      runtimeExtras
	capacity   func(context.Context) placement.Capacity
	dispatcher *runneragent.Dispatcher
	hasLocal   bool

	// online is the session being served, nil between sessions.
	online atomic.Pointer[runnerwire.Peer]
	// dirty: the caps changed while offline, or a send failed, since the org last took them.
	dirty atomic.Bool
	// last is the last caps the org acked: the base of a macOS VM resend while Docker is down, when the
	// host's part of the caps cannot be built.
	last atomic.Pointer[runnerwire.Caps]
}

func (c *capsReporter) ack(ctx context.Context, peer *runnerwire.Peer, caps runnerwire.Caps) error {
	data, err := runnerwire.Marshal(caps)
	if err != nil {
		return err
	}
	if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		return err
	}
	c.extra.report.sent(caps)
	c.last.Store(&caps)
	return nil
}

// send reports the runner's caps on peer; reprobe re-checks the relay first, and always sends even when
// nothing in the report is new (a fresh session's caps are required).
func (c *capsReporter) send(ctx context.Context, peer *runnerwire.Peer, reprobe, always bool) (err error) {
	if c.dirty.Swap(false) {
		always = true
	}
	defer func() {
		if err != nil {
			c.dirty.Store(true)
		}
	}()
	caps, err := hostCaps(ctx, c.sub, c.extra.arch)
	if err != nil {
		return fmt.Errorf("%w: %w", errHostCaps, err)
	}
	caps.Capacity, caps.BYOISelftest, caps.Retire, caps.Resident = c.capacity(ctx), c.dispatcher.Selftests(), true, c.dispatcher.Residents()
	caps.ProxySettings, caps.AllowedRoots, caps.LocalModelPorts = c.extra.proxySettings(), c.extra.driveRoots, c.extra.localModelPorts
	caps.LinuxVM, caps.MacOSVM, caps.LocalAttach = c.extra.linuxVM, c.extra.macOSVM.current(), c.hasLocal
	caps.RunsNamespaces = c.extra.runsNamespaces
	if !c.extra.report.fill(ctx, &caps, reprobe) && !always {
		return nil
	}
	return c.ack(ctx, peer, caps)
}

// afterOnline sends caps once a session is online when they are owed. A resumed session sends none
// unless the relay check on this reconnect changed or a summary waits. A fresh one sent them from
// Replay, so only a change made after that (no peer was set for a resend to reach) goes here.
func (c *capsReporter) afterOnline(ctx context.Context, peer *runnerwire.Peer, resumed bool) {
	if !resumed && !c.dirty.Load() {
		return
	}
	go func() {
		if err := c.send(ctx, peer, true, false); err != nil {
			slog.WarnContext(ctx, "runner caps after resume failed", "error", err)
		}
	}()
}

// resendMacOS is the macOS VM handle's callback after its state changed.
func (c *capsReporter) resendMacOS(ctx context.Context) {
	resendMacOSCaps(ctx, c.online.Load(), &c.dirty, &c.last, c.send, c.ack, c.extra.macOSVM.current)
}

// resendRelay sends the caps again after the relay's status changed.
func (c *capsReporter) resendRelay(ctx context.Context) {
	if peer := c.online.Load(); peer != nil {
		if err := c.send(ctx, peer, false, true); err != nil {
			slog.WarnContext(ctx, "runner caps after a relay change failed", "error", err)
		}
	}
}
