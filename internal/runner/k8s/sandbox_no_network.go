// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// noNetworkPodEnforced is NoNetworkProxyOnly for a No network sandbox whose namespace has NetworkPolicy
// proven enforced: the narrow proxy policy applies, not the proxy alone.
const noNetworkPodEnforced = "pod_enforced"

// noNetworkLookupTimeout bounds the one lookup of Wardyn's internal address at a No network create.
const noNetworkLookupTimeout = 5 * time.Second

// proxyEgressPlan is the egress of a run's proxy NetworkPolicy and, for a No network run in server mode,
// the hostAlias that pins Wardyn's internal name. It runs before anything exists, so a refusal creates nothing.
//
// SECURITY: a No network run's proxy pod has exactly one way out, to Wardyn: no DNS rule and no address
// range, so it can reach nothing else. On a server the pin replaces the lookup. NetworkPolicies are never
// updated, so nothing can widen this later. The peer is a pod selector behind a Service address, so the CNI
// must match it after Service translation; one that does not fails the run at the readback
// (internal/api's unconfirmedNoNetworkCause).
//
// Any other run keeps the three-part list: DNS to the cluster's DNS pods and every address but the
// cloud-metadata one, then every address but the metadata one, then (runner mode) runnerd's own pods.
//
// SECURITY: the first two rules share the metadata-excluding peer — a peer-less DNS rule would permit
// port 53 to the metadata address, voiding the Except on the rule beside it. The DNS rule adds the
// cluster DNS pods as a second peer (peers are OR'd, so the ipBlock and its Except are unchanged): a CNI
// that selects pods by identity does not match a pod IP against an ipBlock.
func (d *Driver) proxyEgressPlan(ctx context.Context, spec runner.SandboxSpec) (egress []networkingv1.NetworkPolicyEgressRule, pin *corev1.HostAlias, err error) {
	if !spec.ProxyConfig.NoNetwork {
		return append([]networkingv1.NetworkPolicyEgressRule{
			{ // DNS
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(53))},
					{Protocol: protoPtr(corev1.ProtocolUDP), Port: intOrStrPtr(intstr.FromInt32(53))},
				},
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: notMetadataIPBlock()}, clusterDNSPeer()},
			},
			{ // everything except the cloud-metadata address
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: notMetadataIPBlock()}},
			},
		}, d.runnerdPeerRules()...), nil, nil
	}
	if d.runnerMode() {
		rules := d.runnerdPeerRules()
		if len(rules) == 0 {
			return nil, nil, errors.New("k8s: a No network run on a runner needs the runner's own pods as its proxy's only peer; the run was not started")
		}
		return rules, nil, nil
	}
	peer := d.cfg.ControlPlanePeer
	if peer == nil {
		return nil, nil, errors.New("k8s: a No network run needs WARDYN_K8S_CONTROL_PLANE_NAMESPACE and WARDYN_K8S_CONTROL_PLANE_POD_LABELS set, so its proxy can reach only Wardyn; the run was not started")
	}
	u, err := url.Parse(spec.ProxyConfig.ControlPlaneURL)
	if err != nil || u.Hostname() == "" {
		return nil, nil, fmt.Errorf("k8s: a No network run needs a control-plane URL it can read, and %q is not one; the run was not started", spec.ProxyConfig.ControlPlaneURL)
	}
	port, err := controlPlanePort(u)
	if err != nil {
		return nil, nil, err
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		addr, err := d.controlPlaneAddress(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		pin = &corev1.HostAlias{IP: addr, Hostnames: []string{host}}
	}
	return []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(port))}},
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": peer.Namespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: maps.Clone(peer.PodLabels)},
		}},
	}}, pin, nil
}

// controlPlanePort is the URL's port, or the proxy's own default for its scheme (443 for https, 80 for http).
func controlPlanePort(u *url.URL) (int32, error) {
	ps := u.Port()
	if ps == "" {
		switch u.Scheme {
		case "https":
			return 443, nil
		case "http":
			return 80, nil
		}
		return 0, fmt.Errorf("k8s: a No network run needs a port for the control-plane URL %q; the run was not started", u.Redacted())
	}
	n, err := strconv.Atoi(ps)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("k8s: a No network run needs a port for the control-plane URL %q; the run was not started", u.Redacted())
	}
	return int32(n), nil
}

// controlPlaneAddress resolves host from this pod and requires exactly one address, as a runner does for its
// relay Service: a Service recreated with a new address leaves no stale pin, since this runs at every No
// network create.
func (d *Driver) controlPlaneAddress(ctx context.Context, host string) (string, error) {
	lookup := d.cfg.LookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	ctx, cancel := context.WithTimeout(ctx, noNetworkLookupTimeout)
	defer cancel()
	addrs, err := lookup(ctx, host)
	if err != nil {
		return "", fmt.Errorf("k8s: a No network run needs Wardyn's internal address %s to resolve to one IP, and its lookup failed (%v); the run was not started", host, err)
	}
	if len(addrs) != 1 {
		return "", fmt.Errorf("k8s: a No network run needs Wardyn's internal address %s to resolve to one IP, and it resolved to %v; the run was not started", host, addrs)
	}
	return addrs[0], nil
}

// noNetworkProxyOnly is "" unless spec is a No network run whose namespace has NetworkPolicy proven
// enforced. Otherwise it says why the proxy layer may be all there is: "netpol_not_enforced" and
// "netpol_ambient_acknowledged" are the two states only an override admits (refuses, canary_recheck.go), and
// "netpol_not_proven" covers a verdict that moved between netPolRefusal's read at the start of the create and
// this one.
func (d *Driver) noNetworkProxyOnly(spec runner.SandboxSpec, ns string) string {
	if !spec.ProxyConfig.NoNetwork {
		return ""
	}
	state := netPolNotProven
	if v := d.netPol.Load(); v != nil {
		if n, ok := v.byNS[ns]; ok {
			state = n.state
		}
	}
	switch state {
	case netPolProven:
		return ""
	case netPolUnenforced:
		return "netpol_not_enforced"
	case netPolAcknowledged:
		return "netpol_ambient_acknowledged"
	}
	return "netpol_not_proven"
}
