// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The patch lands in the one named Secret, keeps keys it does not write, and
// touches no other Secret in the namespace.
func TestK8sSecretPatch_MergesIntoNamedSecret(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "sensors", Name: "wardyn-groundtruth"},
			Data: map[string][]byte{"keep": []byte("me")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "sensors", Name: "other"}},
	)
	w := secretTokenWriter(k8sSecretPatch(cs, "sensors", "wardyn-groundtruth"), "CA PEM")
	if err := w(context.Background(), "tok-1"); err != nil {
		t.Fatal(err)
	}
	got, err := cs.CoreV1().Secrets("sensors").Get(context.Background(), "wardyn-groundtruth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data["token"]) != "tok-1" || string(got.Data["control-plane-ca.pem"]) != "CA PEM" || string(got.Data["keep"]) != "me" {
		t.Fatalf("data = %q", got.Data)
	}
	other, _ := cs.CoreV1().Secrets("sensors").Get(context.Background(), "other", metav1.GetOptions{})
	if len(other.Data) != 0 {
		t.Fatalf("another Secret was written: %q", other.Data)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "patch" && a.GetVerb() != "get" {
			t.Errorf("the writer may only patch (RBAC grants nothing else), saw %s", a.GetVerb())
		}
	}
}

// A missing Secret is an error to retry, never a create: the grant is patch
// on one name, and the chart owns the Secret.
func TestK8sSecretPatch_MissingSecretIsAnError(t *testing.T) {
	cs := fake.NewClientset()
	if err := k8sSecretPatch(cs, "sensors", "wardyn-groundtruth")(context.Background(), map[string][]byte{"token": []byte("t")}); err == nil {
		t.Fatal("patching a missing Secret must fail")
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Fatal("the writer must never create the Secret")
		}
	}
}
