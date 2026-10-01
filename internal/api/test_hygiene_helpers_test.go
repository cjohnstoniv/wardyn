// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// testBaseCtx is a daemon-lifetime BaseCtx bounded by the test. New defaults a
// nil BaseCtx to context.Background(), so a run-watcher or sweeper goroutine a
// test starts through a configured Runner would outlive the test, keep logging
// into whatever slog default a LATER test installed, and race its buffer under
// -race (#1315). Every New(Config{Runner: ...}) in this package passes it.
func testBaseCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// lockedBuffer is a bytes.Buffer safe to install behind the process-global slog
// default. slog.SetDefault is PROCESS-global: for as long as it is installed,
// any goroutine an earlier test left running also writes into it, so a bare
// bytes.Buffer is a data race with the String() read — red under `-race`
// whenever such a goroutine logs inside the window, green otherwise (#1278,
// #1315). Cancelling testBaseCtx does not join those goroutines, which is why
// the buffer is locked rather than the goroutines being awaited.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
