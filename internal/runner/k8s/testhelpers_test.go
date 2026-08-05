// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// podsGVR is the core/v1 Pods GroupVersionResource, used to reach into a fake
// Clientset's backing ObjectTracker directly (bypassing the reactor chain —
// safe to call from inside a reactor without recursing into itself, and the
// simplest way to script a status field the tracker's plain Create/Update
// never populates, since nothing in this package pretends to be the
// kubelet).
var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// setPodStatus reads podName straight from the tracker, applies mutate to a
// copy of its Status, and writes it back — the direct way to script a status
// a real kubelet would set (container states, PodIP, phase) without routing
// through a reactor.
func setPodStatus(t *testing.T, cs *fake.Clientset, ns, name string, mutate func(*corev1.PodStatus)) {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, ns, name)
	if err != nil {
		t.Fatalf("tracker get %q: %v", name, err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	mutate(&pod.Status)
	if err := cs.Tracker().Update(podsGVR, pod, ns); err != nil {
		t.Fatalf("tracker update %q: %v", name, err)
	}
}

// createAgentPodFixture creates a minimal agent pod (just the main
// placeholder container — the shape CreateSandbox's step 5 produces) for
// tests that exercise Exec/Wait/AgentStatus/Status/teardown directly without
// running the full CreateSandbox sequence. Returns the pod's name (== ref).
func createAgentPodFixture(t *testing.T, cs *fake.Clientset, runID uuid.UUID, image string, env map[string]string) string {
	t.Helper()
	name := agentPodName(runID)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: wardynLabels(runID, componentAgent, nil)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: mainContainerName, Image: image, Env: envVars(env)}},
		},
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create agent pod fixture: %v", err)
	}
	return name
}

const testNamespace = "wardyn-test"

// testRestConfig is a synthetic rest.Config good enough for apiserverHostPort
// to parse — nothing in these tests ever dials it for real (the canary
// reactor below synthesizes pod status directly).
func testRestConfig() *rest.Config { return &rest.Config{Host: "https://127.0.0.1:6443"} }

// installCanaryReactor scripts every canary pod Get to report Terminated
// immediately (so wait.PollUntilContextTimeout's immediate=true first
// attempt already succeeds — no real sleeping in tests). Phase A vs phase B
// is tracked via a LOCAL closure counter over distinct pod names seen, never
// by calling back into the fake clientset's typed API: testing.Fake's
// dispatch holds a lock for the duration of each reactor call, and a reactor
// that re-enters the SAME clientset (e.g. NetworkPolicies().List(...)) from
// inside a "get pods" reactor deadlocks on that non-reentrant lock — found
// this the hard way (a real hang, not a slow first compile). Querying
// cs.Tracker() directly would also be lock-safe, but the counter needs no
// clientset call at all.
func installCanaryReactor(t *testing.T, cs *fake.Clientset, unenforced bool) {
	t.Helper()
	phaseBExit := int32(1)
	if unenforced {
		phaseBExit = 0
	}
	installCanaryReactorExitCodes(t, cs, 0, phaseBExit)
}

// installCanaryReactorExitCodes is installCanaryReactor generalized to an
// ARBITRARY phase B exit code — the M1 regression test scripts 128 (a
// StartError shape) to prove that only exactly 0 or 1 is ever read as a
// verdict; anything else is indeterminate.
func installCanaryReactorExitCodes(t *testing.T, cs *fake.Clientset, phaseAExit, phaseBExit int32) {
	t.Helper()
	seen := map[string]bool{}
	phase := 0
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-egress-canary-") {
			return false, nil, nil
		}
		name := ga.GetName()
		if !seen[name] {
			seen[name] = true
			phase++
		}
		// phase 1 => phase A (no NetworkPolicy yet): must read as a clean
		// connect. phase 2 => phase B (deny-all in effect).
		exitCode := phaseAExit
		if phase >= 2 {
			exitCode = phaseBExit
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  canaryContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
				}},
			},
		}
		return true, pod, nil
	})
}

// installStuckCanaryReactor scripts EVERY canary pod Get to report a
// permanently-Waiting container (ImagePullBackOff) — the INDETERMINATE fast
// path: a non-network failure in phase A, before any NetworkPolicy verdict
// could ever be reached.
func installStuckCanaryReactor(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-egress-canary-") {
			return false, nil, nil
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: action.GetNamespace()},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  canaryContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "test: image not found"}},
				}},
			},
		}
		return true, pod, nil
	})
}

// deleteCollectionGVKs maps the three resource kinds this package's teardown
// sweeps to their (singular, per-item) Kind, for installDeleteCollectionSupport.
// The tracker derives the LIST kind itself (Kind+"List" — see
// testing/fixture.go's tracker.List), so this must be the item kind ("Pod"),
// not "PodList" — passing the list kind double-suffixes to "PodListList" and
// the tracker's scheme lookup fails closed.
var deleteCollectionGVKs = map[string]schema.GroupVersionKind{
	"pods":            {Version: "v1", Kind: "Pod"},
	"secrets":         {Version: "v1", Kind: "Secret"},
	"networkpolicies": {Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
}

// installDeleteCollectionSupport makes DeleteCollection actually delete
// matching objects on the fake clientset. client-go's fake ObjectReaction
// (testing/fixture.go) has NEVER implemented the delete-collection verb —
// its switch has no case for DeleteCollectionActionImpl, so Fake.Invokes
// silently falls through to "no reactor handled this" and returns success
// without deleting anything (verified by reading client-go v0.36.3's
// source: a real, still-open upstream gap, not a bug in this package).
// teardown()'s three DeleteCollection calls are correct against a REAL
// apiserver (that IS how a real cluster implements the verb: list, then
// delete each), so this reactor teaches the fake the same trick — using
// cs.Tracker() directly, never the typed clientset, since calling back
// through the clientset from inside a reactor re-enters Fake's non-reentrant
// lock (see installCanaryReactor's doc for the deadlock this caused once
// already).
func installDeleteCollectionSupport(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	cs.PrependReactor("delete-collection", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		dc, ok := action.(clienttesting.DeleteCollectionActionImpl)
		if !ok {
			return false, nil, nil
		}
		gvr := dc.GetResource()
		gvk, known := deleteCollectionGVKs[gvr.Resource]
		if !known {
			return false, nil, nil
		}
		ns := dc.GetNamespace()
		listObj, err := cs.Tracker().List(gvr, gvk, ns, dc.GetListOptions())
		if err != nil {
			return true, nil, err
		}
		items, err := meta.ExtractList(listObj)
		if err != nil {
			return true, nil, err
		}
		for _, item := range items {
			acc, err := meta.Accessor(item)
			if err != nil {
				return true, nil, err
			}
			if err := cs.Tracker().Delete(gvr, ns, acc.GetName(), dc.GetDeleteOptions()); err != nil {
				return true, nil, err
			}
		}
		return true, nil, nil
	})
}

// newTestDriver constructs a Driver against a fresh fake clientset with the
// enforcing canary reactor and DeleteCollection support installed (the
// common case for tests that are not themselves exercising the canary state
// machine).
func newTestDriver(t *testing.T, cfg Config) (*Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	installDeleteCollectionSupport(t, cs)
	if cfg.Namespace == "" {
		cfg.Namespace = testNamespace
	}
	if cfg.ProxyImage == "" {
		cfg.ProxyImage = "wardyn/wardyn-proxy:test"
	}
	d, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	return d, cs
}
