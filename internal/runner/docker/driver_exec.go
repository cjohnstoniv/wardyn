// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingChmodDirs returns the directories prepareRecordingDirs makes agent-
// writable (chmod 0777). CastDir is ALWAYS included: it is container-local scratch
// created fresh under a root-owned prefix, so loosening it only touches the
// container fs. The RecordingMount target is included ONLY for a NAMED VOLUME (a
// fresh volume is root-owned 0755, so the non-root agent otherwise cannot deliver
// its cast, and the volume is Docker-managed — the relax is contained to it). A
// HOST-BIND RecordingMount is deliberately EXCLUDED: chmod 0777 on it would make
// the operator's HOST directory world-writable. A host-bind mount must be
// provisioned agent-writable by the operator; otherwise the shared-mount fallback
// EPERMs and delivery uses the masked proxy-upload path. (The "/"-prefix split is
// the same one that decides bind vs volume when the mount is attached.)
func recordingChmodDirs(cfg Config) []string {
	dirs := []string{defaultCastDir}
	if cfg.RecordingMount != "" && !strings.HasPrefix(cfg.RecordingMount, "/") {
		dirs = append(dirs, RecordingMountTarget)
	}
	return dirs
}

// prepareRecordingDirs creates+chmods the cast dir and (for a named-volume
// RecordingMount) the recording-mount target to 0777 via a one-shot root exec, so
// a non-root agent process can both write its in-progress cast and deliver it to
// the shared mount. Best-effort: any failure is swallowed.
func (d *Driver) prepareRecordingDirs(ctx context.Context, ref string) {
	dirs := recordingChmodDirs(d.cfg)
	// `mkdir -p <each> && chmod 0777 <each>` — idempotent; the mount already
	// exists (chmod still applies), the cast dir is created fresh.
	args := append([]string{"-p"}, dirs...)
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{
		User: "0:0", // root, regardless of the image's default USER
		Cmd:  append([]string{"mkdir"}, args...),
	})
	if err != nil {
		return
	}
	if _, err := d.cli.ExecStart(ctx, created.ID, client.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, created.ID)

	chmodCreated, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{
		User: "0:0",
		Cmd:  append([]string{"chmod", "0777"}, dirs...),
	})
	if err != nil {
		return
	}
	if _, err := d.cli.ExecStart(ctx, chmodCreated.ID, client.ExecStartOptions{}); err != nil {
		return
	}
	d.waitExec(ctx, chmodCreated.ID)
}

// waitExec briefly polls a one-shot exec to completion so a subsequent Exec
// (which races right after) observes the prepared directories. Bounded so a
// stuck exec cannot stall sandbox bring-up.
func (d *Driver) waitExec(ctx context.Context, execID string) {
	for i := 0; i < 50; i++ {
		insp, ierr := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if ierr != nil || !insp.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordCmd wraps argv with the recorder for a Record-mode launch, shared by
// the exec (Exec) and exec-less (runAsMainProcess) launch paths. Default
// delivery is upload through the proxy's brokered recording route (run-token
// injected proxy-side; cross-run uploads 403 at the control plane), which is
// MASKED control-plane-side before the cast is persisted. HIGH-finding: only
// fall back to the UNMASKED, cross-run-writable shared mount (-out-dir) when
// there is NO masked upload path — runner.RecorderArgv enforces the same
// mutual exclusion as defense in depth, so an unmasked cast can never land in the
// API-served replay store when uploads work. castDir differs per caller (the
// root exec path's default cast dir vs the exec-less path's agent-writable
// tmpfs dir); runID recovery also differs per caller's label source, so it is
// passed in already resolved (uuid.Nil if it could not be recovered).
func (d *Driver) recordCmd(runID uuid.UUID, castDir string, argv []string) []string {
	uploadURL := ""
	if runID != uuid.Nil {
		uploadURL = fmt.Sprintf("http://wardyn-proxy:%d/wardyn/v1/recordings/%s", runner.ProxyListenPort, runID)
	}
	outDir := ""
	if d.cfg.RecordingMount != "" && uploadURL == "" {
		outDir = RecordingMountTarget
	}
	return runner.RecorderArgv(castDir, outDir, uploadURL, runID, argv)
}

// Exec launches the agent process inside the sandbox with a TTY attached.
// When recording, the argv is wrapped by wardyn-rec (which execs asciinema or
// falls back to a .log). Returns once the process is started, not finished.
func (d *Driver) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("docker: exec: empty argv")
	}
	// EXEC-LESS path: a deferred (krun) agent is created NOW with the workload as
	// its main process — there is no exec to attach, and the container IS the agent
	// (empty exec id => the reconciler uses container Status for liveness).
	// CLAIM the pending entry (delete + mark creating) in ONE critical section so a
	// concurrent teardown cannot observe a ref that is neither pending nor created.
	d.mu.Lock()
	p, isPending := d.pending[ref]
	if isPending {
		delete(d.pending, ref)
		d.creating[ref] = true
	}
	d.mu.Unlock()
	if isPending {
		return "", d.runAsMainProcess(ctx, ref, p, argv)
	}
	cmd := argv
	if d.cfg.Record {
		// Recover the run id from the container label so wardyn-rec names the
		// recording deterministically. If we cannot, record under a nil id
		// rather than failing the exec.
		runID := uuid.Nil
		if insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{}); err == nil && insp.Container.Config != nil {
			if id, perr := parseRunID(insp.Container.Config.Labels[labelRun]); perr == nil {
				runID = id
			}
		}
		cmd = d.recordCmd(runID, defaultCastDir, argv)
	}
	execCfg := client.ExecCreateOptions{
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	}
	created, err := d.cli.ExecCreate(ctx, ref, execCfg)
	if err != nil {
		return "", fmt.Errorf("docker: exec create: %w", err)
	}
	// Track this exec id as the agent process for ref so Wait can observe its
	// completion + exit code. The latest Exec for a ref wins (re-exec replaces).
	d.mu.Lock()
	d.agentExecs[ref] = created.ID
	d.mu.Unlock()
	// Attach (TTY hijack) so output can be wired to wardyn-rec / a sink. The
	// caller (control plane) owns the lifecycle of the returned stream; for v0
	// we start detached after establishing the attach to confirm liveness.
	attachRes, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return "", fmt.Errorf("docker: exec attach: %w", err)
	}
	resp := attachRes.HijackedResponse
	// We do not block on the process; drain in the background so the PTY does
	// not stall. A real recorder pipeline replaces this drain.
	go func() {
		defer resp.Close()
		_, _ = io.Copy(io.Discard, resp.Reader)
	}()
	return created.ID, nil
}

// runAsMainProcess is the exec-less agent-launch path (krun microVMs): it creates
// the deferred agent container with the (recorder-wrapped) workload as its MAIN
// process and starts it. Unlike the exec path, there is no separate process to
// attach — dockerd drains the main-process PTY into its log driver, and Wait
// blocks on the CONTAINER's exit. Returns once the container is started.
func (d *Driver) runAsMainProcess(ctx context.Context, ref string, p *pendingAgent, argv []string) error {
	cmd := argv
	if d.cfg.Record {
		runID := uuid.Nil
		if p.cfg != nil {
			if id, perr := parseRunID(p.cfg.Labels[labelRun]); perr == nil {
				runID = id
			}
		}
		// Exec-less agents run as a non-root user and have no root exec to create
		// the root-owned default cast dir; record into an agent-writable tmpfs dir
		// instead (mainProcCastDir) — recordCmd's upload-vs-shared-mount mutual
		// exclusion is otherwise identical to Exec's.
		cmd = d.recordCmd(runID, mainProcCastDir, argv)
	}
	p.cfg.Cmd = cmd
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           p.cfg,
		HostConfig:       p.host,
		NetworkingConfig: p.netcfg,
		Name:             ref,
	})
	if err != nil {
		d.mu.Lock()
		delete(d.creating, ref)
		d.mu.Unlock()
		return fmt.Errorf("docker: create main-process agent: %w", err)
	}
	// Same fail-closed cap-enforcement gate the exec-based path applies in
	// CreateSandbox (verifyCapsEnforced) — the exec-less container IS the agent
	// (its main process is the untrusted workload), so it needs the identical
	// guard before ContainerStart.
	// Applied here rather than after Exec returns because the create-response
	// Warnings this reads only exist right after ContainerCreate.
	if capErr := verifyCapsEnforced(created.Warnings); capErr != nil {
		if d.cfg.AllowUnenforceableCaps {
			slog.Warn("wardynd: the daemon discarded a resource limit — proceeding because WARDYN_ALLOW_UNENFORCEABLE_CAPS=1; the sandbox may run without CPU/memory/pids limits",
				slog.String("detail", capErr.Error()))
		} else {
			d.mu.Lock()
			delete(d.creating, ref)
			d.mu.Unlock()
			rmCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			if _, rerr := d.cli.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !isNotFound(rerr) {
				return fmt.Errorf("docker: main-process agent failed the cap check and could not be removed: %w (cap error: %v)", rerr, capErr)
			}
			return capErr
		}
	}
	_, startErr := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	// The container now exists on the daemon, so re-check the claim: a teardown
	// that ran during the create found NO container to remove and reported
	// idempotent success, so removing this one is our job — a killed sandbox must
	// never end up with a live agent.
	d.mu.Lock()
	tornDown := !d.creating[ref]
	delete(d.creating, ref)
	if !tornDown && startErr == nil {
		d.mainProc[ref] = true
	}
	d.mu.Unlock()
	if tornDown {
		// Not ctx: the kill cascade that tore this ref down may already have
		// cancelled it, and the removal must still happen (fail closed).
		rmCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		if _, rerr := d.cli.ContainerRemove(rmCtx, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !isNotFound(rerr) {
			return fmt.Errorf("docker: sandbox %s torn down during agent create, and removing the created agent failed: %w", ref, rerr)
		}
		return fmt.Errorf("docker: sandbox %s torn down during agent create", ref)
	}
	if startErr != nil {
		return fmt.Errorf("docker: start main-process agent: %w", startErr)
	}
	return nil
}

// Wait blocks until the agent process started by Exec for ref has exited and
// returns its exit code. It is only valid after a successful Exec on the same
// ref: it inspects the agent exec id Exec recorded. Unlike waitExec (a bounded
// best-effort poll used for one-shot setup execs), Wait is unbounded and bound
// only by ctx — the agent process may run for as long as the run is alive.
// Returns an error if no agent exec is tracked for ref, or if ctx is cancelled
// before the process exits.
func (d *Driver) Wait(ctx context.Context, ref string) (int, error) {
	d.mu.Lock()
	isMain := d.mainProc[ref]
	execID, ok := d.agentExecs[ref]
	d.mu.Unlock()
	// EXEC-LESS path: the workload IS the container's main process, so its exit is
	// the container's exit.
	if isMain {
		return d.waitMainProcess(ctx, ref)
	}
	if !ok {
		return 0, fmt.Errorf("docker: wait: no agent exec tracked for ref %q (Exec not called?)", ref)
	}
	return d.pollExecExit(ctx, execID)
}

// pollExecExit polls execID via ExecInspect until it stops running and
// returns its exit code. ExecInspect.Running flips to false once the process
// exits; ExitCode is then authoritative. The poll cadence matches waitExec;
// this loop is unbounded (bound only by ctx), which is what distinguishes it
// from waitExec's bounded best-effort poll. Factored out of Wait so
// ExecStream's returned ExecSession.Wait closure can observe a DIFFERENT
// exec's completion (a streamed exec, not the tracked agent exec) via the
// exact same, already-proven polling contract.
func (d *Driver) pollExecExit(ctx context.Context, execID string) (int, error) {
	errs := 0
	for {
		insp, err := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		switch {
		case err == nil:
			errs = 0
			if !insp.Running {
				return insp.ExitCode, nil
			}
		case isNotFound(err):
			return 0, fmt.Errorf("docker: exec wait: exec inspect: %w", err)
		default:
			if errs++; errs >= waitMaxProbeErrors {
				return 0, fmt.Errorf("docker: exec wait: exec inspect (%d consecutive errors): %w", errs, err)
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: exec wait: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// waitMainProcess blocks on the exec-less agent container's exit and returns its
// code. ContainerWait with WaitConditionNotRunning returns the code even if the
// container has already exited, so there is no create/exit race.
func (d *Driver) waitMainProcess(ctx context.Context, ref string) (int, error) {
	errs := 0
	for {
		// v29: ContainerWait returns a single result carrying both the status and
		// error channels (was a bare two-channel return); the select is otherwise
		// unchanged.
		wait := d.cli.ContainerWait(ctx, ref, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		var err error
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: wait (main process): %w", ctx.Err())
		case res := <-wait.Result:
			if res.Error != nil {
				return int(res.StatusCode), fmt.Errorf("docker: wait (main process): %s", res.Error.Message)
			}
			return int(res.StatusCode), nil
		case err = <-wait.Error:
		}
		// Same budget as the exec path: a transient daemon error is not an exit.
		if isNotFound(err) {
			return 0, fmt.Errorf("docker: wait (main process): %w", err)
		}
		if errs++; errs >= waitMaxProbeErrors {
			return 0, fmt.Errorf("docker: wait (main process) (%d consecutive errors): %w", errs, err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("docker: wait (main process): %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func (d *Driver) Status(ctx context.Context, ref string) (runner.Status, error) {
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "container not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("docker: inspect: %w", err)
	}
	return statusFromInspect(res.Container), nil
}

// mainProcessExecID is the sentinel internal/api/runs_dispatch.go persists for
// an EXEC-LESS launch (krun runtime: Exec returns "" with a nil error because
// the workload runs as the container's own main process, not a separate
// exec — see runAsMainProcess). Duplicated here (same literal) rather than
// imported: internal/api sits above this concrete substrate and must stay
// target-agnostic, so it cannot import internal/runner/docker. MUST match the
// literal in runs_dispatch.go — see that constant's doc comment for
// why a bare "" can no longer double for this case.
const mainProcessExecID = "main-process"

// AgentStatus reports the agent's liveness in a restart-safe way. For an
// exec-based ref (agentExecID != "" and not the mainProcessExecID sentinel) it
// inspects that exec: Running => alive (RUNNING); exited => terminal with the
// real exit code, EVEN while the idle sandbox container is still up — the case
// container Status cannot detect after a restart dropped the in-memory exec
// map. When agentExecID is "" OR the mainProcessExecID sentinel (exec-less/
// main-process, or Exec never ran) the container IS the agent, so fall back to
// Status — "main-process" was never actually passed to ExecCreate, so probing
// it as a real exec id would only ever produce the ambiguous-404 error below.
//
// AMBIGUOUS exec-404 (GAP-RECONCILE-1): docker keeps exec records in daemon
// MEMORY only, so a daemon restart under live-restore erases them while the
// container and its exec'd process keep running. An exec-404 is therefore only
// DEFINITIVE when the container is ITSELF gone/stopped; while the container is
// still RUNNING it is ambiguous (the agent may be alive), so return an ERROR to
// route the caller into its bounded-retry/backoff path rather than finalizing a
// healthy live-restore run.
func (d *Driver) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	if agentExecID == "" || agentExecID == mainProcessExecID {
		return d.Status(ctx, ref)
	}
	insp, err := d.cli.ExecInspect(ctx, agentExecID, client.ExecInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			// Only definitive when the container is also gone/stopped.
			if cst, cerr := d.Status(ctx, ref); cerr == nil && cst.State == types.RunStopped {
				return runner.Status{State: types.RunStopped, Message: "agent exec and container both gone"}, nil
			}
			return runner.Status{}, fmt.Errorf("docker: agent exec %q not found but container still running (ambiguous; daemon restart under live-restore?)", agentExecID)
		}
		return runner.Status{}, fmt.Errorf("docker: agent exec inspect: %w", err)
	}
	if insp.Running {
		return runner.Status{State: types.RunRunning}, nil
	}
	code := insp.ExitCode
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
}
