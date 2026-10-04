// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// fitReadTimeout bounds the two list calls CheckFit makes, so a slow apiserver cannot hold a
// create or a preflight past it.
const fitReadTimeout = 5 * time.Second

var _ runner.FitChecker = (*Driver)(nil)

// podDemand is what one pod of a run asks the cluster for: the numbers a quota counts and the
// scheduler places by, and the two attributes a quota scope selects on.
type podDemand struct {
	requests, limits corev1.ResourceList
	priorityClass    string
	// terminating is "the pod carries activeDeadlineSeconds", which selects the Terminating and
	// NotTerminating quota scopes: both run pods carry one exactly when WARDYN_RUN_MAX_AGE is set
	// (activeDeadline, sandbox.go).
	terminating bool
}

// runPods is the run's two pods as the quota and the scheduler see them: the agent pod (its
// requests and limits, the ratio applied) and the proxy pod. Counting the agent alone would
// under-count every run by the proxy's envelope.
func (d *Driver) runPods(res runner.Resources) []podDemand {
	agent, proxy := resourceRequirements(res), proxyResources(false)
	pc, term := d.placement.PriorityClassName, d.activeDeadline() != nil
	return []podDemand{
		{requests: agent.Requests, limits: agent.Limits, priorityClass: pc, terminating: term},
		{requests: proxy.Requests, limits: proxy.Limits, priorityClass: pc, terminating: term},
	}
}

// CheckFit implements runner.FitChecker. The quota half is a refusal's evidence (the caller
// decides); the node half is advisory and runs only when the operator granted the node read.
func (d *Driver) CheckFit(ctx context.Context, res runner.Resources) (runner.Fit, error) {
	ctx, cancel := context.WithTimeout(ctx, fitReadTimeout)
	defer cancel()
	pods := d.runPods(res)
	fit := runner.Fit{Nodes: runner.ReadSkipped}

	// A list, never a get: every quota that applies to a pod counts against it, and a get needs a name.
	quotas, err := d.clientset.CoreV1().ResourceQuotas(d.cfg.Namespace).List(ctx, metav1.ListOptions{})
	fit.Quotas = readState(err)
	if err == nil {
		fit.Quota = quotaFits(quotas.Items, pods)
	}

	if d.cfg.ReadNodes {
		nodes, state := d.cachedNodes(ctx)
		fit.Nodes = state
		if state == runner.ReadOK {
			fit.NodeShortfall = nodeShortfall(nodes, d.placement, pods)
		}
	}
	return fit, nil
}

// readState classes a list error: forbidden is RBAC, anything else is the cluster being
// unreachable or unwell.
func readState(err error) runner.ReadState {
	switch {
	case err == nil:
		return runner.ReadOK
	case apierrors.IsForbidden(err):
		return runner.ReadForbidden
	}
	return runner.ReadUnavailable
}

// quotaFits keeps the quotas that apply to at least one of the pods and, for each, the hard
// limits the run touches. Sorted by name, so the answer does not depend on list order.
func quotaFits(quotas []corev1.ResourceQuota, pods []podDemand) []runner.QuotaFit {
	var out []runner.QuotaFit
	for i := range quotas {
		if axes := quotaAxes(&quotas[i], pods); len(axes) > 0 {
			out = append(out, runner.QuotaFit{Name: quotas[i].Name, Axes: axes})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// quotaAxes is one quota's hard limits that some applicable pod adds to. A limit the run adds
// nothing to is left out (a quota on requests.ephemeral-storage says nothing about a run that
// asks for none; the apiserver's LimitRange default, which this role cannot see, may decide).
// ponytail: RuntimeClass pod overhead also counts against a quota and is not added here.
func quotaAxes(q *corev1.ResourceQuota, pods []podDemand) []runner.QuotaAxis {
	hard, used := q.Status.Hard, q.Status.Used
	if len(hard) == 0 {
		hard = q.Spec.Hard
	}
	var axes []runner.QuotaAxis
	for name, h := range hard {
		var need int64
		for _, p := range pods {
			if quotaAppliesTo(q, p) {
				need += podUse(p, name)
			}
		}
		if need == 0 {
			continue
		}
		hv, uv := quantityOf(name, h), quantityOf(name, used[name])
		axes = append(axes, runner.QuotaAxis{Key: string(name), Need: need, Left: max(hv-uv, 0), Hard: hv})
	}
	sort.Slice(axes, func(i, j int) bool { return axes[i].Key < axes[j].Key })
	return axes
}

// podUse is what one pod adds to the quota key: 0 for a key the run does not touch. A bare
// "cpu", "memory" or "ephemeral-storage" key is the requests figure, as the quota system defines it.
func podUse(p podDemand, key corev1.ResourceName) int64 {
	switch key {
	case corev1.ResourcePods, "count/pods":
		return 1
	case corev1.ResourceCPU, corev1.ResourceRequestsCPU:
		return quantityOf(key, *p.requests.Cpu())
	case corev1.ResourceMemory, corev1.ResourceRequestsMemory:
		return quantityOf(key, *p.requests.Memory())
	case corev1.ResourceEphemeralStorage, corev1.ResourceRequestsEphemeralStorage:
		return quantityOf(key, *p.requests.StorageEphemeral())
	case corev1.ResourceLimitsCPU:
		return quantityOf(key, *p.limits.Cpu())
	case corev1.ResourceLimitsMemory:
		return quantityOf(key, *p.limits.Memory())
	case corev1.ResourceLimitsEphemeralStorage:
		return quantityOf(key, *p.limits.StorageEphemeral())
	}
	return 0
}

// quantityOf is a quantity in the unit QuotaAxis uses for that key: millicores for a CPU key,
// else the plain value (bytes, or a count).
func quantityOf(key corev1.ResourceName, q resource.Quantity) int64 {
	if key == corev1.ResourceCPU || key == corev1.ResourceRequestsCPU || key == corev1.ResourceLimitsCPU {
		return q.MilliValue()
	}
	return q.Value()
}

// quotaAppliesTo is the quota's own scope rule: every listed scope and every scopeSelector
// expression must match the pod. A scope this substrate cannot match (CrossNamespacePodAffinity,
// VolumeAttributesClass) does not apply: the apiserver, not this advisory, is the authority on it.
func quotaAppliesTo(q *corev1.ResourceQuota, p podDemand) bool {
	for _, s := range q.Spec.Scopes {
		if !scopeMatches(s, corev1.ScopeSelectorOpExists, nil, p) {
			return false
		}
	}
	if sel := q.Spec.ScopeSelector; sel != nil {
		for _, e := range sel.MatchExpressions {
			if !scopeMatches(e.ScopeName, e.Operator, e.Values, p) {
				return false
			}
		}
	}
	return true
}

func scopeMatches(scope corev1.ResourceQuotaScope, op corev1.ScopeSelectorOperator, values []string, p podDemand) bool {
	if scope == corev1.ResourceQuotaScopePriorityClass {
		switch op {
		case corev1.ScopeSelectorOpIn:
			return slices.Contains(values, p.priorityClass)
		case corev1.ScopeSelectorOpNotIn:
			return !slices.Contains(values, p.priorityClass)
		case corev1.ScopeSelectorOpExists:
			return p.priorityClass != ""
		case corev1.ScopeSelectorOpDoesNotExist:
			return p.priorityClass == ""
		}
		return false
	}
	var has bool
	switch scope {
	case corev1.ResourceQuotaScopeTerminating:
		has = p.terminating
	case corev1.ResourceQuotaScopeNotTerminating:
		has = !p.terminating
	case corev1.ResourceQuotaScopeBestEffort:
		has = false // every run pod sets requests and limits
	case corev1.ResourceQuotaScopeNotBestEffort:
		has = true
	default:
		return false
	}
	switch op {
	case corev1.ScopeSelectorOpExists:
		return has
	case corev1.ScopeSelectorOpDoesNotExist:
		return !has
	}
	return false
}
