// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// k8sPollInterval paces every bounded poll in this package (canary terminal
// state, proxy podIP, ephemeral-container exit) — mirrors the docker driver's
// pollInterval. canaryWaitTimeout is generous: a canary pod's image (the
// wardyn-proxy image, shared with the real proxy sidecar) may need a cold
// pull the first time a node runs it.
const (
	k8sPollInterval   = 200 * time.Millisecond
	canaryWaitTimeout = 3 * time.Minute
	// podIPWaitTimeout bounds CreateSandbox's wait for the proxy pod's CNI-
	// assigned IP (needed for the agent pod's hostAliases entry). Shorter than
	// canaryWaitTimeout: by CreateSandbox time the proxy image has normally
	// already been pulled once (by the boot-time canary or a prior run).
	podIPWaitTimeout = 90 * time.Second
)

// canaryVerdict is the boot-time egress canary's outcome.
type canaryVerdict int

const (
	// canaryIndeterminate means a non-network failure (ImagePullBackOff,
	// scheduling, crash before Running, or phase A itself failing to prove
	// baseline apiserver reachability) — never a NetworkPolicy verdict.
	canaryIndeterminate canaryVerdict = iota
	// canaryEnforced means phase B's deny-all NetworkPolicy blocked the
	// canary's connect: this cluster's CNI enforces NetworkPolicy.
	canaryEnforced
	// canaryUnenforced means phase B's canary connected DESPITE the deny-all
	// NetworkPolicy: this cluster's CNI does not enforce it.
	canaryUnenforced
)

// terminalWaitingReasons are ContainerStateWaiting.Reason values that will
// never resolve on their own — detecting them lets an INDETERMINATE verdict
// return promptly instead of running out the full canaryWaitTimeout. Not
// exhaustive (the timeout is the general-purpose safety net for anything
// else, including a stuck-Pending scheduling failure); this is the fast path
// for the common cases.
var terminalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerError":       true,
	"CreateContainerConfigError": true,
	"InvalidImageName":           true,
	"CrashLoopBackOff":           true,
}

// canaryPhaseResult is one canary phase's outcome. err non-nil means the
// phase itself could not reach a verdict (indeterminate).
type canaryPhaseResult struct {
	reachedRunning bool
	exitCode       int32
	err            error
}

// runEgressCanary runs the two-phase boot-time canary described in the
// package doc: phase A (no NetworkPolicy) must prove the cluster is
// reachable at all; phase B (a deny-all NetworkPolicy scoped to the canary
// pod) then proves whether that policy actually takes effect. Only a
// Running pod's connect outcome is evidence either way — anything else is
// indeterminate and the constructor refuses to boot.
func (d *Driver) runEgressCanary(ctx context.Context) (canaryVerdict, error) {
	a := d.runCanaryPhase(ctx, "phase A (no NetworkPolicy)", false)
	if a.err != nil {
		return canaryIndeterminate, fmt.Errorf("egress canary: %w: %w", a.err, errCanaryIndeterminate)
	}
	if !a.reachedRunning || a.exitCode != 0 {
		return canaryIndeterminate, fmt.Errorf("egress canary phase A (no NetworkPolicy) did not confirm baseline apiserver reachability (reached_running=%v exit_code=%d): %w",
			a.reachedRunning, a.exitCode, errCanaryIndeterminate)
	}

	b := d.runCanaryPhase(ctx, "phase B (deny-all NetworkPolicy)", true)
	if b.err != nil {
		return canaryIndeterminate, fmt.Errorf("egress canary: %w: %w", b.err, errCanaryIndeterminate)
	}
	if !b.reachedRunning {
		return canaryIndeterminate, fmt.Errorf("egress canary phase B (deny-all NetworkPolicy) pod never reached Running: %w", errCanaryIndeterminate)
	}
	if b.exitCode == 0 {
		return canaryUnenforced, nil
	}
	return canaryEnforced, nil
}

// runCanaryPhase launches one canary pod (optionally behind a deny-all
// NetworkPolicy scoped to it), waits for a terminal verdict, and cleans up
// the pod + netpol in every path (defer). denyAll selects phase B.
func (d *Driver) runCanaryPhase(ctx context.Context, phaseName string, denyAll bool) canaryPhaseResult {
	ns := d.cfg.Namespace
	suffix := uuid.New().String()
	podName := "wardyn-egress-canary-" + suffix
	labels := map[string]string{labelManaged: "true", labelComponent: componentCanary}

	if denyAll {
		netpolName := "wardyn-egress-canary-netpol-" + suffix
		netpol := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: netpolName, Namespace: ns, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: labels},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				// Egress deliberately left nil: "If this field is empty then this
				// NetworkPolicy limits all outgoing traffic" (deny-all egress).
			},
		}
		if _, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, netpol, metav1.CreateOptions{}); err != nil {
			return canaryPhaseResult{err: fmt.Errorf("%s: create deny-all netpol: %w", phaseName, err)}
		}
		defer func() {
			_ = d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(context.Background(), netpolName, metav1.DeleteOptions{})
		}()
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{{
				Name:            canaryContainerName,
				Image:           d.cfg.ProxyImage,
				Args:            []string{"-egress-canary", d.apiserverHostPort},
				SecurityContext: restrictedSecurityContext(),
				Resources:       canaryResources(),
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return canaryPhaseResult{err: fmt.Errorf("%s: create canary pod: %w", phaseName, err)}
	}
	defer func() {
		_ = d.clientset.CoreV1().Pods(ns).Delete(context.Background(), podName, metav1.DeleteOptions{})
	}()

	reached, code, err := d.waitCanaryTerminal(ctx, podName)
	if err != nil {
		return canaryPhaseResult{reachedRunning: reached, err: fmt.Errorf("%s: %w", phaseName, err)}
	}
	return canaryPhaseResult{reachedRunning: reached, exitCode: code}
}

// waitCanaryTerminal polls the canary pod until its canaryContainerName
// status shows Terminated (returning its exit code) or a fast-path terminal
// Waiting reason (returning an error), bounded by canaryWaitTimeout — an
// ordinary scheduling stall that never resolves surfaces via that timeout.
func (d *Driver) waitCanaryTerminal(ctx context.Context, podName string) (reachedRunning bool, exitCode int32, err error) {
	pollErr := wait.PollUntilContextTimeout(ctx, k8sPollInterval, canaryWaitTimeout, true, func(pollCtx context.Context) (bool, error) {
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if gerr != nil {
			return false, gerr
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != canaryContainerName {
				continue
			}
			if cs.State.Running != nil {
				reachedRunning = true
				return false, nil
			}
			if t := cs.State.Terminated; t != nil {
				reachedRunning = true
				exitCode = t.ExitCode
				return true, nil
			}
			if w := cs.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
				return false, fmt.Errorf("canary container stuck waiting (%s): %s", w.Reason, w.Message)
			}
		}
		return false, nil
	})
	if pollErr != nil {
		return reachedRunning, 0, pollErr
	}
	return reachedRunning, exitCode, nil
}

// canaryResources is the egress canary's cgroup envelope: it does nothing but
// dial a TCP socket and exit, so a minimal, fixed footprint (independent of
// any run's spec.Resources — there is no run yet at construction time) is
// always correct.
func canaryResources() corev1.ResourceRequirements {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(32*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}
