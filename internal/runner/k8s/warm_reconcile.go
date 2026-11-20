// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// warmBackoff is how long a pool whose placeholder create failed waits before the next one.
const warmBackoff = 5 * time.Minute

// warmState is the reconciler's memory between passes: each pool's backoff, and the loop's stop function.
type warmState struct {
	mu      sync.Mutex
	backoff map[string]time.Time // pool name -> no creates before this time
	stop    func()
}

// warmBackedOff reports whether pool waits out a failed create.
func (d *Driver) warmBackedOff(pool string) bool {
	d.warm.mu.Lock()
	defer d.warm.mu.Unlock()
	return d.now().Before(d.warm.backoff[pool])
}

// warmBackOff holds pool's creates for warmBackoff.
func (d *Driver) warmBackOff(pool string) {
	d.warm.mu.Lock()
	defer d.warm.mu.Unlock()
	if d.warm.backoff == nil {
		d.warm.backoff = map[string]time.Time{}
	}
	d.warm.backoff[pool] = d.now().Add(warmBackoff)
}

// warmPlaceholderPod builds one idle placeholder: the pool's image, no settings, secrets, files or token, and the
// room a run of the pool's size takes (its agent requests plus a proxy's).
func (d *Driver) warmPlaceholderPod(ctx context.Context, p WarmPool, ns string, i int) (*corev1.Pod, error) {
	proxy := proxyResources(false)
	cpu, mem := resource.MustParse(p.CPU), resource.MustParse(p.Memory)
	cpu.Add(*proxy.Requests.Cpu())
	mem.Add(*proxy.Requests.Memory())
	// requests equal limits: the pod is Guaranteed, so it holds exactly a run's room
	r := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu.DeepCopy(), corev1.ResourceMemory: mem.DeepCopy()},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu.DeepCopy(), corev1.ResourceMemory: mem.DeepCopy()},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: warmPodName(p.Name, i), Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			TerminationGracePeriodSeconds: int64Ptr(1),
			DNSPolicy:                     corev1.DNSNone,
			DNSConfig:                     &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			Containers: []corev1.Container{{
				Name:            "warm",
				Image:           p.Image,
				Command:         []string{"sh", "-c", "while :; do sleep 3600 || exit; done"},
				SecurityContext: agentSecurityContext(),
				Resources:       r,
			}},
		},
	}
	d.placement.apply(pod, uuid.Nil, componentWarm, map[string]string{labelWarmPool: p.Name})
	delete(pod.Labels, labelRun) // a placeholder belongs to no run, so the run sweep and teardown never select it
	if d.cfg.WarmPriorityClass != "" {
		pod.Spec.PriorityClassName = d.cfg.WarmPriorityClass
	}
	name, _, err := d.resolveRuntimeClassName(ctx, types.ConfinementClass(p.Class))
	if err != nil {
		return nil, err
	}
	if name != "" {
		pod.Spec.RuntimeClassName = &name
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	if err := placeholderHoldsNothing(pod, p.Image); err != nil {
		return nil, err
	}
	return pod, nil
}

// warmDenyPolicyObject is the deny-all policy over every placeholder: no ingress and no egress.
func warmDenyPolicyObject(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: warmDenyPolicy, Namespace: ns, Labels: map[string]string{labelManaged: "true", labelComponent: componentWarm}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: componentWarm}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		},
	}
}

// readWarmDeny is the NetworkPolicy named warmDenyPolicy in ns, or nil. It reads by name, never by label, so a same-name
// policy without the warm label is seen too. The Role holds no get on NetworkPolicies, so this is a list.
func (d *Driver) readWarmDeny(ctx context.Context, ns string) (*networkingv1.NetworkPolicy, error) {
	list, err := d.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + warmDenyPolicy})
	if err != nil {
		return nil, fmt.Errorf("k8s: warm pools: read the deny policy: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Name == warmDenyPolicy {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// warmDenyHolds reports whether np is exactly the deny-all policy over every placeholder: the warm component label as
// its whole selector (an extra label or expression would leave placeholders unselected, and an unselected pod is
// default-allow), both policy types, and no rule. A policy being deleted is not the deny policy: it is gone once its
// finalizers finish.
func warmDenyHolds(np *networkingv1.NetworkPolicy) bool {
	sel, pt := np.Spec.PodSelector, np.Spec.PolicyTypes
	return np.DeletionTimestamp == nil &&
		maps.Equal(sel.MatchLabels, map[string]string{labelComponent: componentWarm}) && len(sel.MatchExpressions) == 0 &&
		len(pt) == 2 && slices.Contains(pt, networkingv1.PolicyTypeIngress) && slices.Contains(pt, networkingv1.PolicyTypeEgress) &&
		len(np.Spec.Ingress) == 0 && len(np.Spec.Egress) == 0
}

// ensureWarmDeny returns nil only when the deny policy stands exactly as warmDenyPolicyObject makes it.
// SECURITY: an unselected pod is default-allow, so placeholders go before the policy is fixed, and none is made until it stands.
func (d *Driver) ensureWarmDeny(ctx context.Context, ns string, placeholders []corev1.Pod) error {
	np, err := d.readWarmDeny(ctx, ns)
	if err != nil {
		return err
	}
	if np != nil && warmDenyHolds(np) {
		return nil
	}
	return d.replaceWarmDeny(ctx, ns, np, placeholders)
}

// replaceWarmDeny deletes the placeholders and waits until they are gone, deletes stale (the wrong policy this pass
// read, or nil), creates the deny policy, and reads it back.
// SECURITY: the delete names stale's UID and resourceVersion, so a replica acting on an old read never removes a newer,
// correct policy that another replica's placeholders rely on.
func (d *Driver) replaceWarmDeny(ctx context.Context, ns string, stale *networkingv1.NetworkPolicy, placeholders []corev1.Pod) error {
	for i := range placeholders {
		d.removeWarmPod(ctx, ns, &placeholders[i])
	}
	// a pod still stopping is still running; it must be gone before the policy that selects it changes
	if err := d.waitPodsGone(ctx, ns, metav1.ListOptions{LabelSelector: labelComponent + "=" + componentWarm}, time.Minute); err != nil {
		return err
	}
	nps := d.clientset.NetworkingV1().NetworkPolicies(ns)
	if stale != nil {
		pre := metav1.Preconditions{UID: &stale.UID, ResourceVersion: &stale.ResourceVersion}
		if err := nps.Delete(ctx, warmDenyPolicy, metav1.DeleteOptions{Preconditions: &pre}); err != nil && !isNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("k8s: warm pools: delete the wrong deny policy: %w", err)
		}
	}
	if _, err := nps.Create(ctx, warmDenyPolicyObject(ns), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("k8s: warm pools: create the deny policy: %w", err)
	}
	np, err := d.readWarmDeny(ctx, ns)
	if err != nil {
		return err
	}
	if np == nil || !warmDenyHolds(np) {
		return fmt.Errorf("k8s: warm pools: %s in %s is not the deny-all policy; no placeholder is made", warmDenyPolicy, ns)
	}
	return nil
}

// warmLabelsHold reports whether stored keeps the two labels everything else finds a placeholder by, as intended (the
// pod warmPlaceholderPod built) has them: the deny policy and the reconcile list select the component label, and each
// pool takes its pods by the pool label. A placeholder that lost either is outside the deny policy (an unselected pod is
// default-allow) or outside every later pass.
func warmLabelsHold(stored, intended *corev1.Pod) error {
	for _, k := range []string{labelComponent, labelWarmPool} {
		if stored.Labels[k] != intended.Labels[k] {
			return fmt.Errorf("label %s is %q, not %q", k, stored.Labels[k], intended.Labels[k])
		}
	}
	return nil
}

// createWarmPod creates one placeholder and checks the pod the API server stored, after any webhook changed it.
// SECURITY: a placeholder that holds anything, or lost a label it is found by, is deleted at once, by UID alone, and its
// pool backs off; it may have started before the delete, and without its labels outside the deny policy.
func (d *Driver) createWarmPod(ctx context.Context, ns string, p WarmPool, pod *corev1.Pod) error {
	pods := d.clientset.CoreV1().Pods(ns)
	created, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// the name was free among the listed placeholders, so the pod that holds it is outside the list: another
		// replica's create a moment ago, or a placeholder whose labels were changed. Judge the stored pod either way.
		created, err = pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if isNotFound(err) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	err = placeholderHoldsNothing(created, p.Image)
	if err == nil {
		err = warmLabelsHold(created, pod)
	}
	if err != nil {
		d.removeWarmPod(ctx, ns, created)
		return fmt.Errorf("k8s: warm pools: pool %s: the stored placeholder %s was changed and is deleted: %w", p.Name, created.Name, err)
	}
	return nil
}

// removeWarmPod deletes a placeholder that must go whatever its version: one that holds something, or any placeholder
// while the deny policy is wrong. The scheduler and the kubelet rewrite a new pod within moments, so a version
// precondition would spare it; the UID alone names the pod.
func (d *Driver) removeWarmPod(ctx context.Context, ns string, pod *corev1.Pod) {
	grace := int64(1) // never 0: a zero grace removes the object before its container stops
	err := d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace, Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if err != nil && !isNotFound(err) && !apierrors.IsConflict(err) {
		slog.Warn("wardynd: k8s substrate: could not delete a warm placeholder", slog.String("namespace", ns), slog.String("pod", pod.Name), slog.String("error", err.Error()))
	}
}

// deleteWarmPod deletes a placeholder bound to the UID and resourceVersion this pass read: one that changed since is
// spared, and the next pass judges it again.
func (d *Driver) deleteWarmPod(ctx context.Context, ns string, pod *corev1.Pod) {
	grace := int64(1) // grace 1, never 0: a zero grace removes the object before its container stops
	pre := metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}
	err := d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace, Preconditions: &pre})
	if err != nil && !isNotFound(err) && !apierrors.IsConflict(err) {
		slog.Warn("wardynd: k8s substrate: could not delete a warm placeholder", slog.String("namespace", ns), slog.String("pod", pod.Name), slog.String("error", err.Error()))
	}
}

// listWarmPods lists the placeholders in ns.
func (d *Driver) listWarmPods(ctx context.Context, ns string) ([]corev1.Pod, error) {
	list, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelComponent + "=" + componentWarm})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// reconcileWarmPools makes one pass: the default runs namespace ends with the configured placeholders, behind an exact
// deny-all policy. Errors are logged and end the pass.
func (d *Driver) reconcileWarmPools(ctx context.Context) {
	ns := d.defaultNamespace()
	pods, err := d.listWarmPods(ctx, ns)
	if err != nil {
		slog.Warn("k8s: warm pools: list the placeholders", slog.String("error", err.Error()))
		return
	}
	if len(d.cfg.WarmPools) == 0 {
		for i := range pods {
			d.deleteWarmPod(ctx, ns, &pods[i])
		}
		return
	}
	configured := map[string]bool{}
	for _, p := range d.cfg.WarmPools {
		configured[p.Name] = true
	}
	for i := range pods {
		if !configured[pods[i].Labels[labelWarmPool]] {
			d.deleteWarmPod(ctx, ns, &pods[i])
		}
	}
	// the policy is checked whatever the canary says: reading it needs no proven CNI; the verdict only stops creates
	refuse := d.netPolRefusal(ns)
	if err := d.ensureWarmDeny(ctx, ns, pods); err != nil {
		slog.Warn("k8s: warm pools: the deny policy is not in place", slog.String("error", err.Error()))
		return
	}
	pods, err = d.listWarmPods(ctx, ns)
	if err != nil {
		slog.Warn("k8s: warm pools: list the placeholders", slog.String("error", err.Error()))
		return
	}
	for _, p := range d.cfg.WarmPools {
		var own []corev1.Pod
		for _, pod := range pods {
			if _, ok := warmIndex(pod.Name, p.Name); ok && pod.Labels[labelWarmPool] == p.Name {
				own = append(own, pod)
			}
		}
		d.reconcilePool(ctx, ns, p, own, refuse == nil)
	}
}

// reconcilePool brings one pool's placeholders to its size, with at most one create per pass.
func (d *Driver) reconcilePool(ctx context.Context, ns string, p WarmPool, pods []corev1.Pod, canCreate bool) {
	var used []int
	type liveRef struct {
		idx int
		pod *corev1.Pod
	}
	var live []liveRef
	for i := range pods {
		pod := &pods[i]
		idx, _ := warmIndex(pod.Name, p.Name)
		used = append(used, idx)
		if pod.DeletionTimestamp != nil {
			continue // not live, but its name is still taken
		}
		// a placeholder can gain an ephemeral container or a new image after create; one that holds anything goes
		if err := placeholderHoldsNothing(&pods[i], p.Image); err != nil {
			slog.Warn("k8s: warm pools: a placeholder holds something and is deleted", slog.String("pool", p.Name), slog.String("pod", pod.Name), slog.String("error", err.Error()))
			d.removeWarmPod(ctx, ns, pod)
			d.warmBackOff(p.Name)
			continue
		}
		if phase := pod.Status.Phase; phase == corev1.PodFailed || phase == corev1.PodSucceeded ||
			(phase == corev1.PodPending && d.now().Sub(pod.CreationTimestamp.Time) > d.cfg.StartTimeout) {
			d.deleteWarmPod(ctx, ns, pod)
			continue
		}
		live = append(live, liveRef{idx, pod})
	}
	slices.SortFunc(live, func(a, b liveRef) int { return b.idx - a.idx }) // highest index first
	for len(live) > p.Size {
		d.deleteWarmPod(ctx, ns, live[0].pod)
		live = live[1:]
	}
	if len(live) >= p.Size || !canCreate || d.warmBackedOff(p.Name) {
		return
	}
	pod, err := d.warmPlaceholderPod(ctx, p, ns, lowestFreeIndex(used))
	if err == nil {
		err = d.createWarmPod(ctx, ns, p, pod)
	}
	if err != nil {
		d.warmBackOff(p.Name)
		slog.Warn("k8s: warm pools: no placeholder was made", slog.String("pool", p.Name), slog.String("error", err.Error()))
	}
}
