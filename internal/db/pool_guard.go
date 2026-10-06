// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The nested-acquire guard: a test-only pool hook that fails a test binary when one goroutine asks
// a pool for a connection while it still holds another from the same pool. That is the shape of
// both 0.8.6 release-night deadlocks: on a small or busy pool the second wait never ends.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// nestedAcquireGuardEnv set to 1 turns the guard on for every pool Connect opens, and only inside a
// test binary: a production wardynd ignores it.
const nestedAcquireGuardEnv = "WARDYN_TEST_POOL_GUARD"

func nestedAcquireGuardOn() bool {
	return testing.Testing() && os.Getenv(nestedAcquireGuardEnv) == "1"
}

// allowedHold is a hold designed to keep one pool connection while the same goroutine takes
// another. path is the callers of the hold's Acquire, innermost first, from the first frame outside
// pgx: a package below the module, then a function name, which a frame matches as any one of its
// dot-separated segments, since a closure is named under its function and, once that function is
// inlined, under the caller too (reapTickLock's closure runs as <caller>.reapTickLock.func1).
type allowedHold struct {
	path   []string
	reason string
}

// nestedAcquireAllowlist: a connection acquired along one of these paths does not count as held, so
// a nested acquire under it passes, while any other connection the goroutine holds still counts.
var nestedAcquireAllowlist = []allowedHold{
	{[]string{"internal/db.TryAdvisoryLockConn", "internal/db.TryAdvisoryLock", "cmd/wardynd.reapTickLock"},
		"the reaper's tick lock keeps its connection while the tick prunes AWS SSO spent tokens and scans runs on the pool; one tick runs at a time, and TryAdvisoryLock documents its need of a pool of at least 2"},
	{[]string{"internal/db.TryAdvisoryLockConn", "internal/db.TryAdvisoryLock", "cmd/wardynd.terminalSandboxSweepTickLock"},
		"the terminal-sandbox sweep's tick lock, the reaper's shape: its connection is kept while the tick pages runs and tears sandboxes down on the pool"},
	{[]string{"internal/db.AdvisoryLockKeyed"},
		"the keyed lock keeps its connection while the caller's guarded work uses the pool; it is taken only with two connections free (advisoryLockFreeConnsNeeded) and one hold per process (advisoryLockGate)"},
	{[]string{"internal/store_test.TestPG_Devices_RefusedBatchReplayDoesNotStarveOrgAuditWriters"},
		"a hybrid device push: the test holds the audit chain lock, standing in for an organisation audit writer, while it pushes a refused batch, to prove the push never waits on that lock"},
}

// onNestedAcquire reports a finding. Exiting, not panicking: chi's Recoverer and the launch and
// sweep goroutines recover panics, and a recovered finding would let the suite pass.
var onNestedAcquire = func(finding string) {
	fmt.Fprint(os.Stderr, finding)
	os.Exit(2)
}

// nestedAcquireGuard is the pool's tracer. A holder is recorded by connection when an Acquire ends
// and cleared by connection when the connection is released, so a connection released by another
// goroutine clears its holder. Release is traced rather than hooked with Config.AfterRelease: that
// hook runs on a goroutine of its own after Release returns, and never when the pool destroys the
// connection (one released mid-transaction or closed), so a release followed by an acquire would
// read as a nested acquire. Acquire is traced at its start, before the pool waits, rather than with
// Config.BeforeAcquire, which runs only once a connection is free: a nested acquire on a full pool
// is reported instead of hanging.
//
// ponytail: keyed by goroutine, so a hold another goroutine takes on this goroutine's behalf (or a
// connection handed off and still in use) is not seen; a hijacked connection is never released, so
// it keeps counting; pools opened without Connect are not guarded.
type nestedAcquireGuard struct {
	mu   sync.Mutex
	held map[*pgx.Conn]poolHold
}

type poolHold struct {
	pool      *pgxpool.Pool
	goroutine uint64
	stack     []uintptr
}

func newNestedAcquireGuard() *nestedAcquireGuard {
	return &nestedAcquireGuard{held: map[*pgx.Conn]poolHold{}}
}

// TraceAcquireStart compares by pool as well as goroutine: a test that opens a second pool from
// pool.Config() shares this tracer, and holding one pool's connection while taking another's is
// not this deadlock.
func (g *nestedAcquireGuard) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	id := goroutineID()
	g.mu.Lock()
	var holds []poolHold
	for _, h := range g.held {
		if h.pool == pool && h.goroutine == id {
			holds = append(holds, h)
		}
	}
	g.mu.Unlock()
	holds = slices.DeleteFunc(holds, func(h poolHold) bool { return allowlistedHold(h.stack) })
	if len(holds) == 0 {
		return ctx
	}
	var b strings.Builder
	fmt.Fprintf(&b, "db: nested pool acquire (%s=1): goroutine %d asks the pool for a connection while it holds %d from the same pool\n", nestedAcquireGuardEnv, id, len(holds))
	for _, h := range holds {
		b.WriteString("held connection acquired at:\n")
		writeStack(&b, h.stack)
	}
	b.WriteString("second acquire at:\n")
	writeStack(&b, callers())
	onNestedAcquire(b.String())
	return ctx
}

func (g *nestedAcquireGuard) TraceAcquireEnd(_ context.Context, pool *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	if d.Err != nil || d.Conn == nil {
		return
	}
	h := poolHold{pool: pool, goroutine: goroutineID(), stack: callers()}
	g.mu.Lock()
	g.held[d.Conn] = h
	g.mu.Unlock()
}

// TraceRelease runs synchronously at the start of every Release, before the connection can go back
// to the pool and be acquired again.
func (g *nestedAcquireGuard) TraceRelease(_ *pgxpool.Pool, d pgxpool.TraceReleaseData) {
	g.mu.Lock()
	delete(g.held, d.Conn)
	g.mu.Unlock()
}

// TraceQueryStart and TraceQueryEnd exist because ConnConfig.Tracer is a pgx.QueryTracer.
func (g *nestedAcquireGuard) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (g *nestedAcquireGuard) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// goroutineID reads the running goroutine's id from the first line of its stack trace
// ("goroutine 123 [running]:"); the runtime exposes it nowhere else.
func goroutineID() uint64 {
	var buf [64]byte
	line := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	id, _ := strconv.ParseUint(string(line[:bytes.IndexByte(line, ' ')]), 10, 64)
	return id
}

// callers is the stack above the tracer method that calls it.
func callers() []uintptr {
	pcs := make([]uintptr, 64)
	return pcs[:runtime.Callers(3, pcs)]
}

const modulePath = "github.com/cjohnstoniv/wardyn/"

func allowlistedHold(stack []uintptr) bool {
	var outside []string
	frames := runtime.CallersFrames(stack)
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/jackc/pgx/") && !strings.HasPrefix(f.Function, "runtime.") {
			outside = append(outside, f.Function)
		}
		if !more {
			break
		}
	}
	return allowlisted(outside)
}

// allowlisted reports whether functions, a hold's callers innermost first, follow an allowlist path.
func allowlisted(functions []string) bool {
	return slices.ContainsFunc(nestedAcquireAllowlist, func(a allowedHold) bool {
		if len(a.path) > len(functions) {
			return false
		}
		for i, want := range a.path {
			pkg, name, _ := strings.Cut(want, ".")
			rest, ok := strings.CutPrefix(functions[i], modulePath+pkg+".")
			if !ok || !slices.Contains(strings.Split(rest, "."), name) {
				return false
			}
		}
		return true
	})
}

func writeStack(b *strings.Builder, stack []uintptr) {
	frames := runtime.CallersFrames(stack)
	for {
		f, more := frames.Next()
		fmt.Fprintf(b, "\t%s\n\t\t%s:%d\n", f.Function, f.File, f.Line)
		if !more {
			return
		}
	}
}
