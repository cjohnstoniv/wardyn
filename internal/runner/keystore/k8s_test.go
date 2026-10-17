// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package keystore

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestK8sKeystore(t *testing.T) {
	cs := fake.NewClientset()
	k := NewK8s(cs, "wardyn-runner", "runner-1")
	if _, err := k.Get("run/a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get of a missing value: %v", err)
	}
	if err := k.Set("run/a", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := k.Set("run/a", "v2"); err != nil {
		t.Fatalf("Set over an existing value: %v", err)
	}
	if v, err := k.Get("run/a"); err != nil || v != "v2" {
		t.Fatalf("Get = %q %v", v, err)
	}
	sec, err := cs.CoreV1().Secrets("wardyn-runner").Get(t.Context(), k.SecretName("run/a"), metav1.GetOptions{})
	if err != nil || sec.Labels["wardyn.runner-id"] != "runner-1" {
		t.Fatalf("secret %+v %v", sec, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "list" || a.GetVerb() == "patch" {
			t.Fatalf("the keystore used %s, which the Role does not grant", a.GetVerb())
		}
	}
	if err := k.Delete("run/a"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("run/a"); err != nil {
		t.Fatalf("Delete of a gone value: %v", err)
	}
	if k.SecretName("run/a") == k.SecretName("run/b") || len(k.SecretName("run/a")) > 63 {
		t.Fatal("secret names collide or exceed a DNS label")
	}
}
