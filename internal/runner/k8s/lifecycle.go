// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		st.Message = waitingReason(pod)
	case corev1.PodRunning:
		st.State = types.RunRunning
	case corev1.PodSucceeded:
		code := 0
		st.State = types.RunStopped
		st.ExitCode = &code
	case corev1.PodFailed:
		st.State = types.RunFailed
		st.ExitCode = containerExitCode(pod, mainContainerName)
		st.Message = failureDetail(pod)
	default: // PodUnknown, or the phase hasn't been set yet
		st.State = types.RunStarting
		st.Message = waitingReason(pod)
	}
	return st
}

// failureDetail returns "<reason>: <message>" for a failed pod, or whichever
// half exists. Reason carries the VERDICT (e.g. "Evicted"); dropping it left
// failures reading as an unattributed message with nothing saying who killed it.
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

// waitingReason is what a starting pod is waiting ON, in one line, and unlike
// waitingDetail it always has an answer: a pod NO NODE TOOK has no
// ContainerStatuses, so it falls back to the PodScheduled condition. Same
// `<component>: <Reason>[: <message>]` shape either way; never "" — a bare
// "pod: Pending" still says something.
func waitingReason(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if detail := waitingDetail(pod); detail != "" {
		return detail
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("pod: %s: %s", c.Reason, c.Message)
		}
	}
	return "pod: Pending"
}

// A lost node may never publish a container exit; nil keeps reconciliation
// from treating that missing evidence as a successful exit.
func containerExitCode(pod *corev1.Pod, name string) *int {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			if t := cs.State.Terminated; t != nil {
				code := int(t.ExitCode)
				return &code
			}
		}
	}
	return nil
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

// teardown resolves ref's run id — from the agent pod's wardyn.run-id label,
// or, when that pod is already gone, from the REF ITSELF (a sandbox ref is the
// deterministic agent pod name) — then sweeps every Wardyn-owned object
// carrying that label via three DeleteCollection calls (pods, NetworkPolicies,
// Secrets); one label selector reaches all of them in three calls total.
//
// A 404 on the agent pod does not mean the run is gone: the kubelet routinely
// EVICTS it alone (disk_mib is its ephemeral-storage limit) while a
// credential-bearing proxy pod, Secret, and NetworkPolicies survive. Only a
// ref that is not a wardyn agent pod name at all falls through to a no-op.
func (d *Driver) teardown(ctx context.Context, ref string, gracePeriodSeconds *int64) error {
	ns := d.cfg.Namespace
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			runID, perr := runIDFromAgentPodName(ref)
			if perr != nil {
				return nil // ghost ref we cannot key on: idempotent success
			}
			return d.teardownByRunID(ctx, runID, gracePeriodSeconds)
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
// object carrying runID's label (pods, NetworkPolicies, Secrets), waiting for
// the pods to actually be gone before touching the NetworkPolicies (H3
// below). Split out so CreateSandbox's rollback can share this exact ordering
// even when no live pod exists yet to resolve a label from.
func (d *Driver) teardownByRunID(ctx context.Context, runID uuid.UUID, gracePeriodSeconds *int64) error {
	ns := d.cfg.Namespace
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}

	if err := d.clientset.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: gracePeriodSeconds}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete pods: %w", err)
	}

	// H3: an unselected pod is default-allow, so dropping the NetworkPolicies
	// while the pod is still Terminating would hand it open egress for that
	// window. Wait for pods to actually be gone (not merely marked for
	// deletion) first. Bounded at grace+slack.
	grace := defaultPodGracePeriod
	if gracePeriodSeconds != nil {
		grace = time.Duration(*gracePeriodSeconds) * time.Second
	}
	if err := d.waitPodsGone(ctx, ns, listOpts, grace+teardownPollSlack); err != nil {
		return fmt.Errorf("k8s: teardown: waiting for pods to terminate before dropping NetworkPolicies: %w", err)
	}

	// Secret FIRST, NetworkPolicies after (mirrors CreateSandbox's order): the
	// orphan sweep keys the Secret on the netpols, since listing Secrets needs
	// a body-returning verb it's never granted. Reversing the order would
	// leave a crash-window survivor with nothing left to find it by.
	if err := d.clientset.CoreV1().Secrets(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete secrets: %w", err)
	}
	if err := d.clientset.NetworkingV1().NetworkPolicies(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete network policies: %w", err)
	}
	return nil
}

// defaultPodGracePeriod mirrors the pod-level default (30s) used when
// TerminationGracePeriodSeconds is unset. teardownPollSlack adds the beat the
// kubelet needs after the grace window to report the pod gone.
const (
	defaultPodGracePeriod = 30 * time.Second
	teardownPollSlack     = 15 * time.Second
)

// waitPodsGone polls until no pod matches listOpts (the H3 ordering guard).
// A timeout here is a real error, not best-effort: proceeding without this
// proof reopens the open-egress window. teardown is idempotent, so a retry
// completes the sweep once the pod actually terminates.
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
// (D13). Without it the control-plane's boot-and-cadence sweep was a SILENT
// NO-OP on k8s: only the docker driver satisfied the capability.
//
// Reachable routinely, not just after a crash: disk_mib is the agent
// container's ephemeral-storage LIMIT, so the kubelet EVICTS it (an ordinary
// `dd` reaches it too via the metered emptyDirs). What survives is
// credential-bearing: a proxy pod with resolved upstream creds, and the
// per-run Secret holding SecretEnv values.
//
// Keyed on BOTH agent and proxy pods (an evicted agent pod may be GC'd while
// the proxy lives on), plus the per-run Secret and both NetworkPolicies for
// the case where neither pod survives to name the run (see sweepCandidates).
// Deduped by run id, since teardownByRunID reaches every object from the id.
//
// A drive PVC is never touched: it carries labelDrive, never labelRun, and
// teardownByRunID only DeleteCollections pods, NetworkPolicies and Secrets.
func (d *Driver) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(runID uuid.UUID) bool) (int, error) {
	candidates, hasPod, err := d.sweepCandidates(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-minAge)
	// An orphan has no owner to flush, so kill rather than wait a 30s SIGTERM
	// grace per run — shrinking waitPodsGone's bound too.
	zeroGrace := int64(0)
	swept := 0
	seen := make(map[uuid.UUID]bool, len(candidates))
	var errs []error
	for _, c := range candidates {
		if seen[c.runID] {
			continue // its sibling already swept the whole run, or failed to
		}
		if !c.fromPod && hasPod[c.runID] {
			continue // a pod of this run was listed: that entry owns the verdict
		}
		if c.createdAt.After(cutoff) {
			continue // too young: a dispatch may still be about to SetSandboxRef
		}
		if !isOrphan(c.runID) {
			continue // a live run legitimately owns it
		}
		seen[c.runID] = true
		if terr := d.teardownByRunID(ctx, c.runID, &zeroGrace); terr != nil {
			errs = append(errs, fmt.Errorf("k8s: teardown orphaned run %s: %w", c.runID, terr))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}

// sweepCandidate is one object naming a run the sweep might reclaim, with the
// creation time minAge reads. fromPod marks pod-list entries, so a
// NetworkPolicy of a run whose pods ARE listed defers to those rather than
// its own older timestamp (CreateSandbox writes NetworkPolicies before pods).
type sweepCandidate struct {
	runID     uuid.UUID
	createdAt time.Time
	fromPod   bool
}

// sweepCandidates lists every object that can name an orphaned run: the agent
// and proxy pods, AND both NetworkPolicies. Pods alone would leave a run
// whose pods are BOTH gone (a deleted node, or eviction + GC) permanently
// unreachable, with the credential-bearing Secret and NetworkPolicies stranded.
//
// The Secret is reached WITHOUT being listed: `list` on secrets returns every
// body and RBAC can't scope it by label, so that verb is never granted.
// Instead the NetworkPolicies (no credential) are ordered to strictly outlive
// the Secret — created before it, deleted after it — so a surviving Secret
// always has a surviving NetworkPolicy to be reclaimed by.
//
// The NetworkPolicy list is BEST-EFFORT: it's a privilege only granted from
// chart 0.7.4, so an operator-managed Role can 403 on upgrade. A Forbidden
// degrades to the pod-only candidate set (logged once) rather than taking the
// whole sweep down; every other list error still fails honestly.
//
// Pods are listed LAST: hasPod must be built from the latest snapshot, or a
// pod created in the window between the two lists would be invisible and its
// run's older NetworkPolicy would decide the run's fate on stale age.
//
// All three lists share the SAME agent/proxy component selector, which is
// what excludes a boot canary (labelManaged, not labelComponent) and a drive
// PVC (never carries labelRun at all).
func (d *Driver) sweepCandidates(ctx context.Context) ([]sweepCandidate, map[uuid.UUID]bool, error) {
	ns := d.cfg.Namespace
	listOpts := metav1.ListOptions{
		LabelSelector: labelComponent + " in (" + componentAgent + "," + componentProxy + ")",
	}
	netpols, netpolsErr := d.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, listOpts)
	if netpolsErr != nil && !apierrors.IsForbidden(netpolsErr) {
		return nil, nil, fmt.Errorf("k8s: list sandbox network policies for orphan sweep: %w", netpolsErr)
	}
	// Pods LAST (see above), and never best-effort: this verb has been granted
	// since the substrate shipped, and without it there is no sweep at all.
	pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, listOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("k8s: list sandbox pods for orphan sweep: %w", err)
	}
	if netpolsErr != nil {
		warnSweepListForbidden(netpolsErr)
	}

	hasPod := make(map[uuid.UUID]bool, len(pods.Items))
	candidates := make([]sweepCandidate, 0, len(pods.Items)+len(netpols.Items))
	add := func(meta metav1.ObjectMeta, fromPod bool) {
		runID, perr := uuid.Parse(meta.Labels[labelRun])
		if perr != nil {
			return // not an object whose run id we can key teardown on
		}
		if fromPod {
			hasPod[runID] = true
		}
		candidates = append(candidates, sweepCandidate{runID: runID, createdAt: meta.CreationTimestamp.Time, fromPod: fromPod})
	}
	if netpolsErr == nil {
		for i := range netpols.Items {
			add(netpols.Items[i].ObjectMeta, false)
		}
	}
	for i := range pods.Items {
		add(pods.Items[i].ObjectMeta, true)
	}
	return candidates, hasPod, nil
}

// sweepListForbiddenOnce keeps the degraded-sweep warning to ONE line per
// process: the sweep runs on a cadence, and the condition it reports (an
// operator-managed Role) is static, so repeating it says nothing new.
var sweepListForbiddenOnce sync.Once

// warnSweepListForbidden names the reclaim that is degraded and the exact fix
// — the sweep itself returns success, so this is the only place it surfaces.
func warnSweepListForbidden(netpolsErr error) {
	sweepListForbiddenOnce.Do(func() {
		slog.Warn("wardynd: k8s orphan sweep is running DEGRADED: this ServiceAccount may not list NetworkPolicies, "+
			"so a run whose agent AND proxy pods are both gone (a deleted node takes them together) cannot be reached and its "+
			"per-run Secret — proxy config plus every secret_env value — will not be reclaimed. Every other reclaim still runs. "+
			"Fix: grant `list` on networkpolicies in the Role bound to this ServiceAccount (the chart's own Role does from 0.7.4; "+
			"an operator-managed Role under k8s.rbac.create=false has to be updated by hand). `list` on SECRETS is deliberately "+
			"NOT the fix and must not be granted: it returns every Secret's body",
			slog.Any("networkpolicies_error", netpolsErr))
	})
}
