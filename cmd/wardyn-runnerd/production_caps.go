// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
// is stale. The fallback reads the last caps and the macOS VM state and acks them under sendMu, the lock
// send holds from capture through ack, so a send that lands meanwhile is never overwritten by the older
// caps; send has returned by then, so the lock is never taken twice.
func resendMacOSCaps(ctx context.Context, peer *runnerwire.Peer, dirty *atomic.Bool, last *atomic.Pointer[runnerwire.Caps],
	sendMu *sync.Mutex, send func(context.Context, *runnerwire.Peer, bool, bool) error,
	ack func(context.Context, *runnerwire.Peer, runnerwire.Caps) error, current func() *runnerwire.MacVMCaps) {
	dirty.Store(true)
	if peer == nil {
		return
	}
	err := send(ctx, peer, false, true)
	if errors.Is(err, errHostCaps) {
		sendMu.Lock()
		if prev := last.Load(); prev != nil {
			c := *prev
			c.MacOSVM = current()
			err = ack(ctx, peer, c)
		}
		sendMu.Unlock()
	}
	if err != nil {
		slog.WarnContext(ctx, "runner caps after a macOS VM change failed", "error", err)
	}
}

// capsReporter sends the runner's caps to its organisation: on a fresh session, after a resume that has
// something new, and when a macOS VM, the relay or the Kubernetes NetworkPolicy verdict changes.
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
	// netPol holds one mark from netPolChanged until resendNetPol takes it.
	netPol chan struct{}
	// sendMu holds each send from capturing its caps through their ack, whatever triggered it (a
	// session, a resume, the relay, a macOS VM or a NetworkPolicy change), so an older capture is never
	// acked after a newer one.
	sendMu sync.Mutex
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
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
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
	caps.BoundFolders = c.extra.boundFolders
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
	resendMacOSCaps(ctx, c.online.Load(), &c.dirty, &c.last, &c.sendMu, c.send, c.ack, c.extra.macOSVM.current)
}

// resendRelay sends the caps again after the relay's status changed.
func (c *capsReporter) resendRelay(ctx context.Context) {
	if peer := c.online.Load(); peer != nil {
		if err := c.send(ctx, peer, false, true); err != nil {
			slog.WarnContext(ctx, "runner caps after a relay change failed", "error", err)
		}
	}
}

// netPolChanged is the Kubernetes driver's hook after a canary re-check changed its verdict, or the time
// of the check behind it. It only marks the change, so the re-check loop never waits on the network;
// marks made before resendNetPol takes one are sent as one.
func (c *capsReporter) netPolChanged() {
	select {
	case c.netPol <- struct{}{}:
	default:
	}
}

// resendNetPol sends the caps again after each netPolChanged mark until ctx ends, so the organisation's
// health and audit see a re-check's verdict mid-session. With no session the caps are only marked
// unsent, and the next session sends them.
func (c *capsReporter) resendNetPol(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.netPol:
		}
		c.dirty.Store(true)
		if peer := c.online.Load(); peer != nil {
			if err := c.send(ctx, peer, false, true); err != nil {
				slog.WarnContext(ctx, "runner caps after a NetworkPolicy verdict change failed", "error", err)
			}
		}
	}
}
