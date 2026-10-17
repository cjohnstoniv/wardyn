// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Measure implements runner.Measurer: the runs namespace's ResourceQuota hard requests (the quota
// CheckFit reads), and where a quota sets none of an amount, the nodes' allocatable when the operator
// granted the node read. An amount neither says is 0, never a guess; when any amount is 0 the error
// says which read could not be made, with the amounts it did get.
func (d *Driver) Measure(ctx context.Context) (types.RunnerAmounts, error) {
	ctx, cancel := context.WithTimeout(ctx, fitReadTimeout)
	defer cancel()
	var why []string
	quotas, err := d.clientset.CoreV1().ResourceQuotas(d.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		quotas = &corev1.ResourceQuotaList{}
		why = append(why, fmt.Sprintf("ResourceQuota list in %s: %v", d.cfg.Namespace, err))
	} else if len(quotas.Items) == 0 && !d.cfg.ReadNodes {
		why = append(why, fmt.Sprintf("no ResourceQuota in %s and k8s.readNodes is off", d.cfg.Namespace))
	}
	var nodes []corev1.ResourceList
	if d.cfg.ReadNodes {
		read, state := d.cachedNodes(ctx)
		if state != runner.ReadOK {
			why = append(why, fmt.Sprintf("node list: %s", state))
		}
		for _, n := range read {
			nodes = append(nodes, n.allocatable)
		}
	}
	m := measureCluster(quotas.Items, nodes)
	if err == nil && len(quotas.Items) > 0 && !d.cfg.ReadNodes {
		if unset := unsetQuotaNames(m); len(unset) > 0 {
			why = append(why, fmt.Sprintf("no ResourceQuota in %s sets %s and k8s.readNodes is off", d.cfg.Namespace, strings.Join(unset, ", ")))
		}
	}
	if len(why) > 0 && (m.CPUMillis <= 0 || m.MemoryMiB <= 0 || m.DiskMiB <= 0) {
		return m, errors.New(strings.Join(why, "; "))
	}
	return m, nil
}

// measureCluster is each amount from the tightest quota that sets it, else the nodes' sum.
func measureCluster(quotas []corev1.ResourceQuota, nodes []corev1.ResourceList) types.RunnerAmounts {
	amount := func(quotaName, nodeName corev1.ResourceName, scale func(resource.Quantity) int64) int64 {
		var best int64
		for _, q := range quotas {
			if v, ok := q.Spec.Hard[quotaName]; ok && (best == 0 || scale(v) < best) {
				best = scale(v)
			}
		}
		if best > 0 {
			return best
		}
		for _, n := range nodes {
			if v, ok := n[nodeName]; ok {
				best += scale(v)
			}
		}
		return best
	}
	milli := func(q resource.Quantity) int64 { return q.MilliValue() }
	mib := func(q resource.Quantity) int64 { return q.Value() >> 20 }
	return types.RunnerAmounts{
		CPUMillis: amount(corev1.ResourceRequestsCPU, corev1.ResourceCPU, milli),
		MemoryMiB: amount(corev1.ResourceRequestsMemory, corev1.ResourceMemory, mib),
		DiskMiB:   amount(corev1.ResourceRequestsEphemeralStorage, corev1.ResourceEphemeralStorage, mib),
	}
}

// unsetQuotaNames names the ResourceQuota requests that measured as 0.
func unsetQuotaNames(m types.RunnerAmounts) []string {
	var out []string
	if m.CPUMillis <= 0 {
		out = append(out, string(corev1.ResourceRequestsCPU))
	}
	if m.MemoryMiB <= 0 {
		out = append(out, string(corev1.ResourceRequestsMemory))
	}
	if m.DiskMiB <= 0 {
		out = append(out, string(corev1.ResourceRequestsEphemeralStorage))
	}
	return out
}
