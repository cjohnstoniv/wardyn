// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyRedraws is how many times a runner-mode proxy pod given a fenced IP is replaced before the run
// fails relay_ip_fenced: three draws in all, each after the first preferring a node no earlier draw ran on.
const proxyRedraws = 2

// errRedrawnPod is a read of a proxy pod this run retired as a fenced draw. Its record is closed, so the
// read is never recorded as the run's proxy.
var errRedrawnPod = errors.New("k8s: a proxy pod this run retired as a fenced draw")

// errFencedDraw ends waitPodIP at a read showing an IP the relay still refuses for another run
// (startClock.fenced, set only by startProxyPod on a runner).
var errFencedDraw = errors.New("k8s: the proxy pod was given a fenced address")

// startProxyPod creates the run's proxy pod and waits, on clock, until it is ready with its IP; it returns
// the created pod and that IP. In runner mode an IP another run's relay fence still refuses is not this
// run's fault: the cluster handed out an address whose earlier holder's stop is unconfirmed. It is checked
// on every read of the wait, before Ready: such a proxy never becomes Ready. The pod is
// retired with completion evidence (retireFencedDraw) and another drawn, up to proxyRedraws times. The
// run fails, with an error wrapping errRelayIPFenced as before, only when every draw met a fenced IP or a
// draw could not be retired. Server mode creates and waits once.
func (d *Driver) startProxyPod(ctx context.Context, clock *startClock, spec runner.SandboxSpec, proxyPod *corev1.Pod) (*corev1.Pod, string, error) {
	ns, name := proxyPod.Namespace, proxyPod.Name
	if d.runnerMode() && d.cfg.IPFenced != nil {
		clock.fenced = d.cfg.IPFenced
		defer func() { clock.fenced = nil }()
	}
	var avoid []string
	for draw := 0; ; draw++ {
		pod := proxyPod.DeepCopy()
		avoidNodes(pod, avoid)
		created, err := d.createPod(ctx, spec.RunID, componentProxy, pod)
		if err != nil {
			return nil, "", fmt.Errorf("k8s: create proxy pod: %w", err)
		}
		ip, err := d.waitPodIP(ctx, clock, ns, name, spec.NotifyWaiting)
		if err == nil {
			return created, ip, nil
		}
		if !errors.Is(err, errFencedDraw) {
			return nil, "", fmt.Errorf("k8s: proxy pod never became ready: %w", err)
		}
		if draw == proxyRedraws {
			// Contained before it ever opens, rather than running to its deadline.
			return nil, "", fmt.Errorf("k8s: proxy pod %s: %w (each of its %d draws was given a fenced address)", ip, errRelayIPFenced, draw+1)
		}
		if draw == 0 {
			d.redrawing.Store(spec.RunID, struct{}{})
			defer d.redrawing.Delete(spec.RunID)
		}
		node, err := d.retireFencedDraw(ctx, spec.RunID, ns, name)
		if err != nil {
			return nil, "", fmt.Errorf("k8s: proxy pod %s: %w (it could not be replaced: %v)", ip, errRelayIPFenced, err)
		}
		slog.Warn("wardynd: k8s substrate: a proxy pod was given an address still fenced for another run; drawing another",
			slog.String("run_id", spec.RunID.String()), slog.String("proxy_ip", ip), slog.String("node", node), slog.Int("draw", draw+1))
		if node != "" {
			avoid = append(avoid, node)
		}
	}
}

// redrawUnderTick reports whether the 30 s observation must leave runID's proxy out: the run is replacing a
// fenced draw, the listed pod is a draw it retired, or its recorded proxy is no longer the one whose UID
// (uid) the tick read before its lists. A retired draw's terminal phase, or the gap before the next draw
// exists, is not the run's proxy exiting; reported, it becomes a durable obligation and the run is contained.
func (d *Driver) redrawUnderTick(runID uuid.UUID, uid string, listed *corev1.Pod) (bool, error) {
	if _, ok := d.redrawing.Load(runID); ok {
		return true, nil
	}
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return false, err
	}
	if listed != nil && ev.redrawn(string(listed.UID)) {
		return true, nil
	}
	p := ev.Pods[componentProxy]
	return len(ev.Redrawn) > 0 && (p == nil || p.UID != uid), nil
}

// retireFencedDraw brings this run's proxy pod, which drew a fenced IP, to completion evidence in the one
// order (stop_proxy.go) and closes its record, so the next draw starts a record of its own. It returns the
// node the pod was bound to.
//
// It never fences the run in the relay while the draw may still be replaced: the relay's run fence is
// never lifted (relay_k8s.go, Fence), so it would refuse every later draw. The drawn IP is already refused
// for the other run, so this pod cannot relay. Instead the drawn pod's fence record is persisted, not
// applied, before the delete: a restart before the witness replays it (ReplayFences), and if the evidence
// does not come within proxyStopBound, ensureFence applies it here, the fence v0.10.2 gave the run. No sink
// drain and no producer settlement: the run's one producer is settled when its last proxy stops.
func (d *Driver) retireFencedDraw(ctx context.Context, runID uuid.UUID, ns, name string) (string, error) {
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: read the fenced draw %s: %w", name, err)
	}
	if err := d.ownPod(pod, runID); err != nil {
		return "", err
	}
	if _, err := d.observePod(ctx, runID, componentProxy, pod); err != nil {
		return "", err
	}
	if err := d.recordDrawFence(runID); err != nil {
		return "", err
	}
	giveUp := func(cause error) (string, error) {
		if ferr := d.ensureFence(runID); ferr != nil {
			cause = errors.Join(cause, ferr)
		}
		return "", cause
	}
	oneSecond := int64(1)
	orphan := metav1.DeletePropagationOrphan
	opts := metav1.DeleteOptions{GracePeriodSeconds: runnerGrace(&oneSecond), PropagationPolicy: &orphan, Preconditions: &metav1.Preconditions{UID: &pod.UID}}
	if err := d.clientset.CoreV1().Pods(ns).Delete(ctx, name, opts); err != nil && !isNotFound(err) {
		return giveUp(fmt.Errorf("k8s: delete the fenced draw %s: %w", name, err))
	}
	if err := d.markDeleted(runID, componentProxy, name); err != nil {
		return giveUp(err)
	}
	if _, err := d.closingList(ctx, runID, componentProxy); err != nil {
		return giveUp(err)
	}
	if done, err := d.awaitEvidence(ctx, runID, componentProxy, name, proxyStopBound); !done {
		return giveUp(cmp.Or(err, errTerminationUnknown))
	}
	if err := d.waitPodGone(ctx, ns, pod, proxyStopBound); err != nil {
		return "", fmt.Errorf("k8s: the fenced draw %s is proven stopped but its name is not free: %w", name, err)
	}
	return pod.Spec.NodeName, d.closeDraw(runID, pod.UID)
}

// recordDrawFence persists the drawn proxy's fence record without applying it in the relay (see
// retireFencedDraw). Only a fresh draw qualifies: a run already holding a fence record, or a proxy record
// already deleted or witnessed, is not one.
func (d *Driver) recordDrawFence(runID uuid.UUID) error {
	_, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		p := ev.Pods[componentProxy]
		if ev.Fence != nil || p == nil || p.UID == "" || p.DeleteAt != nil || p.Witness != nil {
			return false, errors.New("k8s: the run's proxy record is not a fresh draw")
		}
		ev.Fence = &relayFence{At: d.now().UTC(), ProxyUID: p.UID, ProxyIP: p.PodIP}
		return true, nil
	})
	return err
}

// closeDraw moves the proven draw's record to Redrawn and drops its fence record, whose IP refusal
// checkOnce released with the witness. Only the draw itself, with its witness persisted and its finalizer
// removed, closes.
func (d *Driver) closeDraw(runID uuid.UUID, uid k8stypes.UID) error {
	_, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		p := ev.Pods[componentProxy]
		if p == nil || p.UID != string(uid) || p.Witness == nil || !p.FinalizerOff {
			return false, errors.New("k8s: the fenced draw has no completion evidence to close its record on")
		}
		ev.Redrawn = append(ev.Redrawn, p)
		delete(ev.Pods, componentProxy)
		ev.Fence = nil
		return true, nil
	})
	return err
}

// avoidNodes asks the scheduler, as a preference only, to place pod on none of nodes: most CNIs give each
// node its own address range, so another node draws another address. A cluster where only those nodes fit
// still places it.
func avoidNodes(pod *corev1.Pod, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	// One value per requirement: the API refuses a node field selector with more.
	names := slices.Compact(slices.Sorted(slices.Values(nodes)))
	reqs := make([]corev1.NodeSelectorRequirement, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{n}})
	}
	na.PreferredDuringSchedulingIgnoredDuringExecution = append(na.PreferredDuringSchedulingIgnoredDuringExecution,
		corev1.PreferredSchedulingTerm{Weight: 100, Preference: corev1.NodeSelectorTerm{MatchFields: reqs}})
}
