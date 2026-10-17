// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func (m *Manager) findRef(ref string) (uuid.UUID, localRun, bool) {
	for id, r := range m.state.Runs {
		if r.Ref == ref && ref != "" {
			return id, r, true
		}
	}
	return uuid.Nil, localRun{}, false
}

// HoldsSandbox reports whether localRef is a sandbox this runner created and
// has not retired.
func (m *Manager) HoldsSandbox(localRef string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, r, ok := m.findRef(localRef)
	return ok && !r.Retired
}

// BeforeRefCall refuses any start, revive or agent execution without the
// authenticated connection's live lease. Containment never needs permission.
func (m *Manager) BeforeRefCall(ctx context.Context, method, ref string, generation uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isContainmentMethod(method) {
		if err := m.ensureDurable(); err != nil {
			return err
		}
	}
	id, r, ok := m.findRef(ref)
	if !ok {
		if isContainmentMethod(method) {
			m.beginMutation(method, ref, generation)
			return nil
		}
		return errors.New("runner reference has no durable binding")
	}
	if r.ContainmentOnly && method != runnerwire.MethodKill && method != runnerwire.MethodStopProxy {
		return errors.New("containment-only identity refuses this operation")
	}
	switch method {
	case runnerwire.MethodExec, runnerwire.MethodStart, runnerwire.MethodReplaceProxy:
		if !m.online || m.state.Revoked || r.FailedCreation || r.ContainmentOnly || r.Quarantine != nil || r.Containment != nil || r.MutationFailure != nil || m.expired(id) {
			return runner.ErrRunnerOffline
		}
	}
	if method == runnerwire.MethodExec || method == runnerwire.MethodStart {
		if generation == uuid.Nil || r.Lease.ExecutionGeneration != generation {
			return errors.New("execution generation does not match authenticated lease")
		}
		if r.Executions[generation] != "" {
			return errors.New("execution generation already attempted; reconcile its durable result")
		}
		if r.Executions == nil {
			r.Executions = map[uuid.UUID]string{}
		}
		r.Executions[generation] = method
		r.Generation = generation
		r.ExecID = ""
		r.Exit = nil
		r.ExitAcked = false
		m.state.Runs[id] = r
		if err := m.save(); err != nil {
			return err
		}
	}
	m.beginMutation(method, ref, generation)
	return nil
}

// CheckSelftest admits a BYOI selftest on ref only under the live lease an
// Exec would need, and only while its generation is the lease's and not yet
// attempted. It records nothing: the selftest is not that execution.
func (m *Manager) CheckSelftest(ctx context.Context, ref string, generation uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.ensureDurable(); err != nil {
		return err
	}
	id, r, ok := m.findRef(ref)
	if !ok {
		return errors.New("runner reference has no durable binding")
	}
	if !m.online || m.state.Revoked || r.FailedCreation || r.ContainmentOnly || r.Quarantine != nil || r.Containment != nil || r.MutationFailure != nil || m.expired(id) {
		return runner.ErrRunnerOffline
	}
	if generation == uuid.Nil || r.Lease.ExecutionGeneration != generation || r.Executions[generation] != "" {
		return errors.New("selftest generation is not the lease's unattempted execution")
	}
	return nil
}

// AfterRefCall records real substrate changes before the org receives success.
// A durable-state failure contains the proxy even when the wire call was lost.
func (m *Manager) AfterRefCall(ctx context.Context, method, ref, execID string, generation uuid.UUID) error {
	m.mu.Lock()
	id, r, ok := m.findRef(ref)
	var err error
	if !ok {
		if isContainmentMethod(method) {
			m.mu.Unlock()
			return nil
		}
		err = errors.New("runner reference has no durable binding")
	} else {
		switch method {
		case runnerwire.MethodExec, runnerwire.MethodStart:
			if generation == uuid.Nil || r.Generation != generation || r.Executions[generation] != method {
				err = errors.New("execution result has no matching durable intent")
				break
			}
			if r.ExecID != "" && r.ExecID != execID && !(execID == "" && r.ExecID == runner.MainProcessExecID) {
				err = errors.New("execution handle changed")
				break
			}
			if execID == "" {
				execID = runner.MainProcessExecID
			}
			r.ExecID = execID
			r.Exit = nil
			r.ExitAcked = false
		case runnerwire.MethodReplaceProxy:
			r.ProxyStoppedAt = nil
		case runnerwire.MethodStopProxy, runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd:
			at := m.now().UTC()
			r.ProxyStoppedAt = &at
		}
		if err == nil {
			m.state.Runs[id] = r
			err = m.save()
		}
	}
	if err == nil && (method == runnerwire.MethodStart || method == runnerwire.MethodReplaceProxy || method == runnerwire.MethodExec) && (!m.online || m.state.Revoked || r.FailedCreation || r.Quarantine != nil || r.Containment != nil || r.MutationFailure != nil || m.expired(id)) {
		err = runner.ErrRunnerOffline
	}
	if err != nil && ok {
		current := m.state.Runs[id]
		current.MutationFailure = mutationFailure(current, method)
		m.state.Runs[id] = current
		err = errors.Join(err, m.save())
	}
	m.mu.Unlock()
	if err == nil {
		return nil
	}
	safety, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if ok {
		return errors.Join(err, m.CheckLapses(safety))
	}
	return errors.Join(err, m.stopper.StopProxy(safety, ref))
}

func isContainmentMethod(method string) bool {
	switch method {
	case runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd, runnerwire.MethodStopProxy:
		return true
	}
	return false
}
