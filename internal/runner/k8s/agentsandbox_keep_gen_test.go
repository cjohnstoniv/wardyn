// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var gateOn = corev1.EnvVar{Name: proxy.FirstRenewGateEnv, Value: "1"}

// labelProxyPod sets the generation label on the run's proxy pod as stored.
func labelProxyPod(t *testing.T, cs *fake.Clientset, gen string) {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(asRunID))
	if err != nil {
		t.Fatal(err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	pod.Labels[labelProxyGeneration] = gen
	if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
}

func storedProxyPod(t *testing.T, cs *fake.Clientset) *corev1.Pod {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(asRunID))
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	return obj.(*corev1.Pod)
}

func TestKeepDriverHonoursGenerations(t *testing.T) {
	on, _, _ := newSandboxRig(t, Config{AgentSandboxKeep: true}, controllerMaking(t, nil))
	s := served(on)
	if _, ok := s.(runner.ProxyGenerationStopper); !ok {
		t.Errorf("served with keep on = %T, not a runner.ProxyGenerationStopper: the orchestrator sends it plain ends", s)
	}
	if _, ok := s.(runner.ProxyReviverAt); !ok {
		t.Errorf("served with keep on = %T, not a runner.ProxyReviverAt: the orchestrator sends it plain replaces", s)
	}
}

func TestReplaceProxyAtMarksTheNewPod(t *testing.T) {
	for _, gen := range []int{0, 3} {
		k, cs, _, ref := keptRun(t)
		if slices.Contains(storedProxyPod(t, cs).Spec.Containers[0].Env, gateOn) {
			t.Fatalf("the created proxy pod carries %s: a create does not set it", proxy.FirstRenewGateEnv)
		}
		if _, err := k.ReplaceProxyHashedAt(context.Background(), ref, replaceCfg, gen); err != nil {
			t.Fatalf("ReplaceProxyHashedAt(%d): %v", gen, err)
		}
		pod := storedProxyPod(t, cs)
		label, labelled := pod.Labels[labelProxyGeneration]
		if (gen == 0 && labelled) || (gen == 3 && label != "3") {
			t.Errorf("gen %d: proxy pod labels = %v, want %s=3 for generation 3 and no such label for none", gen, pod.Labels, labelProxyGeneration)
		}
		env := pod.Spec.Containers[0].Env
		if len(env) == 0 || env[len(env)-1] != gateOn {
			t.Errorf("gen %d: proxy env = %v, want %s=1 last", gen, env, proxy.FirstRenewGateEnv)
		}
	}
}

func TestMarkReplacementPutsTheGateLast(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Env: []corev1.EnvVar{{Name: proxy.FirstRenewGateEnv, Value: "0"}, {Name: "WARDYN_LLM_SCAN", Value: "1"}},
	}}}}
	markReplacement(pod, 0)
	want := []corev1.EnvVar{{Name: "WARDYN_LLM_SCAN", Value: "1"}, gateOn}
	if got := pod.Spec.Containers[0].Env; !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v: an earlier setting dropped, the gate last", got, want)
	}
}

func TestReplaceProxyAtRefusesAnOlderOrSameGeneration(t *testing.T) {
	k, cs, dyn, ref := keptRun(t)
	ctx := context.Background()
	if _, err := k.ReplaceProxyHashedAt(ctx, ref, replaceCfg, 3); err != nil {
		t.Fatalf("ReplaceProxyHashedAt(3): %v", err)
	}
	for _, gen := range []int{2, 3} {
		order := writesInOrder(cs, dyn)
		_, err := k.ReplaceProxyHashedAt(ctx, ref, replaceCfg, gen)
		if !errors.Is(err, runner.ErrProxyGenerationNewer) || errors.Is(err, runner.ErrProxyReplaceFailed) {
			t.Errorf("ReplaceProxyHashedAt(%d) over generation 3 = %v, want ErrProxyGenerationNewer alone", gen, err)
		}
		if len(*order) != 0 {
			t.Errorf("gen %d: writes = %v, want none", gen, *order)
		}
	}
	if _, err := k.ReplaceProxyHashedAt(ctx, ref, replaceCfg, 4); err != nil {
		t.Fatalf("ReplaceProxyHashedAt(4) over generation 3: %v", err)
	}
	if got := storedProxyPod(t, cs).Labels[labelProxyGeneration]; got != "4" {
		t.Errorf("generation label after the newer replace = %q, want 4", got)
	}
}

func TestReplaceProxyAtLooksAgainWhenTheNameIsTaken(t *testing.T) {
	for _, tc := range []struct {
		taken string
		fails bool
	}{{"1", false}, {"5", true}} {
		k, cs, _, ref := keptRun(t)
		took := false
		cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
			pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
			if took || !ok || pod.Name != proxyPodName(asRunID) {
				return false, nil, nil
			}
			// Another replace makes its proxy pod after this one removed the old pod, before this create.
			took = true
			other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: testNamespace, UID: "proxy-uid-9",
				Labels: map[string]string{labelRun: asRunID.String(), labelComponent: componentProxy, labelProxyGeneration: tc.taken}}}
			return false, nil, cs.Tracker().Create(podsGVR, other, testNamespace)
		})
		_, err := k.ReplaceProxyHashedAt(context.Background(), ref, replaceCfg, 3)
		pod := storedProxyPod(t, cs)
		if tc.fails {
			if !errors.Is(err, runner.ErrProxyReplaceFailed) || pod.UID != "proxy-uid-9" {
				t.Errorf("over a generation %s pod that took the name: err %v, pod %s; want ErrProxyReplaceFailed and that pod left", tc.taken, err, pod.UID)
			}
			continue
		}
		if err != nil || pod.Labels[labelProxyGeneration] != "3" {
			t.Errorf("over a generation %s pod that took the name: err %v, labels %v; want it removed and the generation 3 pod made", tc.taken, err, pod.Labels)
		}
	}
}

func TestEndSandboxAtRefusesAnOlderGeneration(t *testing.T) {
	r := newKeptRig(t, nil)
	labelProxyPod(t, r.cs, "2")
	ctx := context.Background()
	order := writesInOrder(r.cs, r.dyn)
	if err := r.k.EndSandboxAt(ctx, r.ref, 1); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("EndSandboxAt(1) beside a generation 2 proxy = %v, want ErrProxyGenerationNewer", err)
	}
	if err := r.k.StopProxyAt(ctx, r.ref, 1); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Errorf("StopProxyAt(1) beside a generation 2 proxy = %v, want ErrProxyGenerationNewer", err)
	}
	if err := r.k.StopProxyAt(ctx, r.ref, 2); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StopProxyAt(2) = %v, want StopProxy's ErrEndUnsupported on a server", err)
	}
	if len(*order) != 0 {
		t.Fatalf("writes = %v, want none: a newer revive owns the run", *order)
	}
	if err := r.k.EndSandboxAt(ctx, r.ref, 2); err != nil {
		t.Fatalf("EndSandboxAt(2) beside a generation 2 proxy: %v", err)
	}
	want := []string{"update endpointslices", "delete pods", "delete-collection secrets", "delete pods"}
	if !slices.Equal(*order, want) {
		t.Errorf("writes of the generation 2 end = %v, want %v", *order, want)
	}
}

func TestEndSandboxAtLeavesARevivedAgentPod(t *testing.T) {
	r := newKeptRig(t, nil)
	swapped := false
	r.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.DeleteAction).GetName()
		if swapped || name != proxyPodName(asRunID) {
			return false, nil, nil
		}
		// A newer revive's proxy takes the name while this end removes the old one.
		swapped = true
		if err := r.cs.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			return true, nil, err
		}
		newer := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "proxy-uid-2",
			Labels: map[string]string{labelRun: asRunID.String(), labelComponent: componentProxy, labelProxyGeneration: "2"}}}
		return true, nil, r.cs.Tracker().Create(podsGVR, newer, testNamespace)
	})
	if err := r.k.EndSandboxAt(context.Background(), r.ref, 1); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("EndSandboxAt(1) = %v, want ErrProxyGenerationNewer once a generation 2 proxy appeared", err)
	}
	if n := len(agentPodDeletes(r.cs)); n != 0 {
		t.Errorf("agent pod deletes = %d, want none: the running pod is the newer revive's", n)
	}
}

// A generation 1 end reads a generation 1 proxy pod; while its delete lands, a newer revive's proxy takes the name and
// that revive writes its replacement config Secret. The end must leave the Secret and refuse with ErrProxyGenerationNewer.
// Without the second read of the proxy pod in stopKeptProxy, the end deletes the Secret and returns nil or an agent-pod
// error.
func TestKeepEndSandboxAtLeavesANewerRevivesConfigSecret(t *testing.T) {
	r := newKeptRig(t, nil)
	labelProxyPod(t, r.cs, "1")
	secretsGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	swapped := false
	r.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.DeleteAction).GetName()
		if swapped || name != proxyPodName(asRunID) {
			return false, nil, nil
		}
		// A newer revive's proxy takes the name, and its config Secret is written, while this end removes the old pod.
		swapped = true
		if err := r.cs.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			return true, nil, err
		}
		newer := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "proxy-uid-2",
			Labels: map[string]string{labelRun: asRunID.String(), labelComponent: componentProxy, labelProxyGeneration: "2"}}}
		if err := r.cs.Tracker().Create(podsGVR, newer, testNamespace); err != nil {
			return true, nil, err
		}
		cfg := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "proxy-cfg-2", Namespace: testNamespace,
			Labels: map[string]string{labelRun: asRunID.String(), labelProxyCfg: "true"}}}
		return true, nil, r.cs.Tracker().Create(secretsGVR, cfg, testNamespace)
	})
	if err := r.k.EndSandboxAt(context.Background(), r.ref, 1); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("EndSandboxAt(1) = %v, want ErrProxyGenerationNewer once a generation 2 proxy appeared", err)
	}
	if _, err := r.cs.Tracker().Get(secretsGVR, testNamespace, "proxy-cfg-2"); err != nil {
		t.Errorf("the newer revive's config Secret after the end: %v; want it left", err)
	}
	for _, a := range r.cs.Actions() {
		if a.GetVerb() == "delete-collection" && a.GetResource().Resource == "secrets" {
			t.Errorf("recorded %s %s, want no Secret delete: the Secret is the newer revive's", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestKeepEndSandboxRemovesOnlyTheProxyPodItRead(t *testing.T) {
	r := newKeptRig(t, nil)
	honourPodDeletePreconditions(r.cs)
	swapped := false
	r.cs.PrependReactor("update", "endpointslices", func(clienttesting.Action) (bool, runtime.Object, error) {
		if swapped {
			return false, nil, nil
		}
		// Between the end's read of the proxy pod and its delete, another proxy pod takes the name.
		swapped = true
		other := storedProxyPod(t, r.cs).DeepCopy()
		other.UID = k8stypes.UID("proxy-uid-2")
		return false, nil, r.cs.Tracker().Update(podsGVR, other, testNamespace)
	})
	r.end(t)
	obj, err := r.cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(asRunID))
	if err != nil || obj.(*corev1.Pod).UID != "proxy-uid-2" {
		t.Errorf("proxy pod after the end: err %v; want the pod that took the name, left as it is", err)
	}
}

func TestKeepStartSandboxRefusesAPartialStart(t *testing.T) {
	r := newKeptRig(t, nil)
	r.cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.GetAction).GetName() != agentPodName(asRunID) {
			return false, nil, nil
		}
		obj, err := r.cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID))
		if err != nil {
			return true, nil, err
		}
		// Ungated by an earlier start, placed, its container not yet running.
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Spec.NodeName, pod.Status.Phase = "node-1", corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		return true, pod, nil
	})
	err := r.k.StartSandbox(context.Background(), r.ref)
	if err == nil || !strings.Contains(err.Error(), "a start that stopped partway") || strings.Contains(err.Error(), "Ask your administrator") {
		t.Fatalf("StartSandbox = %v, want the partial start named, not the refusal of a pod that ran before Wardyn's check", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none", n)
	}
}
