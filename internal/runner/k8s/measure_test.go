// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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
