// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnertest serves the runner side of the stream over a supplied
// substrate.Substrate (Docker, or the Fake in this package), so the org side is
// built and tested against the real wire without a wardyn-runnerd.
package runnertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Server is the runner end of one session.
type Server struct {
	Peer     *runnerwire.Peer
	runnerID string
	sub      substrate.Substrate

	mu       sync.Mutex
	sessions map[uint32]runner.Session
	execs    map[uint32]*runner.ExecSession
	// Resident holds the values delivered through deliver_resident, by run id.
	Resident map[uuid.UUID][]runnerwire.DeliverResidentArgs
	Erased   []uuid.UUID
}

// Serve runs the runner side of a session over conn, backed by sub, until the
// link ends. It does not publish capabilities; call PublishCaps.
func Serve(ctx context.Context, conn runnerwire.Conn, runnerID string, sub substrate.Substrate) *Server {
	s := &Server{runnerID: runnerID, sub: sub, sessions: map[uint32]runner.Session{}, execs: map[uint32]*runner.ExecSession{},
		Resident: map[uuid.UUID][]runnerwire.DeliverResidentArgs{}}
	s.Peer = runnerwire.NewPeer(conn, runnerwire.PeerConfig{OnCall: s.handle})
	go func() { _ = s.Peer.Run(ctx) }()
	return s
}

// PublishCaps sends the `caps` event: the backing substrate's classes plus the
// capacity and roots given.
func (s *Server) PublishCaps(ctx context.Context, capacity placement.Capacity, roots []string, version string) error {
	cs, err := s.sub.Classes(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(runnerwire.Caps{Support: cs, Capacity: capacity, AllowedRoots: roots, Version: version})
	if err != nil {
		return err
	}
	return s.Peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
}

func (s *Server) local(ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, placement.RunnerRefPrefix(s.runnerID))
	if !ok {
		return "", runnerwire.Refuse("ref %q is not this runner's", ref)
	}
	return rest, nil
}

func decode[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 {
		return v, nil
	}
	return v, json.Unmarshal(args, &v)
}

func (s *Server) handle(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
	switch method {
	case runnerwire.MethodCreateSandbox:
		return s.create(ctx, id, args)
	case runnerwire.MethodAttach:
		return s.attach(ctx, id, args)
	case runnerwire.MethodExecStream:
		return s.execStream(ctx, id, args)
	case runnerwire.MethodRecoverOutput:
		return s.recoverOutput(ctx, id, args)
	case runnerwire.MethodResize:
		return s.resize(args)
	case runnerwire.MethodExecWait:
		return s.execWait(args)
	case runnerwire.MethodExecClose:
		a, err := decode[runnerwire.ExecWaitArgs](args)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		e := s.execs[a.Stream]
		delete(s.execs, a.Stream)
		s.mu.Unlock()
		if e != nil {
			return nil, e.Close()
		}
		return nil, nil
	case runnerwire.MethodOutputAck:
		// This fixture has no persistent output spool. Production runnerio tests
		// exercise cursor persistence, reconnect and byte-range validation.
		a, err := decode[runnerwire.OutputAckArgs](args)
		if err != nil {
			return nil, err
		}
		if a.RunID == uuid.Nil || a.Offset < 0 {
			return nil, runnerwire.Refuse("invalid output acknowledgment")
		}
		return nil, nil
	case runnerwire.MethodSweep:
		return s.sweep(ctx, args)
	case runnerwire.MethodDeliverResident:
		a, err := decode[runnerwire.DeliverResidentArgs](args)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.Resident[a.RunID] = append(s.Resident[a.RunID], a)
		s.mu.Unlock()
		return nil, nil
	case runnerwire.MethodEraseResident:
		a, err := decode[runnerwire.EraseResidentArgs](args)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.Resident, a.RunID)
		s.Erased = append(s.Erased, a.RunID)
		s.mu.Unlock()
		return nil, nil
	case runnerwire.MethodProbeDrive:
		p, ok := s.sub.(runner.DriveProber)
		if !ok {
			return nil, errors.New("drive probing unsupported")
		}
		m, err := decode[types.DriveMount](args)
		if err != nil {
			return nil, err
		}
		return p.ProbeDrive(ctx, m)
	case runnerwire.MethodEnsureProxyImg:
		rv, ok := s.sub.(runner.ProxyReviver)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		return nil, rv.EnsureProxyImage(ctx)
	}
	return s.refMethod(ctx, method, args)
}

// refMethod serves the verbs whose only argument is a ref.
func (s *Server) refMethod(ctx context.Context, method string, args json.RawMessage) (any, error) {
	switch method {
	case runnerwire.MethodExec:
		a, err := decode[runnerwire.ExecArgs](args)
		if err != nil {
			return nil, err
		}
		ref, err := s.local(a.Ref)
		if err != nil {
			return nil, err
		}
		id, err := s.sub.Exec(ctx, ref, a.Argv)
		return runnerwire.ExecResult{ExecID: id}, err
	case runnerwire.MethodAgentStatus:
		a, err := decode[runnerwire.AgentStatusArgs](args)
		if err != nil {
			return nil, err
		}
		ref, err := s.local(a.Ref)
		if err != nil {
			return nil, err
		}
		return s.sub.AgentStatus(ctx, ref, a.AgentExecID)
	case runnerwire.MethodReplaceProxy:
		a, err := decode[runnerwire.ReplaceProxyArgs](args)
		if err != nil {
			return nil, err
		}
		ref, err := s.local(a.Ref)
		if err != nil {
			return nil, err
		}
		rv, ok := s.sub.(runner.ProxyReviver)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		return nil, rv.ReplaceProxy(ctx, ref, a.CfgJSON)
	}
	a, err := decode[runnerwire.RefArgs](args)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(a.Ref)
	if err != nil {
		return nil, err
	}
	switch method {
	case runnerwire.MethodWait:
		code, err := s.sub.Wait(ctx, ref)
		return runnerwire.WaitResult{ExitCode: code}, err
	case runnerwire.MethodStatus:
		return s.sub.Status(ctx, ref)
	case runnerwire.MethodStop:
		return nil, s.sub.StopSandbox(ctx, ref)
	case runnerwire.MethodKill:
		return nil, s.sub.KillSandbox(ctx, ref)
	case runnerwire.MethodEnd:
		e, ok := s.sub.(runner.SandboxEnder)
		if !ok {
			return nil, runner.ErrEndUnsupported
		}
		return nil, e.EndSandbox(ctx, ref)
	case runnerwire.MethodStopProxy:
		p, ok := s.sub.(runner.ProxyStopper)
		if !ok {
			return nil, runner.ErrEndUnsupported
		}
		return nil, p.StopProxy(ctx, ref)
	case runnerwire.MethodStart:
		st, ok := s.sub.(runner.SandboxStarter)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		return nil, st.StartSandbox(ctx, ref)
	case runnerwire.MethodCanReplace:
		rv, ok := s.sub.(runner.ProxyReviver)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		return nil, rv.CanReplaceProxy(ctx, ref)
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

func (s *Server) create(ctx context.Context, id uint32, args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.CreateSandboxArgs](args)
	if err != nil {
		return nil, err
	}
	spec := a.Spec
	spec.OnWaiting = func(detail string) {
		data, _ := json.Marshal(runnerwire.Waiting{Detail: detail})
		_ = s.Peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventWaiting, Call: id, RunID: &spec.RunID, Data: data})
	}
	if a.ExecOutput {
		spec.ExecOutput = &outputWriter{s: s, runID: spec.RunID}
	}
	sb, err := s.sub.CreateSandbox(ctx, spec)
	if err != nil {
		return nil, err
	}
	sb.Ref = placement.RunnerRefPrefix(s.runnerID) + sb.Ref
	return runnerwire.CreateSandboxResult{Sandbox: sb}, nil
}

// outputWriter opens the runner's `output` stream on first write, named by run
// id, and closes its write side when the driver's copy ends.
type outputWriter struct {
	s     *Server
	runID uuid.UUID
	once  sync.Once
	st    *runnerwire.Stream
	err   error
}

func (w *outputWriter) Write(b []byte) (int, error) {
	w.once.Do(func() {
		w.st, w.err = w.s.Peer.Open(context.Background(), runnerwire.Open{Kind: runnerwire.KindOutput, Target: w.runID.String()})
	})
	if w.err != nil {
		return 0, w.err
	}
	return w.st.Write(b)
}

func (w *outputWriter) Close() error {
	if w.st != nil {
		return w.st.CloseWrite()
	}
	return nil
}

func (s *Server) attach(ctx context.Context, id uint32, args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.AttachArgs](args)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(a.Ref)
	if err != nil {
		return nil, err
	}
	sess, err := s.sub.Attach(ctx, ref, a.Options)
	if err != nil {
		return nil, err
	}
	st, err := s.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTY, Target: ref, Call: id})
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	s.mu.Lock()
	s.sessions[st.ID()] = sess
	s.mu.Unlock()
	go func() { // stream -> pty
		_, _ = io.Copy(writerFunc(sess.Write), st)
		_ = sess.Close()
	}()
	go func() { // pty -> stream
		_, _ = io.Copy(st, readerFunc(sess.Read))
		_ = st.CloseWrite()
	}()
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

func (s *Server) resize(args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.ResizeArgs](args)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	sess, ex := s.sessions[a.Stream], s.execs[a.Stream]
	s.mu.Unlock()
	switch {
	case sess != nil:
		return nil, sess.Resize(context.Background(), a.Cols, a.Rows)
	case ex != nil:
		return nil, ex.Resize(a.Cols, a.Rows)
	}
	return nil, runner.ErrSandboxGone
}

func (s *Server) execStream(ctx context.Context, id uint32, args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.ExecStreamArgs](args)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(a.Ref)
	if err != nil {
		return nil, err
	}
	es, err := s.sub.ExecStream(ctx, ref, a.Spec)
	if err != nil {
		return nil, err
	}
	st, err := s.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindExec, Target: ref, Call: id})
	if err != nil {
		_ = es.Close()
		return nil, err
	}
	res := runnerwire.ExecStreamResult{Exec: st.ID()}
	var errSt *runnerwire.Stream
	if es.Stderr != nil {
		if errSt, err = s.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindExecStderr, Target: ref, Call: id}); err != nil {
			_ = es.Close()
			_ = st.Close()
			return nil, err
		}
		res.Stderr = errSt.ID()
	}
	s.mu.Lock()
	s.execs[st.ID()] = es
	s.mu.Unlock()
	go func() { // exec stdout -> stream, then the half-close the caller reads as EOF
		_, _ = io.Copy(st, es.Stdout)
		_ = st.CloseWrite()
	}()
	if errSt != nil {
		go func() {
			_, _ = io.Copy(errSt, es.Stderr)
			_ = errSt.CloseWrite()
		}()
	}
	go func() { // stream -> exec stdin; the caller's half-close ends stdin
		_, _ = io.Copy(es.Stdin, st)
		_ = es.Stdin.Close()
	}()
	return res, nil
}

func (s *Server) execWait(args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.ExecWaitArgs](args)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ex := s.execs[a.Stream]
	s.mu.Unlock()
	if ex == nil {
		return nil, runner.ErrSandboxGone
	}
	code, err := ex.Wait()
	return runnerwire.WaitResult{ExitCode: code}, err
}

func (s *Server) recoverOutput(ctx context.Context, id uint32, args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.RefArgs](args)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(a.Ref)
	if err != nil {
		return nil, err
	}
	rc, ok := s.sub.(runner.OutputRecoverer)
	if !ok {
		return runnerwire.RecoverResult{Unrecoverable: true}, nil
	}
	st, err := s.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: ref, Call: id})
	if err != nil {
		return nil, err
	}
	go func() {
		err := rc.RecoverOutput(context.Background(), ref, st)
		if err != nil {
			_ = st.Reset(runnerwire.ResetInternal)
			return
		}
		_ = st.CloseWrite()
	}()
	return runnerwire.RecoverResult{Stream: st.ID()}, nil
}

type sweeper interface {
	SweepOrphanedSandboxes(context.Context, time.Duration, func(uuid.UUID) bool) (int, error)
}

func (s *Server) sweep(ctx context.Context, args json.RawMessage) (any, error) {
	a, err := decode[runnerwire.SweepArgs](args)
	if err != nil {
		return nil, err
	}
	sw, ok := s.sub.(sweeper)
	if !ok {
		return runnerwire.SweepResult{}, nil
	}
	if a.Remove == nil {
		var seen []uuid.UUID
		_, err := sw.SweepOrphanedSandboxes(ctx, a.MinAge, func(id uuid.UUID) bool { seen = append(seen, id); return false })
		return runnerwire.SweepResult{Candidates: seen}, err
	}
	rm := map[uuid.UUID]bool{}
	for _, id := range a.Remove {
		rm[id] = true
	}
	n, err := sw.SweepOrphanedSandboxes(ctx, a.MinAge, func(id uuid.UUID) bool { return rm[id] })
	return runnerwire.SweepResult{Removed: n}, err
}

type (
	writerFunc func([]byte) (int, error)
	readerFunc func([]byte) (int, error)
)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
func (f readerFunc) Read(b []byte) (int, error)  { return f(b) }
