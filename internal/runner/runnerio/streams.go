// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type attached struct {
	session runner.Session
	stream  *runnerwire.Stream
	cancel  context.CancelFunc
	once    sync.Once
}

func (a *attached) close() {
	a.once.Do(func() { a.cancel(); _ = a.session.Close(); _ = a.stream.Close() })
}

type execution struct {
	session        *runner.ExecSession
	stream, stderr *runnerwire.Stream
	cancel         context.CancelFunc
	once           sync.Once
	done           chan struct{}
	outputs        sync.WaitGroup
	release        func()
	localOnce      sync.Once
	exit           int
	err            error
}

func (e *execution) close() {
	e.once.Do(func() {
		e.stopLocal()
		e.release()
		_ = e.stream.Close()
		if e.stderr != nil {
			_ = e.stderr.Close()
		}
	})
}

func (s *Server) attach(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.AttachArgs](raw)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(s.life(peer))
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	sess, err := s.sub.Attach(life, ref, args.Options)
	if err != nil {
		cancel()
		return nil, err
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTY, Target: ref, Call: id})
	if err != nil {
		cancel()
		_ = sess.Close()
		return nil, err
	}
	a := &attached{session: sess, stream: st, cancel: cancel}
	if !stop() || ctx.Err() != nil {
		a.close()
		return nil, ctx.Err()
	}
	key := streamKey{peer, st.ID()}
	s.mu.Lock()
	if life.Err() != nil {
		s.mu.Unlock()
		a.close()
		return nil, runner.ErrRunnerOffline
	}
	s.sessions[key] = a
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); delete(s.sessions, key); s.mu.Unlock(); a.close() }()
		_, _ = io.Copy(writerFunc(sess.Write), st)
	}()
	go func() {
		_, err := io.Copy(st, readerFunc(sess.Read))
		if err != nil {
			_ = st.Reset(runnerwire.ResetInternal)
		} else {
			_ = st.CloseWrite()
		}
	}()
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

func (s *Server) execStream(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	select {
	case s.execSlots <- struct{}{}:
	default:
		return nil, runnerwire.Refuse("too many open exec sessions")
	}
	handed := false
	defer func() {
		if !handed {
			<-s.execSlots
		}
	}()

	args, err := decode[runnerwire.ExecStreamArgs](raw)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(s.life(peer))
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	sess, err := s.sub.ExecStream(life, ref, args.Spec)
	if err != nil {
		cancel()
		return nil, err
	}
	if sess == nil || sess.Stdin == nil || sess.Stdout == nil || sess.Wait == nil || sess.Close == nil || sess.Resize == nil {
		cancel()
		if sess != nil && sess.Close != nil {
			_ = sess.Close()
		}
		return nil, errors.New("runnerio: incomplete exec session")
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindExec, Target: ref, Call: id})
	if err != nil {
		cancel()
		_ = sess.Close()
		return nil, err
	}
	e := &execution{session: sess, stream: st, cancel: cancel, done: make(chan struct{}), release: func() {
		if handed {
			<-s.execSlots
		}
	}}
	if sess.Stderr != nil {
		e.stderr, err = peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindExecStderr, Target: ref, Call: id})
		if err != nil {
			e.close()
			return nil, err
		}
	}
	if !stop() || ctx.Err() != nil {
		e.close()
		return nil, ctx.Err()
	}
	key := streamKey{peer, st.ID()}
	s.mu.Lock()
	if life.Err() != nil {
		s.mu.Unlock()
		e.close()
		return nil, runner.ErrRunnerOffline
	}
	s.execs[key] = e
	handed = true
	s.mu.Unlock()
	e.outputs.Add(1)
	if e.stderr != nil {
		e.outputs.Add(1)
	}
	go func() { e.exit, e.err = sess.Wait(); close(e.done) }()
	go s.watchExec(key, e)
	go func() {
		_, err := io.Copy(sess.Stdin, st)
		_ = sess.Stdin.Close()
		if err != nil {
			s.removeExec(key, e)
		}
	}()
	go s.copyExecOutput(key, e, st, sess.Stdout)
	if e.stderr != nil {
		go s.copyExecOutput(key, e, e.stderr, sess.Stderr)
	}
	result := runnerwire.ExecStreamResult{Exec: st.ID()}
	if e.stderr != nil {
		result.Stderr = e.stderr.ID()
	}
	return result, nil
}

func (s *Server) copyExecOutput(key streamKey, e *execution, st *runnerwire.Stream, src io.Reader) {
	defer e.outputs.Done()
	_, err := io.Copy(st, src)
	if err != nil {
		s.removeExec(key, e)
		return
	}
	_ = st.CloseWrite()
}

func (s *Server) removeExec(key streamKey, e *execution) {
	s.mu.Lock()
	if s.execs[key] == e {
		delete(s.execs, key)
	}
	s.mu.Unlock()
	e.close()
}

func (s *Server) resize(ctx context.Context, peer *runnerwire.Peer, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.ResizeArgs](raw)
	if err != nil {
		return nil, err
	}
	key := streamKey{peer, args.Stream}
	s.mu.Lock()
	a, e := s.sessions[key], s.execs[key]
	s.mu.Unlock()
	if a != nil {
		return nil, a.session.Resize(ctx, args.Cols, args.Rows)
	}
	if e != nil {
		return nil, e.session.Resize(args.Cols, args.Rows)
	}
	return nil, runner.ErrSandboxGone
}

func (s *Server) execWait(ctx context.Context, peer *runnerwire.Peer, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.ExecWaitArgs](raw)
	if err != nil {
		return nil, err
	}
	key := streamKey{peer, args.Stream}
	s.mu.Lock()
	e := s.execs[key]
	s.mu.Unlock()
	if e == nil {
		return nil, runner.ErrSandboxGone
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.done:
		return runnerwire.WaitResult{ExitCode: e.exit}, e.err
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// Completed execs retain their bounded exit result until Close. Explicit
// exec_close also works after graceful CLOSE removed streams from the wire table.
func (s *Server) execClose(peer *runnerwire.Peer, raw json.RawMessage) error {
	args, err := decode[runnerwire.ExecWaitArgs](raw)
	if err != nil {
		return err
	}
	key := streamKey{peer, args.Stream}
	s.mu.Lock()
	e := s.execs[key]
	s.mu.Unlock()
	if e != nil {
		s.removeExec(key, e)
	}
	return nil
}

func (s *Server) watchExec(key streamKey, e *execution) {
	<-e.stream.Done()
	if e.stream.Err() != nil {
		s.removeExec(key, e)
		return
	}
	if e.stderr != nil {
		<-e.stderr.Done()
		if e.stderr.Err() != nil {
			s.removeExec(key, e)
			return
		}
	}
	// Both wire directions have closed gracefully. Bytes already received by
	// the org remain readable; release local resources without sending RESET.
	e.outputs.Wait()
	<-e.done
	e.stopLocal()
}

func (e *execution) stopLocal() {
	e.localOnce.Do(func() { e.cancel(); _ = e.session.Close() })
}
