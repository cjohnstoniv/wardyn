// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBuildProxyPodTakesItsInputs pins that every field of proxyPodInputs reaches the pod: a rebuilt proxy on a kept
// run is made from these alone, so a field the builder ignored would be silently lost there.
func TestBuildProxyPodTakesItsInputs(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	pin := corev1.HostAlias{IP: "10.0.0.9", Hostnames: []string{"wardyn.example"}}
	const cpURL = "https://wardyn.example:8443"
	pod, err := d.buildProxyPod(proxyPodInputs{
		RunID: id, Namespace: "ns1", SecretName: "s1", Deadline: int64Ptr(600), ControlPlaneURL: cpURL,
		Azure: true, Labels: map[string]string{"team": "a"}, CPPin: &pin,
	})
	if err != nil {
		t.Fatalf("buildProxyPod: %v", err)
	}
	if pod.Name != proxyPodName(id) || pod.Namespace != "ns1" {
		t.Errorf("name/namespace = %q/%q, want %q/ns1", pod.Name, pod.Namespace, proxyPodName(id))
	}
	if got := pod.Spec.ActiveDeadlineSeconds; got == nil || *got != 600 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 600", got)
	}
	var secretVol string
	for _, v := range pod.Spec.Volumes {
		if v.Name == proxyConfigSecretVolumeName && v.Secret != nil {
			secretVol = v.Secret.SecretName
		}
	}
	if secretVol != "s1" {
		t.Errorf("proxy config Secret volume = %q, want s1", secretVol)
	}
	main := pod.Spec.Containers[0]
	var gotURL string
	for _, e := range main.Env {
		if e.Name == "WARDYN_CONTROL_PLANE_URL" {
			gotURL = e.Value
		}
	}
	if gotURL != cpURL {
		t.Errorf("WARDYN_CONTROL_PLANE_URL = %q, want %q", gotURL, cpURL)
	}
	if !reflect.DeepEqual(main.Resources, proxyResources(true)) {
		t.Errorf("resources = %v, want the Azure-sized proxyResources(true)", main.Resources)
	}
	if pod.Labels["team"] != "a" || pod.Labels[labelComponent] != componentProxy {
		t.Errorf("labels = %v, want team=a and %s=%s", pod.Labels, labelComponent, componentProxy)
	}
	if ha := pod.Spec.HostAliases; len(ha) == 0 || !reflect.DeepEqual(ha[len(ha)-1], pin) {
		t.Errorf("HostAliases = %v, want the pin last", ha)
	}
}

// spiffeVolume returns the pod's SPIRE Workload API volume, or nil.
func spiffeVolume(pod *corev1.Pod) *corev1.Volume {
	for i, v := range pod.Spec.Volumes {
		if v.CSI != nil || v.Name == spiffeVolumeName {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// TestBuildProxyPodSPIFFEMount: with the SPIRE mount set, the proxy pod runs as the proxy account with the token
// automount still off, and its container mounts the CSI volume read-only at /spiffe-workload-api; unset, the pod
// names no account and has no CSI volume.
//
// COUNTERFACTUAL: drop the withSPIFFEMount call and the "set" case goes red; apply it unconditionally and the
// "unset" case goes red.
func TestBuildProxyPodSPIFFEMount(t *testing.T) {
	in := proxyPodInputs{RunID: uuid.New(), Namespace: "ns1", SecretName: "s1", ControlPlaneURL: "https://wardyn.example:8443"}

	d, _ := newTestDriver(t, Config{SPIFFE: SPIFFEMount{CSIDriver: "csi.spiffe.io", ProxyServiceAccount: "wardyn-proxy"}})
	pod, err := d.buildProxyPod(in)
	if err != nil {
		t.Fatalf("buildProxyPod: %v", err)
	}
	if pod.Spec.ServiceAccountName != "wardyn-proxy" {
		t.Errorf("ServiceAccountName = %q, want wardyn-proxy", pod.Spec.ServiceAccountName)
	}
	if a := pod.Spec.AutomountServiceAccountToken; a == nil || *a {
		t.Errorf("AutomountServiceAccountToken = %v, want false", a)
	}
	v := spiffeVolume(pod)
	if v == nil || v.CSI == nil || v.CSI.Driver != "csi.spiffe.io" || v.CSI.ReadOnly == nil || !*v.CSI.ReadOnly {
		t.Fatalf("CSI volume = %+v, want a read-only csi.spiffe.io volume", v)
	}
	var mount *corev1.VolumeMount
	for i, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == v.Name {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil || mount.MountPath != "/spiffe-workload-api" || !mount.ReadOnly {
		t.Errorf("proxy container mount = %+v, want read-only at /spiffe-workload-api", mount)
	}
	for _, c := range pod.Spec.InitContainers {
		for _, m := range c.VolumeMounts {
			if m.Name == v.Name {
				t.Errorf("init container %s mounts the socket; only the proxy container may", c.Name)
			}
		}
	}

	d0, _ := newTestDriver(t, Config{})
	pod0, err := d0.buildProxyPod(in)
	if err != nil {
		t.Fatalf("buildProxyPod unset: %v", err)
	}
	if pod0.Spec.ServiceAccountName != "" || spiffeVolume(pod0) != nil {
		t.Errorf("unset: ServiceAccountName %q, CSI volume %+v, want neither", pod0.Spec.ServiceAccountName, spiffeVolume(pod0))
	}
}

// TestCreateSandbox_AgentPodNeverGetsSPIFFEMount: with the SPIRE mount set, the agent pod still names no service
// account (the namespace's default) and has no CSI volume, so it can never attest as the run's proxy.
func TestCreateSandbox_AgentPodNeverGetsSPIFFEMount(t *testing.T) {
	d, cs := newTestDriver(t, Config{SPIFFE: SPIFFEMount{CSIDriver: "csi.spiffe.io", ProxyServiceAccount: "wardyn-proxy"}})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pods := cs.CoreV1().Pods(testNamespace)
	agent, err := pods.Get(context.Background(), agentPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if agent.Spec.ServiceAccountName != "" || spiffeVolume(agent) != nil {
		t.Errorf("agent pod: ServiceAccountName %q, CSI volume %+v, want neither", agent.Spec.ServiceAccountName, spiffeVolume(agent))
	}
	proxyPod, err := pods.Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	if proxyPod.Spec.ServiceAccountName != "wardyn-proxy" || spiffeVolume(proxyPod) == nil {
		t.Errorf("proxy pod: ServiceAccountName %q, CSI volume %+v, want the account and the volume", proxyPod.Spec.ServiceAccountName, spiffeVolume(proxyPod))
	}
}
