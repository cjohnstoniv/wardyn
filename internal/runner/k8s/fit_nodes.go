// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// nodeCacheTTL is how long one node list answers every preflight and create. Node sizes change
// on autoscaling timescales; a list per keystroke of a New Run form would be a cluster-wide read each time.
const nodeCacheTTL = 30 * time.Second

// nodeInfo is the part of a Node the fit advisory reads, so the cache holds no full objects.
type nodeInfo struct {
	name        string
	labels      map[string]string
	taints      []corev1.Taint
	allocatable corev1.ResourceList
}

// nodeCache is the last node read, whatever its outcome: a forbidden answer is cached too, so a
// tenant without the grant does not re-ask on every check.
type nodeCache struct {
	mu    sync.Mutex
	at    time.Time
	nodes []nodeInfo
	state runner.ReadState
}

// cachedNodes returns the node list, read at most once per nodeCacheTTL. The lock is never
// held across the apiserver read, so a slow List cannot serialise every other check behind it;
// two callers racing an expired cache may each read once, and the later store wins.
func (d *Driver) cachedNodes(ctx context.Context) ([]nodeInfo, runner.ReadState) {
	c := &d.nodeCache
	c.mu.Lock()
	if c.state != "" && time.Since(c.at) < nodeCacheTTL {
		nodes, state := c.nodes, c.state
		c.mu.Unlock()
		return nodes, state
	}
	c.mu.Unlock()

	list, err := d.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	state := readState(err)
	var nodes []nodeInfo
	if err == nil {
		for i := range list.Items {
			n := &list.Items[i]
			nodes = append(nodes, nodeInfo{name: n.Name, labels: n.Labels, taints: n.Spec.Taints, allocatable: n.Status.Allocatable})
		}
	}
	c.mu.Lock()
	c.at, c.state, c.nodes = time.Now(), state, nodes
	c.mu.Unlock()
	return nodes, state
}

// nodeShortfall answers "is there any node at all this run's pods may go to and are small
// enough for": nil when every pod has one, else the largest pod request that has none. It
// compares requests to allocatable, which is size, not free capacity: pods of other namespaces
// are invisible to this role, so nothing here claims room, and the scheduler stays authoritative.
func nodeShortfall(nodes []nodeInfo, pl Placement, pods []podDemand) *runner.NodeShortfall {
	var short *runner.NodeShortfall
	for _, p := range pods {
		if slices.ContainsFunc(nodes, func(n nodeInfo) bool { return n.holds(p, pl) }) {
			continue
		}
		cand := runner.NodeShortfall{CPUMillis: p.requests.Cpu().MilliValue(), MemoryBytes: p.requests.Memory().Value()}
		if short == nil || cand.CPUMillis > short.CPUMillis || (cand.CPUMillis == short.CPUMillis && cand.MemoryBytes > short.MemoryBytes) {
			short = &cand
		}
	}
	return short
}

// holds reports whether the pod could be scheduled on n by placement and size alone: the node
// selector, the required node affinity, the taints the placement tolerates, and the requests
// against allocatable. Preferred affinity and every co-tenant effect are out of scope.
func (n nodeInfo) holds(p podDemand, pl Placement) bool {
	for k, v := range pl.NodeSelector {
		if got, ok := n.labels[k]; !ok || got != v {
			return false
		}
	}
	if !n.matchesRequiredAffinity(pl.Affinity) {
		return false
	}
	for _, t := range n.taints {
		if t.Effect != corev1.TaintEffectPreferNoSchedule && !tolerated(pl.Tolerations, t) {
			return false
		}
	}
	for name, want := range p.requests {
		if have, ok := n.allocatable[name]; ok && want.Cmp(have) > 0 {
			return false
		}
	}
	return true
}

func (n nodeInfo) matchesRequiredAffinity(aff *corev1.Affinity) bool {
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	return slices.ContainsFunc(terms, n.matchesTerm)
}

// matchesTerm: a term's expressions and fields are ANDed; an empty term matches no node, as in the scheduler.
func (n nodeInfo) matchesTerm(t corev1.NodeSelectorTerm) bool {
	if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
		return false
	}
	return matchesAll(t.MatchExpressions, labels.Set(n.labels)) &&
		matchesAll(t.MatchFields, labels.Set{"metadata.name": n.name})
}

var nodeSelectorOps = map[corev1.NodeSelectorOperator]selection.Operator{
	corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn,
	corev1.NodeSelectorOpExists: selection.Exists, corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
	corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan,
}

func matchesAll(reqs []corev1.NodeSelectorRequirement, set labels.Set) bool {
	for _, r := range reqs {
		req, err := labels.NewRequirement(r.Key, nodeSelectorOps[r.Operator], r.Values)
		if err != nil || !req.Matches(set) {
			return false
		}
	}
	return true
}

// tolerated is the scheduler's toleration rule for one taint (the Gt and Lt toleration
// operators are an alpha gate and are not honoured: such a toleration reads as not matching).
func tolerated(tols []corev1.Toleration, t corev1.Taint) bool {
	return slices.ContainsFunc(tols, func(tol corev1.Toleration) bool {
		if tol.Effect != "" && tol.Effect != t.Effect {
			return false
		}
		if tol.Key != "" && tol.Key != t.Key {
			return false
		}
		switch tol.Operator {
		case corev1.TolerationOpExists:
			return true
		case corev1.TolerationOpEqual, "":
			return tol.Key != "" && tol.Value == t.Value
		}
		return false
	})
}
