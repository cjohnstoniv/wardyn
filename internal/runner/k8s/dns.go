// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// agentToProxyPorts are the ports the agent pod may reach on its proxy pod: the
// proxy, and 53 over TCP and UDP for its DNS listener, which refuses every
// query and records it (proxy.DNSListenEnv). 53 reaches only the proxy pod.
func agentToProxyPorts() []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{
		{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(runner.ProxyListenPort))},
		{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(53))},
		{Protocol: protoPtr(corev1.ProtocolUDP), Port: intOrStrPtr(intstr.FromInt32(53))},
	}
}

// proxyDNSSysctls lets the proxy's non-root, capability-less container bind
// its DNS listener on 53. net.ipv4.ip_unprivileged_port_start is namespaced and
// on Kubernetes' safe-sysctl list, so it changes only the proxy pod's own
// network namespace and needs no capability or kubelet allow-list.
func proxyDNSSysctls() []corev1.Sysctl {
	return []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "53"}}
}
