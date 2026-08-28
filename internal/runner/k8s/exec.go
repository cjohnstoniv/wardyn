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

	"github.com/google/uuid"
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
	// execWaitMaxProbeErrors mirrors docker's waitMaxProbeErrors BUDGET
	// (~1 minute), not its raw number: docker polls at 200ms so 300 errors
	// there is ~1 minute; this package polls at 1s, so the same ~1 minute
	// budget is 60, not 300 (L1 finding — the copied literal carried
	// docker's cadence-scaled count, not its time budget). A transient
	// error is NOT "the agent exited" — it must not surface as one (see
	// docker's identical comment on waitMaxProbeErrors).
	execWaitMaxProbeErrors = 60
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

	runID := uuid.Nil
	if id, perr := uuid.Parse(pod.Labels[labelRun]); perr == nil {
		runID = id
	}
	podCopy := pod.DeepCopy()
	podCopy.Spec.EphemeralContainers = append(podCopy.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:            execContainerName,
			Image:           main.Image,
			Command:         recordCmd(runID, argv),
			Env:             main.Env, // copied verbatim: ephemeral containers inherit nothing
			SecurityContext: agentSecurityContext(),
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

// recordCmd wraps argv with the recorder for every Exec — mirrors docker's
// recordCmd (internal/runner/docker/driver.go's recordCmd/recordCmd), but
// simpler: k8s has no RecordingMount config (SandboxSpec's Recording doc:
// "no shared-volume path" — mounts are impossible on this substrate), so
// delivery is ALWAYS the masked brokered upload, never the unmasked
// shared-mount fallback. hostAliases + NO_PROXY + the agent NetworkPolicy
// already permit exactly this route (the agent's only egress peer is the
// proxy on 3128). castDir is a plain "/tmp": unlike docker (one container,
// one filesystem), the ephemeral container has its OWN rootfs — ephemeral
// containers do NOT share the main container's filesystem, so
// AgentIdleScript's /tmp/wardyn (written by the MAIN container's idle
// process) is not visible here. Do not assume a shared /tmp.
//
// runID mirrors docker's own tolerance for an unresolvable label: recording
// is best-effort, so a missing/corrupt wardyn.run-id label degrades to a
// local-only (never-uploaded) cast rather than failing Exec outright.
//
// The Command IS the bare recorder argv — no shell, no "is wardyn-rec on
// PATH" fallback. This substrate has no docker-style Config.Record opt-out:
// Classes always advertises SessionRecording:true (driver.go), so an image
// that cannot honour it must fail closed, not silently downgrade to
// unrecorded — the same fail-closed posture invariant 5 (runner/substrate's
// package doc) requires everywhere else. An agent image on this substrate
// MUST ship wardyn-rec (image contract §3, deploy/images/README.md); one
// that doesn't fails Exec closed via the normal container-start error path
// (parity with docker: a Record-enabled docker exec with a missing
// wardyn-rec fails the same way, for the same reason). The conformance
// suite's own agent image is built from deploy/kind/Dockerfile.conformance-
// agent specifically so this path is exercised for real, not skipped.
func recordCmd(runID uuid.UUID, argv []string) []string {
	uploadURL := ""
	if runID != uuid.Nil {
		uploadURL = fmt.Sprintf("http://wardyn-proxy:%d/wardyn/v1/recordings/%s", runner.ProxyListenPort, runID)
	}
	return runner.RecorderArgv("/tmp", "", uploadURL, runID, argv)
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
				// Fail closed on a Waiting status the platform will never
				// resolve on its own (terminalWaitingReasons, canary.go) --
				// the shape a platform admission/policy engine or a missing
				// ephemeral-container feature produces: the container never
				// starts, so polling for Terminated would run out the ctx
				// deadline instead of ever returning. Without this, run.exec
				// still read success (the apiserver accepted the container
				// add) and the caller had no way to tell "still starting"
				// from "will never start" -- exactly the shape that made a
				// k8s connectivity probe hang for its full wait budget
				// whatever the network did (0.6.6).
				if w := cs.State.Waiting; w != nil && execNeverStartedReasons[w.Reason] {
					return 0, fmt.Errorf("k8s: exec wait: agent container never started (%s: %s): %w",
						w.Reason, w.Message, runner.ErrExecNeverStarted)
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
		if w := cs.State.Waiting; w != nil {
			// Named, not just RunStarting: this is the one detail an
			// operator (or the site-config probe's timed_out detail,
			// site_config_probe.go) has no other way to see -- whether the
			// container is still legitimately starting or is stuck on a
			// Reason that will never resolve (terminalWaitingReasons).
			return runner.Status{State: types.RunStarting, Message: waitingMessage(w)}, nil
		}
		return runner.Status{State: types.RunStarting}, nil // no state populated yet
	}
	// Present in Spec but absent from Status is the pre-kubelet window: the
	// apiserver accepted the exec and the kubelet has not published its first
	// status yet. Reporting THAT terminal (with a nil error, so nothing retries)
	// makes sweepRunWatchers and reconcileWatch finalize a healthy just-started
	// run FAILED and tear its sandbox down. It is the k8s half of
	// GAP-RECONCILE-1, which the docker driver hardened ("ambiguous; daemon
	// restart under live-restore?", driver.go) and this substrate never did --
	// with strictly better evidence available, since the pod Get succeeded.
	// An exec id absent from Spec was never exec'd against this pod; terminal
	// is the right answer there and stays.
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == agentExecID {
			return runner.Status{State: types.RunStarting, Message: "agent exec accepted; kubelet has not started it yet"}, nil
		}
	}
	return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
}

// execNeverStartedReasons are the Waiting reasons under which the ephemeral
// exec container will never run its process: terminalWaitingReasons
// (canary.go) minus CrashLoopBackOff, because a crash-looping container DID
// start -- its process ran and died, which is a task failure, not
// ErrExecNeverStarted's "the task never ran". (Ephemeral containers are not
// restarted, so the reason is not expected here; excluded for correctness.)
var execNeverStartedReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerError":       true,
	"CreateContainerConfigError": true,
	"InvalidImageName":           true,
	"RunContainerError":          true,
}

// waitingMessage words a Waiting container state for AgentStatus: the reason,
// plus the kubelet's message only when it carries one.
func waitingMessage(w *corev1.ContainerStateWaiting) string {
	if w.Message == "" {
		return "waiting: " + w.Reason
	}
	return "waiting: " + w.Reason + ": " + w.Message
}
