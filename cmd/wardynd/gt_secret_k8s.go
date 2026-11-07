// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// newGroundtruthSecretPatch builds the client for the sensor Secret: the
// in-cluster service account, else the default kubeconfig, the order the
// Kubernetes runner uses. Building it contacts nothing; the first patch does.
func newGroundtruthSecretPatch(ns, name string) (gtSecretPatch, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig(); err != nil {
			return nil, fmt.Errorf("WARDYN_GROUNDTRUTH_TOKEN_SECRET: no in-cluster config or kubeconfig: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_GROUNDTRUTH_TOKEN_SECRET: %w", err)
	}
	return k8sSecretPatch(cs, ns, name), nil
}

// k8sSecretPatch merge-patches data into ns/name. Patch is the only verb: the
// chart grants patch on this one resourceName and nothing else, so a missing
// Secret is an error the rotator retries, never a create.
func k8sSecretPatch(cs kubernetes.Interface, ns, name string) gtSecretPatch {
	return func(ctx context.Context, data map[string][]byte) error {
		body, err := json.Marshal(map[string]any{"data": data}) // []byte encodes as base64, the Secret wire form
		if err != nil {
			return err
		}
		_, err = cs.CoreV1().Secrets(ns).Patch(ctx, name, k8stypes.MergePatchType, body, metav1.PatchOptions{FieldManager: "wardynd"})
		return err
	}
}
