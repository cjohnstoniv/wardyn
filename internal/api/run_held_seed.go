// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envHeldSeed carries an interactive container run's boot seed in place of envInteractiveSeed while
// WARDYN_START_AFTER_READBACK is on. A current agent image adopts it as its seed and its boot pane waits for the
// marker releaseHeldSeed writes once the proxy has confirmed its policy. An image older than the hold never
// reads it, so nothing starts at boot.
const envHeldSeed = "WARDYN_HELD_SEED"

// heldSeedReleaseTimeout bounds the release exec: one shell writing one file.
const heldSeedReleaseTimeout = 30 * time.Second

// heldSeedReleaseArgv writes the marker the boot pane waits for. It exits 3, writing nothing, when the image's
// agent-run has no held_seed_adopt line: such an image never adopts the held seed, so a marker would start nothing.
var heldSeedReleaseArgv = []string{"sh", "-c",
	`l=$(command -v agent-run) && grep -qx 'held_seed_adopt' "$l" || exit 3; h="${HOME:-/home/agent}"; mkdir -p "$h/.wardyn" && : > "$h/.wardyn/seed-release"`}

// heldSeedUnsupportedHint fails a held run whose image cannot hold its task until the readback.
const heldSeedUnsupportedHint = "This deployment starts an agent only after the run's proxy confirms its policy (WARDYN_START_AFTER_READBACK=on), and this run's image cannot hold its task until then. Rebuild the image on a current Wardyn agent image, start the run without a task, or ask an admin to turn the setting off"

// heldSeedAWSSSOHint fails a held run on the aws-sso image: that image never adopts the held seed, and a rebuild
// on a current image does not change that.
const heldSeedAWSSSOHint = "This deployment starts an agent only after the run's proxy confirms its policy (WARDYN_START_AFTER_READBACK=on), and the aws-sso image cannot hold its task until that readback, so the run was stopped. Start the run without a task, or ask an admin to turn the setting off"

// heldSeedReleaseFailedHint fails a held run whose task could not be released after the readback.
const heldSeedReleaseFailedHint = "The run's proxy confirmed its policy, but the run's task could not be started in the sandbox, so the run was stopped. Start the run again"

// releaseHeldSeed lets a held boot seed start. dispatchRun calls it once the readback confirmed the run's policy
// and the run is RUNNING. true when there is no held seed or it was released; false when the run was failed and
// its sandbox stopped, with a run.exec row saying why.
func (s *Server) releaseHeldSeed(ctx context.Context, run types.AgentRun, ref string, spec runner.SandboxSpec) bool {
	if spec.Env[envHeldSeed] == "" {
		return true
	}
	rctx, cancel := context.WithTimeout(ctx, heldSeedReleaseTimeout)
	exit, err := s.execToExit(rctx, ref, heldSeedReleaseArgv)
	cancel()
	if err == nil && exit == 0 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": heldSeedReleaseArgv, "reason": "seed_release"})))
		return true
	}
	hint := heldSeedReleaseFailedHint
	if exit == 3 || errors.Is(err, runner.ErrExecStreamUnsupported) {
		hint = heldSeedUnsupportedHint
		if run.Agent == awsSSOAgent {
			hint = heldSeedAWSSSOHint
		}
	}
	fields := map[string]any{"argv": heldSeedReleaseArgv, "exit_code": exit, "reason": "seed_release"}
	if err != nil {
		fields["error"] = auditStep(ctx, auditStepExecOpen, err, slog.String("run_id", run.ID.String()))
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
		run.ID.String(), "failure", mustJSON(fields)))
	s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
	s.failAndRevoke(ctx, run.ID, types.RunRunning, hint)
	return false
}

// execToExit runs argv in the sandbox to its end and returns its exit code. Its output is drained and discarded:
// an undrained stream blocks the exec's Wait. A ctx that ends first closes the exec and is the error.
func (s *Server) execToExit(ctx context.Context, ref string, argv []string) (int, error) {
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: argv})
	if err != nil {
		return -1, err
	}
	defer func() { _ = sess.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	var wg sync.WaitGroup
	for _, r := range []io.Reader{sess.Stdout, sess.Stderr} {
		if r == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(io.Discard, r)
		}()
	}
	wg.Wait()
	exit, werr := sess.Wait()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if werr != nil {
		return -1, werr
	}
	return exit, nil
}
