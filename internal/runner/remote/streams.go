// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Attach opens a pty stream on the runner. Close resets the stream only.
func (s *Substrate) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	if err := s.call(ctx, runnerwire.MethodAttach, runnerwire.AttachArgs{Ref: ref, Options: opts}, &res); err != nil {
		return nil, err
	}
	st, err := s.t.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	return &ptySession{s: s, st: st}, nil
}

type ptySession struct {
	s  *Substrate
	st *runnerwire.Stream
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.st.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.st.Write(b) }
func (p *ptySession) Close() error                { return p.st.Close() }
func (p *ptySession) Resize(ctx context.Context, cols, rows uint16) error {
	return p.s.call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: p.st.ID(), Cols: cols, Rows: rows}, nil)
}

// ExecStream starts an exec on the runner: stdin and stdout share the `exec`
// stream (stdin half-close is CLOSE) and stderr is its own stream, drained
// concurrently by the caller as runner.ExecSession requires.
func (s *Substrate) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	var res runnerwire.ExecStreamResult
	if err := s.call(ctx, runnerwire.MethodExecStream, runnerwire.ExecStreamArgs{Ref: ref, Spec: spec}, &res); err != nil {
		return nil, err
	}
	ex, err := s.t.TakeStream(res.Exec)
	if err != nil {
		return nil, err
	}
	var stderr *runnerwire.Stream
	if res.Stderr != 0 {
		if stderr, err = s.t.TakeStream(res.Stderr); err != nil {
			_ = ex.Close()
			return nil, err
		}
	}
	if stderr != nil {
		_ = stderr.CloseWrite()
	}
	var closeOnce sync.Once
	var closeErr error
	sess := &runner.ExecSession{
		Stdin:  stdin{ex},
		Stdout: ex,
		Resize: func(cols, rows uint16) error {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			return s.call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: ex.ID(), Cols: cols, Rows: rows}, nil)
		},
		Wait: func() (int, error) {
			var w runnerwire.WaitResult
			err := s.call(context.Background(), runnerwire.MethodExecWait, runnerwire.ExecWaitArgs{Stream: ex.ID()}, &w)
			return w.ExitCode, err
		},
		Close: func() error {
			closeOnce.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
				defer cancel()
				closeErr = s.call(ctx, runnerwire.MethodExecClose, runnerwire.ExecWaitArgs{Stream: ex.ID()}, nil)
				if stderr != nil {
					closeErr = errors.Join(closeErr, stderr.Close())
				}
				closeErr = errors.Join(closeErr, ex.Close())
			})
			return closeErr
		},
	}
	if stderr != nil {
		sess.Stderr = stderr
	}
	return sess, nil
}

// stdin writes to the exec stream; Close half-closes it, leaving stdout readable.
type stdin struct{ st *runnerwire.Stream }

func (s stdin) Write(b []byte) (int, error) { return s.st.Write(b) }
func (s stdin) Close() error                { return s.st.CloseWrite() }

var _ io.WriteCloser = stdin{}
