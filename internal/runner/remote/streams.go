// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// pin is the session a new stream is opened, claimed and controlled on (Link.pinSession). A transport that
// cannot pin (a test double) is its own one session.
func (s *Substrate) pin() (session, error) {
	if p, ok := s.t.(interface{ pinSession() (session, error) }); ok {
		return p.pinSession()
	}
	return s.t, nil
}

// Attach opens a pty stream on the runner. Close resets the stream only.
func (s *Substrate) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodAttach, runnerwire.AttachArgs{Ref: ref, Options: opts}, &res); err != nil {
		return nil, err
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	return &ptySession{sess: sess, st: st}, nil
}

type ptySession struct {
	sess session
	st   *runnerwire.Stream
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.st.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.st.Write(b) }
func (p *ptySession) Close() error                { return p.st.Close() }
func (p *ptySession) Resize(ctx context.Context, cols, rows uint16) error {
	return p.sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: p.st.ID(), Cols: cols, Rows: rows}, nil)
}

// DialDesktop opens a VM sandbox's display on the runner (runner.DesktopDialer). A runner whose caps do
// not offer it is refused here, so a runner that predates the call never sees it.
func (s *Substrate) DialDesktop(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	if c, ok := s.Caps(); !ok || c.LinuxVM == nil || !c.LinuxVM.Desktop {
		return nil, runner.ErrDesktopUnsupported
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodDialDesktop, runnerwire.DialDesktopArgs{Ref: ref}, &res); err != nil {
		return nil, err
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// LocalAttach asks the runner to attach the CLI holding req.Nonce on the runner's own computer. A
// runner that does not advertise Caps.LocalAttach is refused here, before any call. The tee stream
// answers the call; any other kind of stream is closed and refused.
func (s *Substrate) LocalAttach(ctx context.Context, ref string, req runner.LocalAttachRequest) (runner.LocalSession, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	if c, ok := s.Caps(); !ok || !c.LocalAttach {
		return nil, fmt.Errorf("%w: upgrade the runner", runner.ErrLocalAttachUnsupported)
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodLocalAttach, runnerwire.LocalAttachArgs{Ref: ref, Nonce: req.Nonce, Options: req.Options, Tee: req.Tee}, &res); err != nil {
		return nil, err
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	if st.Kind() != runnerwire.KindPTYTee {
		_ = st.Close()
		return nil, runnerwire.Refuse("the runner answered local attach with a %q stream", st.Kind())
	}
	return &localSession{sess: sess, st: st, tee: req.Tee}, nil
}

// localSession is the org's end of one local attach: the pty_tee stream. Closing it resets the
// stream, which is how the org ends the session.
type localSession struct {
	sess  session
	st    *runnerwire.Stream
	tee   bool
	read  atomic.Int64 // bytes Read has returned: the most AckTee may acknowledge
	acked atomic.Int64 // the highest offset the runner has been told
}

func (l *localSession) Read(b []byte) (int, error) {
	n, err := l.st.Read(b)
	l.read.Add(int64(n))
	return n, err
}

func (l *localSession) Close() error { return l.st.Close() }

// SetObserver fails closed: a change the runner did not acknowledge may have left it in the other
// mode, so the stream is reset and the runner ends the PTY.
func (l *localSession) SetObserver(ctx context.Context, observer bool, cols, rows uint16) error {
	err := l.sess.Call(ctx, runnerwire.MethodLocalAttachMode, runnerwire.LocalAttachModeArgs{Stream: l.st.ID(), Observer: observer, Cols: cols, Rows: rows}, nil)
	if err != nil {
		_ = l.st.Close()
	}
	return err
}

func (l *localSession) AckTee(ctx context.Context, offset int64) error {
	if !l.tee {
		return errors.New("remote: local attach without a tee has nothing to acknowledge")
	}
	if offset < 0 || offset > l.read.Load() {
		return fmt.Errorf("remote: tee ack %d is past the %d bytes read", offset, l.read.Load())
	}
	if offset <= l.acked.Load() { // cumulative: nothing new to tell the runner
		return nil
	}
	if err := l.sess.Call(ctx, runnerwire.MethodLocalAttachTeeAck, runnerwire.LocalAttachTeeAckArgs{Stream: l.st.ID(), Offset: offset}, nil); err != nil {
		return err
	}
	for cur := l.acked.Load(); offset > cur && !l.acked.CompareAndSwap(cur, offset); cur = l.acked.Load() {
	}
	return nil
}

// ExecStream starts an exec on the runner: stdin and stdout share the `exec`
// stream (stdin half-close is CLOSE) and stderr is its own stream, drained
// concurrently by the caller as runner.ExecSession requires.
func (s *Substrate) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.ExecStreamResult
	if err := sess.Call(ctx, runnerwire.MethodExecStream, runnerwire.ExecStreamArgs{Ref: ref, Spec: spec}, &res); err != nil {
		return nil, err
	}
	ex, err := sess.TakeStream(res.Exec)
	if err != nil {
		return nil, err
	}
	var stderr *runnerwire.Stream
	if res.Stderr != 0 {
		if stderr, err = sess.TakeStream(res.Stderr); err != nil {
			_ = ex.Close()
			return nil, err
		}
	}
	if stderr != nil {
		_ = stderr.CloseWrite()
	}
	var closeOnce sync.Once
	var closeErr error
	es := &runner.ExecSession{
		Stdin:  stdin{ex},
		Stdout: ex,
		Resize: func(cols, rows uint16) error {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			return sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: ex.ID(), Cols: cols, Rows: rows}, nil)
		},
		Wait: func() (int, error) {
			var w runnerwire.WaitResult
			err := sess.Call(context.Background(), runnerwire.MethodExecWait, runnerwire.ExecWaitArgs{Stream: ex.ID()}, &w)
			return w.ExitCode, err
		},
		Close: func() error {
			closeOnce.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
				defer cancel()
				closeErr = sess.Call(ctx, runnerwire.MethodExecClose, runnerwire.ExecWaitArgs{Stream: ex.ID()}, nil)
				if stderr != nil {
					closeErr = errors.Join(closeErr, stderr.Close())
				}
				closeErr = errors.Join(closeErr, ex.Close())
			})
			return closeErr
		},
	}
	if stderr != nil {
		es.Stderr = stderr
	}
	return es, nil
}

// stdin writes to the exec stream; Close half-closes it, leaving stdout readable.
type stdin struct{ st *runnerwire.Stream }

func (s stdin) Write(b []byte) (int, error) { return s.st.Write(b) }
func (s stdin) Close() error                { return s.st.CloseWrite() }

var _ io.WriteCloser = stdin{}
