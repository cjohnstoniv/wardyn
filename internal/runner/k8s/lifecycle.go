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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Status reports the sandbox (agent pod) lifecycle state.
func (d *Driver) Status(ctx context.Context, ref string) (runner.Status, error) {
	pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "sandbox not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("k8s: status: get pod: %w", err)
	}
	return statusFromPod(pod), nil
}

// statusFromPod maps a pod's phase to a Wardyn RunState, surfacing a stuck
// container's waiting.reason (ImagePullBackOff etc.) in the status detail so
// an operator sees WHY a sandbox is stuck in STARTING, not just that it is.
func statusFromPod(pod *corev1.Pod) runner.Status {
	st := runner.Status{}
	switch pod.Status.Phase {
	case corev1.PodPending:
		st.State = types.RunStarting
		st.Message = waitingDetail(pod)
	case corev1.PodRunning:
		st.State = types.RunRunning
	case corev1.PodSucceeded:
		code := containerExitCode(pod, mainContainerName)
		st.State = types.RunStopped
		st.ExitCode = &code
	case corev1.PodFailed:
		code := containerExitCode(pod, mainContainerName)
		st.State = types.RunFailed
		st.ExitCode = &code
		st.Message = failureDetail(pod)
	default: // PodUnknown, or the phase hasn't been set yet
		st.State = types.RunStarting
		st.Message = waitingDetail(pod)
	}
	return st
}

// failureDetail returns "<reason>: <message>" for a failed pod, or whichever half
// exists. pod.Status.Reason carries the VERDICT — an eviction's is "Evicted" —
// and Message the detail that names the limit the kubelet measured past, so
// dropping the reason left a run failure reading like an unattributed sentence
// ("Pod ephemeral local storage usage exceeds the total limit of containers 64Mi"
// with nothing saying who killed it, or why).
func failureDetail(pod *corev1.Pod) string {
	reason, msg := pod.Status.Reason, pod.Status.Message
	switch {
	case reason == "":
		return msg
	case msg == "":
		return reason
	default:
		return reason + ": " + msg
	}
}

// waitingDetail returns "<container>: <reason>[: <message>]" for the first
// container status currently Waiting with a reason, or "" if none.
func waitingDetail(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil || w.Reason == "" {
			continue
		}
		if w.Message != "" {
			return fmt.Sprintf("%s: %s: %s", cs.Name, w.Reason, w.Message)
		}
		return fmt.Sprintf("%s: %s", cs.Name, w.Reason)
	}
	return ""
}

// containerExitCode returns the named container's terminated exit code, or 0
// if it never terminated (should not happen for a Succeeded/Failed pod, but
// zero is a safe fallback rather than a panic on an unexpected shape).
func containerExitCode(pod *corev1.Pod, name string) int {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			if t := cs.State.Terminated; t != nil {
				return int(t.ExitCode)
			}
		}
	}
	return 0
}

// StopSandbox is the graceful teardown path (server-default grace period —
// SIGTERM, then SIGKILL after the grace window). Idempotent on a gone sandbox.
func (d *Driver) StopSandbox(ctx context.Context, ref string) error {
	return d.teardown(ctx, ref, nil)
}

// KillSandbox is the immediate kill-switch teardown path (zero grace period —
// SIGKILL now). Idempotent on a gone sandbox.
func (d *Driver) KillSandbox(ctx context.Context, ref string) error {
	zero := int64(0)
	return d.teardown(ctx, ref, &zero)
}

// teardown resolves ref's run id from its wardyn.run-id label (a ghost ref —
// already gone — is idempotent success), then sweeps every Wardyn-owned
// object carrying that label via three DeleteCollection calls: pods,
// NetworkPolicies, Secrets. One label selector reaches the agent pod, the
// proxy pod, both NetworkPolicies, and the config Secret in three calls total
// — simpler than docker's per-object-name removal loop, because k8s's label
// selector does in one call what docker's driver needs several names for.
func (d *Driver) teardown(ctx context.Context, ref string, gracePeriodSeconds *int64) error {
	ns := d.cfg.Namespace
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil // ghost ref: already gone, idempotent success
		}
		return fmt.Errorf("k8s: teardown: get pod %q: %w", ref, err)
	}
	runIDStr := pod.Labels[labelRun]
	if runIDStr == "" {
		return fmt.Errorf("k8s: teardown of %q: %w", ref, errTeardownUnresolved)
	}
	runID, perr := uuid.Parse(runIDStr)
	if perr != nil {
		return fmt.Errorf("k8s: teardown of %q (run-id label %q unparseable): %w", ref, runIDStr, errTeardownUnresolved)
	}
	return d.teardownByRunID(ctx, runID, gracePeriodSeconds)
}

// teardownByRunID is teardown's run-id-keyed core: sweeps every Wardyn-owned
// object carrying runID's label via three DeleteCollection calls (pods,
// NetworkPolicies, Secrets), waiting for the pods to actually be gone before
// touching the NetworkPolicies (see the H3 comment below). Split out from
// teardown so CreateSandbox's failure-path rollback can share this exact
// guard: it knows the run id directly (spec.RunID) and must not skip the
// wait-before-netpol-drop ordering just because no live pod exists yet to
// resolve a label from.
func (d *Driver) teardownByRunID(ctx context.Context, runID uuid.UUID, gracePeriodSeconds *int64) error {
	ns := d.cfg.Namespace
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}

	if err := d.clientset.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: gracePeriodSeconds}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete pods: %w", err)
	}

	// H3: an unselected pod is default-allow, so dropping the NetworkPolicies
	// while the pod is still Terminating (a SIGTERM-trapping agent can run
	// for up to its full grace period) would hand it open egress for that
	// whole window. Wait for the DeleteCollection above to actually take
	// effect — pods gone, not merely marked for deletion — before touching
	// the netpols confining them. Bounded at grace+slack: never longer than
	// the pod would legitimately take to terminate, plus a beat for the
	// kubelet to report it gone.
	grace := defaultPodGracePeriod
	if gracePeriodSeconds != nil {
		grace = time.Duration(*gracePeriodSeconds) * time.Second
	}
	if err := d.waitPodsGone(ctx, ns, listOpts, grace+teardownPollSlack); err != nil {
		return fmt.Errorf("k8s: teardown: waiting for pods to terminate before dropping NetworkPolicies: %w", err)
	}

	if err := d.clientset.NetworkingV1().NetworkPolicies(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete network policies: %w", err)
	}
	if err := d.clientset.CoreV1().Secrets(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete secrets: %w", err)
	}
	return nil
}

// defaultPodGracePeriod mirrors the pod-level default when
// TerminationGracePeriodSeconds is left unset (every pod spec this package
// creates does): 30s. teardownPollSlack is added on top: the kubelet needs a
// beat after the grace window elapses to actually report the pod gone.
const (
	defaultPodGracePeriod = 30 * time.Second
	teardownPollSlack     = 15 * time.Second
)

// waitPodsGone polls until no pod matches listOpts — the ordering guard H3
// exists for. A timeout here is a real error (not best-effort): proceeding
// to drop the NetworkPolicies without this proof is exactly the open-egress
// window this function exists to close. teardown is already idempotent, so
// a caller retry (or wardynd's own reconciler) completes the sweep once the
// pod actually terminates.
func (d *Driver) waitPodsGone(ctx context.Context, ns string, listOpts metav1.ListOptions, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, k8sPollInterval, timeout, true, func(pollCtx context.Context) (bool, error) {
		pods, err := d.clientset.CoreV1().Pods(ns).List(pollCtx, listOpts)
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})
}

// SweepOrphanedSandboxes tears down the sandbox objects of every run whose row
// no longer owns them — this substrate's half of api.SandboxOrphanSweeper
// (D13), the sibling of docker/driver.go's. Until it existed the control
// plane's boot-and-cadence sweep (internal/api/reconcile.go) was a SILENT
// NO-OP here: it reaches the capability by type assertion, and only the docker
// driver satisfied it, so on k8s nothing ever revisited a run's objects once
// no sandbox_ref pointed at them.
//
// 0.7.2 is what makes that reachable routinely rather than only after a
// control-plane crash: a run's disk_mib is now the agent container's
// ephemeral-storage LIMIT (naming.go's resourceRequirements), so the kubelet
// EVICTS the agent pod — a kill path no Wardyn code is on, and one an operator
// can trigger with an ordinary `dd`. Nothing then tears down the run's
// siblings, and the credential-bearing ones are the point: the proxy pod stays
// Running with its resolved upstream creds in memory, and the per-run Secret
// (proxy config JSON + every SecretEnv value) stays in the namespace.
//
// Keyed on the agent AND proxy pods, where docker keys on its agent container
// alone: an evicted pod is Failed, and the kubelet's terminated-pod GC may
// reap it while the proxy pod lives on — keying on the agent alone would miss
// exactly the shape this exists for. One list, deduped by run id, since
// teardownByRunID already reaches every object of that run from the id.
//
// Canary pods are excluded BY that selector: their labelRun carries a
// per-invocation uuid rather than a run id (canary.go's M2 note), and
// runCanaryPhase cleans its own up on every path.
//
// A drive PVC is never touched, on two independent counts: it carries
// labelDrive/labelDriveHome/labelDriveSubject and NEVER labelRun (drives.go's
// ensureDrivePVC), and teardownByRunID only ever DeleteCollections pods,
// NetworkPolicies and Secrets. A drive outlives every run that mounts it —
// which is why the chart grants no claim delete verb at all.
func (d *Driver) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(runID uuid.UUID) bool) (int, error) {
	ns := d.cfg.Namespace
	pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelComponent + " in (" + componentAgent + "," + componentProxy + ")",
	})
	if err != nil {
		return 0, fmt.Errorf("k8s: list sandbox pods for orphan sweep: %w", err)
	}
	cutoff := time.Now().Add(-minAge)
	// An orphan has no owner left to flush, so kill rather than wait out a
	// 30s SIGTERM grace window per run — and waitPodsGone's bound shrinks with
	// it, which matters when a sweep finds several.
	zeroGrace := int64(0)
	swept := 0
	seen := make(map[uuid.UUID]bool, len(pods.Items))
	var errs []error
	for _, pod := range pods.Items {
		runID, perr := uuid.Parse(pod.Labels[labelRun])
		if perr != nil {
			continue // not a pod whose run id we can key teardown on
		}
		if seen[runID] {
			continue // its sibling already swept the whole run, or failed to
		}
		if pod.CreationTimestamp.Time.After(cutoff) {
			continue // too young: a dispatch may still be about to SetSandboxRef
		}
		if !isOrphan(runID) {
			continue // a live run legitimately owns it
		}
		seen[runID] = true
		if terr := d.teardownByRunID(ctx, runID, &zeroGrace); terr != nil {
			errs = append(errs, fmt.Errorf("k8s: teardown orphaned run %s: %w", runID, terr))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}
