// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// agentPodWhereTimeout bounds the one read agentPodWhere makes.
const agentPodWhereTimeout = 5 * time.Second

// agentPodWhere is where the run's agent pod runs: the node the scheduler bound it to, and the RuntimeClass and
// handler createPreflight resolved. CreateSandbox calls it once the main container is Running, so the node is set.
// A failed read only leaves the node out: where a sandbox runs is a record, never a reason to fail its create.
// nil when nothing is known.
func (d *Driver) agentPodWhere(ctx context.Context, ns string, runID uuid.UUID, runtimeClass, runtimeHandler string) *runner.SandboxWhere {
	w := runner.SandboxWhere{RuntimeClass: runtimeClass, RuntimeHandler: runtimeHandler}
	gctx, cancel := context.WithTimeout(ctx, agentPodWhereTimeout)
	defer cancel()
	if pod, err := d.clientset.CoreV1().Pods(ns).Get(gctx, agentPodName(runID), metav1.GetOptions{}); err == nil {
		w.Node = pod.Spec.NodeName
	} else {
		slog.WarnContext(ctx, "k8s: read the agent pod's node for its start record", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	if w == (runner.SandboxWhere{}) {
		return nil
	}
	return &w
}
