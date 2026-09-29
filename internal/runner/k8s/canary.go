// Copyright 2025 The Wardyn Authors
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
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const k8sPollInterval = 200 * time.Millisecond

// Vars, not consts, only so tests can shorten them.
var (
	// canaryWaitTimeout is generous: an image may need a cold pull on a node's first run.
	canaryWaitTimeout = 3 * time.Minute
	// podIPWaitTimeout bounds scheduling only: the CNI assigns a pod's IP before any image pull.
	podIPWaitTimeout = 90 * time.Second
)

// canaryVerdict is the boot-time egress canary's outcome.
type canaryVerdict int

const (
	canaryIndeterminate canaryVerdict = iota // non-network failure — never a NetworkPolicy verdict
	canaryEnforced                           // phase B's deny-all blocked the connect: CNI enforces NetworkPolicy
	canaryUnenforced                         // phase B connected DESPITE deny-all: CNI does not enforce NetworkPolicy
	// canaryAcknowledged: phase A failed in the shape an ambient default-deny NetworkPolicy produces, and the
	// operator accepted that via WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1. Phase B is SKIPPED — behind an existing
	// default-deny it could only also exit 1, proving nothing. Never proof of enforcement, only an accepted risk.
	canaryAcknowledged
)

// terminalWaitingReasons are Waiting reasons that never resolve on their own, so an INDETERMINATE verdict can
// return promptly instead of running out canaryWaitTimeout. Not exhaustive; the timeout covers the rest.
// Lives in runner.TerminalWaitingReasons (tagless: the control plane needs it and can't import a k8s-tagged symbol).
var terminalWaitingReasons = runner.TerminalWaitingReasons

// canaryPhaseResult is one canary phase's outcome; err non-nil means indeterminate.
type canaryPhaseResult struct {
	reachedRunning bool
	exitCode       int32
	err            error
}

// runEgressCanary runs the two-phase boot-time canary: phase A (no NetworkPolicy) proves the cluster is
// reachable; phase B (deny-all scoped to the canary pod) proves whether that policy takes effect. Only a
// Running pod's connect outcome is evidence either way — anything else is indeterminate and boot refuses.
func (d *Driver) runEgressCanary(ctx context.Context) (canaryVerdict, error) {
	a := d.runCanaryPhase(ctx, "phase A (no NetworkPolicy)", false)
	if a.err != nil {
		return canaryIndeterminate, fmt.Errorf("egress canary: %w: %w", a.err, errCanaryIndeterminate)
	}
	if !a.reachedRunning || a.exitCode != 0 {
		// Reached RUNNING then exited exactly 1 is the one shape phase A can produce that looks like an
		// ambient default-deny NetworkPolicy rather than a non-network failure; the ack is scoped to it.
		if d.cfg.AckAmbientDefaultDeny && a.reachedRunning && a.exitCode == 1 {
			return canaryAcknowledged, nil
		}
		// SECURITY: phase A applies no NetworkPolicy of its own, so a failure here is indistinguishable from
		// an existing default-deny elsewhere. The remediation must NOT be an allow policy for
		// wardyn.managed=true: allows are additive and both agent and proxy pods carry that label, so it
		// would widen every run past sandbox.go's per-run deny.
		return canaryIndeterminate, fmt.Errorf("egress canary phase A (no NetworkPolicy) did not confirm baseline apiserver "+
			"reachability (reached_running=%v exit_code=%d) — if this namespace already has a default-deny NetworkPolicy from "+
			"elsewhere (unrelated to Wardyn), that is the likely cause: use a namespace with no ambient default-deny, or exempt "+
			"Wardyn's pods from THAT policy's own podSelector (a matchExpressions entry with key wardyn.managed, operator NotIn, "+
			"values [\"true\"]). Do NOT add a separate allow policy for wardyn.managed=true — NetworkPolicy allows are additive "+
			"and both the agent and proxy pods carry that label, so it would widen every sandbox pod's egress past Wardyn's own "+
			"per-run deny+proxy-only rule. If a canary pod reaching Running and exiting exactly 1 IS the expected shape here — "+
			"an ambient default-deny NetworkPolicy the platform already applies, not a Wardyn misconfiguration — set "+
			"WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1 (exact case, exact value \"1\") to acknowledge it and boot anyway: %w",
			a.reachedRunning, a.exitCode, errCanaryIndeterminate)
	}

	b := d.runCanaryPhase(ctx, "phase B (deny-all NetworkPolicy)", true)
	if b.err != nil {
		return canaryIndeterminate, fmt.Errorf("egress canary: %w: %w", b.err, errCanaryIndeterminate)
	}
	if !b.reachedRunning {
		return canaryIndeterminate, fmt.Errorf("egress canary phase B (deny-all NetworkPolicy) pod never reached Running: %w", errCanaryIndeterminate)
	}
	switch b.exitCode {
	case 0:
		return canaryUnenforced, nil
	case 1:
		return canaryEnforced, nil
	default:
		// -egress-canary only ever exits 0 or 1; anything else is not evidence of either verdict.
		return canaryIndeterminate, fmt.Errorf("egress canary phase B (deny-all NetworkPolicy) exited %d, neither the expected 0 (connected) nor 1 (blocked): %w", b.exitCode, errCanaryIndeterminate)
	}
}

// runCanaryPhase launches one canary pod (optionally behind a deny-all NetworkPolicy scoped to it), waits
// for a terminal verdict, and cleans up the pod + netpol in every path (defer). denyAll selects phase B.
func (d *Driver) runCanaryPhase(ctx context.Context, phaseName string, denyAll bool) canaryPhaseResult {
	ns := d.cfg.Namespace
	suffix := uuid.New().String()
	podName := "wardyn-egress-canary-" + suffix
	// labelRun carries this invocation's own unique suffix so two wardynd instances booting concurrently
	// don't share a selector — instance A's phase-B deny-all netpol must never match instance B's phase-A pod.
	labels := map[string]string{labelManaged: "true", labelComponent: componentCanary, labelRun: suffix}

	if denyAll {
		netpolName := "wardyn-egress-canary-netpol-" + suffix
		netpol := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: netpolName, Namespace: ns, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: labels},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				// Egress left nil: an empty rule set means deny-all outgoing traffic.
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
			// Same as the sandbox pods: the canary dials the one host:port it was handed, reads no environment.
			EnableServiceLinks: boolPtr(false),
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

// waitCanaryTerminal polls until the canary container Terminates (returns its exit code) or hits a
// fast-path terminal Waiting reason (returns an error), bounded by canaryWaitTimeout.
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
		// Name the remedy on timeout — "timed out" alone gives an operator nothing to act on.
		if errors.Is(pollErr, context.DeadlineExceeded) {
			return reachedRunning, 0, fmt.Errorf("timed out after %s waiting for the canary pod to reach a terminal state (reached_running=%v) — pre-pull the wardyn-proxy image onto this cluster's nodes, or check scheduling capacity (node resources, taints/tolerations): %w",
				canaryWaitTimeout, reachedRunning, pollErr)
		}
		return reachedRunning, 0, pollErr
	}
	return reachedRunning, exitCode, nil
}

// canaryResources: the canary only dials a TCP socket and exits, so a minimal fixed footprint suffices.
func canaryResources() corev1.ResourceRequirements {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(32*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}
