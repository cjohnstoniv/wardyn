// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestWarmPoolParse(t *testing.T) {
	if pools, err := parseWarmPools("  \n"); pools != nil || err != nil {
		t.Fatalf("empty = %v, %v; want nil, nil", pools, err)
	}
	good := `[{"name":"a","image":"r/agent:1","class":"CC1","cpu":"500m","memory":"1Gi","size":2},
	          {"name":"b-2","image":"r/agent:2","class":"CC2","cpu":"1","memory":"2Gi","size":0}]`
	pools, err := parseWarmPools(good)
	if err != nil {
		t.Fatalf("a valid value is refused: %v", err)
	}
	want := []WarmPool{
		{Name: "a", Image: "r/agent:1", Class: "CC1", CPU: "500m", Memory: "1Gi", Size: 2},
		{Name: "b-2", Image: "r/agent:2", Class: "CC2", CPU: "1", Memory: "2Gi", Size: 0},
	}
	if !reflect.DeepEqual(pools, want) {
		t.Fatalf("pools = %+v, want %+v", pools, want)
	}
	one := func(name, image, class, cpu, mem string, size int) string {
		return `[{"name":"` + name + `","image":"` + image + `","class":"` + class + `","cpu":"` + cpu + `","memory":"` + mem + `","size":` + strconv.Itoa(size) + `}]`
	}
	cases := []struct{ name, raw, want string }{
		{"unknown field", `[{"name":"a","image":"i","class":"CC1","cpu":"1","memory":"1Gi","size":1,"extra":1}]`, "unknown field"},
		{"trailing data", one("a", "i", "CC1", "1", "1Gi", 1) + ` []`, "data after the array"},
		{"not an array", `{"name":"a"}`, "cannot read"},
		{"bad name", one("A_b", "i", "CC1", "1", "1Gi", 1), "DNS label"},
		{"name of 31 characters", one(strings.Repeat("a", 31), "i", "CC1", "1", "1Gi", 1), "DNS label"},
		{"duplicate", strings.Replace(good, `"b-2"`, `"a"`, 1), "is repeated"},
		{"empty image", one("a", "", "CC1", "1", "1Gi", 1), "image"},
		{"image with whitespace", one("a", "x y", "CC1", "1", "1Gi", 1), "image"},
		{"class CC4", one("a", "i", "CC4", "1", "1Gi", 1), "class"},
		{"cpu zero", one("a", "i", "CC1", "0", "1Gi", 1), "cpu"},
		{"memory not a quantity", one("a", "i", "CC1", "1", "x", 1), "memory"},
		{"size 21", one("a", "i", "CC1", "1", "1Gi", 21), "size"},
		{"negative size", one("a", "i", "CC1", "1", "1Gi", -1), "size"},
		{"total 51", `[{"name":"a","image":"i","class":"CC1","cpu":"1","memory":"1Gi","size":20},` +
			`{"name":"b","image":"i","class":"CC1","cpu":"1","memory":"1Gi","size":20},` +
			`{"name":"c","image":"i","class":"CC1","cpu":"1","memory":"1Gi","size":11}]`, "add up to 51"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWarmPools(tc.raw)
			if err == nil || !strings.HasPrefix(err.Error(), warmPoolsRefusal) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestWarmPoolClassesNeedRuntimes(t *testing.T) {
	pools := []WarmPool{{Name: "a", Class: "CC1"}, {Name: "b", Class: "CC2"}}
	if err := checkWarmPoolClasses(pools, nil); err == nil || !strings.Contains(err.Error(), `pool "b" is CC2, and no RuntimeClass is pinned`) {
		t.Fatalf("an unpinned CC2 pool = %v", err)
	}
	if err := checkWarmPoolClasses(pools[:1], nil); err != nil {
		t.Fatalf("a CC1 pool needs no RuntimeClass: %v", err)
	}
	if err := checkWarmPoolClasses(pools, map[types.ConfinementClass]string{types.CC2: "gvisor"}); err != nil {
		t.Fatalf("a pinned CC2 pool is refused: %v", err)
	}
	if err := checkWarmPoolClasses([]WarmPool{{Name: "c", Class: "CC3"}}, map[types.ConfinementClass]string{types.CC2: "gvisor"}); err == nil {
		t.Fatal("an unpinned CC3 pool passed")
	}
}

func TestWarmPodNameAndIndex(t *testing.T) {
	for _, i := range []int{0, 1, 9, 10, 42} {
		if got, ok := warmIndex(warmPodName("a", i), "a"); !ok || got != i {
			t.Errorf("round trip of %d = %d, %v", i, got, ok)
		}
	}
	for _, name := range []string{"wardyn-warm-a-01", "wardyn-warm-a--1", "wardyn-warm-b-1", "wardyn-warm-ab-1", "wardyn-warm-a-", "wardyn-warm-a-1x", "other-a-1"} {
		if got, ok := warmIndex(name, "a"); ok {
			t.Errorf("warmIndex(%q, a) = %d, true; want false", name, got)
		}
	}
}

func TestWarmPoolLowestFreeIndex(t *testing.T) {
	for _, tc := range []struct {
		used []int
		want int
	}{{nil, 0}, {[]int{0, 1, 2}, 3}, {[]int{1, 2}, 0}, {[]int{0, 2, 3}, 1}, {[]int{2, 0, 1, 1}, 3}} {
		if got := lowestFreeIndex(tc.used); got != tc.want {
			t.Errorf("lowestFreeIndex(%v) = %d, want %d", tc.used, got, tc.want)
		}
	}
}

func TestWarmPoolMatch(t *testing.T) {
	q := resource.MustParse
	pools := []WarmPool{
		{Name: "off", Image: "i", Class: "CC1", CPU: "1", Memory: "1Gi", Size: 0},
		{Name: "small", Image: "i", Class: "CC1", CPU: "500m", Memory: "1Gi", Size: 1},
		{Name: "big", Image: "i", Class: "CC1", CPU: "2", Memory: "4Gi", Size: 1},
		{Name: "gvisor", Image: "i", Class: "CC2", CPU: "2", Memory: "4Gi", Size: 1},
		{Name: "other", Image: "j", Class: "CC1", CPU: "2", Memory: "4Gi", Size: 1},
	}
	for _, tc := range []struct {
		name, image string
		class       types.ConfinementClass
		cpu, mem    string
		want        string
	}{
		{"first match wins", "i", types.CC1, "250m", "512Mi", "small"},
		{"empty class is CC1", "i", "", "250m", "512Mi", "small"},
		{"cpu excludes the small pool", "i", types.CC1, "1", "512Mi", "big"},
		{"memory excludes the small pool", "i", types.CC1, "250m", "2Gi", "big"},
		{"class", "i", types.CC2, "250m", "512Mi", "gvisor"},
		{"image", "j", types.CC1, "250m", "512Mi", "other"},
		{"a size-0 pool is skipped", "i", types.CC1, "1", "1Gi", "big"},
		{"cpu above every pool", "i", types.CC1, "3", "1Gi", ""},
		{"memory above every pool", "i", types.CC1, "1", "8Gi", ""},
		{"no pool for the image", "k", types.CC1, "1", "1Gi", ""},
		{"no pool for the class", "i", types.CC3, "1", "1Gi", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := matchWarmPool(pools, tc.image, tc.class, q(tc.cpu), q(tc.mem))
			if ok != (tc.want != "") || p.Name != tc.want {
				t.Fatalf("matched %q, %v; want %q", p.Name, ok, tc.want)
			}
		})
	}
}

func TestPreferNodeKeepsRequiredTerms(t *testing.T) {
	required := &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
		{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"agents"}},
	}}}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: required,
	}}}}
	preferNode(pod, "")
	if got := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution; len(got) != 0 {
		t.Fatalf("an empty node added %d preferred terms", len(got))
	}
	preferNode(pod, "node-1")
	na := pod.Spec.Affinity.NodeAffinity
	if !reflect.DeepEqual(na.RequiredDuringSchedulingIgnoredDuringExecution, required) {
		t.Fatalf("the required terms changed: %+v", na.RequiredDuringSchedulingIgnoredDuringExecution)
	}
	prefs := na.PreferredDuringSchedulingIgnoredDuringExecution
	if len(prefs) != 1 || prefs[0].Weight != 100 {
		t.Fatalf("preferred terms = %+v, want one of weight 100", prefs)
	}
	f := prefs[0].Preference.MatchFields
	if len(f) != 1 || f[0].Key != "metadata.name" || f[0].Operator != corev1.NodeSelectorOpIn || !reflect.DeepEqual(f[0].Values, []string{"node-1"}) {
		t.Fatalf("preferred term = %+v", f)
	}
	bare := &corev1.Pod{}
	preferNode(bare, "node-2")
	if bare.Spec.Affinity == nil || bare.Spec.Affinity.NodeAffinity == nil || len(bare.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("a pod with no affinity gained %+v", bare.Spec.Affinity)
	}
	untouched := &corev1.Pod{}
	preferNode(untouched, "")
	if untouched.Spec.Affinity != nil {
		t.Fatal("an empty node created an affinity")
	}
}

func TestPlaceholderHoldsNothing(t *testing.T) {
	const image = "registry.example/agent:1"
	clean := func() *corev1.Pod {
		token, links := false, false
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "wardyn-warm-a-0", Labels: map[string]string{"wardyn.component": "warm", "wardyn.warm-pool": "a"}},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &token,
				EnableServiceLinks:           &links,
				Containers:                   []corev1.Container{{Name: "warm", Image: image}},
			},
		}
	}
	if err := placeholderHoldsNothing(clean(), image); err != nil {
		t.Fatalf("a clean placeholder is refused: %v", err)
	}
	on := true
	cases := []struct {
		name string
		edit func(p *corev1.Pod)
		want string
	}{
		{"volume", func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: "v"}} }, "has a volume"},
		{"init container", func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "warm", Image: image}} }, "init container"},
		{"ephemeral container", func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: image}}}
		}, "ephemeral container"},
		{"no container", func(p *corev1.Pod) { p.Spec.Containers = nil }, "has 0 containers"},
		{"extra plain container", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "side", Image: "other.example/tool:1"})
		}, "has 2 containers"},
		{"container name", func(p *corev1.Pod) { p.Spec.Containers[0].Name = "agent" }, `has container "agent"`},
		{"another image", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "other.example/agent:1" }, `runs image "other.example/agent:1"`},
		{"host network", func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "shares a host namespace"},
		{"host pid", func(p *corev1.Pod) { p.Spec.HostPID = true }, "shares a host namespace"},
		{"host ipc", func(p *corev1.Pod) { p.Spec.HostIPC = true }, "shares a host namespace"},
		{"env", func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "K", Value: "v"}} }, "env, envFrom"},
		{"envFrom", func(p *corev1.Pod) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}}}}
		}, "env, envFrom"},
		{"volume mount", func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/v"}}
		}, "env, envFrom"},
		{"token unset", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil }, "service account token"},
		{"token on", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = &on }, "service account token"},
		{"service links unset", func(p *corev1.Pod) { p.Spec.EnableServiceLinks = nil }, "service link"},
		{"service links on", func(p *corev1.Pod) { p.Spec.EnableServiceLinks = &on }, "service link"},
		{"run label", func(p *corev1.Pod) { p.Labels["wardyn.run-id"] = "r" }, "run label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := clean()
			tc.edit(p)
			if err := placeholderHoldsNothing(p, image); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
