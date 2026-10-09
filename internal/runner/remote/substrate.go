// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package remote is the organisation's view of one registered runner: a
// substrate.Substrate that carries every call over a Transport to a
// wardyn-runnerd and maps the runner's typed errors back to the runner package's
// sentinels. It decides nothing; the org dispatches, the runner executes.
package remote

import (
	"bytes"
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

var (
	errOffline = fmt.Errorf("%w: no session", runner.ErrRunnerOffline)

	// ErrResidentLeak refuses a CreateSandbox whose payload would carry a value
	// already delivered to the runner as runner_resident: the spec names such a
	// grant by placeholder, and the value travels only in DeliverResident.
	ErrResidentLeak = errors.New("remote: a runner_resident value is in the CreateSandbox payload")
	// ErrForeignRef refuses a ref that is not this runner's.
	ErrForeignRef = errors.New("remote: ref does not belong to this runner")
	// ErrUnsupportedStream refuses a runner-opened stream the substrate does not serve.
	ErrUnsupportedStream = errors.New("remote: unsupported stream")
)

// Pending action kinds a Substrate queues while its runner is offline.
const (
	ActionKill      = runnerwire.ActionKill
	ActionEnd       = runnerwire.ActionEnd
	ActionStopProxy = runnerwire.ActionStopProxy
)

// Options are the seams the runner hub fills.
type Options struct {
	// Queue durably records an action to apply when the runner reconnects. With
	// it set, a teardown issued while offline returns runner.ErrPendingOnRunner
	// (queued, not failed); without it, runner.ErrRunnerOffline.
	Queue func(ctx context.Context, kind, ref string) error
	// OnEvent receives every runner event except `caps` (kept here) and
	// `waiting` (delivered to the create that owns it).
	OnEvent func(runnerwire.Event)
}

// callTimeout bounds the calls an io-shaped method (Resize, ExecSession.Wait)
// makes without a caller context.
const callTimeout = 30 * time.Second

// Substrate is one runner as a substrate.Substrate.
type Substrate struct {
	id   string
	t    Transport
	opts Options

	mu       sync.Mutex
	caps     *runnerwire.Caps
	outputs  map[uuid.UUID]io.Writer
	resident map[uuid.UUID][][]byte
}

// New builds the substrate of runnerID over t.
func New(runnerID string, t Transport, opts Options) *Substrate {
	s := &Substrate{id: runnerID, t: t, opts: opts, outputs: map[uuid.UUID]io.Writer{}, resident: map[uuid.UUID][][]byte{}}
	t.SetHandler(s)
	return s
}

var (
	_ substrate.Substrate    = (*Substrate)(nil)
	_ runner.SandboxEnder    = (*Substrate)(nil)
	_ runner.ProxyStopper    = (*Substrate)(nil)
	_ runner.ProxyReviver    = (*Substrate)(nil)
	_ runner.SandboxStarter  = (*Substrate)(nil)
	_ runner.SubstrateProber = (*Substrate)(nil)
	_ runner.DriveProber     = (*Substrate)(nil)
	_ runner.OutputRecoverer = (*Substrate)(nil)
)

// Name is "runner:<id>".
func (s *Substrate) Name() string { return placement.RunnerSubstrateName(s.id) }

// RunnerID is the runner's id.
func (s *Substrate) RunnerID() string { return s.id }

// Caps is the runner's last advertised capabilities, if any.
func (s *Substrate) Caps() (runnerwire.Caps, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caps == nil {
		return runnerwire.Caps{}, false
	}
	return *s.caps, true
}

// HandleEvent keeps the `caps` event and passes the rest to Options.OnEvent.
func (s *Substrate) HandleEvent(ev runnerwire.Event) {
	if ev.Kind == runnerwire.EventCaps {
		var c runnerwire.Caps
		if json.Unmarshal(ev.Data, &c) == nil {
			s.mu.Lock()
			s.caps = &c
			s.mu.Unlock()
		}
		return
	}
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(ev)
	}
}

// HandleOpen serves the runner-opened `output` stream: the agent's output
// copied into the writer the run's CreateSandbox supplied.
func (s *Substrate) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	if o.Kind != runnerwire.KindOutput {
		return ErrUnsupportedStream
	}
	runID, err := uuid.Parse(o.Target)
	if err != nil {
		return ErrUnsupportedStream
	}
	s.mu.Lock()
	w := s.outputs[runID]
	s.mu.Unlock()
	if w == nil {
		return ErrUnsupportedStream
	}
	end := runner.BeginOutputDrain(w)
	go func() {
		_, err := io.Copy(w, st)
		s.mu.Lock()
		delete(s.outputs, runID)
		s.mu.Unlock()
		end(err)
	}()
	return nil
}

// Classes serves the last `caps` event: no call is made.
func (s *Substrate) Classes(context.Context) (substrate.ClassSupport, error) {
	if !s.t.Online() {
		return substrate.ClassSupport{}, errOffline
	}
	c, ok := s.Caps()
	if !ok {
		return substrate.ClassSupport{}, fmt.Errorf("%w: no capabilities reported yet", runner.ErrRunnerOffline)
	}
	return c.Support, nil
}

func (s *Substrate) call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	return s.t.Call(ctx, method, args, result, opts...)
}

func (s *Substrate) own(ref string) error {
	if !strings.HasPrefix(ref, placement.RunnerRefPrefix(s.id)) {
		return fmt.Errorf("%w: %q", ErrForeignRef, ref)
	}
	return nil
}

func (s *Substrate) refCall(ctx context.Context, method, ref string, result any) error {
	if err := s.own(ref); err != nil {
		return err
	}
	return s.call(ctx, method, runnerwire.RefArgs{Ref: ref}, result)
}

// CreateSandbox sends the spec minus OnWaiting and ExecOutput. `waiting` events
// call spec.NotifyWaiting on this goroutine, in order, before it returns.
func (s *Substrate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	if err := s.checkClass(ctx, spec.ConfinementClass); err != nil {
		return runner.Sandbox{}, err
	}
	args := runnerwire.CreateSandboxArgs{Spec: spec, ExecOutput: spec.ExecOutput != nil}
	if err := s.refuseResident(spec.RunID, args); err != nil {
		return runner.Sandbox{}, err
	}
	if spec.ExecOutput != nil {
		s.mu.Lock()
		s.outputs[spec.RunID] = spec.ExecOutput
		s.mu.Unlock()
	}
	var res runnerwire.CreateSandboxResult
	err := s.call(ctx, runnerwire.MethodCreateSandbox, args, &res, runnerwire.WithEvents(func(ev runnerwire.Event) {
		var w runnerwire.Waiting
		if ev.Kind == runnerwire.EventWaiting && json.Unmarshal(ev.Data, &w) == nil {
			spec.NotifyWaiting(w.Detail)
		}
	}))
	if err == nil {
		err = s.own(res.Sandbox.Ref)
	}
	if err != nil {
		s.mu.Lock()
		delete(s.outputs, spec.RunID)
		s.mu.Unlock()
		return runner.Sandbox{}, err
	}
	return res.Sandbox, nil
}

// checkClass refuses a class the runner did not advertise, before anything is sent.
func (s *Substrate) checkClass(ctx context.Context, class types.ConfinementClass) error {
	cs, err := s.Classes(ctx)
	if err != nil {
		return err
	}
	if class == "" {
		class = types.CC1
	}
	for _, c := range cs.Classes {
		if c == class {
			return nil
		}
	}
	return fmt.Errorf("remote: %s does not advertise class %q", s.Name(), class)
}

// refuseResident fails the create when its payload carries a value already
// delivered for the run as runner_resident.
func (s *Substrate) refuseResident(runID uuid.UUID, args runnerwire.CreateSandboxArgs) error {
	s.mu.Lock()
	vals := s.resident[runID]
	s.mu.Unlock()
	if len(vals) == 0 {
		return nil
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return err
	}
	for _, v := range vals {
		if bytes.Contains(payload, v) {
			return ErrResidentLeak
		}
	}
	return nil
}

func (s *Substrate) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if err := s.own(ref); err != nil {
		return "", err
	}
	var res runnerwire.ExecResult
	if err := s.call(ctx, runnerwire.MethodExec, runnerwire.ExecArgs{Ref: ref, Argv: argv}, &res); err != nil {
		return "", err
	}
	return res.ExecID, nil
}

// Wait is a long call: a link drop answers runner.ErrRunnerOffline, which is
// transient (the runner also sends a durable agent_exit event).
func (s *Substrate) Wait(ctx context.Context, ref string) (int, error) {
	var res runnerwire.WaitResult
	if err := s.refCall(ctx, runnerwire.MethodWait, ref, &res); err != nil {
		return 0, err
	}
	return res.ExitCode, nil
}

func (s *Substrate) Status(ctx context.Context, ref string) (runner.Status, error) {
	var st runner.Status
	err := s.refCall(ctx, runnerwire.MethodStatus, ref, &st)
	return st, err
}

func (s *Substrate) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	if err := s.own(ref); err != nil {
		return runner.Status{}, err
	}
	var st runner.Status
	err := s.call(ctx, runnerwire.MethodAgentStatus, runnerwire.AgentStatusArgs{Ref: ref, AgentExecID: agentExecID}, &st)
	return st, err
}

// teardown calls a verb that keeps working while the runner is away: offline,
// it is queued as a pending action and answers runner.ErrPendingOnRunner.
func (s *Substrate) teardown(ctx context.Context, method, kind, ref string) error {
	err := s.refCall(ctx, method, ref, nil)
	if err == nil || !errors.Is(err, runner.ErrRunnerOffline) || s.opts.Queue == nil {
		return err
	}
	if qerr := s.opts.Queue(ctx, kind, ref); qerr != nil {
		return errors.Join(err, qerr)
	}
	return runner.ErrPendingOnRunner
}

func (s *Substrate) StopSandbox(ctx context.Context, ref string) error {
	return s.teardown(ctx, runnerwire.MethodStop, ActionEnd, ref)
}

func (s *Substrate) KillSandbox(ctx context.Context, ref string) error {
	return s.teardown(ctx, runnerwire.MethodKill, ActionKill, ref)
}

func (s *Substrate) EndSandbox(ctx context.Context, ref string) error {
	return s.teardown(ctx, runnerwire.MethodEnd, ActionEnd, ref)
}

func (s *Substrate) StopProxy(ctx context.Context, ref string) error {
	return s.teardown(ctx, runnerwire.MethodStopProxy, ActionStopProxy, ref)
}

func (s *Substrate) StartSandbox(ctx context.Context, ref string) error {
	return s.refCall(ctx, runnerwire.MethodStart, ref, nil)
}

func (s *Substrate) CanReplaceProxy(ctx context.Context, ref string) error {
	return s.refCall(ctx, runnerwire.MethodCanReplace, ref, nil)
}

// ReplaceProxy hands the org's stored, already-stripped proxy config to the
// runner, which re-applies its resident values from its keystore.
func (s *Substrate) ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error {
	if err := s.own(ref); err != nil {
		return err
	}
	return s.call(ctx, runnerwire.MethodReplaceProxy, runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: cfgJSON}, nil)
}

func (s *Substrate) EnsureProxyImage(ctx context.Context) error {
	return s.call(ctx, runnerwire.MethodEnsureProxyImg, nil, nil)
}

// ProbeSubstrate answers link state; it makes no call.
func (s *Substrate) ProbeSubstrate(context.Context) runner.SubstrateState {
	if s.t.Online() {
		return runner.SubstrateOK
	}
	return runner.SubstrateUnreachable
}

func (s *Substrate) ProbeDrive(ctx context.Context, mount types.DriveMount) (runner.DriveProbe, error) {
	var res runner.DriveProbe
	err := s.call(ctx, runnerwire.MethodProbeDrive, mount, &res)
	return res, err
}

// RecoverOutput reads the runner-side output buffer into w.
func (s *Substrate) RecoverOutput(ctx context.Context, ref string, w io.Writer) error {
	var res runnerwire.RecoverResult
	if err := s.refCall(ctx, runnerwire.MethodRecoverOutput, ref, &res); err != nil {
		return err
	}
	if res.Unrecoverable {
		return runner.ErrOutputUnrecoverable
	}
	st, err := s.t.TakeStream(res.Stream)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = io.Copy(w, st)
	return err
}

// SweepOrphanedSandboxes runs the crash-orphan sweep on the runner in two
// calls (a func cannot cross the wire): the runner lists what it holds older
// than minAge, the org keeps the run ids isOrphan says are orphans, and the
// runner removes exactly those. It is skipped, with the runner offline, by the
// ErrRunnerOffline of the first call.
func (s *Substrate) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	var list runnerwire.SweepResult
	if err := s.call(ctx, runnerwire.MethodSweep, runnerwire.SweepArgs{MinAge: minAge}, &list); err != nil {
		return 0, err
	}
	var orphans []uuid.UUID
	for _, id := range list.Candidates {
		if isOrphan(id) {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	var done runnerwire.SweepResult
	if err := s.call(ctx, runnerwire.MethodSweep, runnerwire.SweepArgs{MinAge: minAge, Remove: orphans}, &done); err != nil {
		return 0, err
	}
	return done.Removed, nil
}

// DeliverResident hands one runner_resident value to the runner's keystore,
// after claim and before CreateSandbox. It is the only way the value travels:
// never /internal/*, never inside CreateSandbox.
func (s *Substrate) DeliverResident(ctx context.Context, a runnerwire.DeliverResidentArgs) error {
	if err := s.call(ctx, runnerwire.MethodDeliverResident, a, nil); err != nil {
		return err
	}
	if a.Value != "" {
		quoted, err := json.Marshal(a.Value)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.resident[a.RunID] = append(s.resident[a.RunID], quoted[1:len(quoted)-1])
		s.mu.Unlock()
	}
	return nil
}

// EraseResident asks the runner to erase the run's resident values, best effort.
func (s *Substrate) EraseResident(ctx context.Context, runID uuid.UUID) error {
	s.mu.Lock()
	delete(s.resident, runID)
	s.mu.Unlock()
	return s.call(ctx, runnerwire.MethodEraseResident, runnerwire.EraseResidentArgs{RunID: runID}, nil)
}
