// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// labelProxyGeneration, on a replacement proxy pod, is the policy generation it was started for (ReplaceProxyHashedAt).
// A proxy pod without it, or with one that is not a positive number, is generation 0: none.
const labelProxyGeneration = "wardyn.proxy.generation"

var (
	_ runner.ProxyGenerationStopper = (*keepDriver)(nil)
	_ runner.ProxyReviverAt         = (*keepDriver)(nil)
)

// proxyNotNewer reads the run's proxy pod for a call that acts for policy generation gen and answers it, nil when there is
// none. With gen > 0, a pod labelled newer than gen refuses the call with runner.ErrProxyGenerationNewer, and for a
// replace one labelled gen refuses it too. It writes nothing, so a refusal leaves the run as it was.
func (d *Driver) proxyNotNewer(ctx context.Context, ns string, runID uuid.UUID, gen int, replace bool) (*corev1.Pod, error) {
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, proxyPodName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: read the proxy pod: %w", err)
	}
	have, perr := strconv.Atoi(pod.Labels[labelProxyGeneration])
	if perr != nil || have < 0 {
		have = 0
	}
	if gen > 0 && (have > gen || (replace && have == gen)) {
		return nil, fmt.Errorf("%w: it is for generation %d, this call is for %d", runner.ErrProxyGenerationNewer, have, gen)
	}
	return pod, nil
}

// removeProxyPod deletes pod, a proxy pod as read, under its UID, and waits until it is gone. A pod made under its name
// since is not it: the UID precondition refuses that delete with a conflict, and the wait ends on the other UID. nil is no
// pod, and nothing is removed.
func (d *Driver) removeProxyPod(ctx context.Context, ns string, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	uid := pod.UID
	err := d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !isNotFound(err) && !apierrors.IsConflict(err) { // a conflict: that pod is already gone
		return fmt.Errorf("delete: %w", err)
	}
	if err := d.waitPodGone(ctx, ns, pod, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for it to go: %w", err)
	}
	return nil
}

// markReplacement sets a replacement proxy pod apart from the one a create makes: the generation label (none for gen 0),
// and the first-renew gate, last in the proxy container's env with any earlier setting of it dropped, so no operator knob
// copied in ahead of it can turn it off. A gated proxy serves only after a token renewal under its own token has landed,
// so one started late by a superseded revive, whose token the newer revive retired, never serves.
func markReplacement(pod *corev1.Pod, gen int) {
	if gen > 0 {
		pod.Labels[labelProxyGeneration] = strconv.Itoa(gen)
	}
	c := &pod.Spec.Containers[0]
	c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == proxy.FirstRenewGateEnv })
	c.Env = append(c.Env, corev1.EnvVar{Name: proxy.FirstRenewGateEnv, Value: "1"})
}

// createReplacementPod creates pod, the run's new proxy pod. A proxy pod that took the name after the replace read it is
// read again, once, under the same generation rule, and removed before the create is tried again.
func (d *Driver) createReplacementPod(ctx context.Context, ns string, runID uuid.UUID, pod *corev1.Pod, gen int) error {
	_, err := d.createPod(ctx, runID, componentProxy, pod)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	taken, err := d.proxyNotNewer(ctx, ns, runID, gen, true)
	if err != nil {
		return err
	}
	if err := d.removeProxyPod(ctx, ns, taken); err != nil {
		return err
	}
	_, err = d.createPod(ctx, runID, componentProxy, pod)
	return err
}

// StopProxyAt is StopProxy acting for policy generation gen (runner.ProxyGenerationStopper): a proxy pod started for a
// newer generation refuses it with runner.ErrProxyGenerationNewer. gen 0 is StopProxy.
func (k *keepDriver) StopProxyAt(ctx context.Context, ref string, gen int) error {
	if gen > 0 {
		runID, ns, err := k.refRun(ctx, ref)
		if err != nil {
			return err
		}
		if _, err := k.proxyNotNewer(ctx, ns, runID, gen, false); err != nil {
			return err
		}
	}
	return k.StopProxy(ctx, ref)
}

// deleteProxyCfgSecrets removes the run's replacement proxy config Secrets, except keep and, for gen > 0, any labelled
// with a newer generation: a newer revive writes its Secret before its proxy pod exists, so a slower end or replace
// cannot tell it from the pod. Each is deleted by name with its UID, so one written since the list is left.
func (d *Driver) deleteProxyCfgSecrets(ctx context.Context, ns string, runID uuid.UUID, gen int, keep string) error {
	list, err := d.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String() + "," + labelProxyCfg})
	if err != nil {
		return err
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Name == keep {
			continue
		}
		if have, perr := strconv.Atoi(s.Labels[labelProxyGeneration]); gen > 0 && perr == nil && have > gen {
			continue
		}
		uid := s.UID
		if err := d.clientset.CoreV1().Secrets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !isNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}
