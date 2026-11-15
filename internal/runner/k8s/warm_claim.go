// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// claimWarm removes one Ready placeholder of the pool the run fits and returns its node, which the run's pods prefer.
// The run's pods are still built and checked as always: the placeholder only held room on a node with the image pulled.
// The delete carries the UID and resourceVersion just read, so two runs never both take one placeholder.
// It waits briefly for the placeholder to go, so the room it held is free when the run's pods schedule.
func (d *Driver) claimWarm(ctx context.Context, spec runner.SandboxSpec, class types.ConfinementClass) string {
	if len(d.cfg.WarmPools) == 0 || d.runnerMode() || spec.MachineKind.Normalize() != "" {
		return ""
	}
	req := resourceRequirements(spec.Resources).Requests
	p, ok := matchWarmPool(d.cfg.WarmPools, spec.Image, class, *req.Cpu(), *req.Memory())
	if !ok {
		return ""
	}
	ns := d.defaultNamespace()
	list, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelComponent + "=" + componentWarm + "," + labelWarmPool + "=" + p.Name})
	if err != nil {
		slog.Warn("k8s: warm pools: list the placeholders to claim", slog.String("pool", p.Name), slog.String("error", err.Error()))
		return ""
	}
	var ready []*corev1.Pod
	for i := range list.Items {
		if warmPodReady(&list.Items[i], p.Name) {
			ready = append(ready, &list.Items[i])
		}
	}
	slices.SortFunc(ready, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, pod := range ready {
		err := d.claimOne(ctx, ns, pod)
		if err == nil {
			node := pod.Spec.NodeName
			slog.Info("k8s: warm pool placeholder claimed", "pool", p.Name, "node", node, "run_id", spec.RunID.String())
			if err := d.waitPodGone(ctx, ns, pod, 15*time.Second); err != nil {
				slog.Info("k8s: warm pool placeholder still stopping", "pool", p.Name, "node", node)
			}
			return node
		}
		if isNotFound(err) || apierrors.IsConflict(err) {
			continue // another run took it first
		}
		slog.Warn("k8s: warm pools: could not claim a placeholder", slog.String("pool", p.Name), slog.String("error", err.Error()))
		return ""
	}
	return ""
}

// warmPodReady reports whether pod is a placeholder of pool that is bound to a node, Ready and not being deleted.
func warmPodReady(pod *corev1.Pod, pool string) bool {
	if _, ok := warmIndex(pod.Name, pool); !ok || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// claimOne deletes one placeholder, bound to the UID and resourceVersion it was listed with, and returns the error.
func (d *Driver) claimOne(ctx context.Context, ns string, pod *corev1.Pod) error {
	grace := int64(1) // grace 1, never 0: a zero grace removes the object before its container stops
	pre := metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}
	return d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace, Preconditions: &pre})
}
