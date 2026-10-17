// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package keystore

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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
	if err != nil || sec.Labels["wardyn.runner-id"] != "runner-1" || sec.Labels["wardyn.component"] != "resident" {
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

func TestK8sKeystorePrecheck(t *testing.T) {
	k := NewK8s(fake.NewClientset(), "wardyn-runner", "runner-1")
	if err := k.Precheck("run/a", strings.Repeat("x", k8sValueLimit)); err != nil {
		t.Fatalf("a value at the limit: %v", err)
	}
	if err := k.Precheck("run/a", strings.Repeat("x", k8sValueLimit+1)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a value over the limit: %v", err)
	}
}

// The fake clientset ignores a deletecollection's selector, so a reactor applies it to the tracker
// the way the API server does.
func TestK8sKeystoreDeleteAll(t *testing.T) {
	const ns = "wardyn-runner"
	cs := fake.NewClientset()
	tracker := cs.Tracker()
	cs.PrependReactor("delete-collection", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		dc := a.(clienttesting.DeleteCollectionAction)
		sel := dc.GetListRestrictions().Labels
		obj, err := tracker.List(corev1.SchemeGroupVersion.WithResource("secrets"), corev1.SchemeGroupVersion.WithKind("Secret"), ns)
		if err != nil {
			return true, nil, err
		}
		for _, sec := range obj.(*corev1.SecretList).Items {
			if sel.Matches(labels.Set(sec.Labels)) {
				if err := tracker.Delete(corev1.SchemeGroupVersion.WithResource("secrets"), ns, sec.Name); err != nil {
					return true, nil, err
				}
			}
		}
		return true, nil, nil
	})
	mine, other := NewK8s(cs, ns, "runner-1"), NewK8s(cs, ns, "runner-2")
	for _, k := range []*K8s{mine, other} {
		for _, acct := range []string{"run/a", "run/b"} {
			if err := k.Set(acct, "v"); err != nil {
				t.Fatal(err)
			}
		}
	}
	unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: ns, Labels: map[string]string{"wardyn.runner-id": "runner-1"}}}
	if _, err := cs.CoreV1().Secrets(ns).Create(t.Context(), unrelated, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := mine.DeleteAll(); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"run/a", "run/b"} {
		if _, err := mine.Get(acct); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("this runner's %s survived DeleteAll: %v", acct, err)
		}
		if v, err := other.Get(acct); err != nil || v != "v" {
			t.Fatalf("another runner's %s = %q %v after DeleteAll", acct, v, err)
		}
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(t.Context(), "registration", metav1.GetOptions{}); err != nil {
		t.Fatalf("a non-resident Secret did not survive DeleteAll: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" {
			t.Fatal("DeleteAll listed Secrets, which the Role does not grant")
		}
	}
}
