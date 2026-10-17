// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
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
