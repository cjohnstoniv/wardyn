// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The agent pod's node is read from the pod; the class and handler are the ones the create resolved.
func TestAgentPodWhere_ReadsTheNodeAndKeepsTheClass(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	ns := d.defaultNamespace()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agentPodName(runID), Namespace: ns}, Spec: corev1.PodSpec{NodeName: "node-1"}}
	if err := cs.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
	got := d.agentPodWhere(context.Background(), ns, runID, "kata", "kata-qemu")
	want := &runner.SandboxWhere{Node: "node-1", RuntimeClass: "kata", RuntimeHandler: "kata-qemu"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("where (-want +got):\n%s", diff)
	}
}

// A pod that cannot be read leaves only the node out, and nothing at all when there is no class either.
func TestAgentPodWhere_AFailedReadKeepsTheClass(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	runID := uuid.New()
	ns := d.defaultNamespace()
	got := d.agentPodWhere(context.Background(), ns, runID, "kata", "kata-qemu")
	if diff := cmp.Diff(&runner.SandboxWhere{RuntimeClass: "kata", RuntimeHandler: "kata-qemu"}, got); diff != "" {
		t.Errorf("where (-want +got):\n%s", diff)
	}
	if got := d.agentPodWhere(context.Background(), ns, runID, "", ""); got != nil {
		t.Errorf("where = %+v, want nil", got)
	}
}

// bindAgentPods makes the fake API server schedule every agent pod onto node.
func bindAgentPods(cs *fake.Clientset, node string) {
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		if pod, ok := ca.GetObject().(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "wardyn-agent-") {
			pod.Spec.NodeName = node
		}
		return false, nil, nil
	})
}

// A create under a RuntimeClass hands back the node, the class and its handler.
func TestCreateSandbox_RecordsWhereItRuns(t *testing.T) {
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "gvisor"}})
	mustCreateRuntimeClass(t, cs, "gvisor", "runsc")
	bindAgentPods(cs, "node-1")
	spec := testSandboxSpec()
	spec.ConfinementClass = types.CC2
	sb := createSandboxOn(t, d, cs, spec)
	if diff := cmp.Diff(&runner.SandboxWhere{Node: "node-1", RuntimeClass: "gvisor", RuntimeHandler: "runsc"}, sb.Where); diff != "" {
		t.Errorf("Where (-want +got):\n%s", diff)
	}
}

// A CC1 pod has no RuntimeClass, so only its node is recorded.
func TestCreateSandbox_CC1RecordsTheNodeOnly(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	bindAgentPods(cs, "node-1")
	sb := createSandboxOn(t, d, cs, testSandboxSpec())
	if diff := cmp.Diff(&runner.SandboxWhere{Node: "node-1"}, sb.Where); diff != "" {
		t.Errorf("Where (-want +got):\n%s", diff)
	}
}
