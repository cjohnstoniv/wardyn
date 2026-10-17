// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ProbeSubstrate implements runner.SubstrateProber with one namespaced pod list
// of limit 1. The Role already grants `list` on pods (rbac.yaml), so the probe
// needs no verb of its own, and a list is the cheapest call that fails the way
// every real operation here would: a refused token is 401, a deleted
// RoleBinding is 403, a black-holed apiserver runs into ctx's deadline.
func (d *Driver) ProbeSubstrate(ctx context.Context) runner.SubstrateState {
	_, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil:
		return runner.SubstrateOK
	case ctx.Err() != nil:
		return runner.SubstrateUnreachable
	case apierrors.IsUnauthorized(err):
		return runner.SubstrateUnauthorized
	case apierrors.IsForbidden(err):
		return runner.SubstrateForbidden
	default:
		return runner.SubstrateUnreachable
	}
}
