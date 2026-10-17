// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Startup refusals (design K1 §3.4), each named.
var (
	errRunnerAccessMissing    = errors.New("runner_access_missing")
	errRunnerForeignObjects   = errors.New("runner_foreign_objects")
	errKubeletBelowFloor      = errors.New("kubelet_below_floor")
	errRelaySourceRewritten   = errors.New("relay_source_rewritten")
	errRelayProbeInconclusive = errors.New("relay_probe_inconclusive")
)

// Clientset is the driver's API client, for the runner's own-namespace keystore and boot checks.
func (d *Driver) Clientset() kubernetes.Interface { return d.clientset }

// accessCheck is one verb the runner's RBAC must grant (design K1 §4).
type accessCheck struct {
	ns, group, resource, verb string
}

func (c accessCheck) String() string {
	r := c.resource
	if c.group != "" {
		r += "." + c.group
	}
	if c.ns == "" {
		return r + ":" + c.verb
	}
	return c.ns + "/" + r + ":" + c.verb
}

// runnerAccess lists every verb §4 grants: the server Role in the runs namespace plus pods: patch,
// secrets create/get/delete/deletecollection in runnerd's own namespace, and the ClusterRole's runtimeclasses: get
// and nodes: list.
func runnerAccess(runsNS, ownNS string) []accessCheck {
	var out []accessCheck
	add := func(ns, group, resource string, verbs ...string) {
		for _, v := range verbs {
			out = append(out, accessCheck{ns, group, resource, v})
		}
	}
	add(runsNS, "", "pods", "create", "get", "list", "delete", "deletecollection", "patch")
	add(runsNS, "", "pods/ephemeralcontainers", "update")
	add(runsNS, "", "pods/exec", "get", "create")
	add(runsNS, "", "pods/log", "get")
	add(runsNS, "", "secrets", "create", "delete", "deletecollection")
	add(runsNS, "networking.k8s.io", "networkpolicies", "create", "list", "delete", "deletecollection")
	add(runsNS, "", "events", "list")
	add(runsNS, "metrics.k8s.io", "pods", "list")
	add(runsNS, "", "resourcequotas", "list")
	add(ownNS, "", "secrets", "create", "get", "delete", "deletecollection")
	add("", "node.k8s.io", "runtimeclasses", "get")
	add("", "", "nodes", "list")
	return out
}

// CheckRunnerAccess asks a SelfSubjectAccessReview for every verb the runner needs and refuses boot
// naming each one not granted (§3.4 item 2).
func (d *Driver) CheckRunnerAccess(ctx context.Context, ownNS string) error {
	var missing []string
	for _, c := range runnerAccess(d.cfg.Namespace, ownNS) {
		sar := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: c.ns, Group: c.group, Resource: c.resource, Verb: c.verb},
		}}
		if sub, ok := strings.CutPrefix(c.resource, "pods/"); ok {
			sar.Spec.ResourceAttributes.Resource, sar.Spec.ResourceAttributes.Subresource = "pods", sub
		}
		res, err := d.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("%w: access review %s: %w", errRunnerAccessMissing, c, err)
		}
		if !res.Status.Allowed {
			missing = append(missing, c.String())
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: not granted: %s", errRunnerAccessMissing, strings.Join(missing, ", "))
	}
	return nil
}

// CheckForeignObjects refuses boot when the runs namespace holds wardyn.managed pods or
// NetworkPolicies of another runner, or of none (a server's), naming them (§3.4 item 3).
func (d *Driver) CheckForeignObjects(ctx context.Context) error {
	opts := metav1.ListOptions{LabelSelector: labelManaged + "=true"}
	var foreign []string
	pods, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("%w: list pods: %w", errRunnerForeignObjects, err)
	}
	for _, p := range pods.Items {
		if p.Labels[runner.LabelRunnerID] != d.cfg.RunnerID {
			foreign = append(foreign, "pod/"+p.Name)
		}
	}
	pols, err := d.clientset.NetworkingV1().NetworkPolicies(d.cfg.Namespace).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("%w: list networkpolicies: %w", errRunnerForeignObjects, err)
	}
	for _, p := range pols.Items {
		if p.Labels[runner.LabelRunnerID] != d.cfg.RunnerID {
			foreign = append(foreign, "networkpolicy/"+p.Name)
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%w: namespace %s holds objects another runner or a server created: %s", errRunnerForeignObjects, d.cfg.Namespace, strings.Join(foreign, ", "))
	}
	return nil
}

// kubeletFloor is the oldest kubelet minor a runner boots against (§3.4 item 6).
const kubeletFloorMinor = 31

// CheckKubeletFloor refuses boot when a schedulable node of arch reports a kubelet below 1.31, naming
// the nodes. A version that does not parse counts as below the floor.
func (d *Driver) CheckKubeletFloor(ctx context.Context, arch string) error {
	nodes, err := d.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/arch=" + arch})
	if err != nil {
		return fmt.Errorf("%w: list nodes: %w", errKubeletBelowFloor, err)
	}
	var low []string
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			continue
		}
		if !kubeletAtFloor(n.Status.NodeInfo.KubeletVersion) {
			low = append(low, n.Name+" ("+n.Status.NodeInfo.KubeletVersion+")")
		}
	}
	if len(low) > 0 {
		slices.Sort(low)
		return fmt.Errorf("%w: kubelet 1.%d or later is required: %s", errKubeletBelowFloor, kubeletFloorMinor, strings.Join(low, ", "))
	}
	return nil
}

func kubeletAtFloor(v string) bool {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil {
		return false
	}
	return major > 1 || (major == 1 && minor >= kubeletFloorMinor)
}

// ProxyRunByIP maps a relay connection's source address to the run whose live proxy pod (this
// runner's, Pending or Running) holds it. A miss is (Nil, false).
func (d *Driver) ProxyRunByIP(ctx context.Context, ip netip.Addr) (uuid.UUID, bool, error) {
	pods, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManaged + "=true," + labelComponent + "=" + componentProxy + "," + runner.LabelRunnerID + "=" + d.cfg.RunnerID,
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodPending {
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		got, perr := netip.ParseAddr(p.Status.PodIP)
		if perr != nil || got.Unmap() != ip.Unmap() {
			continue
		}
		id, ierr := uuid.Parse(p.Labels[labelRun])
		if ierr != nil {
			continue
		}
		return id, true, nil
	}
	return uuid.Nil, false, nil
}

// relayProbeBound bounds the boot source probe.
const relayProbeBound = 2 * time.Minute

// ProbeRelaySource runs the boot source probe (§3.4 item 5, N3): a pod in the runs namespace,
// labelled as a proxy so runnerd's ingress admits it, dials target:port (the relay Service) with
// the proxy image's own -egress-canary; observed returns the source address the relay accepted it
// from. A source other than the probe pod's IP is relay_source_rewritten. The probe's pod and its
// NetworkPolicy are deleted on return.
func (d *Driver) ProbeRelaySource(ctx context.Context, target string, port int32, observed func(context.Context) (netip.Addr, error)) error {
	ctx, cancel := context.WithTimeout(ctx, relayProbeBound)
	defer cancel()
	runID := uuid.New()
	labels := d.stampRunner(wardynLabels(runID, componentProxy, nil))
	name := "wardyn-relay-probe-" + runID.String()
	ns := d.cfg.Namespace
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(port))}},
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: hostCIDR(target)}}},
			}},
		},
	}
	pol.Spec.Egress = append(pol.Spec.Egress, d.runnerdPeerRules()...)
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer ccancel()
		_ = d.clientset.CoreV1().Pods(ns).Delete(cctx, name, metav1.DeleteOptions{})
		_ = d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(cctx, name, metav1.DeleteOptions{})
	}
	defer cleanup()
	if _, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, pol, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("%w: create probe policy: %w", errRelayProbeInconclusive, err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			Containers: []corev1.Container{{
				Name:            proxyContainerName,
				Image:           d.cfg.ProxyImage,
				Args:            []string{"-egress-canary", netipJoin(target, port)},
				SecurityContext: restrictedSecurityContext(),
				Resources:       canaryResources(),
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	d.placement.apply(pod, runID, componentProxy, nil)
	pod.Labels = d.stampRunner(pod.Labels)
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("%w: create probe pod: %w", errRelayProbeInconclusive, err)
	}
	seen := make(chan string, 1)
	go d.watchProbeIP(ctx, ns, name, seen)
	src, err := observed(ctx)
	if err != nil {
		return fmt.Errorf("%w: the relay saw no probe connection: %w", errRelayProbeInconclusive, err)
	}
	// The probe connects and exits within moments, before its kubelet has posted the pod's status, so
	// the pod's IP can still be missing when the relay sees the connection. Wait for it, bounded.
	var seenIP string
	select {
	case seenIP = <-seen:
	case <-time.After(probeIPWait):
	case <-ctx.Done():
	}
	if seenIP != "" {
		return relaySourceVerdict(seenIP, src)
	}
	got, err := d.clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: probe pod vanished", errRelayProbeInconclusive)
		}
		return fmt.Errorf("%w: read probe pod: %w", errRelayProbeInconclusive, err)
	}
	return relaySourceVerdict(got.Status.PodIP, src)
}

// probeIPPoll is how often watchProbeIP reads the probe pod.
const probeIPPoll = 100 * time.Millisecond

// probeIPWait is how long the verdict waits for the IP after the relay saw the connection (a
// variable so a test can bound it short).
var probeIPWait = 30 * time.Second

// watchProbeIP sends the probe pod's IP on seen the first time a read shows one, then returns. A pod
// that is gone sends "" at once, so the verdict falls through without waiting out probeIPWait. It
// also returns when ctx ends.
func (d *Driver) watchProbeIP(ctx context.Context, ns, name string, seen chan<- string) {
	t := time.NewTicker(probeIPPoll)
	defer t.Stop()
	for {
		got, err := d.clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil && got.Status.PodIP != "":
			seen <- got.Status.PodIP
			return
		case apierrors.IsNotFound(err):
			seen <- ""
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// relaySourceVerdict compares the probe pod's IP with the source the relay saw.
func relaySourceVerdict(podIP string, src netip.Addr) error {
	ip, err := netip.ParseAddr(podIP)
	if err != nil {
		return fmt.Errorf("%w: probe pod reports no IP", errRelayProbeInconclusive)
	}
	if ip.Unmap() != src.Unmap() {
		return fmt.Errorf("%w: the probe pod %s reached the relay from %s; the cluster rewrites pod-to-ClusterIP source addresses, which runners do not support", errRelaySourceRewritten, ip, src)
	}
	return nil
}

func hostCIDR(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip + "/32" // refused by the apiserver: the probe then fails inconclusive
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()).String()
}

func netipJoin(host string, port int32) string {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host + ":" + strconv.Itoa(int(port))
	}
	return netip.AddrPortFrom(a, uint16(port)).String()
}
