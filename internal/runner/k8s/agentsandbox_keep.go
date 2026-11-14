// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"

	"github.com/google/uuid"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keepAddressAccess is the verbs a kept run's proxy address needs in each runs namespace: the run's Service, and the
// EndpointSlice Wardyn writes for it.
var keepAddressAccess = []authzv1.ResourceAttributes{
	{Resource: "services", Verb: "create"},
	{Resource: "services", Verb: "get"},
	{Resource: "services", Verb: "delete"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "create"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "get"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "update"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "delete"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "deletecollection"},
}

// checkAgentSandboxKeep refuses the keep switch without the agent-sandbox backend.
func checkAgentSandboxKeep(cfg Config) error {
	if cfg.AgentSandboxKeep && !cfg.AgentSandbox {
		return errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX_KEEP needs WARDYN_K8S_AGENT_SANDBOX on")
	}
	return nil
}

func (d *Driver) keepOn() bool { return d.cfg.AgentSandbox && d.cfg.AgentSandboxKeep }

func keepServiceName(runID uuid.UUID) string { return "wardyn-proxy-svc-" + runID.String() }

// labelKeep marks a run kept at its end. It is set on the agent pod, and so on its Sandbox and the Sandbox's template.
const labelKeep = "wardyn.keep"

// keepRun is whether a run is kept at its end: keep on, interactive, and its work folder on a kept drive.
func (d *Driver) keepRun(spec runner.SandboxSpec) bool {
	return d.keepOn() && spec.Interactive &&
		slices.ContainsFunc(spec.Drives, func(m types.DriveMount) bool { return m.Target == scratchWorkPath })
}

// keepPortNames name the three ports of a run's Service and EndpointSlice, in the order agentToProxyPorts lists them.
var keepPortNames = []string{"proxy", "dns-tcp", "dns-udp"}

func keepServicePorts() []corev1.ServicePort {
	var out []corev1.ServicePort
	for i, p := range agentToProxyPorts() {
		out = append(out, corev1.ServicePort{Name: keepPortNames[i], Protocol: *p.Protocol, Port: p.Port.IntVal, TargetPort: *p.Port})
	}
	return out
}

func keepSlicePorts() []discoveryv1.EndpointPort {
	var out []discoveryv1.EndpointPort
	for i, p := range agentToProxyPorts() {
		name, port := keepPortNames[i], p.Port.IntVal
		out = append(out, discoveryv1.EndpointPort{Name: &name, Protocol: p.Protocol, Port: &port})
	}
	return out
}

// addressFamily is the EndpointSlice address type of ip: IPv6 for an IPv6 address, else IPv4.
func addressFamily(ip string) discoveryv1.AddressType {
	if a := net.ParseIP(ip); a != nil && a.To4() == nil {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

// createKeepService makes the run's ClusterIP Service and returns it with the address the API server gave it.
// No selector: Wardyn alone names the endpoint, so a pod labelled like the proxy is never sent the agent's traffic.
func (d *Driver) createKeepService(ctx context.Context, ns string, runID uuid.UUID, labels map[string]string) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keepServiceName(runID), Namespace: ns, Labels: labels},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: keepServicePorts()},
	}
	created, err := d.clientset.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: create the run's Service: %w", err)
	}
	if created.Spec.ClusterIP == "" {
		return nil, errors.New("k8s: keep: the run's Service has no ClusterIP")
	}
	return created, nil
}

// writeKeepEndpoints points the run's EndpointSlice at ip, or at nothing when ip is "". A slice already as wanted is
// not written again, so a repeated end does not rewrite it. The slice's address type cannot change after create.
func (d *Driver) writeKeepEndpoints(ctx context.Context, ns string, runID uuid.UUID, owner *corev1.Service, ip string) error {
	eps := d.clientset.DiscoveryV1().EndpointSlices(ns)
	existing, err := eps.Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: read the run's EndpointSlice: %w", err)
	}
	labels := maps.Clone(owner.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[discoveryv1.LabelServiceName] = owner.Name
	labels[discoveryv1.LabelManagedBy] = "wardyn.dev"
	var endpoints []discoveryv1.Endpoint
	if ip != "" {
		endpoints = []discoveryv1.Endpoint{{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}}}
	}
	if isNotFound(err) {
		family := addressFamily(owner.Spec.ClusterIP)
		if ip != "" {
			family = addressFamily(ip)
		}
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: keepServiceName(runID), Namespace: ns, Labels: labels, OwnerReferences: ownedByService(owner)},
			AddressType: family,
			Ports:       keepSlicePorts(),
			Endpoints:   endpoints,
		}
		if _, err := eps.Create(ctx, slice, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("k8s: keep: create the run's EndpointSlice: %w", err)
		}
		return nil
	}
	if ip != "" && addressFamily(ip) != existing.AddressType {
		return fmt.Errorf("k8s: keep: the proxy's address %s is not of the %s family of the run's EndpointSlice", ip, existing.AddressType)
	}
	ports := keepSlicePorts()
	if reflect.DeepEqual(existing.Labels, labels) && reflect.DeepEqual(existing.Ports, ports) && reflect.DeepEqual(existing.Endpoints, endpoints) {
		return nil
	}
	// update the object read: its annotations carry state other code writes, and must survive
	existing.Labels, existing.Ports, existing.Endpoints = labels, ports, endpoints
	if _, err := eps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("k8s: keep: update the run's EndpointSlice: %w", err)
	}
	return nil
}

// ownedByService is ownedByPod for a Service: the owner whose deletion garbage-collects the object.
func ownedByService(svc *corev1.Service) []metav1.OwnerReference {
	if svc == nil || svc.UID == "" {
		return nil
	}
	return []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Name, UID: svc.UID}}
}

// deleteKeepAddress removes the run's Service and its EndpointSlices.
func (d *Driver) deleteKeepAddress(ctx context.Context, ns string, runID uuid.UUID) error {
	if err := d.clientset.CoreV1().Services(ns).Delete(ctx, keepServiceName(runID), metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: delete the run's Service: %w", err)
	}
	err := d.clientset.DiscoveryV1().EndpointSlices(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: delete the run's EndpointSlices: %w", err)
	}
	return nil
}

// keepService makes the run's Service when the run is kept at its end, and returns nil for every other run.
func (d *Driver) keepService(ctx context.Context, ns string, spec runner.SandboxSpec, labels map[string]string) (*corev1.Service, error) {
	if !d.keepRun(spec) {
		return nil, nil
	}
	return d.createKeepService(ctx, ns, spec.RunID, labels)
}

// keepAddress is where the agent pins "wardyn-proxy" and its nameserver: the proxy pod's IP or, for a kept run, its
// Service's ClusterIP once the EndpointSlice points at that IP.
func (d *Driver) keepAddress(ctx context.Context, ns string, runID uuid.UUID, svc *corev1.Service, proxyIP string) (string, error) {
	if svc == nil {
		return proxyIP, nil
	}
	if err := d.writeKeepEndpoints(ctx, ns, runID, svc, proxyIP); err != nil {
		return "", err
	}
	return svc.Spec.ClusterIP, nil
}

// keepAgent marks a kept run's agent pod with the keep label and makes the Service its owner: the agent outlives its proxy
// pod, and teardown deletes the Service.
func keepAgent(pod *corev1.Pod, svc *corev1.Service) {
	if svc == nil {
		return
	}
	pod.Labels[labelKeep] = "true"
	pod.OwnerReferences = ownedByService(svc)
}
