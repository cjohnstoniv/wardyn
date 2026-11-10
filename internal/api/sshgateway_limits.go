// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// The bounds a refusal names, in its warning and its metric label.
const (
	sshBoundHandshake   = "handshake"
	sshBoundSource      = "source"
	sshBoundEstablished = "established"
	sshBoundPrincipal   = "principal"
)

// sshConnLimits is the gateway's connection bounds (maxSSHHandshakes, the
// per-source share of it, Config.SSHMaxConnections) and the throttled warning
// a refusal at any of them writes.
type sshConnLimits struct {
	handshakes, established chan struct{}
	perSource               int
	sourcesMu               sync.Mutex
	sources                 map[string]int // handshakes in progress per source, under sourcesMu
	perPrincipal            int
	principalsMu            sync.Mutex
	principals              map[string]int // established connections per principal, under principalsMu
	refusals                map[string]int // refusal-grace connections per principal, under principalsMu
	refusing                int            // refusal-grace connections in all, under principalsMu
	metrics                 *metrics
	lastWarn                atomic.Int64 // unix nanos of the last warning
	quiet                   atomic.Int64 // refusals since it
}

// enterSource takes one of src's handshake slots; false when it holds them all.
func (l *sshConnLimits) enterSource(src string) bool {
	l.sourcesMu.Lock()
	defer l.sourcesMu.Unlock()
	if l.sources[src] >= l.perSource {
		return false
	}
	l.sources[src]++
	return true
}

// enterPrincipal takes one of principal's established-connection slots; false when it holds them all.
func (l *sshConnLimits) enterPrincipal(principal string) bool {
	l.principalsMu.Lock()
	defer l.principalsMu.Unlock()
	if l.principals[principal] >= l.perPrincipal {
		return false
	}
	l.principals[principal]++
	return true
}

// enterRefusal takes a refusal-grace slot: one per principal and sshRefusalPool in all. A refused
// connection that finds none is closed at once, so the grace can never hold more than the pool.
func (l *sshConnLimits) enterRefusal(principal string) bool {
	l.principalsMu.Lock()
	defer l.principalsMu.Unlock()
	if l.refusing >= sshRefusalPool || l.refusals[principal] >= 1 {
		return false
	}
	if l.refusals == nil {
		l.refusals = map[string]int{}
	}
	l.refusals[principal]++
	l.refusing++
	return true
}

func (l *sshConnLimits) leaveRefusal(principal string) {
	l.principalsMu.Lock()
	defer l.principalsMu.Unlock()
	if l.refusals[principal]--; l.refusals[principal] <= 0 {
		delete(l.refusals, principal)
	}
	l.refusing--
}

func (l *sshConnLimits) leavePrincipal(principal string) {
	l.principalsMu.Lock()
	defer l.principalsMu.Unlock()
	if l.principals[principal]--; l.principals[principal] <= 0 {
		delete(l.principals, principal)
	}
}

func (l *sshConnLimits) leaveSource(src string) {
	l.sourcesMu.Lock()
	defer l.sourcesMu.Unlock()
	if l.sources[src]--; l.sources[src] <= 0 {
		delete(l.sources, src)
	}
}

// refused records one connection closed at a cap: the
// wardyn_ssh_connections_refused_total count, and a warning at most every
// sshCapWarnEvery carrying how many were refused since the last one.
func (l *sshConnLimits) refused(bound string, limit int) {
	l.metrics.sshRefusedInc(bound)
	n := l.quiet.Add(1)
	now, last := time.Now().UnixNano(), l.lastWarn.Load()
	if now-last < int64(sshCapWarnEvery) || !l.lastWarn.CompareAndSwap(last, now) {
		return
	}
	l.quiet.Add(-n)
	slog.Warn("ssh gateway: connection refused at a cap", slog.String("bound", bound),
		slog.Int("limit", limit), slog.Int64("refused", n))
}

// sshCapWarnEvery spaces the cap-refusal warnings, so a flood writes a line a
// minute rather than a line a connection.
const sshCapWarnEvery = time.Minute

// sshRefusalGrace is how long a connection refused over its person's share is kept for the client's
// first channel to carry the reason; a client that opens none is closed after it. sshRefusalPool
// bounds those connections in all (and one per person), so the wait cannot be used to hold many.
// Like the keepalive, the grace is an atomic only so a test can shrink it.
const (
	sshRefusalGrace = 5 * time.Second
	sshRefusalPool  = 8
)

var sshRefusalGraceNS = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(sshRefusalGrace))
	return &v
}()

func sshRefusalGraceFor() time.Duration { return time.Duration(sshRefusalGraceNS.Load()) }

func setSSHRefusalGrace(d time.Duration) (prev time.Duration) {
	return time.Duration(sshRefusalGraceNS.Swap(int64(d)))
}
