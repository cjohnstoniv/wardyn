// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerConnected is the runner's caps while its session is live on this replica (folderRunner's test,
// in run_computer_folder.go); last-known caps alone never count.
func (s *Server) runnerConnected(id uuid.UUID) (runnerwire.Caps, bool) {
	caps, ok := s.runnerLastCaps(id)
	return caps, ok && s.runnerOnline(id)
}

// cutKind is how cutAgent cut a run's agent off.
type cutKind int

const (
	cutFenced       cutKind = iota // FenceProxy: the agent is cut off every network
	cutProxyStopped                // StopProxy: the proxy is stopped, the agent is left running
	cutUnsupported                 // the substrate can do neither; nothing was sent
)

// errRunnerNotHere is a run on a runner whose session is not live on this replica.
var errRunnerNotHere = errors.New("the run's runner is not connected to this server")

// notObserved reports an error that says the call did not reach the sandbox: its runner is not here, is
// offline, or the action was only queued on it.
func notObserved(err error) bool {
	return errors.Is(err, errRunnerNotHere) || errors.Is(err, runner.ErrRunnerOffline) || errors.Is(err, runner.ErrPendingOnRunner)
}

// cutAgent cuts a terminal run's agent off its networks (runner.ProxyFencer), or stops its proxy alone
// (runner.ProxyStopper) where the substrate cannot fence. It answers the kind it tried; cutUnsupported
// with a nil error means the substrate does neither and nothing was sent. A run on a runner is sent a
// call only while its runner is connected here (runnerConnected).
func (s *Server) cutAgent(ctx context.Context, run types.AgentRun) (cutKind, error) {
	if !isTerminalRunState(run.State) {
		return cutUnsupported, errors.New("cutAgent: the run is not terminal")
	}
	if run.SandboxRef == "" || s.cfg.Runner == nil {
		return cutUnsupported, nil
	}
	if run.MachineKind.IsVM() {
		return cutUnsupported, nil
	}
	kind := cutFenced
	var err error
	switch {
	case run.RunnerID != nil:
		caps, ok := s.runnerConnected(*run.RunnerID)
		if !ok {
			return cutProxyStopped, errRunnerNotHere
		}
		if fencer, can := s.cfg.Runner.(runner.ProxyFencer); can && caps.ProxyFence {
			err = fencer.FenceProxy(ctx, run.SandboxRef)
		} else {
			kind, err = s.stopProxyOnly(ctx, run.SandboxRef)
		}
	default:
		fencer, can := s.cfg.Runner.(runner.ProxyFencer)
		if can {
			err = fencer.FenceProxy(ctx, run.SandboxRef)
		}
		if !can || errors.Is(err, runner.ErrEndUnsupported) {
			kind, err = s.stopProxyOnly(ctx, run.SandboxRef)
		}
	}
	if errors.Is(err, runner.ErrRunnerOffline) || errors.Is(err, runner.ErrPendingOnRunner) {
		err = fmt.Errorf("%w: %w", errRunnerNotHere, err)
	}
	return kind, err
}

// stopProxyOnly is cutAgent's fallback: StopProxy through runner.ProxyStopper. A substrate with no
// proxy stop, or one that answers ErrEndUnsupported, is cutUnsupported with no error.
func (s *Server) stopProxyOnly(ctx context.Context, ref string) (cutKind, error) {
	stopper, ok := s.cfg.Runner.(runner.ProxyStopper)
	if !ok {
		return cutUnsupported, nil
	}
	switch err := stopper.StopProxy(ctx, ref); {
	case err == nil:
		return cutProxyStopped, nil
	case errors.Is(err, runner.ErrEndUnsupported):
		return cutUnsupported, nil
	default:
		return cutProxyStopped, err
	}
}

// keepable reports a sandbox this server can keep with its agent cut off: rule R, stopTerminalSandbox
// and quiesceKeptSandbox ask it.
func (s *Server) keepable(run types.AgentRun) bool {
	if run.MachineKind.IsVM() || s.cfg.Runner == nil {
		return false
	}
	if run.RunnerID == nil {
		_, ok := s.cfg.Runner.(runner.ProxyStopper)
		return ok
	}
	caps, ok := s.runnerConnected(*run.RunnerID)
	return ok && caps.ProxyFence
}

// copyBackCut is stopTerminalSandbox's keep test: nil keeps the sandbox, errProxyAloneUnsupported stops it,
// errProxyStopUnconfirmed keeps it with its failure row, as stopProxyOrAudit answers
// (run_proxy_readback.go); errRunnerNotHere leaves a runner run in place this round, with no stop
// sent or queued, until its runner is here. It is asked only for a sandbox kept for its copy back or whose
// record could not be read.
func (s *Server) copyBackCut(ctx context.Context, run types.AgentRun, action string) error {
	if run.MachineKind.IsVM() {
		return errProxyAloneUnsupported
	}
	kind, err := s.cutAgent(ctx, run)
	switch {
	case err == nil && (kind == cutFenced || kind == cutProxyStopped):
		return nil
	case err == nil:
		return errProxyAloneUnsupported // Kubernetes server mode: the sandbox is stopped; it keeps nothing
	case notObserved(err):
		if run.RunnerID != nil {
			// The runner is not here: a stop would only be queued (the remote Substrate's teardown), and the
			// runner contains egress itself at its lapse (the offline Manager's CheckLapses).
			return errRunnerNotHere
		}
		return errProxyAloneUnsupported
	}
	ok := s.teardownOrAudit(ctx, run.ID, run.SandboxRef, action, func(context.Context, string) error { return err })
	switch {
	case run.RunnerID != nil:
		return errProxyAloneUnsupported // a runner sandbox whose cut failed is not kept
	case ok:
		return nil
	}
	return errProxyStopUnconfirmed
}
