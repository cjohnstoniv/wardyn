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

// teardown resolves ref's run id — from the agent pod's wardyn.run-id label,
// or, when that pod is already gone, from the REF ITSELF (a sandbox ref is the
// deterministic agent pod name) — then sweeps every Wardyn-owned object
// carrying that label via three DeleteCollection calls: pods, NetworkPolicies,
// Secrets. One label selector reaches the agent pod, the proxy pod, both
// NetworkPolicies, and the config Secret in three calls total — simpler than
// docker's per-object-name removal loop, because k8s's label selector does in
// one call what docker's driver needs several names for.
//
// B9-F1: a 404 on the agent pod used to end the whole teardown as "already
// gone". It is not: the agent pod is the one object of a run that routinely
// disappears on its own (0.7.2 made disk_mib its ephemeral-storage limit, so
// the kubelet EVICTS it and its terminated-pod GC reaps it — a kill path no
// Wardyn code is on; a deleted node does the same), and what it leaves behind
// is the credential-bearing half: a proxy pod still Running with resolved
// upstream creds, the per-run Secret carrying every SecretEnv value verbatim,
// and both NetworkPolicies. Recovering the id from the ref costs one string
// parse and no API call, so only a ref that is not a wardyn agent pod name at
// all is still the idempotent no-op it was meant to be (docker's
// runIDFromAgentName fallback, on the substrate that lacked it).
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
// exactly the shape this exists for. And keyed on the per-run Secret and both
// NetworkPolicies besides, for the case where neither pod is left to name the
// run at all — see sweepCandidates, which also states what keeps a boot
// canary's own objects out. Deduped by run id, since teardownByRunID already
// reaches every object of that run from the id.
//
// A drive PVC is never touched, on two independent counts: it carries
// labelDrive/labelDriveHome/labelDriveSubject and NEVER labelRun (drives.go's
// ensureDrivePVC), and teardownByRunID only ever DeleteCollections pods,
// NetworkPolicies and Secrets. A drive outlives every run that mounts it —
// which is why the chart grants no claim delete verb at all.
func (d *Driver) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(runID uuid.UUID) bool) (int, error) {
	candidates, hasPod, err := d.sweepCandidates(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-minAge)
	// An orphan has no owner left to flush, so kill rather than wait out a
	// 30s SIGTERM grace window per run — and waitPodsGone's bound shrinks with
	// it, which matters when a sweep finds several.
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

// sweepCandidate is one object that names a run the sweep might have to
// reclaim, with the creation time the minAge gate reads. fromPod marks the ones
// the pod list produced, so a Secret or NetworkPolicy of a run whose pods ARE
// listed defers to those entries rather than re-deciding with its own (older)
// timestamp — CreateSandbox writes the Secret before either pod exists.
type sweepCandidate struct {
	runID     uuid.UUID
	createdAt time.Time
	fromPod   bool
}

// sweepCandidates lists every object that can name an orphaned run: the agent
// and proxy pods, AND the per-run Secret and both NetworkPolicies.
//
// B9-F5: listing pods alone left a run whose pods are BOTH gone unreachable —
// permanently, since the sweep is the only thing that revisits a run no
// sandbox_ref points at. That is not a corner: a deleted node takes both pods
// together, and an eviction plus the kubelet's terminated-pod GC gets there on
// its own. What survives is the object the whole sweep exists for — the Secret
// holding the proxy config JSON and every SecretEnv value verbatim — plus two
// NetworkPolicies. Any fix that leaves the Secret reachable only via a pod
// label repeats the bug.
//
// The Secret and NetworkPolicy lists are BEST-EFFORT, and that is the whole
// difference between this working on a real cluster and not. They are a NEW
// privilege this release asks for (the chart's Role grants it from 0.7.4; see
// deploy/helm/wardyn/templates/rbac.yaml), and an operator running their own
// Role under k8s.rbac.create=false has a pre-0.7.4 one that the chart cannot
// upgrade for them. On upgrade day those two lists 403. Aborting here on that
// 403 would take the WHOLE sweep down with them — including the evicted-agent
// reclaim that worked in 0.7.3 — trading a partial credential leak for a total
// one. So a Forbidden degrades to the pod-only candidate set, logged once per
// process, and every other list error still fails the sweep honestly.
//
// Pods are listed LAST, deliberately: hasPod decides which Secret/NetworkPolicy
// entries defer to a pod entry, so it has to be built from the LATEST snapshot.
// A pod created in the window between the lists would otherwise be invisible,
// and its run's older Secret would then decide the run's fate on its own age —
// reclaiming a dispatch that had just come up.
//
// All three lists use the SAME agent/proxy component selector. That is what
// keeps a boot canary out: its objects carry labelManaged and a labelRun that
// IS a parseable uuid (canary.go's M2 per-invocation suffix), so only
// labelComponent tells them apart from a run's — and runCanaryPhase cleans its
// own up on every path. A drive PVC is excluded twice over: it is not one of the
// three kinds listed here, and it never carries labelRun at all.
func (d *Driver) sweepCandidates(ctx context.Context) ([]sweepCandidate, map[uuid.UUID]bool, error) {
	ns := d.cfg.Namespace
	listOpts := metav1.ListOptions{
		LabelSelector: labelComponent + " in (" + componentAgent + "," + componentProxy + ")",
	}
	secrets, secretsErr := d.clientset.CoreV1().Secrets(ns).List(ctx, listOpts)
	if secretsErr != nil && !apierrors.IsForbidden(secretsErr) {
		return nil, nil, fmt.Errorf("k8s: list sandbox secrets for orphan sweep: %w", secretsErr)
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
	if secretsErr != nil || netpolsErr != nil {
		warnSweepListForbidden(secretsErr, netpolsErr)
	}

	hasPod := make(map[uuid.UUID]bool, len(pods.Items))
	candidates := make([]sweepCandidate, 0, len(pods.Items)+len(secrets.Items)+len(netpols.Items))
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
	if secretsErr == nil {
		for i := range secrets.Items {
			add(secrets.Items[i].ObjectMeta, false)
		}
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
// process. The sweep runs on a cadence (internal/api/reconcile.go), so a Role
// missing the verb would otherwise write the same line forever; the condition it
// reports is static — an operator-managed Role — so saying it once is saying it.
var sweepListForbiddenOnce sync.Once

// warnSweepListForbidden names the reclaim that is degraded and the exact fix,
// because the symptom (a run's Secret surviving until someone notices) has no
// other place to surface: the sweep returns success, correctly, and the whole
// point is that it keeps working.
func warnSweepListForbidden(secretsErr, netpolsErr error) {
	sweepListForbiddenOnce.Do(func() {
		slog.Warn("wardynd: k8s orphan sweep is running DEGRADED: this ServiceAccount may not list Secrets/NetworkPolicies, "+
			"so a run whose agent AND proxy pods are both gone (a deleted node takes them together) cannot be reached and its "+
			"per-run Secret — proxy config plus every secret_env value — will not be reclaimed. Every other reclaim still runs. "+
			"Fix: grant `list` on secrets and networkpolicies in the Role bound to this ServiceAccount (the chart's own Role does "+
			"from 0.7.4; an operator-managed Role under k8s.rbac.create=false has to be updated by hand)",
			slog.Any("secrets_error", secretsErr), slog.Any("networkpolicies_error", netpolsErr))
	})
}
