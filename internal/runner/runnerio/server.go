// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerio bridges authenticated runner calls to local byte streams.
package runnerio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type streamKey struct {
	peer *runnerwire.Peer
	id   uint32
}
type peerLife struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Server retains output across connections; interactive resources belong to one peer.
type Server struct {
	sub           substrate.Substrate
	runnerID, dir string
	mu            sync.Mutex
	current       *runnerwire.Peer
	peers         map[*runnerwire.Peer]peerLife
	sessions      map[streamKey]*attached
	execs         map[streamKey]*execution
	execSlots     chan struct{}
	outputs       map[uuid.UUID]*outputBuffer
}

// New opens private output storage beneath the runner's state directory.
func New(sub substrate.Substrate, runnerID, stateDir string) (*Server, error) {
	if sub == nil || runnerID == "" || stateDir == "" {
		return nil, errors.New("runnerio: substrate, runner and state directory required")
	}
	dir := filepath.Join(stateDir, "output")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runnerio: private output directory required")
	}
	s := &Server{sub: sub, runnerID: runnerID, dir: dir, peers: map[*runnerwire.Peer]peerLife{}, sessions: map[streamKey]*attached{}, execs: map[streamKey]*execution{}, execSlots: make(chan struct{}, 256), outputs: map[uuid.UUID]*outputBuffer{}}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		id, err := uuid.Parse(f.Name())
		if err != nil || id == uuid.Nil || f.Name() != id.String() {
			return nil, errors.New("runnerio: unknown output directory")
		}
		b, err := newOutputBuffer(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		s.outputs[id] = b
	}
	return s, nil
}

func (s *Server) local(ref string) (string, error) {
	local, ok := strings.CutPrefix(ref, placement.RunnerRefPrefix(s.runnerID))
	if !ok || local == "" {
		return "", runnerwire.Refuse("ref does not belong to this runner")
	}
	return local, nil
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

func (s *Server) life(peer *runnerwire.Peer) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if life, ok := s.peers[peer]; ok {
		return life.ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.peers[peer] = peerLife{ctx, cancel}
	go func() {
		select {
		case <-peer.Done():
			s.Offline(peer)
		case <-ctx.Done():
		}
	}()
	return ctx
}

// Online reopens retained output only after the caller has completed pending actions and READY.
func (s *Server) Online(peer *runnerwire.Peer) {
	_ = s.life(peer)
	s.mu.Lock()
	old := s.current
	if old == peer {
		s.mu.Unlock()
		return
	}
	s.current = peer
	outputs := make(map[uuid.UUID]*outputBuffer, len(s.outputs))
	for id, b := range s.outputs {
		outputs[id] = b
	}
	s.mu.Unlock()
	if old != nil {
		s.Offline(old)
	}
	for id, b := range outputs {
		s.sendOutput(peer, id, b)
	}
}

// Offline releases only this connection's stream resources; sandboxes and output buffers survive.
func (s *Server) Offline(peer *runnerwire.Peer) {
	s.mu.Lock()
	life, ok := s.peers[peer]
	delete(s.peers, peer)
	if s.current == peer {
		s.current = nil
	}
	var closeFns []func()
	for k, a := range s.sessions {
		if k.peer == peer {
			delete(s.sessions, k)
			closeFns = append(closeFns, a.close)
		}
	}
	for k, e := range s.execs {
		if k.peer == peer {
			delete(s.execs, k)
			closeFns = append(closeFns, e.close)
		}
	}
	s.mu.Unlock()
	if ok {
		life.cancel()
	}
	for _, closeFn := range closeFns {
		closeFn()
	}
}

// Call implements stream RPCs after the session's authenticated admission checks.
func (s *Server) Call(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, args json.RawMessage) (any, error) {
	if peer == nil {
		return nil, runner.ErrRunnerOffline
	}
	switch method {
	case runnerwire.MethodAttach:
		return s.attach(ctx, peer, id, args)
	case runnerwire.MethodExecStream:
		return s.execStream(ctx, peer, id, args)
	case runnerwire.MethodResize:
		return s.resize(ctx, peer, args)
	case runnerwire.MethodExecWait:
		return s.execWait(ctx, peer, args)
	case runnerwire.MethodExecClose:
		return nil, s.execClose(peer, args)
	case runnerwire.MethodRecoverOutput:
		return s.recoverOutput(ctx, peer, id, args)
	case runnerwire.MethodOutputAck:
		return nil, s.outputAck(args)
	default:
		return nil, fmt.Errorf("runnerio: unsupported method %q", method)
	}
}

// Output returns the idempotent per-run writer; disk failures refuse sandbox creation.
func (s *Server) Output(id uuid.UUID) (io.Writer, error) {
	if id == uuid.Nil {
		return nil, errors.New("runnerio: missing output run id")
	}
	s.mu.Lock()
	if b := s.outputs[id]; b != nil {
		s.mu.Unlock()
		return b, nil
	}
	b, err := newOutputBuffer(filepath.Join(s.dir, id.String()))
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.outputs[id] = b
	peer := s.current
	s.mu.Unlock()
	if peer != nil {
		s.sendOutput(peer, id, b)
	}
	return b, nil
}

// BindOutput associates a successful sandbox with its retained output.
func (s *Server) BindOutput(id uuid.UUID, ref string) error {
	if ref == "" || strings.HasPrefix(ref, "runner:") {
		return runnerwire.Refuse("local sandbox ref required")
	}
	s.mu.Lock()
	b := s.outputs[id]
	s.mu.Unlock()
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Ref != "" && b.state.Ref != ref {
		return runnerwire.Refuse("output is bound to another sandbox")
	}
	b.state.Ref = ref
	return b.save()
}

// ForgetOutput removes a failed new run's output; never call it for a retry of an existing run.
func (s *Server) ForgetOutput(id uuid.UUID) error {
	s.mu.Lock()
	b := s.outputs[id]
	delete(s.outputs, id)
	s.mu.Unlock()
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = io.ErrClosedPipe
	b.notify()
	return os.RemoveAll(b.dir)
}
