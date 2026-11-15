// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// addCfgSecret stores a replacement proxy config Secret of the run, labelled with generation gen ("" for none).
func addCfgSecret(t *testing.T, cs *fake.Clientset, name, gen string) {
	t.Helper()
	labels := map[string]string{labelRun: asRunID.String(), labelProxyCfg: "true"}
	if gen != "" {
		labels[labelProxyGeneration] = gen
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels}}
	if err := cs.Tracker().Create(secretsGVR, sec, testNamespace); err != nil {
		t.Fatal(err)
	}
}

// secretDeletes returns the names the recorded actions delete a Secret by, and whether any delete-collection hit Secrets.
func secretDeletes(cs *fake.Clientset) (names []string, collection bool) {
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "secrets" {
			continue
		}
		switch a.GetVerb() {
		case "delete":
			names = append(names, a.(clienttesting.DeleteAction).GetName())
		case "delete-collection":
			collection = true
		}
	}
	return names, collection
}

// secretReads is the get, list and watch calls on Secrets the fake recorded: the chart's Role grants none of them.
func secretReads(cs *fake.Clientset) []string {
	var reads []string
	for _, a := range cs.Actions() {
		if v := a.GetVerb(); a.GetResource().Resource == "secrets" && (v == "get" || v == "list" || v == "watch") {
			reads = append(reads, v)
		}
	}
	return reads
}

// mountCfgSecret stores a replacement proxy config Secret of the run, labelled with generation gen ("" for none), and
// points the stored proxy pod's config volume at it, as a replace leaves them.
func mountCfgSecret(t *testing.T, cs *fake.Clientset, name, gen string) {
	t.Helper()
	addCfgSecret(t, cs, name, gen)
	pod := storedProxyPod(t, cs).DeepCopy()
	found := false
	for i := range pod.Spec.Volumes {
		if v := &pod.Spec.Volumes[i]; v.Name == proxyConfigSecretVolumeName && v.Secret != nil {
			v.Secret.SecretName, found = name, true
		}
	}
	if !found {
		t.Fatalf("the proxy pod has no %s volume", proxyConfigSecretVolumeName)
	}
	if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
}

// secretStored answers whether the tracker holds Secret name; any error but its absence fails the test.
func secretStored(t *testing.T, cs *fake.Clientset, name string) bool {
	t.Helper()
	_, err := cs.Tracker().Get(secretsGVR, testNamespace, name)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// cfgSecretNames is the sorted names of the stored Secrets that carry labelProxyCfg, read from the tracker.
func cfgSecretNames(t *testing.T, cs *fake.Clientset) []string {
	t.Helper()
	obj, err := cs.Tracker().List(secretsGVR, deleteCollectionGVKs["secrets"], testNamespace, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items, err := meta.ExtractList(obj)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		acc, err := meta.Accessor(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := acc.GetLabels()[labelProxyCfg]; ok {
			names = append(names, acc.GetName())
		}
	}
	slices.Sort(names)
	return names
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
		cfg := replacementCfgSecret(asRunID, pod)
		obj, err := cs.Tracker().Get(secretsGVR, testNamespace, cfg)
		if err != nil {
			t.Fatalf("gen %d: the new proxy pod's config Secret %q: %v", gen, cfg, err)
		}
		sl := obj.(*corev1.Secret).Labels
		sgen, slabelled := sl[labelProxyGeneration]
		if (gen == 0 && slabelled) || (gen == 3 && sgen != "3") || sl[labelRun] != asRunID.String() {
			t.Errorf("gen %d: config Secret labels = %v, want %s=3 for generation 3, no such label for none, and the run's %s, by which its teardown deletes it", gen, sl, labelProxyGeneration, labelRun)
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

// Another replace's proxy pod takes the name after this generation 3 replace removed the old pod, and before its create.
// A pod of an older generation is removed with the config Secret it mounted, and the create is tried again. One of
// generation 3 is the same-generation race: that replace won the name, so this one answers ErrProxyReplaceFailed and
// leaves its pod and Secret; its own Secret, mounted by no pod, is left for the run's teardown. A newer one is left too.
func TestReplaceProxyAtLooksAgainWhenTheNameIsTaken(t *testing.T) {
	for _, tc := range []struct {
		taken string
		fails bool
	}{{"1", false}, {"3", true}, {"5", true}} {
		k, cs, _, ref := keptRun(t)
		theirs := secretName(asRunID) + "-r9"
		addCfgSecret(t, cs, theirs, tc.taken)
		took := false
		cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
			pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
			if took || !ok || pod.Name != proxyPodName(asRunID) {
				return false, nil, nil
			}
			// Another replace makes its proxy pod after this one removed the old pod, before this create.
			took = true
			other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: testNamespace, UID: "proxy-uid-9",
				Labels: map[string]string{labelRun: asRunID.String(), labelComponent: componentProxy, labelProxyGeneration: tc.taken}},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: proxyConfigSecretVolumeName,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: theirs}}}}}}
			return false, nil, cs.Tracker().Create(podsGVR, other, testNamespace)
		})
		_, err := k.ReplaceProxyHashedAt(context.Background(), ref, replaceCfg, 3)
		pod := storedProxyPod(t, cs)
		kept := secretStored(t, cs, theirs)
		if tc.fails {
			if !errors.Is(err, runner.ErrProxyReplaceFailed) || pod.UID != "proxy-uid-9" || !kept {
				t.Errorf("over a generation %s pod that took the name: err %v, pod %s, its Secret kept %v; want ErrProxyReplaceFailed, and that pod and its Secret left", tc.taken, err, pod.UID, kept)
			}
			continue
		}
		if err != nil || pod.Labels[labelProxyGeneration] != "3" || kept {
			t.Errorf("over a generation %s pod that took the name: err %v, labels %v, its Secret kept %v; want it removed with its Secret and the generation 3 pod made", tc.taken, err, pod.Labels, kept)
		}
	}
}

func TestEndSandboxAtRefusesAnOlderGeneration(t *testing.T) {
	r := newKeptRig(t, nil)
	labelProxyPod(t, r.cs, "2")
	ctx := context.Background()
	mountCfgSecret(t, r.cs, secretName(asRunID)+"-r2", "2")
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
	want := []string{"update endpointslices", "delete pods", "delete secrets", "delete pods"}
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

// A generation 1 end reads a generation 1 proxy pod, which mounts its replace's config Secret. While its delete lands, a
// newer revive's proxy takes the name and that revive writes its own config Secret. The end deletes only the Secret the
// removed pod mounted, by name, and refuses with ErrProxyGenerationNewer on its second read of the proxy pod. The newer
// Secret carries no generation: the end never names a Secret that no removed pod mounted, whatever its labels. The agent
// pod is left stored, as it is the newer revive's. The agent pod is run twice, running and held: a running one is refused by
// deleteKeptAgentPod's own read of the proxy pod, so only a held one, which that read lets through, shows that the end
// itself refuses on its second read.
func TestKeepEndSandboxAtLeavesANewerRevivesConfigSecret(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) { keepEndLeavesANewerRevivesConfigSecret(t, held) })
	}
}

func keepEndLeavesANewerRevivesConfigSecret(t *testing.T, held bool) {
	r := newKeptRig(t, nil)
	labelProxyPod(t, r.cs, "1")
	mounted, newer := secretName(asRunID)+"-r1", secretName(asRunID)+"-r2"
	mountCfgSecret(t, r.cs, mounted, "1")
	if held {
		obj, err := r.cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID))
		if err != nil {
			t.Fatal(err)
		}
		gated := obj.(*corev1.Pod).DeepCopy()
		gated.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "wardyn.dev/held"}}
		if err := r.cs.Tracker().Update(podsGVR, gated, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
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
		other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "proxy-uid-2",
			Labels: map[string]string{labelRun: asRunID.String(), labelComponent: componentProxy, labelProxyGeneration: "2"}}}
		if err := r.cs.Tracker().Create(podsGVR, other, testNamespace); err != nil {
			return true, nil, err
		}
		cfg := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: newer, Namespace: testNamespace,
			Labels: map[string]string{labelRun: asRunID.String(), labelProxyCfg: "true"}}}
		return true, nil, r.cs.Tracker().Create(secretsGVR, cfg, testNamespace)
	})
	if err := r.k.EndSandboxAt(context.Background(), r.ref, 1); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("EndSandboxAt(1) with the agent pod held=%v = %v, want ErrProxyGenerationNewer once a generation 2 proxy appeared", held, err)
	}
	if _, err := r.cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID)); err != nil {
		t.Errorf("the agent pod after a refused end: %v; want it stored, as a refused end must not delete the newer revive's agent pod", err)
	}
	if !secretStored(t, r.cs, newer) {
		t.Errorf("the newer revive's config Secret %s is gone after the end; want it left", newer)
	}
	if names, collection := secretDeletes(r.cs); !slices.Equal(names, []string{mounted}) || collection {
		t.Errorf("Secret deletes %v, delete-collection %v; want only %s, the one the removed pod mounted", names, collection, mounted)
	}
	if reads := secretReads(r.cs); len(reads) != 0 {
		t.Errorf("Secret reads %v; the chart's Role grants no get, list or watch on Secrets", reads)
	}
}

// A newer revive creates its config Secret before its proxy pod, so for a while the run has the Secret and no proxy pod.
// A generation 1 end that read a generation 1 pod, which mounts its replace's Secret, finds no pod on its second read, and
// nothing it reads tells the newer Secret apart. It need not: it deletes only the Secret the removed pod mounted, by name,
// and answers nil. The rig applies a delete by name and a delete-collection (installDeleteCollectionSupport, which
// newSandboxRig installs), though not a delete's preconditions. It also refuses a get or a list of a Secret, as the
// chart's Role does (denySecretReads). An end that lists fails, and one that deletes the newer Secret, by name or by
// selector, fails the tracker check. The agent pod is held, so deleteKeptAgentPod returns nil and only the Secret path is
// under test.
func TestKeepEndSandboxAtLeavesANewerRevivesSecretBeforeItsPod(t *testing.T) {
	r := newKeptRig(t, nil)
	labelProxyPod(t, r.cs, "1")
	mounted, newer := secretName(asRunID)+"-r1", secretName(asRunID)+"-r2"
	mountCfgSecret(t, r.cs, mounted, "1")
	obj, err := r.cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID))
	if err != nil {
		t.Fatal(err)
	}
	held := obj.(*corev1.Pod).DeepCopy()
	held.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "wardyn.dev/held"}}
	if err := r.cs.Tracker().Update(podsGVR, held, testNamespace); err != nil {
		t.Fatal(err)
	}
	swapped := false
	r.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.DeleteAction).GetName()
		if swapped || name != proxyPodName(asRunID) {
			return false, nil, nil
		}
		// A newer revive has written its config Secret and not yet made its proxy pod.
		swapped = true
		if err := r.cs.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			return true, nil, err
		}
		cfg := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: newer, Namespace: testNamespace,
			Labels: map[string]string{labelRun: asRunID.String(), labelProxyCfg: "true", labelProxyGeneration: "2"}}}
		return true, nil, r.cs.Tracker().Create(secretsGVR, cfg, testNamespace)
	})
	if err := r.k.EndSandboxAt(context.Background(), r.ref, 1); err != nil {
		t.Fatalf("EndSandboxAt(1) = %v, want nil: no proxy pod is left to refuse it", err)
	}
	if names, collection := secretDeletes(r.cs); !slices.Equal(names, []string{mounted}) || collection {
		t.Errorf("Secret deletes %v, delete-collection %v; want only %s, the one the removed pod mounted", names, collection, mounted)
	}
	if !secretStored(t, r.cs, newer) {
		t.Errorf("the newer revive's config Secret %s is gone after the end; want it left", newer)
	}
	if reads := secretReads(r.cs); len(reads) != 0 {
		t.Errorf("Secret reads %v; the chart's Role grants no get, list or watch on Secrets", reads)
	}
}

// A replace deletes, once the old proxy pod is gone and before it makes the new one, the replacement config Secret that
// pod mounted, by name, and reads no Secret. A newer revive's Secret, written before its pod exists, is mounted by no pod
// this replace removes and is left, and so is the run's own.
func TestReplaceProxyAtDeletesOnlyTheSecretTheOldPodMounted(t *testing.T) {
	k, cs, dyn, ref := keptRun(t)
	mounted, newer := secretName(asRunID)+"-r1", secretName(asRunID)+"-r2"
	mountCfgSecret(t, cs, mounted, "")
	addCfgSecret(t, cs, newer, "2")
	cs.ClearActions()
	order := writesInOrder(cs, dyn)
	if _, err := k.ReplaceProxyHashedAt(context.Background(), ref, replaceCfg, 1); err != nil {
		t.Fatalf("ReplaceProxyHashedAt(1): %v", err)
	}
	want := []string{"create secrets", "update endpointslices", "delete pods", "delete secrets", "create pods", "update endpointslices"}
	if !slices.Equal(*order, want) {
		t.Errorf("writes = %v\nwant %v: the old pod's Secret once that pod is gone, before the new pod", *order, want)
	}
	if names, collection := secretDeletes(cs); !slices.Equal(names, []string{mounted}) || collection {
		t.Errorf("Secret deletes %v, delete-collection %v; want only %s by name", names, collection, mounted)
	}
	for _, kept := range []string{newer, secretName(asRunID)} {
		if !secretStored(t, cs, kept) {
			t.Errorf("Secret %s is gone; a replace deletes only the Secret the old proxy pod mounted", kept)
		}
	}
	if reads := secretReads(cs); len(reads) != 0 {
		t.Errorf("Secret reads %v; the chart's Role grants no get, list or watch on Secrets", reads)
	}
}

// The end deletes the one replacement config Secret its removed proxy pod mounted, by name, and reads no Secret. Another
// run's replacement Secret is left, and so is the run's own. A Secret of the run that no removed pod mounted is left as
// well: a newer revive's looks the same before its pod exists, and the run's teardown deletes one a failed replace left.
// The rig applies a delete by name and a delete-collection, so an end that sweeps by selector, or deletes the wrong name,
// fails the tracker checks, and it refuses a get or a list of a Secret, as the chart's Role does.
func TestKeepEndSandboxDeletesOnlyTheSecretItsProxyMounted(t *testing.T) {
	r := newKeptRig(t, nil)
	mounted, unmounted := secretName(asRunID)+"-r1", secretName(asRunID)+"-r2"
	mountCfgSecret(t, r.cs, mounted, "")
	addCfgSecret(t, r.cs, unmounted, "")
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wardyn-proxy-cfg-other-run-r1", Namespace: testNamespace,
		Labels: map[string]string{labelRun: "other-run", labelProxyCfg: "1"}}}
	if err := r.cs.Tracker().Create(secretsGVR, other, testNamespace); err != nil {
		t.Fatal(err)
	}
	order := writesInOrder(r.cs, r.dyn)
	r.end(t)
	if want := []string{"update endpointslices", "delete pods", "delete secrets", "delete pods"}; !slices.Equal(*order, want) {
		t.Errorf("writes = %v\nwant %v: the slice, the proxy pod, its Secret once the pod is gone, the agent pod", *order, want)
	}
	if names, collection := secretDeletes(r.cs); !slices.Equal(names, []string{mounted}) || collection {
		t.Errorf("Secret deletes %v, delete-collection %v; want only %s by name", names, collection, mounted)
	}
	if secretStored(t, r.cs, mounted) {
		t.Errorf("Secret %s, which the removed proxy pod mounted, is still stored", mounted)
	}
	for _, kept := range []string{unmounted, other.Name, secretName(asRunID)} {
		if !secretStored(t, r.cs, kept) {
			t.Errorf("Secret %s is gone; the end deletes only the Secret its removed proxy pod mounted", kept)
		}
	}
	if reads := secretReads(r.cs); len(reads) != 0 {
		t.Errorf("Secret reads %v; the chart's Role grants no get, list or watch on Secrets", reads)
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
