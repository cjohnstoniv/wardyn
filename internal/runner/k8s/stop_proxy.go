// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// StopProxy and runner-mode teardown (design K1 §3.1 StopProxy, R6-M1, R7-M1, r7 L3, Q2, P2).
// Every path keeps one order: read the pod and check an arm; persist the witness; remove only
// runnerd's finalizer with a JSON patch guarded by UID and resourceVersion; then the manager stamps
// ProxyStoppedAt, and teardown drops pods, Secrets, NetworkPolicies in that order.

var (
	errRunnerNetPolOverride = errors.New("k8s: a runner boots only on an enforced NetworkPolicy canary verdict: WARDYN_K8S_ALLOW_UNENFORCED_NETPOL and WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY are refused " +
		"(for an ambient platform default-deny, use a runs namespace that deny does not cover)")
	// errRelayIPFenced: a new proxy pod reported an IP a fenced run still holds (Fable L5).
	errRelayIPFenced = errors.New("relay_ip_fenced: the proxy pod's IP is still fenced for another run whose termination is unknown")
	// errDecisionSinkUnwired: runner mode has no decision sink to hand the proxy.
	errDecisionSinkUnwired = errors.New("k8s: this runner has no decision sink for the proxy's decisions; refusing to create a sandbox")
	// errTerminationUnknown: no arm of completion evidence holds yet.
	errTerminationUnknown = errors.New("k8s: termination unknown: no completion evidence for the pod yet")
	errStaleWitnessPatch  = errors.New("k8s: the stop-proof finalizer patch met a changed object; re-reading")
)

const (
	proxyStopBound = 20 * time.Second
	agentStopBound = defaultPodGracePeriod + teardownPollSlack // 45 s
	sinkDrainBound = 5 * time.Second
	evidencePoll   = 500 * time.Millisecond
	// staleWitnessRetries bounds the immediate re-checks after a 409 or 422 on the finalizer patch.
	staleWitnessRetries = 3
)

// runnerGrace never lets a runner-mode delete be a force delete: a zero grace removes the object
// without the kubelet confirming (R4-M2). nil keeps the pod's own grace.
func runnerGrace(g *int64) *int64 {
	if g == nil || *g >= 1 {
		return g
	}
	one := int64(1)
	return &one
}

// StopProxy stops a runner-mode sandbox's proxy and returns only once completion evidence for the
// proxy is persisted and runnerd's finalizer removed, or a validated lapse proves its egress
// stopped. Without evidence the agent pod is deleted too; the answer is then a typed
// ContainmentPendingError (agent_only, or pending_delete once both delete bounds passed) or a plain
// error, and the manager retries each tick. Server mode is unchanged: ErrEndUnsupported.
func (d *Driver) StopProxy(ctx context.Context, ref string) error {
	if !d.runnerMode() {
		return runner.ErrEndUnsupported
	}
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return fmt.Errorf("k8s: stop proxy: %w", err)
	}
	oneSecond := int64(1)
	proxyDone, perr := d.containPod(ctx, runID, componentProxy, proxyPodName(runID), &oneSecond, proxyStopBound, true)
	if proxyDone {
		return nil
	}
	if ev, err := d.loadEvidence(runID); err == nil && ev.Lapsed {
		return nil
	}
	agentDone, aerr := d.containPod(ctx, runID, componentAgent, agentPodName(runID), nil, agentStopBound, false)
	cause := errors.Join(perr, aerr)
	if cause == nil {
		cause = errTerminationUnknown
	}
	var kind string
	ev, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		now := d.now()
		past := func(comp string, bound time.Duration) bool {
			pe := ev.Pods[comp]
			return pe != nil && pe.DeleteAt != nil && now.Sub(*pe.DeleteAt) >= bound
		}
		switch {
		case agentDone:
			kind = runner.ContainmentAgentOnly
		case past(componentProxy, proxyStopBound) && past(componentAgent, agentStopBound):
			kind = runner.ContainmentPendingDelete
		default:
			return false, nil
		}
		changed := false
		if kind == runner.ContainmentPendingDelete && !ev.Hold {
			ev.Hold, changed = true, true
		}
		if ev.PendingSince == nil {
			t := now.UTC()
			ev.PendingSince, changed = &t, true
		}
		return changed, nil
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	if kind == "" {
		return fmt.Errorf("k8s: stop proxy of run %s: %w", runID, cause)
	}
	return &runner.ContainmentPendingError{Kind: kind, NameReused: len(ev.Reuse) > 0, NoCausalAssociation: ev.noCausalAssociation(), Tuple: ev.tuple(runID), Err: cause}
}

// NoteValidatedLapse records the proxy's validated lapsed frame (§6.2): an authenticated session
// at the current gen. It proves egress stopped; it removes no finalizer and releases no hold.
func (d *Driver) NoteValidatedLapse(runID uuid.UUID) error {
	_, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		changed := !ev.Lapsed
		ev.Lapsed = true
		return changed, nil
	})
	return err
}

// ensureFence persists the run's relay fence, once, then applies it in the relay. It runs before
// runnerd's first delete of either pod, on every containment path (R4-M1).
func (d *Driver) ensureFence(runID uuid.UUID) error {
	ev, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		if ev.Fence != nil {
			return false, nil
		}
		f := &relayFence{At: d.now().UTC()}
		if p := ev.Pods[componentProxy]; p != nil {
			f.ProxyUID, f.ProxyIP = p.UID, p.PodIP
		}
		ev.Fence = f
		return true, nil
	})
	if err != nil {
		return err
	}
	if d.cfg.Fence != nil {
		d.cfg.Fence(runID, ev.Fence.ProxyIP)
	}
	return nil
}

// ReplayFences applies every persisted fence of runIDs in the relay. runnerd calls it at startup
// before Online sets the relay's peer, so the replay lands before READY.
func (d *Driver) ReplayFences(runIDs []uuid.UUID) error {
	var errs []error
	for _, id := range runIDs {
		ev, err := d.loadEvidence(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.Fence != nil && d.cfg.Fence != nil {
			d.cfg.Fence(id, ev.Fence.ProxyIP)
		}
	}
	return errors.Join(errs...)
}

// containPod brings one pod to completion evidence: the pre-delete read (which also records a pod
// first met without a record), the fence, runnerd's own delete, the closing list, then the evidence
// wait bounded at bound from the recorded delete. done is true only once the pod's witness is
// persisted and runnerd's finalizer removed, or when no pod object was ever created.
func (d *Driver) containPod(ctx context.Context, runID uuid.UUID, component, name string, grace *int64, bound time.Duration, drain bool) (bool, error) {
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return false, err
	}
	pe := ev.Pods[component]
	if pe != nil && pe.Witness != nil && pe.FinalizerOff {
		return d.settled(ctx, runID, component) // a crash may have come before the settlement
	}
	if pe == nil || pe.DeleteAt == nil {
		if drain && d.cfg.Sink != nil {
			// Drain before the delete: the proxy stops reserving and flushes, at most 5 s.
			// Containment proceeds whatever it answers.
			dctx, cancel := context.WithTimeout(ctx, sinkDrainBound)
			_ = d.cfg.Sink.Drain(dctx, runID)
			cancel()
		}
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case isNotFound(gerr):
			if pe == nil || pe.UID == "" {
				// No object was ever held under the finalizer, so no process can be running.
				return d.settled(ctx, runID, component)
			}
		case gerr != nil:
			return false, fmt.Errorf("k8s: read %s before delete: %w", name, gerr)
		default:
			if err := d.ownPod(pod, runID); err != nil {
				return false, err
			}
			if _, err := d.observePod(ctx, runID, component, pod); err != nil {
				return false, err
			}
			if err := d.ensureFence(runID); err != nil {
				return false, err
			}
			opts := metav1.DeleteOptions{GracePeriodSeconds: runnerGrace(grace), Preconditions: &metav1.Preconditions{UID: &pod.UID}}
			if component == componentProxy {
				orphan := metav1.DeletePropagationOrphan
				opts.PropagationPolicy = &orphan
			}
			if err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Delete(ctx, name, opts); err != nil && !isNotFound(err) {
				return false, fmt.Errorf("k8s: delete %s: %w", name, err)
			}
			if _, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
				p := ev.pod(component, name)
				if p.DeleteAt != nil {
					return false, nil
				}
				t := d.now().UTC()
				p.DeleteAt = &t
				return true, nil
			}); err != nil {
				return false, err
			}
			if _, err := d.closingList(ctx, runID, component); err != nil {
				return false, err
			}
		}
	} else if pe.Closing == nil && pe.NodeName != "" {
		// A crash between the delete and the persisted closing list lost it: take it now.
		if _, err := d.closingList(ctx, runID, component); err != nil {
			return false, err
		}
	}
	done, err := d.awaitEvidence(ctx, runID, component, name, bound)
	if !done {
		return false, err
	}
	return d.settled(ctx, runID, component)
}

// settled finishes a pod that has completion evidence: for the proxy, its producer is settled.
func (d *Driver) settled(ctx context.Context, runID uuid.UUID, component string) (bool, error) {
	if component != componentProxy {
		return true, nil
	}
	return d.settleProducer(ctx, runID)
}

// settleProducer writes off the proxy's sink liability, once, and only here: after completion
// evidence for the proxy pod is persisted and its finalizer removed (lane K9: never on a NotFound
// alone, termination unknown or a lapsed frame).
func (d *Driver) settleProducer(ctx context.Context, runID uuid.UUID) (bool, error) {
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return false, err
	}
	if ev.ProducerGone || ev.SinkKeyHash == "" || d.cfg.Sink == nil {
		return true, nil
	}
	if err := d.cfg.Sink.ProducerGone(ctx, runID); err != nil {
		return false, fmt.Errorf("k8s: settle the proxy's decision producer: %w", err)
	}
	_, err = d.mutate(runID, func(ev *runEvidence) (bool, error) {
		ev.ProducerGone = true
		return true, nil
	})
	return err == nil, err
}

// ReregisterSinks registers each run's persisted sink key hash again. runnerd calls it at startup
// before Online, so a proxy reconnecting after a restart authenticates.
func (d *Driver) ReregisterSinks(runIDs []uuid.UUID) error {
	if d.cfg.Sink == nil {
		return nil
	}
	var errs []error
	for _, id := range runIDs {
		ev, err := d.loadEvidence(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.SinkKeyHash == "" {
			continue
		}
		b, err := hex.DecodeString(ev.SinkKeyHash)
		if err != nil || len(b) != 32 {
			errs = append(errs, fmt.Errorf("k8s: run %s: malformed sink key hash", id))
			continue
		}
		var h [32]byte
		copy(h[:], b)
		if err := d.cfg.Sink.Register(id, h); err != nil {
			errs = append(errs, fmt.Errorf("k8s: re-register run %s with the decision sink: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// ownPod refuses a pod that does not carry this runner's and this run's labels.
func (d *Driver) ownPod(pod *corev1.Pod, runID uuid.UUID) error {
	l := pod.Labels
	if l[labelManaged] != "true" || l[labelRun] != runID.String() || l[runner.LabelRunnerID] != d.cfg.RunnerID {
		return fmt.Errorf("k8s: pod %s: %w", pod.Name, runner.ErrNotRunnerSandbox)
	}
	return nil
}

// awaitEvidence polls the held object until an arm holds or the bound, counted from runnerd's
// recorded delete, passes.
func (d *Driver) awaitEvidence(ctx context.Context, runID uuid.UUID, component, name string, bound time.Duration) (bool, error) {
	var last error
	for stale := 0; ; {
		done, err := d.checkOnce(ctx, runID, component, name)
		if done {
			return true, nil
		}
		last = err
		if errors.Is(err, errStaleWitnessPatch) && stale < staleWitnessRetries {
			stale++ // the object changed under the witness: re-read and re-check now (r7 L3)
			continue
		}
		ev, lerr := d.loadEvidence(runID)
		if lerr != nil {
			return false, errors.Join(last, lerr)
		}
		pe := ev.Pods[component]
		if pe == nil || pe.DeleteAt == nil || d.now().Sub(*pe.DeleteAt) >= bound {
			return false, last
		}
		select {
		case <-ctx.Done():
			return false, errors.Join(last, ctx.Err())
		case <-time.After(evidencePoll):
		}
	}
}

// checkOnce is one pass of the one order: read the held object, check every arm against it and a
// name-scoped list sent after that read returned, persist the witness, and only then remove
// runnerd's own finalizer. NotFound completes only a pod whose witness is already persisted.
func (d *Driver) checkOnce(ctx context.Context, runID uuid.UUID, component, name string) (bool, error) {
	pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if isNotFound(gerr) {
		ev, err := d.loadEvidence(runID)
		if err != nil {
			return false, err
		}
		if pe := ev.Pods[component]; pe != nil && pe.Witness != nil {
			_, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
				p := ev.Pods[component]
				changed := !p.FinalizerOff
				p.FinalizerOff = true
				return changed, nil
			})
			return err == nil, err
		}
		return false, fmt.Errorf("%w: %s is not found, which is never evidence", errTerminationUnknown, name)
	}
	if gerr != nil {
		return false, fmt.Errorf("k8s: read %s: %w", name, gerr)
	}
	ev, err := d.observePod(ctx, runID, component, pod)
	if err != nil {
		return false, err
	}
	pe := ev.Pods[component]
	var post *nodeHolder
	if pe.NodeName != "" {
		ev, err = d.nameList(ctx, runID, component, func(_ *podEvidence, h *nodeHolder) bool { post = h; return false })
		if err != nil {
			return false, err
		}
		pe = ev.Pods[component]
	}
	v := ev.evaluate(pe, pod, post)
	if !v.ok() {
		return false, fmt.Errorf("%w: %s: %s", errTerminationUnknown, name, v.why)
	}
	w := newWitness(runID, pe, pod, v, post, ev.Record, d.now().UTC())
	if _, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		ev.Pods[component].Witness = w
		return true, nil
	}); err != nil {
		// A failed witness save stops the sequence: the patch never follows it (Codex R8).
		return false, err
	}
	return d.removeFinalizer(ctx, runID, component, pod)
}

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// stopProofPatch removes only runnerd's own finalizer entry, after test ops check the recorded pod
// UID, the resourceVersion the witness read, and the entry at that index (r6 L2). It never sends a
// merge patch that empties finalizers, which would strip a third party's.
func stopProofPatch(pod *corev1.Pod) ([]byte, bool, error) {
	idx := slices.Index(pod.Finalizers, stopProofFinalizer)
	if idx < 0 {
		return nil, false, nil
	}
	at := fmt.Sprintf("/metadata/finalizers/%d", idx)
	b, err := json.Marshal([]jsonPatchOp{
		{Op: "test", Path: "/metadata/uid", Value: string(pod.UID)},
		{Op: "test", Path: "/metadata/resourceVersion", Value: pod.ResourceVersion},
		{Op: "test", Path: at, Value: stopProofFinalizer},
		{Op: "remove", Path: at},
	})
	return b, true, err
}

// removeFinalizer sends the guarded patch. A failed test op (422 Invalid) and a version conflict
// (409) are not refusals: the caller re-reads, re-checks the arm, refreshes the witness and retries
// (r7 L3). Any other refusal, such as an admission webhook's, is reported and retried each tick.
func (d *Driver) removeFinalizer(ctx context.Context, runID uuid.UUID, component string, pod *corev1.Pod) (bool, error) {
	patch, present, err := stopProofPatch(pod)
	if err != nil {
		return false, err
	}
	if present {
		_, err = d.clientset.CoreV1().Pods(d.cfg.Namespace).Patch(ctx, pod.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
		switch {
		case err == nil, isNotFound(err):
		case apierrors.IsConflict(err), apierrors.IsInvalid(err):
			return false, fmt.Errorf("%w: %v", errStaleWitnessPatch, err)
		default:
			return false, fmt.Errorf("k8s: the stop-proof finalizer patch on %s was refused (retried each tick): %w", pod.Name, err)
		}
	}
	_, err = d.mutate(runID, func(ev *runEvidence) (bool, error) {
		ev.Pods[component].FinalizerOff = true
		return true, nil
	})
	return err == nil, err
}

// teardownRunner is label teardown in runner mode: each pod is brought to completion evidence in
// the one order before anything else goes, and the NetworkPolicies are dropped last, only once
// both pods are proven stopped and gone. Without evidence it returns an error and drops nothing:
// the run's hold stays, across restarts, until an arm holds.
func (d *Driver) teardownRunner(ctx context.Context, runID uuid.UUID, grace *int64) error {
	bound := defaultPodGracePeriod
	if grace != nil {
		bound = time.Duration(*runnerGrace(grace)) * time.Second
	}
	proxyDone, perr := d.containPod(ctx, runID, componentProxy, proxyPodName(runID), grace, bound+teardownPollSlack, true)
	agentDone, aerr := d.containPod(ctx, runID, componentAgent, agentPodName(runID), grace, bound+teardownPollSlack, false)
	if !proxyDone || !agentDone {
		if _, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
			changed := !ev.Hold
			ev.Hold = true
			return changed, nil
		}); err != nil {
			return err
		}
		cause := errors.Join(perr, aerr)
		if cause == nil {
			cause = errTerminationUnknown
		}
		return fmt.Errorf("k8s: teardown of run %s held, NetworkPolicies kept: %w", runID, cause)
	}
	ns := d.cfg.Namespace
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String() + "," + runner.LabelRunnerID + "=" + d.cfg.RunnerID}
	if err := d.waitPodsGone(ctx, ns, listOpts, bound+teardownPollSlack); err != nil {
		return fmt.Errorf("k8s: teardown: waiting for pods to go before dropping NetworkPolicies: %w", err)
	}
	if err := d.clientset.CoreV1().Secrets(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete secrets: %w", err)
	}
	if err := d.clientset.NetworkingV1().NetworkPolicies(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, listOpts); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: teardown: delete network policies: %w", err)
	}
	return nil
}

// AcceptNodeRecord checks a security operator's node record against the run's own record and an
// acceptance list sent now, and accepts it as an infrastructure fence for the run's pods (design K1
// §3.1 (b), r9 M1(c), Codex R9-L1, R11-M1, Fable r12 M3, r13 M1, owner #7a, #8).
func (d *Driver) AcceptNodeRecord(ctx context.Context, ref string, rec runner.NodeRecord) error {
	if !d.runnerMode() {
		return runner.ErrEndUnsupported
	}
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return fmt.Errorf("k8s: node record: %w", err)
	}
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return err
	}
	if ev.Record != nil && ev.Record.ID == rec.ID {
		return nil
	}
	// Status reads, then each acceptance list after its read returned. Any holder these lists show
	// latches its reuse generation first, so the tuple comparison below sees it.
	pods := map[string]*corev1.Pod{}
	holders := map[string]*nodeHolder{}
	for _, comp := range []string{componentProxy, componentAgent} {
		pe := ev.Pods[comp]
		if pe == nil || pe.NodeName == "" || pe.UID == "" {
			continue
		}
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, pe.Name, metav1.GetOptions{})
		switch {
		case gerr == nil && string(pod.UID) == pe.UID:
			pods[comp] = pod
		case gerr != nil && !isNotFound(gerr):
			return fmt.Errorf("k8s: node record status read: %w", gerr)
		}
		comp := comp
		if ev, err = d.nameList(ctx, runID, comp, func(_ *podEvidence, h *nodeHolder) bool { holders[comp] = h; return false }); err != nil {
			return err
		}
	}
	if !sameTuple(*ev.tuple(runID), rec.Tuple) {
		return refuseRecord("the run's own record no longer matches the approved tuple; the approvals lapse")
	}
	need := 1
	if rec.FourEyes || ev.noCausalAssociation() || len(ev.Reuse) > 0 {
		need = 2
	}
	humans := map[string]bool{}
	for _, a := range rec.Approvers {
		if !a.Human {
			return refuseRecord("an admin token cannot make a node record")
		}
		who := strings.ToLower(strings.TrimSpace(a.Principal))
		if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
			who = e
		}
		if who != "" {
			humans[who] = true
		}
	}
	if len(humans) < need {
		return refuseRecord(fmt.Sprintf("this record needs %d distinct human approvers", need))
	}
	switch rec.Attestation {
	case runner.AttestNodePoweredOff:
		for comp, h := range holders {
			if !h.None {
				return refuseRecord(fmt.Sprintf("a Node still holds %s's node name %s on the acceptance list", comp, h.Node.Name))
			}
		}
	case runner.AttestNoPriorHolder:
		if !ev.noCausalAssociation() {
			return refuseRecord("no prior holder is for the no-association path only")
		}
		if len(ev.Reuse) > 0 {
			return refuseRecord("node name reused: only node powered off releases this run")
		}
		for comp, h := range holders {
			if why := ev.Pods[comp].holderBound(pods[comp], h); why != "" {
				return refuseRecord(why)
			}
		}
	default:
		return refuseRecord("unknown attestation")
	}
	_, err = d.mutate(runID, func(ev *runEvidence) (bool, error) {
		r := rec
		ev.Record = &r
		return true, nil
	})
	return err
}

// holderBound is precondition (ii): on a run that is not (u2) and has no latched ID, the post-bind
// holder still holds the name with its recorded machineID and systemUUID, and the pod's status is
// bound to it: the terminal arm's three conditions or the never-initialized status, and a
// non-empty hostIP equal to the holder's current InternalIP.
func (pe *podEvidence) holderBound(pod *corev1.Pod, h *nodeHolder) string {
	a := pe.anchor()
	switch {
	case pe.Assoc == assocU2:
		return "another Node held the name before the binding: the record must be node powered off"
	case pe.hasLatch():
		return "a container ran twice under this pod: the record must be node powered off"
	case pod == nil:
		return "no status read of the recorded pod: no prior holder needs the holder's own status"
	case a == nil || h == nil || h.None:
		return "no Node holds the name: no prior holder never releases on an empty acceptance list"
	case h.Node.UID != a.UID || !sameMachine(h.Node, *a):
		return "the name's holder is not the post-bind holder with its recorded identity"
	case pod.Status.HostIP == "" || pod.Status.HostIP != h.Node.InternalIP:
		return "the pod's host IP is not the holder's current InternalIP"
	}
	if terminalThree(pod) {
		return ""
	}
	if _, ok := pe.neverInitialized(pod); ok {
		if h.OutOfService {
			return "the holder carries the out-of-service taint"
		}
		return ""
	}
	return "the pod's status is not terminal"
}

// terminalThree is the terminal arm's three conditions without a tier: a terminal phase, every
// spec container terminated, and no DeletionByPodGC.
func terminalThree(pod *corev1.Pod) bool {
	if (pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded) || podGCMarked(pod) {
		return false
	}
	inits, mains := specContainerNames(pod)
	statuses := statusByName(pod)
	for _, name := range slices.Concat(inits, mains) {
		if cs, ok := statuses[name]; !ok || cs.State.Terminated == nil {
			return false
		}
	}
	return true
}

// sameTuple compares tuples by their wire form, so an absent list and an empty one are equal.
func sameTuple(a, b runner.NodeTuple) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func refuseRecord(why string) error {
	return fmt.Errorf("%w: %s", runner.ErrNodeRecordRefused, why)
}
