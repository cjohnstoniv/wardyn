// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.RunnerSandboxProver = (*Driver)(nil)

// ProveRunnerSandbox proves that ref names a sandbox Wardyn created for runnerID, checking the run
// and runner labels on each live pod of the run (design K1 §3.1; mirrors the docker driver). A
// missing pod proves nothing either way and is skipped; any pod of the run that exists must carry
// both labels, or the answer is ErrNotRunnerSandbox.
func (d *Driver) ProveRunnerSandbox(ctx context.Context, ref, runnerID string) error {
	id, err := runIDFromAgentPodName(ref)
	if err != nil || ref != agentPodName(id) || runnerID == "" {
		return fmt.Errorf("k8s: %q is not a Wardyn agent pod name: %w", ref, runner.ErrNotRunnerSandbox)
	}
	for _, name := range []string{ref, proxyPodName(id)} {
		pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("k8s: read %s: %w", name, err)
		}
		l := pod.Labels
		if l[labelManaged] != "true" || l[labelRun] != id.String() || l[runner.LabelRunnerID] != runnerID {
			return fmt.Errorf("k8s: pod %s: %w", name, runner.ErrNotRunnerSandbox)
		}
	}
	return nil
}

// SandboxRef is the ref CreateSandbox returns for runID: the agent pod's name.
func SandboxRef(runID uuid.UUID) string { return agentPodName(runID) }
