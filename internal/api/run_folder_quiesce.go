// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noProxyStop is quiesceKeptSandbox's answer for a sandbox not on a runner whose substrate can neither cut its agent
// off nor stop its proxy alone (Kubernetes in server mode). Nothing then cuts its egress, so its pass back runs only
// on an agent that has already ended (agentEnded).
var noProxyStop = notQuiescable("its substrate cannot stop its proxy alone")

// runnerEgressStopped is quiesceRunnerRun's answer for a runner that cannot cut its agent off (Caps.ProxyFence false:
// a Kubernetes runner, or one older than the fence) once its proxy stop returned: the agent has no way out, and it is
// left running, since its own exec is all a kill could reach there. The pass back runs, and the sandbox is not kept.
var runnerEgressStopped = notQuiescable("a sandbox on a runner is not kept: its proxy is stopped and its agent left running")

// errEgressOpen is agentEnded's answer for an agent that still runs, or whose state could not be read, in a sandbox
// whose egress nothing could cut: no pass runs, and the sandbox is stopped with the record saying why.
var errEgressOpen = errors.New("its substrate cannot stop its proxy alone, and its agent still runs")

// quiesceRunnerRun is quiesceKeptSandbox for an ended run on a runner. A runner not connected here is sent nothing and
// keeps the answer it had before the cut existed: its sandbox cannot be reached from here for a pass either. A runner
// that fences (Caps.ProxyFence) has its agent cut off every network, then killed, as the server's Docker does; a kill
// that fails after the cut runs the pass with the cut held. One that cannot fence has its proxy stopped, sent only
// while it is connected (runnerEgressStopped); a Kubernetes runner's stop answers nil only once the proxy's stop is
// proven, and deletes the agent's pod otherwise. A cut or a stop that fails, or whose runner left during it, runs no
// pass.
func (s *Server) quiesceRunnerRun(ctx context.Context, run types.AgentRun, ref string) error {
	if _, ok := s.runnerConnected(*run.RunnerID); !ok {
		return notQuiescable("a sandbox on a runner is not kept")
	}
	kind, err := s.cutAgent(ctx, run)
	switch {
	case err != nil && kind == cutFenced:
		return fmt.Errorf("its agent could not be cut off its networks: %w", err)
	case err != nil:
		return fmt.Errorf("its proxy did not stop: %w", err)
	case kind == cutUnsupported:
		// No released runner answers this; one that did has nothing cutting its egress, so its pass runs only on an
		// ended agent.
		return noProxyStop
	case kind == cutProxyStopped:
		return runnerEgressStopped
	}
	err = s.killKeptAgent(ctx, run, ref)
	if err != nil && !errors.Is(err, errSandboxStopped) {
		return notQuiescable("a sandbox on a runner is not kept: " + err.Error())
	}
	return err
}

// agentEnded is qerr, unless qerr is noProxyStop: nothing cut that sandbox's egress, so its pass back runs as it is
// only once run's agent reads as ended (qerr again), and otherwise not at all (errEgressOpen). It is asked after a
// person's bring-back has stopped the agent. An exec-less sandbox's agent is its main process, read as the sandbox
// (AgentStatus with no exec id); an agent whose exec is not known never reads as ended.
func (s *Server) agentEnded(ctx context.Context, run types.AgentRun, ref string, qerr error) error {
	var cannot notQuiescable
	if !errors.As(qerr, &cannot) || cannot != noProxyStop {
		return qerr
	}
	if run.AgentExecID == "" {
		return errEgressOpen
	}
	id := run.AgentExecID
	if id == runner.MainProcessExecID {
		id = ""
	}
	actx, cancel := teardownCtx(ctx)
	defer cancel()
	if st, err := s.cfg.Runner.AgentStatus(actx, ref, id); err == nil && isTerminalRunState(st.State) {
		return qerr
	}
	return errEgressOpen
}
