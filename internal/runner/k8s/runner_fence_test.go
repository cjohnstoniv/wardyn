// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// fenceSet stands in for the relay's IP refusals.
type fenceSet map[string]uuid.UUID

func (f fenceSet) wire(h *runnerHarness) {
	h.d.cfg.Fence = func(id uuid.UUID, ip string) { h.fences = append(h.fences, ip); f[ip] = id }
	h.d.cfg.Unfence = func(id uuid.UUID, ip string) {
		if f[ip] == id {
			delete(f, ip)
		}
	}
	h.d.cfg.IPFenced = func(ip string) bool { _, ok := f[ip]; return ok }
}

const testProxyIP = "10.244.1.7"

// TestRunnerObserveProxiesPreCreate: a run inside CreateSandbox (its Ref stored, no proxy pod yet)
// is left out of the observation, never reported not_found; a created proxy whose pod is gone is.
func TestRunnerObserveProxiesPreCreate(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	seen, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := seen[h.runID]; ok {
		t.Fatalf("a run with no proxy pod yet was observed as %q", o)
	}
	pod := h.boundProxy(t)
	if err := h.cs.Tracker().Delete(podsGVR, testNamespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	if seen, err = h.d.ObserveProxies(ctx, []uuid.UUID{h.runID}); err != nil || seen[h.runID] != runner.ProxyNotFound {
		t.Fatalf("created proxy, pod gone: %q %v", seen[h.runID], err)
	}
}

// TestRunnerFenceReleasedOnWitness: a normal stop releases the proxy IP once its witness is
// persisted, so a new proxy on the recycled IP is accepted, and a restart does not replay it; a run
// whose termination is unknown keeps its refusal, and replays it.
func TestRunnerFenceReleasedOnWitness(t *testing.T) {
	ctx := context.Background()
	t.Run("released after a normal stop", func(t *testing.T) {
		h := newRunnerHarness(t)
		f := fenceSet{}
		f.wire(h)
		h.boundProxy(t)
		h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
		if err := h.d.StopProxy(ctx, agentPodName(h.runID)); err != nil {
			t.Fatal(err)
		}
		if len(h.fences) != 1 || h.d.cfg.IPFenced(testProxyIP) {
			t.Fatalf("fences %v, IP still refused %v", h.fences, h.d.cfg.IPFenced(testProxyIP))
		}
		// The sandbox stop that follows, which contains the still-held agent pod, keeps the released
		// IP released.
		h.boundAgent(t)
		if err := h.d.StopSandbox(ctx, agentPodName(h.runID)); err != nil {
			t.Fatal(err)
		}
		if h.d.cfg.IPFenced(testProxyIP) {
			t.Fatalf("StopSandbox after StopProxy refused the released IP again: fences %v", h.fences)
		}
		restarted := fenceSet{}
		restarted.wire(h)
		h.fences = nil
		if err := h.d.ReplayFences([]uuid.UUID{h.runID}); err != nil || len(h.fences) != 0 {
			t.Fatalf("a witnessed run was replayed: %v %v", h.fences, err)
		}
	})
	t.Run("kept while termination is unknown", func(t *testing.T) {
		h := newRunnerHarness(t)
		f := fenceSet{}
		f.wire(h)
		h.boundProxy(t)
		var pending *runner.ContainmentPendingError
		if err := h.d.StopProxy(ctx, agentPodName(h.runID)); !errors.As(err, &pending) {
			t.Fatalf("StopProxy: %v", err)
		}
		if !h.d.cfg.IPFenced(testProxyIP) {
			t.Fatal("a run with unknown termination lost its IP refusal")
		}
		restarted := fenceSet{}
		restarted.wire(h)
		if err := h.d.ReplayFences([]uuid.UUID{h.runID}); err != nil || restarted[testProxyIP] != h.runID {
			t.Fatalf("pending run not replayed: %v %v", restarted, err)
		}
	})
}

// TestRunnerVanishedPodFencedAndTyped: held objects gone before runnerd's own delete (a stripped
// finalizer) are fenced and bounded from when they were first seen gone, so the run becomes typed
// pending_delete rather than erroring on every tick.
func TestRunnerVanishedPodFencedAndTyped(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	f := fenceSet{}
	f.wire(h)
	pod := h.boundProxy(t)
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.pod(componentAgent, agentPodName(h.runID)).UID = "uid-agent"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.cs.Tracker().Delete(podsGVR, testNamespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 0; i < 3; i++ {
		err = h.d.StopProxy(ctx, agentPodName(h.runID))
	}
	var pending *runner.ContainmentPendingError
	if !errors.As(err, &pending) || pending.Kind != runner.ContainmentPendingDelete {
		t.Fatalf("StopProxy with both objects gone: %v", err)
	}
	if len(h.fences) == 0 || !h.d.cfg.IPFenced(testProxyIP) {
		t.Fatalf("fences %v", h.fences)
	}
	ev := h.ev.get(t, h.runID)
	if ev.Pods[componentProxy].Witness != nil || ev.Pods[componentProxy].DeleteAt == nil {
		t.Fatalf("proxy %+v", ev.Pods[componentProxy])
	}
}

// TestRunnerPreDeleteReadChecksOwnership: a pod under the run's name that is not this runner's is
// never deleted.
func TestRunnerPreDeleteReadChecksOwnership(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	pod := h.boundProxy(t)
	pod.Labels[runner.LabelRunnerID] = "another-runner"
	if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
	if err := h.d.StopProxy(ctx, agentPodName(h.runID)); !errors.Is(err, runner.ErrNotRunnerSandbox) {
		t.Fatalf("StopProxy of a foreign pod: %v", err)
	}
	got, err := h.cs.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil || got.DeletionTimestamp != nil {
		t.Fatalf("the foreign pod was deleted: %v", err)
	}
}

// TestRunnerNoPriorHolderRefusedOnU2: a (u2) run, whose name another Node held before the binding,
// is never released by "no prior holder", even with a holder-bound terminal status.
func TestRunnerNoPriorHolderRefusedOnU2(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	pod := h.boundProxy(t)
	terminate(pod, "containerd://p1")
	if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.Pods[componentProxy].Assoc = assocU2
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := nodeRecordFixture(h, t, runner.AttestNoPriorHolder, "alice", "bob")
	err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec)
	if !errors.Is(err, runner.ErrNodeRecordRefused) || !strings.Contains(err.Error(), "another Node held the name") {
		t.Fatalf("AcceptNodeRecord on a (u2) run: %v", err)
	}
}

// TestRunnerApproversDedupeOnEitherKey: one human listed under two principals with one email, or
// one principal with and without an email, is one approver.
func TestRunnerApproversDedupeOnEitherKey(t *testing.T) {
	ctx := context.Background()
	for name, second := range map[string]runner.NodeRecordApprover{
		"same email":                  {Principal: "alice-2", Email: "alice@example.com", Human: true},
		"same principal and no email": {Principal: "alice", Human: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.boundProxy(t)
			h.dropNode(t, "n1")
			rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
			rec.Approvers = append(rec.Approvers, second)
			rec.FourEyes = true
			if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); !errors.Is(err, runner.ErrNodeRecordRefused) {
				t.Fatalf("AcceptNodeRecord: %v", err)
			}
		})
	}
}

// TestRunnerApproversLinkAcrossOrder: three listings that chain into one human (A and B share an
// email, B and C share a principal) are one approver whatever their order, and an approver with no
// principal is refused.
func TestRunnerApproversLinkAcrossOrder(t *testing.T) {
	a := runner.NodeRecordApprover{Principal: "p1", Email: "e1@example.com", Human: true}
	b := runner.NodeRecordApprover{Principal: "p2", Email: "e1@example.com", Human: true}
	c := runner.NodeRecordApprover{Principal: "p2", Email: "e2@example.com", Human: true}
	for name, order := range map[string][]runner.NodeRecordApprover{"abc": {a, b, c}, "acb": {a, c, b}, "cab": {c, a, b}} {
		if n, err := distinctHumanApprovers(order); err != nil || n != 1 {
			t.Errorf("%s: %d humans, %v; want 1", name, n, err)
		}
	}
	other := runner.NodeRecordApprover{Principal: "p3", Email: "e3@example.com", Human: true}
	if n, err := distinctHumanApprovers([]runner.NodeRecordApprover{a, c, b, other}); err != nil || n != 2 {
		t.Errorf("a chain plus a stranger: %d humans, %v; want 2", n, err)
	}
	if _, err := distinctHumanApprovers([]runner.NodeRecordApprover{a, {Email: "e9@example.com", Human: true}}); err == nil {
		t.Error("an approver with no principal was counted")
	}
}

// TestRunnerFenceAndReleaseHoldTheRunLock: the relay is told to refuse a proxy IP, and to release
// it, only while the run's lock is held, so the two cannot interleave around the evidence saves.
func TestRunnerFenceAndReleaseHoldTheRunLock(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	f := fenceSet{}
	f.wire(h)
	held := func() bool {
		v, ok := h.d.runLocks.Load(h.runID)
		if !ok {
			return false
		}
		mu := v.(*sync.Mutex)
		if mu.TryLock() {
			mu.Unlock()
			return false
		}
		return true
	}
	var fenceHeld, unfenceHeld bool
	fence, unfence := h.d.cfg.Fence, h.d.cfg.Unfence
	h.d.cfg.Fence = func(id uuid.UUID, ip string) { fenceHeld = held(); fence(id, ip) }
	h.d.cfg.Unfence = func(id uuid.UUID, ip string) { unfenceHeld = held(); unfence(id, ip) }
	h.boundProxy(t)
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	if err := h.d.StopProxy(ctx, agentPodName(h.runID)); err != nil {
		t.Fatal(err)
	}
	if !fenceHeld || !unfenceHeld {
		t.Fatalf("the relay was called without the run lock: fence held %v, release held %v", fenceHeld, unfenceHeld)
	}
}

// boundAgent records the harness run's agent pod on node n1 as the start wait would, held under the
// stop-proof finalizer like the proxy.
func (h *runnerHarness) boundAgent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pod := h.proxyPod("n1")
	pod.Name, pod.UID = agentPodName(h.runID), "uid-agent-1"
	pod.Labels = wardynLabels(h.runID, componentAgent, nil)
	pod.Labels[runner.LabelRunnerID] = testRunnerID
	pod.Status.PodIP = "10.244.1.8"
	b := &bracket{d: h.d, runID: h.runID, component: componentAgent}
	if err := b.before(ctx, pod.Name); err != nil {
		t.Fatal(err)
	}
	created, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.created(created); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.observePod(ctx, h.runID, componentAgent, created); err != nil {
		t.Fatal(err)
	}
}

// TestRunnerHeldTeardownIsTyped: a runner-mode teardown (a kill) whose pods have no completion
// evidence past both delete bounds answers the typed ContainmentPendingError, so the manager holds
// that run and the runner's session still reaches READY (§8 P2), and keeps the NetworkPolicies.
func TestRunnerHeldTeardownIsTyped(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	f := fenceSet{}
	f.wire(h)
	pod := h.boundProxy(t)
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.pod(componentAgent, agentPodName(h.runID)).UID = "uid-agent"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.cs.Tracker().Delete(podsGVR, testNamespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = h.d.StopProxy(ctx, agentPodName(h.runID))
	}
	zero := int64(0)
	err := h.d.teardownByRunID(ctx, h.runID, &zero)
	var pending *runner.ContainmentPendingError
	if !errors.As(err, &pending) || pending.Kind != runner.ContainmentPendingDelete || !strings.Contains(err.Error(), "NetworkPolicies kept") {
		t.Fatalf("held teardown: %v", err)
	}
}
