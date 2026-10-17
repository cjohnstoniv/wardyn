// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package keystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// K8s holds a k8s runner's runner_resident values as Secrets in runnerd's own namespace (design K1
// §3.1 "Resident keystore"): one Secret per run and grant, with create, get and delete only, never
// list. A Secret's name derives from its account, so whoever holds the account (the resident index
// in runnerd's state) deletes it by name.
type K8s struct {
	cs       kubernetes.Interface
	ns       string
	runnerID string
}

// NewK8s returns the keystore for namespace ns.
func NewK8s(cs kubernetes.Interface, ns, runnerID string) *K8s {
	return &K8s{cs: cs, ns: ns, runnerID: runnerID}
}

// k8sCallBound bounds one Secret call: the resident path is synchronous with a delivery.
const k8sCallBound = 10 * time.Second

// SecretName is the Secret holding account's value: a hash of the runner's own
// keystore service (For(runnerID)) and the account.
func (k *K8s) SecretName(account string) string {
	sum := sha256.Sum256([]byte(For(k.runnerID).service + "/" + account))
	return "wardyn-resident-" + hex.EncodeToString(sum[:20])
}

const k8sValueKey = "value"

// Set stores value under account. A Secret already there is deleted and created again: the Role
// grants no update.
func (k *K8s) Set(account, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCallBound)
	defer cancel()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k.SecretName(account), Namespace: k.ns,
			Labels: map[string]string{"wardyn.managed": "true", "wardyn.runner-id": k.runnerID, "wardyn.component": "resident"}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{k8sValueKey: []byte(value)},
	}
	_, err := k.cs.CoreV1().Secrets(k.ns).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		if derr := k.cs.CoreV1().Secrets(k.ns).Delete(ctx, sec.Name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			return fmt.Errorf("%w: replace %s: %w", ErrUnavailable, sec.Name, derr)
		}
		_, err = k.cs.CoreV1().Secrets(k.ns).Create(ctx, sec, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Get returns the value stored under account.
func (k *K8s) Get(account string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCallBound)
	defer cancel()
	sec, err := k.cs.CoreV1().Secrets(k.ns).Get(ctx, k.SecretName(account), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	v, ok := sec.Data[k8sValueKey]
	if !ok {
		return "", fmt.Errorf("%w: %s holds no value", ErrUnavailable, sec.Name)
	}
	return string(v), nil
}

// Delete removes account; one that is already gone is not an error.
func (k *K8s) Delete(account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sCallBound)
	defer cancel()
	if err := k.cs.CoreV1().Secrets(k.ns).Delete(ctx, k.SecretName(account), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}
