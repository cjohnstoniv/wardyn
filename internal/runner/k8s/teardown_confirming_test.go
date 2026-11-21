// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// confirmingRig is a created sandbox whose pods the kubelet is still stopping: a delete of the pods
// answers success and removes nothing, as a pod held by its grace period does.
type confirmingRig struct {
	d     *Driver
	cs    *fake.Clientset
	runID uuid.UUID
	ref   string
}

func newConfirmingRig(t *testing.T) *confirmingRig {
	t.Helper()
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	prev := teardownWaitFor
	teardownWaitFor = func(*int64) time.Duration { return time.Second }
	t.Cleanup(func() { teardownWaitFor = prev })
	cs.PrependReactor("delete-collection", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	cs.ClearActions()
	return &confirmingRig{d: d, cs: cs, runID: spec.RunID, ref: sb.Ref}
}

// markDeleting stamps a DeletionTimestamp on every pod of the run for which keep reports false.
func (r *confirmingRig) markDeleting(t *testing.T, skip func(name string) bool) {
	t.Helper()
	opts := metav1.ListOptions{LabelSelector: labelRun + "=" + r.runID.String()}
	pods, err := r.cs.CoreV1().Pods(testNamespace).List(context.Background(), opts)
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods: %v (%d)", err, len(pods.Items))
	}
	now := metav1.Now()
	for i := range pods.Items {
		if skip != nil && skip(pods.Items[i].Name) {
			continue
		}
		pods.Items[i].DeletionTimestamp = &now
		if _, err := r.cs.CoreV1().Pods(testNamespace).Update(context.Background(), &pods.Items[i], metav1.UpdateOptions{}); err != nil {
			t.Fatalf("mark %s deleting: %v", pods.Items[i].Name, err)
		}
	}
}

// swept reports whether the recorded actions delete-collected resource.
func (r *confirmingRig) swept(resource string) bool {
	for _, a := range r.cs.Actions() {
		if a.GetVerb() == "delete-collection" && a.GetResource().Resource == resource {
			return true
		}
	}
	return false
}

func (r *confirmingRig) requireHeld(t *testing.T) {
	t.Helper()
	if r.swept("secrets") || r.swept("networkpolicies") {
		t.Errorf("the Secret or the NetworkPolicies were swept while the pods were still there (H3)")
	}
	if got := runObjectCounts(t, r.cs, r.runID); got.secrets != 1 || got.netpols != 2 || got.pods != 2 {
		t.Errorf("run objects = %+v, want 2 pods, 1 secret, 2 network policies kept", got)
	}
}

// TestKillSandbox_PodsStillTerminatingAnswerConfirming: when the wait runs out while every pod is
// already marked for deletion, the kill answers the typed not-a-failure error, and nothing after the
// wait runs; a retry completes once the pods are gone.
func TestKillSandbox_PodsStillTerminatingAnswerConfirming(t *testing.T) {
	r := newConfirmingRig(t)
	r.markDeleting(t, nil)

	err := r.d.KillSandbox(context.Background(), r.ref)
	if !errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("KillSandbox = %v, want runner.ErrTeardownConfirming", err)
	}
	r.requireHeld(t)

	// The pods go (the kubelet finished); the same kill now completes the sweep.
	opts := metav1.ListOptions{LabelSelector: labelRun + "=" + r.runID.String()}
	pods, _ := r.cs.CoreV1().Pods(testNamespace).List(context.Background(), opts)
	for _, p := range pods.Items {
		if err := r.cs.Tracker().Delete(podsGVR, testNamespace, p.Name); err != nil {
			t.Fatalf("remove pod %s: %v", p.Name, err)
		}
	}
	if err := r.d.KillSandbox(context.Background(), r.ref); err != nil {
		t.Fatalf("KillSandbox after the pods went = %v, want nil", err)
	}
	assertRunObjectsGone(t, r.cs, r.runID)
	if got := runObjectCounts(t, r.cs, r.runID); got.secrets != 0 {
		t.Errorf("secrets after the completed retry = %d, want 0", got.secrets)
	}
}

// TestKillSandbox_PodNotMarkedForDeletionKeepsTheError: one pod that is not terminating at all is a
// real failure, and the NetworkPolicies stay.
func TestKillSandbox_PodNotMarkedForDeletionKeepsTheError(t *testing.T) {
	r := newConfirmingRig(t)
	r.markDeleting(t, func(name string) bool { return name == r.ref })

	err := r.d.KillSandbox(context.Background(), r.ref)
	if err == nil || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("KillSandbox = %v, want today's error, not ErrTeardownConfirming", err)
	}
	r.requireHeld(t)
}

// TestKillSandbox_CallerContextDoneKeepsTheError: a wait that ends because the caller's own context
// did is not the kubelet being slow.
func TestKillSandbox_CallerContextDoneKeepsTheError(t *testing.T) {
	r := newConfirmingRig(t)
	r.markDeleting(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := r.d.KillSandbox(ctx, r.ref)
	if err == nil || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("KillSandbox = %v, want today's error, not ErrTeardownConfirming", err)
	}
	r.requireHeld(t)
}
