// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errClassPlacement is a pinned RuntimeClass whose scheduling cannot keep its pods on its own nodes, or
// fights the runner's own placement.
var errClassPlacement = errors.New("k8s: refusing to boot: a pinned RuntimeClass's scheduling does not fit the sandbox placement")

// admittedPlacement is an agent pod's placement as RuntimeClass admission leaves it, and the class's fixed
// overhead, which the scheduler and a quota add to the pod's own requests.
type admittedPlacement struct {
	Placement
	overhead corev1.ResourceList
}

// classPlacement is the agent pod's placement for class: the runner's placement run through
// runtimeClassAdmitted on a pod that carries only that and the pinned RuntimeClass. CC1, and a class
// with no pin, is the runner's placement with no overhead.
func (d *Driver) classPlacement(ctx context.Context, class types.ConfinementClass) (admittedPlacement, error) {
	out := admittedPlacement{Placement: d.placement}
	name := d.cfg.ConfinementRuntimes[class]
	if class == "" || class == types.CC1 || name == "" {
		return out, nil
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{RuntimeClassName: &name,
		NodeSelector: maps.Clone(d.placement.NodeSelector), Tolerations: slices.Clone(d.placement.Tolerations)}}
	admitted, err := d.runtimeClassAdmitted(ctx, pod)
	if err != nil {
		return out, err
	}
	out.NodeSelector, out.Tolerations, out.overhead = admitted.Spec.NodeSelector, admitted.Spec.Tolerations, admitted.Spec.Overhead
	return out, nil
}

// podPlacement is pl joined with what pod carries: its node selector (its value wins on a key both set),
// its tolerations and its affinity. The apiserver's copy of a pod holds the runner's placement and, for an
// agent pod, its RuntimeClass's scheduling; pl covers a pod read back without a spec.
func podPlacement(pl Placement, pod *corev1.Pod) Placement {
	if pod == nil {
		return pl
	}
	out := pl
	out.NodeSelector = maps.Clone(pl.NodeSelector)
	if out.NodeSelector == nil && len(pod.Spec.NodeSelector) > 0 {
		out.NodeSelector = map[string]string{}
	}
	maps.Copy(out.NodeSelector, pod.Spec.NodeSelector)
	out.Tolerations = append(slices.Clone(pl.Tolerations), pod.Spec.Tolerations...)
	if pod.Spec.Affinity != nil {
		out.Affinity = pod.Spec.Affinity
	}
	return out
}

// withOverhead is a pod's requests and limits with its RuntimeClass's overhead counted as Kubernetes counts
// it: added to every request the overhead names, whether or not the pod sets one, and to a limit only where
// the pod sets that limit. A limit the pod does not set is not one the overhead creates.
func withOverhead(req, lim, overhead corev1.ResourceList) (corev1.ResourceList, corev1.ResourceList) {
	if len(overhead) == 0 {
		return req, lim
	}
	outReq, outLim := req.DeepCopy(), lim.DeepCopy()
	if outReq == nil {
		outReq = corev1.ResourceList{}
	}
	for name, q := range overhead {
		have := outReq[name]
		have.Add(q)
		outReq[name] = have
		if l, ok := outLim[name]; ok {
			l.Add(q)
			outLim[name] = l
		}
	}
	return outReq, outLim
}

// checkClassPlacement refuses to boot when a pinned RuntimeClass cannot keep its pods on its own nodes or
// fights the runner's placement (k8s.sandbox.*, with the architecture selector in runner mode):
//   - scheduling tolerations with no node selector (a pool is configured and its taint tolerated) let the
//     class's pods go to any node;
//   - a node-selector key the placement sets to another value leaves no node for the class's pods;
//   - a placement toleration that tolerates a taint the class tolerates (tolerationsOverlap) lets the
//     proxy, the canary and runs of other classes onto the class's nodes.
//
// It returns each pinned class's node selector, for readUntaintedPools, and the pinned classes whose RuntimeClass
// sets no scheduling at all: no pool is configured, so their pods run on any node with the runtime, which boot
// allows and setup status warns of. A class that does not exist is left to resolveRuntimeClassName, which
// refuses each run of it; one that cannot be read refuses boot.
func (d *Driver) checkClassPlacement(ctx context.Context) (map[types.ConfinementClass]map[string]string, []types.ConfinementClass, error) {
	selectors := map[types.ConfinementClass]map[string]string{}
	var anyNode []types.ConfinementClass
	for _, c := range []types.ConfinementClass{types.CC2, types.CC3} {
		name := d.cfg.ConfinementRuntimes[c]
		if name == "" {
			continue
		}
		rc, err := d.clientset.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("k8s: refusing to boot: read the %s RuntimeClass %q: %w", c, name, err)
		}
		if rc.Scheduling == nil || (len(rc.Scheduling.NodeSelector) == 0 && len(rc.Scheduling.Tolerations) == 0) {
			anyNode = append(anyNode, c)
			continue
		}
		if len(rc.Scheduling.NodeSelector) == 0 {
			return nil, nil, fmt.Errorf("%w: RuntimeClass %q (%s) tolerates a taint but has no scheduling.nodeSelector, so %s pods could be scheduled on any node, not only on the pool. "+
				"Add scheduling.nodeSelector naming the pool's nodes. If the class is meant for every node, remove the tolerations, or set a selector every node carries, such as kubernetes.io/os: linux.",
				errClassPlacement, name, c, c)
		}
		for _, k := range slices.Sorted(maps.Keys(rc.Scheduling.NodeSelector)) {
			want := rc.Scheduling.NodeSelector[k]
			if have, set := d.placement.NodeSelector[k]; set && have != want {
				fix := "Remove " + k + " from k8s.sandbox.nodeSelector, or change the RuntimeClass."
				if k == corev1.LabelArchStable {
					fix = "Set sandbox.arch to " + want + ", or change the RuntimeClass."
				}
				return nil, nil, fmt.Errorf("%w: RuntimeClass %q (%s) selects %s=%s and the sandbox placement selects %s=%s, so no %s pod could be scheduled. %s",
					errClassPlacement, name, c, k, want, k, have, c, fix)
			}
		}
		for _, tol := range rc.Scheduling.Tolerations {
			if i := slices.IndexFunc(d.placement.Tolerations, func(t corev1.Toleration) bool { return tolerationsOverlap(t, tol) }); i >= 0 {
				return nil, nil, fmt.Errorf("%w: k8s.sandbox.tolerations has a toleration (key %q) that also tolerates a taint RuntimeClass %q (%s) tolerates, so the proxy, the canary and runs of other classes could land on that class's nodes. "+
					"Remove it from k8s.sandbox.tolerations, or narrow its key, value or effect: the RuntimeClass adds its own toleration to %s pods.",
					errClassPlacement, d.placement.Tolerations[i].Key, name, c, c)
			}
		}
		selectors[c] = rc.Scheduling.NodeSelector
	}
	return selectors, anyNode, nil
}

// tolerationsOverlap reports whether some taint is tolerated by both a and b, by the Kubernetes rule: the key
// (an empty key with Exists matches every key), the value and the effect (an empty effect matches every
// effect). Values are compared only when both operators are Equal (or empty): Exists matches every value, and
// a comparison operator (Lt, Gt) or an unknown one counts as overlapping, so the check fails closed.
func tolerationsOverlap(a, b corev1.Toleration) bool {
	every := func(t corev1.Toleration) bool { return t.Key == "" && t.Operator == corev1.TolerationOpExists }
	if !every(a) && !every(b) && a.Key != b.Key {
		return false
	}
	equal := func(t corev1.Toleration) bool { return t.Operator == "" || t.Operator == corev1.TolerationOpEqual }
	if equal(a) && equal(b) && a.Value != b.Value {
		return false
	}
	return a.Effect == "" || b.Effect == "" || a.Effect == b.Effect
}

// readUntaintedPools is each pinned class whose node selector picks at least one node that the runner's own
// placement (the proxy, the canary and CC1 runs) may also use: a pool that is labelled and not tainted,
// so general pods can land on it. Read once at boot and only with the node read on; nil when the nodes
// cannot be read, so the setup row stays silent rather than guess.
func (d *Driver) readUntaintedPools(ctx context.Context, selectors map[types.ConfinementClass]map[string]string) []types.ConfinementClass {
	if !d.cfg.ReadNodes || len(selectors) == 0 {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, fitReadTimeout)
	defer cancel()
	nodes, state := d.cachedNodes(rctx)
	if state != runner.ReadOK {
		return nil
	}
	anyTaint := func(corev1.Taint) bool { return true }
	var out []types.ConfinementClass
	for _, c := range []types.ConfinementClass{types.CC2, types.CC3} {
		pool := Placement{NodeSelector: selectors[c]}
		if len(pool.NodeSelector) > 0 && slices.ContainsFunc(nodes, func(n nodeInfo) bool {
			return n.placementOK(pool, anyTaint) && n.placementOK(d.placement, transientTaint)
		}) {
			out = append(out, c)
		}
	}
	return out
}
