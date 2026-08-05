// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// byoiImagePrefix marks a BYOI (bring-your-own-image) run — mirrors
// internal/api/runs_dispatch.go's own `strings.HasPrefix(image, "wardyn-byoi/")`
// check. Re-derived here (not imported: internal/api is outside this
// package's build tag) so Exec can refuse it BEFORE creating any ephemeral
// container, entirely at the substrate seam.
const byoiImagePrefix = "wardyn-byoi/"

const (
	execWaitPollInterval = 1 * time.Second
	// execWaitMaxProbeErrors mirrors docker's waitMaxProbeErrors: ~5 minutes of
	// tolerance for a transient apiserver blip at 1s spacing before Wait gives
	// up. A transient error is NOT "the agent exited" — it must not surface as
	// one (see docker's identical comment on waitMaxProbeErrors).
	execWaitMaxProbeErrors = 300
)

// notFoundExitCode is what Wait reports when the pod vanishes without ever
// showing a terminated exec status ("pod NotFound is authoritative
// completion" — the A0 contract correction). Non-zero (routes to FAILED, not
// a false COMPLETED): most of the time this is our OWN KillSandbox having
// already deleted the pod, in which case the run is already KILLED and this
// value is never written (runs_lifecycle.go's completion-watcher CAS only
// applies FROM RUNNING) — but on the rarer path where the pod disappeared for
// some other reason (node drain, an external delete) while the run was still
// RUNNING, reporting failure is the honest choice: Wardyn has no idea what
// actually happened and must never claim a vanished sandbox as a false
// success.
const notFoundExitCode = -1

// Exec launches the agent process as an ephemeral container named
// "wardyn-agent" (execContainerName) on the sandbox ref. THREE hard API
// facts drive this shape (the A0 contract):
//   - the ephemeral container's OWN securityContext must carry the full
//     restricted set — Pod Security Standards admission checks ephemeral
//     containers, not just the pod;
//   - it must NOT set Resources — the apiserver rejects a resource request on
//     an ephemeral container (pod-level bounding only);
//   - ephemeral containers are ADD-ONLY, so this is one-shot per ref: a
//     second Exec (see errSecondExec) and a BYOI image (see errBYOIUnsupported,
//     refused before the first) both fail closed with a clear error rather
//     than a silent no-op or a stale id.
//
// Env is copied from the pod's existing main container spec — ephemeral
// containers inherit nothing — read from the live pod object, so this is
// restart-safe (no in-memory state).
func (d *Driver) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("k8s: exec: empty argv")
	}
	ns := d.cfg.Namespace
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: exec: get pod %q: %w", ref, err)
	}

	main, ok := findContainer(pod.Spec.Containers, mainContainerName)
	if !ok {
		return "", fmt.Errorf("k8s: exec: sandbox %q has no %q container", ref, mainContainerName)
	}
	// BYOI refusal BEFORE any ephemeral container is created — see
	// errBYOIUnsupported's doc for why this must happen on the FIRST Exec
	// call (the selftest), not merely the second.
	if strings.HasPrefix(main.Image, byoiImagePrefix) {
		return "", errBYOIUnsupported
	}
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == execContainerName {
			return "", errSecondExec
		}
	}

	podCopy := pod.DeepCopy()
	podCopy.Spec.EphemeralContainers = append(podCopy.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:            execContainerName,
			Image:           main.Image,
			Command:         argv,
			Env:             main.Env, // copied verbatim: ephemeral containers inherit nothing
			SecurityContext: restrictedSecurityContext(),
			// Resources deliberately left zero-value: the apiserver rejects a
			// resource request on an ephemeral container.
		},
	})
	if _, err := d.clientset.CoreV1().Pods(ns).UpdateEphemeralContainers(ctx, ref, podCopy, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("k8s: exec: add ephemeral container: %w", err)
	}
	return execContainerName, nil
}

// findContainer returns the container named name, if present.
func findContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, c := range containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

// Wait blocks until the ephemeral exec Exec added terminates and returns its
// exit code. Restart-safe by construction: every poll reads live from the
// apiserver (pod.Status.EphemeralContainerStatuses), carrying no in-memory
// exec-id map to lose across a wardynd restart (unlike docker's agentExecs
// map). Returns an error if Exec was never called for ref.
func (d *Driver) Wait(ctx context.Context, ref string) (int, error) {
	// ponytail: a 1s Get-poll of the pod, not a Watch. A watch.Interface
	// (informer) would save the apiserver a little chatter per in-flight run,
	// but polling is a five-line loop against the exact same
	// kubernetes.Interface every fake and real clientset already satisfies.
	// Upgrade to a watch if poll volume ever shows up as real apiserver load.
	errs := 0
	for {
		pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
		switch {
		case err == nil:
			errs = 0
			found := false
			for _, ec := range pod.Spec.EphemeralContainers {
				if ec.Name == execContainerName {
					found = true
					break
				}
			}
			if !found {
				return 0, fmt.Errorf("k8s: exec wait: no agent exec tracked for ref %q (Exec not called?)", ref)
			}
			for _, cs := range pod.Status.EphemeralContainerStatuses {
				if cs.Name != execContainerName {
					continue
				}
				if t := cs.State.Terminated; t != nil {
					return int(t.ExitCode), nil
				}
				break
			}
			// Added but not yet terminated (Waiting/Running/no status yet): keep polling.
		case isNotFound(err):
			return notFoundExitCode, nil
		default:
			if errs++; errs >= execWaitMaxProbeErrors {
				return 0, fmt.Errorf("k8s: exec wait: get pod (%d consecutive errors): %w", errs, err)
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("k8s: exec wait: %w", ctx.Err())
		case <-time.After(execWaitPollInterval):
		}
	}
}

// AgentStatus reports the agent's liveness restart-safely: it reads the
// ephemeral container's status straight from the apiserver on every call, so
// there is no in-memory state to lose across a wardynd restart. When
// agentExecID is "" (Exec never ran, or an exec-less path — none exists on
// this substrate today) it falls back to Status, where the pod IS the agent.
func (d *Driver) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	if agentExecID == "" {
		return d.Status(ctx, ref)
	}
	pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("k8s: agent status: get pod: %w", err)
	}
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if cs.Name != agentExecID {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			code := int(t.ExitCode)
			return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
		}
		if cs.State.Running != nil {
			return runner.Status{State: types.RunRunning}, nil
		}
		return runner.Status{State: types.RunStarting}, nil // Waiting, or no state populated yet
	}
	return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
}
