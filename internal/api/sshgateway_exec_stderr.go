// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The stderr half of an SSH exec bridge: draining the sandbox's stderr to the
// channel, and how long the bridge waits for it once stdout has ended.
package api

import (
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// drainExecStderr starts copying sess.Stderr to the channel's own extended-
// data stderr stream IMMEDIATELY — before the caller ever touches Stdout.
//
// HARD CONTRACT (runner.ExecSession's doc): Stdout/Stderr are UNBUFFERED
// io.Pipes fed by ONE demux goroutine; a single undrained stderr byte blocks
// that goroutine, Stdout, AND Wait. Every ExecStream consumer here (exec,
// sftp, direct-tcpip/socat) MUST start this before the first Stdout read;
// sftp-server's error path writes stderr before any stdout. The returned
// channel closes when the copy ends (already closed with no Stderr), so the
// caller can hold exit-status and Close for the stderr tail.
func drainExecStderr(channel ssh.Channel, sess *runner.ExecSession) <-chan struct{} {
	done := make(chan struct{})
	if sess.Stderr == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		_, _ = io.Copy(channel.Stderr(), sess.Stderr)
	}()
	return done
}

// execStderrTailGraceNS bounds the wait for stderr to end after stdout did (also
// ended by ctx.Done()), so a client that stopped reading cannot hold the bridge. It is
// not waited at all once the channel refused a write.
// The copy itself ends when the client answers the Close or the connection
// ends. An atomic only so a test can shrink it.
var execStderrTailGraceNS = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(5 * time.Second))
	return &v
}()

// firstWriteErrWriter passes writes through and keeps the first error one returns, so the
// caller can tell a copy that ended on the destination from one that ended on its source.
type firstWriteErrWriter struct {
	w   io.Writer
	err error
}

func (f *firstWriteErrWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}
