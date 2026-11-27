// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.LostProxyStopper = (*Driver)(nil)

// StopLostProxyAt stops the proxy of a server run marked lost but not ended, and keeps its agent with no network,
// acting for policy generation gen (runner.LostProxyStopper). Only the lost-run path calls it; StopProxy keeps its
// answer. It keeps a run only when every gate passes: the run has its Service (a run created before the Service was
// made has none), its namespace's NetworkPolicy verdict is proven (an acknowledged or unenforced one never keeps, as
// the proxy is then the agent's only fence), and the object that carries the agent's owner is owned by exactly the
// proxy pod read, by the run's Service, or by nothing (lostAgentOwner). Any miss answers runner.ErrEndUnsupported and
// the run is torn down.
//
// Then the run's EndpointSlice is emptied and the proxy pod read is deleted with a 1 s grace, under its UID and with
// orphan propagation, so the garbage collector removes it from the agent's owners instead of deleting the agent; the
// call returns nil only once that pod is gone. From then on the agent has no owner. If the slice or the pod cannot be
// settled, the agent is stopped too and the answer is a plain error, which the lease sweep retries; its next pass finds
// no agent and tears the run down. A proxy pod of a newer generation refuses the call with
// runner.ErrProxyGenerationNewer and nothing of that revive is stopped.
func (d *Driver) StopLostProxyAt(ctx context.Context, ref string, gen int) error {
	if d.runnerMode() {
		return runner.ErrEndUnsupported
	}
	runID, ns, svc, err := d.readRunService(ctx, ref)
	if errors.Is(err, runner.ErrReviveUnsupported) {
		return runner.ErrEndUnsupported
	}
	if err != nil {
		return err
	}
	if !d.netPolProvenIn(ns) {
		return fmt.Errorf("%w: namespace %q has no proven NetworkPolicy verdict", runner.ErrEndUnsupported, ns)
	}
	old, err := d.proxyNotNewer(ctx, ns, runID, gen, false)
	if err != nil {
		return err
	}
	agent, err := d.lostAgentOwner(ctx, ns, runID, old, svc)
	if err != nil {
		return err
	}
	stepErr := d.emptyKeepEndpointsAt(ctx, ns, runID, svc, gen, false)
	if stepErr == nil {
		if rerr := d.removeProxyPod(ctx, ns, runID, old, true); rerr != nil {
			stepErr = fmt.Errorf("remove the proxy pod: %w", rerr)
		}
	}
	if stepErr != nil {
		if errors.Is(stepErr, runner.ErrProxyGenerationNewer) {
			return stepErr
		}
		if gen > 0 { // a newer revive's agent is never stopped
			if _, perr := d.proxyNotNewer(ctx, ns, runID, gen, false); errors.Is(perr, runner.ErrProxyGenerationNewer) {
				return perr
			}
		}
		return fmt.Errorf("k8s: stop a lost run's proxy: %w", errors.Join(stepErr, d.stopLostAgent(ctx, ns, runID, agent)))
	}
	if gen > 0 {
		if _, err := d.proxyNotNewer(ctx, ns, runID, gen, false); err != nil {
			return err
		}
	}
	return nil
}

// netPolProvenIn is whether ns's live NetworkPolicy verdict is proven, whatever the override settings allow.
func (d *Driver) netPolProvenIn(ns string) bool {
	v := d.netPol.Load()
	return v != nil && v.byNS[ns].state == netPolProven
}

// lostAgentOwner reads the run's agent pod and the object that carries its owner: the pod itself, or on the
// agent-sandbox backend its Sandbox (the pod's own owner there is the Sandbox, which never decides). It answers the
// agent pod when that object is owned by exactly one of: proxy, the proxy pod as read (a run whose agent its proxy
// owns from create); the run's Service svc (a run kept at its end); or nothing (already released by an earlier
// stop). An agent or Sandbox that is gone or being deleted, or any other owner, answers runner.ErrEndUnsupported; a
// read that fails any other way is a plain error.
func (d *Driver) lostAgentOwner(ctx context.Context, ns string, runID uuid.UUID, proxy *corev1.Pod, svc *corev1.Service) (*corev1.Pod, error) {
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return nil, fmt.Errorf("%w: the agent pod is gone", runner.ErrEndUnsupported)
	}
	if err != nil {
		return nil, fmt.Errorf("k8s: read the agent pod: %w", err)
	}
	if pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("%w: the agent pod is being deleted", runner.ErrEndUnsupported)
	}
	owners := pod.OwnerReferences
	if d.cfg.AgentSandbox {
		sb, err := d.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: the agent's Sandbox is gone", runner.ErrEndUnsupported)
		}
		if err != nil {
			return nil, fmt.Errorf("k8s: read the agent's Sandbox: %w", err)
		}
		if sb.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("%w: the agent's Sandbox is being deleted", runner.ErrEndUnsupported)
		}
		owners = sb.GetOwnerReferences()
	}
	if len(owners) == 0 {
		return pod, nil
	}
	if o := owners[0]; len(owners) == 1 &&
		(o.Kind == "Pod" && proxy != nil && o.Name == proxyPodName(runID) && o.UID == proxy.UID ||
			o.Kind == "Service" && svc != nil && o.UID == svc.UID) {
		return pod, nil
	}
	return nil, fmt.Errorf("%w: the agent is owned by something other than its proxy pod as read or its Service", runner.ErrEndUnsupported)
}

// stopLostAgent stops the agent of a lost run whose proxy could not be confirmed gone: the agent pod read, under its
// UID, or on the agent-sandbox backend its Sandbox, as the controller would make a deleted pod again; then it waits for
// the pod to go.
func (d *Driver) stopLostAgent(ctx context.Context, ns string, runID uuid.UUID, agent *corev1.Pod) error {
	if d.cfg.AgentSandbox {
		if err := d.deleteRunSandbox(ctx, ns, runID); err != nil {
			return fmt.Errorf("stop the agent: %w", err)
		}
	} else {
		uid := agent.UID
		err := d.clientset.CoreV1().Pods(ns).Delete(ctx, agent.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("stop the agent: delete its pod: %w", err)
		}
	}
	if err := d.waitPodGone(ctx, ns, agent, 2*time.Minute); err != nil {
		return fmt.Errorf("stop the agent: wait for its pod to go: %w", err)
	}
	return nil
}

// lostReplaceGate lets a new proxy start only for a run that StopLostProxyAt kept: the run has its Service, its
// namespace's verdict is proven, its agent is owned by its Service or by nothing, no proxy pod is there, and the run's
// EndpointSlice names no endpoint. So a running run, whose proxy pod is there even when it crashed, is never sent a
// new proxy. A miss answers runner.ErrReviveUnsupported itself, the answer a substrate with no proxy replacement
// gives; a read that fails any other way is a plain error.
func (d *Driver) lostReplaceGate(ctx context.Context, ref string) (uuid.UUID, string, *corev1.Service, error) {
	runID, ns, svc, err := d.readRunService(ctx, ref)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	if !d.netPolProvenIn(ns) {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	}
	if _, err := d.lostAgentOwner(ctx, ns, runID, nil, svc); errors.Is(err, runner.ErrEndUnsupported) {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	} else if err != nil {
		return uuid.Nil, "", nil, err
	}
	pod, err := d.proxyNotNewer(ctx, ns, runID, 0, false)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	if pod != nil {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	}
	slice, err := d.clientset.DiscoveryV1().EndpointSlices(ns).Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return uuid.Nil, "", nil, fmt.Errorf("k8s: read the run's EndpointSlice: %w", err)
	}
	if err == nil && len(slice.Endpoints) > 0 {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	}
	return runID, ns, svc, nil
}
