// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func TestRunnerAccessReviewNamesMissingVerbs(t *testing.T) {
	h := newRunnerHarness(t)
	h.cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sar := a.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		ra := sar.Spec.ResourceAttributes
		sar.Status.Allowed = !(ra.Resource == "pods" && ra.Verb == "patch") && !(ra.Resource == "nodes" && ra.Verb == "list")
		return true, sar, nil
	})
	err := h.d.CheckRunnerAccess(context.Background(), "wardyn-runner")
	if !errors.Is(err, errRunnerAccessMissing) || !strings.Contains(err.Error(), testNamespace+"/pods:patch") || !strings.Contains(err.Error(), "nodes:list") {
		t.Fatalf("CheckRunnerAccess: %v", err)
	}
}

func TestRunnerForeignObjects(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	if err := h.d.CheckForeignObjects(ctx); err != nil {
		t.Fatalf("an empty namespace: %v", err)
	}
	mine := h.proxyPod("n1")
	if _, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, mine, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.CheckForeignObjects(ctx); err != nil {
		t.Fatalf("own objects: %v", err)
	}
	server := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "server-run", Namespace: testNamespace, Labels: map[string]string{labelManaged: "true"}}}
	if _, err := h.cs.NetworkingV1().NetworkPolicies(testNamespace).Create(ctx, server, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.CheckForeignObjects(ctx); !errors.Is(err, errRunnerForeignObjects) || !strings.Contains(err.Error(), "networkpolicy/server-run") {
		t.Fatalf("a server's object: %v", err)
	}
}

func TestRunnerKubeletFloor(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	add := func(name, arch, version string, unschedulable bool) {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/arch": arch}},
			Spec: corev1.NodeSpec{Unschedulable: unschedulable}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: version}}}
		if err := h.cs.Tracker().Add(n); err != nil {
			t.Fatal(err)
		}
	}
	add("new", "amd64", "v1.31.0", false)
	add("arm-old", "arm64", "v1.29.3", false)
	add("cordoned-old", "amd64", "v1.30.1", true)
	if err := h.d.CheckKubeletFloor(ctx, "amd64"); err != nil {
		t.Fatalf("floor: %v", err)
	}
	add("old", "amd64", "v1.30.9-eks-1", false)
	add("odd", "amd64", "unknown", false)
	err := h.d.CheckKubeletFloor(ctx, "amd64")
	if !errors.Is(err, errKubeletBelowFloor) || !strings.Contains(err.Error(), "old (v1.30.9-eks-1)") || !strings.Contains(err.Error(), "odd (unknown)") || strings.Contains(err.Error(), "cordoned") {
		t.Fatalf("CheckKubeletFloor: %v", err)
	}
}

func TestRunnerProxyRunByIP(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	if _, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, h.proxyPod("n1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	id, ok, err := h.d.ProxyRunByIP(ctx, netip.MustParseAddr("10.244.1.7"))
	if err != nil || !ok || id != h.runID {
		t.Fatalf("ProxyRunByIP = %v %v %v", id, ok, err)
	}
	if _, ok, _ := h.d.ProxyRunByIP(ctx, netip.MustParseAddr("10.244.1.8")); ok {
		t.Fatal("an unknown source mapped to a run")
	}
	other := h.proxyPod("n1")
	other.Name, other.Labels[labelRun], other.Labels[runner.LabelRunnerID], other.Status.PodIP = "foreign", uuid.NewString(), "another-runner", "10.244.1.9"
	if _, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.d.ProxyRunByIP(ctx, netip.MustParseAddr("10.244.1.9")); ok {
		t.Fatal("another runner's proxy was admitted")
	}
}

func TestRunnerRelaySourceVerdict(t *testing.T) {
	if err := relaySourceVerdict("10.244.1.7", netip.MustParseAddr("10.244.1.7")); err != nil {
		t.Fatal(err)
	}
	if err := relaySourceVerdict("10.244.1.7", netip.MustParseAddr("10.0.0.5")); !errors.Is(err, errRelaySourceRewritten) {
		t.Fatalf("rewritten source: %v", err)
	}
	if err := relaySourceVerdict("", netip.MustParseAddr("10.0.0.5")); !errors.Is(err, errRelayProbeInconclusive) {
		t.Fatalf("no pod IP: %v", err)
	}
}

// The probe connects and exits before its kubelet has posted the pod's status, so the pod's IP can be missing
// when the relay sees the connection: the verdict waits for it instead of reading once.
func TestRunnerRelayProbeWaitsForThePodIP(t *testing.T) {
	h := newRunnerHarness(t)
	var connected atomic.Bool
	var laterGets atomic.Int32
	h.cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		if !strings.HasPrefix(name, "wardyn-relay-probe-") {
			return false, nil, nil
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
		if connected.Load() && laterGets.Add(1) > 1 {
			pod.Status.PodIP = "10.244.1.7"
		}
		return true, pod, nil
	})
	observed := func(context.Context) (netip.Addr, error) {
		connected.Store(true)
		return netip.MustParseAddr("10.244.1.7"), nil
	}
	if err := h.d.ProbeRelaySource(context.Background(), "10.96.0.9", 8443, observed); err != nil {
		t.Fatalf("ProbeRelaySource with the pod's IP posted after the connection: %v", err)
	}
}
