// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runneragent dispatches authenticated runner RPCs to a local substrate.
// Session admission, durable offline enforcement and stream bridges remain
// separate dependencies; no test server is used by the production daemon.
package runneragent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
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

// Config supplies the local executor and the production safety adapters.
type Config struct {
	RunnerID  string
	Substrate substrate.Substrate
	// HoldsSandbox reports whether a local ref is a sandbox this runner created
	// and durably holds. Any other ref reaches the substrate only when the
	// substrate proves it a sandbox created for this runner.
	HoldsSandbox func(string) bool
	// BeforeCreate durably establishes a unique creation intent and live lease.
	// Nil proves a NEW run; every existing or ambiguous intent must refuse.
	BeforeCreate func(context.Context, runner.SandboxSpec) error
	// Created durably binds the run to its local ref before success is reported.
	Created func(context.Context, runner.SandboxSpec, string) error
	// CreateFailed durably contains a failed NEW intent, including partial
	// provisioning. It must retain output and retry proxy containment locally.
	CreateFailed func(context.Context, runner.SandboxSpec) error
	// BeforeRefCall enforces offline/lease policy before a state-changing call.
	// The generation is the explicit Exec/Start CALL identity, zero otherwise.
	BeforeRefCall func(context.Context, string, string, uuid.UUID) error
	// AfterRefCall persists successful state changes with the same generation.
	// The exec id is populated for MethodExec, empty otherwise. Persistence
	// failures must contain the proxy before returning their error.
	AfterRefCall func(context.Context, string, string, string, uuid.UUID) error
	// MutationFinished runs after every admitted mutation, including substrate
	// failures, output rollback and detached persistence. Create supplies run ID;
	// ref calls supply the checked local ref and explicit execution generation.
	MutationFinished func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error
	// ExecutionResult recovers a durable intent/handle without executing it again.
	ExecutionResult func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error)
	// Streams serves attach/exec/resize/wait/output, with ref validation of its own.
	Streams      func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error)
	Output       func(uuid.UUID) (io.Writer, error)
	BindOutput   func(uuid.UUID, string) error
	ForgetOutput func(uuid.UUID) error
	// CheckSelftest admits a BYOI selftest under the task's unattempted
	// generation without spending it; nil refuses the selftest.
	CheckSelftest func(context.Context, string, uuid.UUID) error
	// Resident is the runner_resident adapter; nil refuses both verbs and any
	// create or proxy replacement that names a resident grant.
	Resident *Resident
	// LinuxVM says the substrate starts Linux VM sandboxes, as the runner's
	// caps say; false refuses create_vm before any intent is recorded.
	LinuxVM bool
}

type Dispatcher struct {
	cfg Config
	// selftests holds the local refs of BYOI sandboxes whose selftest has not
	// run yet (awaitSelftest).
	selftests sync.Map
}

// Residents reports whether the runner holds runner_resident credentials, its
// caps' Resident.
func (d *Dispatcher) Residents() bool { return d.cfg.Resident != nil }

func New(cfg Config) (*Dispatcher, error) {
	id, err := uuid.Parse(cfg.RunnerID)
	if err != nil || id.String() != cfg.RunnerID || cfg.Substrate == nil || cfg.HoldsSandbox == nil || cfg.BeforeCreate == nil || cfg.Created == nil || cfg.CreateFailed == nil || cfg.BeforeRefCall == nil || cfg.AfterRefCall == nil || cfg.MutationFinished == nil || cfg.ExecutionResult == nil {
		return nil, errors.New("runner dispatcher requires a registered id, local substrate and durable lifecycle handlers")
	}
	if substrate.IsRemote(cfg.Substrate) {
		return nil, errors.New("runner dispatcher requires a local substrate")
	}
	return &Dispatcher{cfg: cfg}, nil
}

// Call is RunnerHooks.Call. Byte stream verbs go to the H4 adapter; everything
// else calls the same substrate interfaces as the local orchestrator.
func (d *Dispatcher) Call(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, args json.RawMessage) (any, error) {
	if !peer.Online() {
		return nil, runner.ErrRunnerOffline
	}
	switch method {
	case runnerwire.MethodCreateSandbox, runnerwire.MethodCreateVM:
		return d.create(ctx, peer, id, method, args)
	case runnerwire.MethodExecutionResult:
		return d.executionResult(ctx, args)
	case runnerwire.MethodAttach, runnerwire.MethodExecStream, runnerwire.MethodResize, runnerwire.MethodExecWait, runnerwire.MethodRecoverOutput, runnerwire.MethodOutputAck, runnerwire.MethodExecClose:
		if d.cfg.Streams == nil {
			return nil, runnerwire.Refuse("runner stream adapter is unavailable")
		}
		return d.cfg.Streams(ctx, peer, id, method, args)
	case runnerwire.MethodDeliverResident, runnerwire.MethodEraseResident:
		if d.cfg.Resident == nil {
			return nil, runnerwire.Refuse("runner credential delivery is unavailable")
		}
		return d.cfg.Resident.Call(ctx, peer, method, args)
	case runnerwire.MethodEnsureProxyImg:
		proxy, ok := d.cfg.Substrate.(runner.ProxyReviver)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		return nil, proxy.EnsureProxyImage(ctx)
	case runnerwire.MethodProbeDrive:
		probe, ok := d.cfg.Substrate.(runner.DriveProber)
		if !ok {
			return nil, runnerwire.Refuse("runner cannot probe drives")
		}
		var drive types.DriveMount
		if err := runnerwire.Unmarshal(args, &drive); err != nil {
			return nil, err
		}
		return probe.ProbeDrive(ctx, drive)
	case runnerwire.MethodSweep:
		return d.sweep(ctx, args)
	case runnerwire.MethodSelftest:
		return d.selftest(ctx, args)
	}
	return d.refCall(ctx, method, args)
}

func (d *Dispatcher) local(ref string) (string, error) {
	local, ok := strings.CutPrefix(ref, placement.RunnerRefPrefix(d.cfg.RunnerID))
	if !ok || local == "" {
		return "", runnerwire.Refuse("sandbox reference does not belong to this runner")
	}
	return local, nil
}

func (d *Dispatcher) create(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, args json.RawMessage) (result any, err error) {
	var a runnerwire.CreateSandboxArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.Spec.RunID == uuid.Nil || a.Spec.RunnerID != d.cfg.RunnerID {
		return nil, runnerwire.Refuse("create must name this runner and a run id")
	}
	// Each create method starts exactly one kind: a container is never started for a VM run, nor a
	// VM for a container run.
	if err := a.Spec.MachineKind.Validate(); err != nil {
		return nil, runnerwire.Refuse("%v", err)
	}
	switch vm := a.Spec.MachineKind.IsVM(); {
	case method == runnerwire.MethodCreateSandbox && a.Spec.MachineKind.Normalize() != "":
		return nil, runnerwire.Refuse("create_sandbox starts containers only; a %s sandbox needs create_vm", a.Spec.MachineKind)
	case method == runnerwire.MethodCreateVM && !vm:
		return nil, runnerwire.Refuse("create_vm starts Linux VM sandboxes only")
	case vm && !d.cfg.LinuxVM:
		return nil, runnerwire.Refuse("this runner does not offer Linux VM sandboxes")
	}
	if err := d.cfg.BeforeCreate(ctx, a.Spec); err != nil {
		return nil, err
	}
	defer func() { err = d.finishMutation(ctx, runnerwire.MethodCreateSandbox, "", a.Spec.RunID, uuid.Nil, err) }()
	defer func() {
		if err != nil {
			contain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			err = errors.Join(err, d.cfg.CreateFailed(contain, a.Spec))
			// A run that was never created keeps no resident value: erase
			// what was delivered for it (the org's erase retries any rest).
			if d.cfg.Resident != nil {
				err = errors.Join(err, d.cfg.Resident.erase(contain, peer, a.Spec.RunID))
			}
		}
	}()
	if a.ExecOutput {
		if d.cfg.Output == nil || d.cfg.BindOutput == nil || d.cfg.ForgetOutput == nil {
			return nil, runnerwire.Refuse("durable output adapter is unavailable")
		}
		var err error
		a.Spec.ExecOutput, err = d.cfg.Output(a.Spec.RunID)
		if err != nil {
			return nil, err
		}
	}
	a.Spec.OnWaiting = func(detail string) {
		data, _ := runnerwire.Marshal(runnerwire.Waiting{Detail: detail})
		_ = peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventWaiting, Call: id, RunID: &a.Spec.RunID, Data: data})
	}
	// Only the substrate sees resident values; a.Spec, which the lifecycle
	// handlers persist, keeps the placeholders.
	filled, err := d.cfg.Resident.fill(ctx, a.Spec)
	if err != nil {
		return nil, err
	}
	// The label is what proves the sandbox this runner's own once its durable
	// record is lost.
	filled.Labels = maps.Clone(filled.Labels)
	if filled.Labels == nil {
		filled.Labels = map[string]string{}
	}
	filled.Labels[runner.LabelRunnerID] = d.cfg.RunnerID
	sandbox, err := d.cfg.Substrate.CreateSandbox(ctx, filled)
	if err != nil {
		// A substrate may lose its create response or fail best-effort rollback.
		// Without a known ref and confirmed cleanup, retain the output history.
		return nil, err
	}
	// Once provisioning succeeds, link cancellation alone must not prevent
	// durable local state from keeping the existing sandbox enforceable.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	err = d.cfg.Created(persist, a.Spec, sandbox.Ref)
	cancel()
	if err != nil {
		return nil, d.rollbackCreate(ctx, a, sandbox.Ref, err)
	}
	if a.ExecOutput {
		if err := d.cfg.BindOutput(a.Spec.RunID, sandbox.Ref); err != nil {
			return nil, d.rollbackCreate(ctx, a, sandbox.Ref, err)
		}
	}
	d.awaitSelftest(sandbox.Ref, a.Spec.Image)
	sandbox.Ref = placement.RunnerRefPrefix(d.cfg.RunnerID) + sandbox.Ref
	return runnerwire.CreateSandboxResult{Sandbox: sandbox}, nil
}

func (d *Dispatcher) rollbackCreate(ctx context.Context, a runnerwire.CreateSandboxArgs, ref string, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if cleanupErr := d.cfg.Substrate.KillSandbox(cleanup, ref); cleanupErr != nil {
		return errors.Join(err, cleanupErr)
	}
	// Only confirmed cleanup of this durably new attempt permits deletion.
	// Its lifecycle intent remains durable, so an ambiguous retry refuses.
	if a.ExecOutput {
		return errors.Join(err, d.cfg.ForgetOutput(a.Spec.RunID))
	}
	return err
}

type refArgs struct {
	Ref         string   `json:"ref"`
	Argv        []string `json:"argv"`
	AgentExecID string   `json:"agent_exec_id"`
	CfgJSON     []byte   `json:"cfg_json"`
}

func (d *Dispatcher) refCall(ctx context.Context, method string, args json.RawMessage) (result any, err error) {
	var a refArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	ref, err := d.local(a.Ref)
	if err != nil {
		return nil, err
	}
	if err := d.owned(ctx, ref); err != nil {
		return nil, err
	}
	changing := changesRef(method)
	generation, err := executionGeneration(method, args)
	if err != nil {
		return nil, err
	}
	if changing {
		if err := d.cfg.BeforeRefCall(ctx, method, ref, generation); err != nil {
			return nil, err
		}
		defer func() { err = d.finishMutation(ctx, method, ref, uuid.Nil, generation, err) }()
		if method == runnerwire.MethodExec || method == runnerwire.MethodKill || method == runnerwire.MethodEnd {
			// An attempted Exec generation can no longer admit the selftest, and a
			// killed or ended sandbox has none to run.
			d.selftests.Delete(ref)
		}
	}
	result, err = d.dispatchRef(ctx, method, ref, a)
	if err != nil || !changing {
		return result, err
	}
	execID := ""
	if exec, ok := result.(runnerwire.ExecResult); ok {
		execID = exec.ExecID
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := d.cfg.AfterRefCall(persist, method, ref, execID, generation); err != nil {
		return nil, err
	}
	return result, nil
}

// owned refuses a ref the runner neither durably holds nor can prove it
// created, so the control plane cannot stop, remove or read any other
// container on the runner's engine. The proof keeps containment working for a
// run whose local identity was lost.
func (d *Dispatcher) owned(ctx context.Context, ref string) error {
	if d.cfg.HoldsSandbox(ref) {
		return nil
	}
	prover, ok := d.cfg.Substrate.(runner.RunnerSandboxProver)
	if !ok {
		return runnerwire.Refuse("sandbox reference is not a sandbox this runner created")
	}
	if err := prover.ProveRunnerSandbox(ctx, ref, d.cfg.RunnerID); err != nil {
		return runnerwire.Refuse("sandbox reference is not a sandbox this runner created: %v", err)
	}
	return nil
}

func executionGeneration(method string, args json.RawMessage) (uuid.UUID, error) {
	switch method {
	case runnerwire.MethodExec:
		var a runnerwire.ExecArgs
		err := runnerwire.Unmarshal(args, &a)
		return a.ExecutionGeneration, err
	case runnerwire.MethodStart:
		var a runnerwire.RefArgs
		err := runnerwire.Unmarshal(args, &a)
		return a.ExecutionGeneration, err
	default:
		return uuid.Nil, nil
	}
}

func changesRef(method string) bool {
	switch method {
	case runnerwire.MethodExec, runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd, runnerwire.MethodStopProxy, runnerwire.MethodStart, runnerwire.MethodReplaceProxy:
		return true
	default:
		return false
	}
}

func (d *Dispatcher) dispatchRef(ctx context.Context, method, ref string, a refArgs) (any, error) {
	sub := d.cfg.Substrate
	switch method {
	case runnerwire.MethodExec:
		id, err := sub.Exec(ctx, ref, a.Argv)
		return runnerwire.ExecResult{ExecID: id}, err
	case runnerwire.MethodWait:
		code, err := sub.Wait(ctx, ref)
		return runnerwire.WaitResult{ExitCode: code}, err
	case runnerwire.MethodStatus:
		return sub.Status(ctx, ref)
	case runnerwire.MethodAgentStatus:
		return sub.AgentStatus(ctx, ref, a.AgentExecID)
	case runnerwire.MethodStop:
		d.selftests.Delete(ref)
		return nil, sub.StopSandbox(ctx, ref)
	case runnerwire.MethodKill:
		d.selftests.Delete(ref)
		return nil, sub.KillSandbox(ctx, ref)
	case runnerwire.MethodEnd:
		d.selftests.Delete(ref)
		if end, ok := sub.(runner.SandboxEnder); ok {
			return nil, runner.KeptDespiteAWSCacheWipe(ctx, ref, end.EndSandbox(ctx, ref))
		}
		return nil, runner.ErrEndUnsupported
	case runnerwire.MethodStopProxy:
		if stop, ok := sub.(runner.ProxyStopper); ok {
			return nil, stop.StopProxy(ctx, ref)
		}
		return nil, runner.ErrEndUnsupported
	case runnerwire.MethodStart:
		if start, ok := sub.(runner.SandboxStarter); ok {
			return nil, start.StartSandbox(ctx, ref)
		}
		return nil, runner.ErrReviveUnsupported
	case runnerwire.MethodCanReplace, runnerwire.MethodReplaceProxy:
		proxy, ok := sub.(runner.ProxyReviver)
		if !ok {
			return nil, runner.ErrReviveUnsupported
		}
		if method == runnerwire.MethodCanReplace {
			return nil, proxy.CanReplaceProxy(ctx, ref)
		}
		cfg, err := d.cfg.Resident.fillReplacedProxy(ctx, a.CfgJSON)
		if err != nil {
			return nil, err
		}
		return nil, proxy.ReplaceProxy(ctx, ref, cfg)
	default:
		return nil, runnerwire.Refuse("unknown runner method")
	}
}

// sweep uses the established two-call orphan-sweep protocol. The driver still
// proves age and ownership of its sandboxes; the org supplies the removal set.
func (d *Dispatcher) sweep(ctx context.Context, args json.RawMessage) (any, error) {
	var a runnerwire.SweepArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.MinAge < 0 {
		return nil, runnerwire.Refuse("negative sweep age")
	}
	sweep, ok := d.cfg.Substrate.(interface {
		SweepOrphanedSandboxes(context.Context, time.Duration, func(uuid.UUID) bool) (int, error)
	})
	if !ok {
		return runnerwire.SweepResult{}, nil
	}
	if a.Remove == nil {
		var candidates []uuid.UUID
		_, err := sweep.SweepOrphanedSandboxes(ctx, a.MinAge, func(id uuid.UUID) bool { candidates = append(candidates, id); return false })
		// An error from the same pass (a leftover drive seed that would not go)
		// must not hide the candidates it listed: the reply carries one or the
		// other, and the org removes nothing without the list. The local path
		// sweeps its candidates whatever the seed sweep answered too.
		if err != nil && len(candidates) > 0 {
			slog.WarnContext(ctx, "runner: orphan sweep listed its candidates with an error", slog.String("err", err.Error()))
			err = nil
		}
		return runnerwire.SweepResult{Candidates: candidates}, err
	}
	remove := make(map[uuid.UUID]bool, len(a.Remove))
	for _, id := range a.Remove {
		remove[id] = true
	}
	n, err := sweep.SweepOrphanedSandboxes(ctx, a.MinAge, func(id uuid.UUID) bool { return remove[id] })
	return runnerwire.SweepResult{Removed: n}, err
}
