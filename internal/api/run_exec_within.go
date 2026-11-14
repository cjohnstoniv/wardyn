// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// waitExecWithin reads at most limit bytes of sess's stdout (none when limit is 0), then waits for the exec to end,
// and returns when it ends or ctx does, whichever comes first. A driver's Wait need not watch the context the exec
// was started with (the remote runner's waits on its own), so a caller under a run's lock bounds the wait here and
// is never held by an exec that does not end. When ctx ends first it answers ctx's error with exit code -1, and the
// read and the wait end when the caller's sess.Close tears the stream down.
func waitExecWithin(ctx context.Context, sess *runner.ExecSession, limit int64) (string, int, error) {
	type result struct {
		out  string
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var out []byte
		if limit > 0 && sess.Stdout != nil {
			out, _ = io.ReadAll(io.LimitReader(sess.Stdout, limit))
		}
		code, err := sess.Wait()
		done <- result{string(out), code, err}
	}()
	select {
	case r := <-done:
		return r.out, r.code, r.err
	case <-ctx.Done():
		return "", -1, ctx.Err()
	}
}
