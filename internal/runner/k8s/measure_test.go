// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A cluster's measure is each amount from the tightest quota that sets it, else the nodes' allocatable
// summed; an amount neither says is 0, never a guess.
func TestMeasureCluster(t *testing.T) {
	quota := func(cpu, mem string) corev1.ResourceQuota {
		return corev1.ResourceQuota{Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse(cpu), corev1.ResourceRequestsMemory: resource.MustParse(mem)}}}
	}
	nodes := []corev1.ResourceList{
		{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("100Gi")},
		{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("100Gi")},
	}
	got := measureCluster([]corev1.ResourceQuota{quota("12", "48Gi"), quota("6", "64Gi")}, nodes)
	if want := (types.RunnerAmounts{CPUMillis: 6000, MemoryMiB: 48 * 1024, DiskMiB: 200 * 1024}); got != want {
		t.Fatalf("quotas and nodes = %+v, want %+v", got, want)
	}
	if got := measureCluster(nil, nil); got != (types.RunnerAmounts{}) {
		t.Fatalf("nothing readable = %+v, want all unknown", got)
	}
}

// A quota that sets an amount to zero is that amount's measure, in either order with a positive quota,
// and the nodes do not replace it.
func TestMeasureClusterKeepsAnExplicitZeroQuota(t *testing.T) {
	cpu := func(v string) corev1.ResourceQuota {
		return corev1.ResourceQuota{Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(v)}}}
	}
	nodes := []corev1.ResourceList{{corev1.ResourceCPU: resource.MustParse("4")}}
	for name, quotas := range map[string][]corev1.ResourceQuota{
		"zero":             {cpu("0")},
		"zero then 6":      {cpu("0"), cpu("6")},
		"6 then zero":      {cpu("6"), cpu("0")},
		"zero beside none": {cpu("0"), {}},
	} {
		if got := measureCluster(quotas, nodes); got.CPUMillis != 0 {
			t.Errorf("%s: cpu = %d, want 0", name, got.CPUMillis)
		}
	}
	if got := measureCluster([]corev1.ResourceQuota{{}}, nodes); got.CPUMillis != 4000 {
		t.Errorf("a quota that does not set cpu: cpu = %d, want the nodes' 4000", got.CPUMillis)
	}
}

// Measure says which read it could not make whenever an amount is left unknown, and says nothing when
// every amount is known.
func TestMeasureSaysWhyAnAmountIsUnknown(t *testing.T) {
	ctx := context.Background()
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: testNamespace}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("8"), corev1.ResourceRequestsMemory: resource.MustParse("16Gi"),
		corev1.ResourceRequestsEphemeralStorage: resource.MustParse("100Gi")}}}
	forbid := func(resource string) func(clienttesting.Action) (bool, runtime.Object, error) {
		return func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("no list"))
		}
	}
	for name, tc := range map[string]struct {
		cfg    Config
		setup  func(*fake.Clientset)
		want   string // a part of the reason; empty: no error
		wantMi int64  // MemoryMiB measured with it
	}{
		"quota list forbidden, nodes off": {cfg: Config{}, setup: func(cs *fake.Clientset) { cs.PrependReactor("list", "resourcequotas", forbid("resourcequotas")) },
			want: "ResourceQuota list in " + testNamespace + ": "},
		"no quota, nodes off": {cfg: Config{}, setup: func(*fake.Clientset) {},
			want: "no ResourceQuota in " + testNamespace + " and k8s.readNodes is off"},
		"no quota, node list forbidden": {cfg: Config{ReadNodes: true}, setup: func(cs *fake.Clientset) { cs.PrependReactor("list", "nodes", forbid("nodes")) },
			want: "node list: forbidden"},
		"a quota that sets only CPU and memory, nodes off": {cfg: Config{}, setup: func(cs *fake.Clientset) {
			cpuMem := quota.DeepCopy()
			delete(cpuMem.Spec.Hard, corev1.ResourceRequestsEphemeralStorage)
			seed(t, cs, cpuMem)
		}, want: "no ResourceQuota in " + testNamespace + " sets requests.ephemeral-storage and k8s.readNodes is off", wantMi: 16 * 1024},
		"a quota that sets everything": {cfg: Config{}, setup: func(cs *fake.Clientset) { seed(t, cs, quota) }, wantMi: 16 * 1024},
	} {
		t.Run(name, func(t *testing.T) {
			d, cs := newTestDriver(t, tc.cfg)
			tc.setup(cs)
			m, err := d.Measure(ctx)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Measure = %+v, %v; want no error", m, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Measure = %+v, %v; want an error containing %q", m, err, tc.want)
			case m.MemoryMiB != tc.wantMi:
				t.Fatalf("MemoryMiB = %d, want %d", m.MemoryMiB, tc.wantMi)
			}
		})
	}
}
