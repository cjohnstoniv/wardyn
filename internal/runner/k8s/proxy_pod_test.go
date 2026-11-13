// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
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
