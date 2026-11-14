// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedReady stores placeholder i of p, bound to node and Ready, with the given UID and resourceVersion.
func seedReady(t *testing.T, d *Driver, cs *fake.Clientset, p WarmPool, i int, node, uid, rv string) {
	t.Helper()
	seedPlaceholder(t, d, cs, p, i, func(pod *corev1.Pod) {
		pod.UID, pod.ResourceVersion = k8stypes.UID(uid), rv
		pod.Spec.NodeName = node
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
}

// warmClaimSpec is a run that fits the pool warmTestPool makes.
func warmClaimSpec() runner.SandboxSpec {
	spec := testSandboxSpec()
	spec.Image = warmTestImage
	spec.Resources = runner.Resources{CPUMillis: 250, MemoryMiB: 128}
	return spec
}

func TestClaimWarmTakesAReadyPlaceholderAndReturnsItsNode(t *testing.T) {
	p := warmTestPool("a", 2)
	d, cs := warmTestDriver(t, p)
	seedReady(t, d, cs, p, 1, "n2", "u1", "5")
	seedReady(t, d, cs, p, 0, "n1", "u0", "4")
	cs.ClearActions()
	if got := d.claimWarm(context.Background(), warmClaimSpec(), types.CC1); got != "n1" {
		t.Fatalf("claimWarm = %q, want n1 (the first by name)", got)
	}
	dels := deletesOf(cs, "pods")
	if len(dels) != 1 || dels[0].GetName() != "wardyn-warm-a-0" {
		t.Fatalf("pod deletes %v, want wardyn-warm-a-0 alone", dels)
	}
	pre := dels[0].GetDeleteOptions().Preconditions
	if pre == nil || pre.UID == nil || *pre.UID != "u0" || pre.ResourceVersion == nil || *pre.ResourceVersion != "4" {
		t.Fatalf("delete preconditions %+v, want UID u0 and resourceVersion 4", pre)
	}
}

func TestClaimWarmMissesOnImageClassOrSize(t *testing.T) {
	p := warmTestPool("a", 1)
	for _, tc := range []struct {
		name  string
		class types.ConfinementClass
		edit  func(*runner.SandboxSpec)
	}{
		{"image", types.CC1, func(s *runner.SandboxSpec) { s.Image = "other.example/agent:1" }},
		{"class", types.CC2, func(*runner.SandboxSpec) {}},
		{"cpu", types.CC1, func(s *runner.SandboxSpec) { s.Resources.CPUMillis = 4000 }},
		{"memory", types.CC1, func(s *runner.SandboxSpec) { s.Resources.MemoryMiB = 8192 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := warmTestDriver(t, p)
			seedReady(t, d, cs, p, 0, "n1", "u0", "4")
			cs.ClearActions()
			spec := warmClaimSpec()
			tc.edit(&spec)
			if got := d.claimWarm(context.Background(), spec, tc.class); got != "" {
				t.Fatalf("claimWarm = %q, want a miss", got)
			}
			if n := len(deletesOf(cs, "pods")); n != 0 {
				t.Fatalf("%d pod deletes on a miss", n)
			}
		})
	}
}

func TestClaimWarmSkipsAPlaceholderLostToAnotherRun(t *testing.T) {
	p := warmTestPool("a", 2)
	d, cs := warmTestDriver(t, p)
	seedReady(t, d, cs, p, 0, "n1", "u0", "4")
	seedReady(t, d, cs, p, 1, "n2", "u1", "5")
	cs.ClearActions()
	lost := false
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if lost {
			return false, nil, nil
		}
		lost = true
		return true, nil, apierrors.NewConflict(podsGVR.GroupResource(), a.(clienttesting.DeleteAction).GetName(), errors.New("taken"))
	})
	if got := d.claimWarm(context.Background(), warmClaimSpec(), types.CC1); got != "n2" {
		t.Fatalf("claimWarm = %q, want n2 (the first was lost)", got)
	}
}

func TestClaimWarmWaitsForThePlaceholderToGo(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedReady(t, d, cs, p, 0, "n1", "u0", "4")
	cs.ClearActions()
	if got := d.claimWarm(context.Background(), warmClaimSpec(), types.CC1); got != "n1" {
		t.Fatalf("claimWarm = %q, want n1", got)
	}
	requireInOrder(t, warmSeq(cs), "delete pods", "get pods")
}

func TestClaimWarmVMRunNeverClaims(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedReady(t, d, cs, p, 0, "n1", "u0", "4")
	cs.ClearActions()
	spec := warmClaimSpec()
	spec.MachineKind = types.MachineLinuxVM
	if got := d.claimWarm(context.Background(), spec, types.CC1); got != "" {
		t.Fatalf("claimWarm = %q for a VM run, want none", got)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Fatalf("%d actions for a VM run, want none", n)
	}
}

// warmRequiredPlacement is a placement with one required node term, which a claimed run must keep.
const warmRequiredPlacement = `{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"pool","operator":"In","values":["sandbox"]}]}]}}}}`

// createdPods runs CreateSandbox for spec and returns the proxy and agent pods it made.
func createdPods(t *testing.T, d *Driver, cs *fake.Clientset, spec runner.SandboxSpec) (proxy, agent *corev1.Pod) {
	t.Helper()
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	proxy, err = cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	agent, err = cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the agent pod: %v", err)
	}
	return proxy, agent
}

func TestCreateSandboxPrefersTheClaimedNode(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := newTestDriver(t, Config{WarmPools: []WarmPool{p}, SandboxPlacement: warmRequiredPlacement})
	seedReady(t, d, cs, p, 0, "n7", "u0", "4")
	proxy, agent := createdPods(t, d, cs, warmClaimSpec())
	for name, pod := range map[string]*corev1.Pod{"proxy": proxy, "agent": agent} {
		na := pod.Spec.Affinity.NodeAffinity
		prefs := na.PreferredDuringSchedulingIgnoredDuringExecution
		if len(prefs) != 1 || len(prefs[0].Preference.MatchFields) != 1 ||
			prefs[0].Preference.MatchFields[0].Key != "metadata.name" || len(prefs[0].Preference.MatchFields[0].Values) != 1 || prefs[0].Preference.MatchFields[0].Values[0] != "n7" {
			t.Errorf("%s pod preferred terms %+v, want one naming n7", name, prefs)
		}
		req := na.RequiredDuringSchedulingIgnoredDuringExecution
		if req == nil || len(req.NodeSelectorTerms) == 0 || len(req.NodeSelectorTerms[0].MatchExpressions) == 0 || req.NodeSelectorTerms[0].MatchExpressions[0].Key != "pool" {
			t.Errorf("%s pod lost the placement's required term: %+v", name, req)
		}
	}
	if n := len(deletesOf(cs, "pods")); n < 1 {
		t.Error("the placeholder was not removed")
	}
}

func TestCreateSandboxNoPoolUnchanged(t *testing.T) {
	d, cs := newTestDriver(t, Config{SandboxPlacement: warmRequiredPlacement})
	proxy, agent := createdPods(t, d, cs, warmClaimSpec())
	for name, pod := range map[string]*corev1.Pod{"proxy": proxy, "agent": agent} {
		if a := pod.Spec.Affinity; a != nil && a.NodeAffinity != nil && len(a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
			t.Errorf("%s pod has preferred terms %+v with no pool", name, a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
		}
	}
}
