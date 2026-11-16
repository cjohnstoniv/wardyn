// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

// emptyKeepEndpointsAt empties the run's EndpointSlice for a replace or an end acting for policy generation gen, so the
// agent reaches no proxy while its proxy pod changes. With gen > 0 the slice is read first, then the proxy pod
// (proxyNotNewer), and the slice is updated as read. A newer revive points the slice at its proxy only after making
// that pod, so either the pod read refuses with runner.ErrProxyGenerationNewer, or the update, which carries the
// resourceVersion read, fails with a conflict; either way the newer revive's endpoint stays. With gen 0 it is
// writeKeepEndpoints with no address.
func (d *Driver) emptyKeepEndpointsAt(ctx context.Context, ns string, runID uuid.UUID, svc *corev1.Service, gen int, replace bool) error {
	if gen <= 0 {
		return d.writeKeepEndpoints(ctx, ns, runID, svc, "")
	}
	eps := d.clientset.DiscoveryV1().EndpointSlices(ns)
	slice, err := eps.Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: read the run's EndpointSlice: %w", err)
	}
	if _, perr := d.proxyNotNewer(ctx, ns, runID, gen, replace); perr != nil {
		return perr
	}
	if err != nil { // no slice yet: make it with no address
		return d.writeKeepEndpoints(ctx, ns, runID, svc, "")
	}
	if len(slice.Endpoints) == 0 {
		return nil
	}
	slice.Endpoints = nil
	if _, err := eps.Update(ctx, slice, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("k8s: keep: empty the run's EndpointSlice: %w", err)
	}
	return nil
}

// pointKeepEndpointsAt points the run's EndpointSlice at ip, the address of the proxy pod a replace acting for policy
// generation gen made, so the agent reaches it. With gen > 0 the slice is read first, then the proxy pod
// (proxyNotNewer), and the slice is updated as read: a newer revive that removed this replace's pod and pointed the
// slice at its own either is still there for the pod read to refuse with runner.ErrProxyGenerationNewer, or wrote
// after the slice read, and the update, which carries the resourceVersion read, fails with a conflict. With no proxy
// pod at all, the one this replace made was removed after its address was read, and nothing is written. With gen 0 it
// is writeKeepEndpoints.
func (d *Driver) pointKeepEndpointsAt(ctx context.Context, ns string, runID uuid.UUID, svc *corev1.Service, gen int, ip string) error {
	if gen <= 0 {
		return d.writeKeepEndpoints(ctx, ns, runID, svc, ip)
	}
	eps := d.clientset.DiscoveryV1().EndpointSlices(ns)
	slice, err := eps.Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: read the run's EndpointSlice: %w", err)
	}
	pod, perr := d.proxyNotNewer(ctx, ns, runID, gen, false)
	if perr != nil {
		return perr
	}
	if pod == nil {
		return errors.New("k8s: keep: the proxy pod this replace made is gone")
	}
	if err != nil { // no slice yet: make it, pointed at ip
		return d.writeKeepEndpoints(ctx, ns, runID, svc, ip)
	}
	if addressFamily(ip) != slice.AddressType {
		return fmt.Errorf("k8s: keep: the proxy's address %s is not of the %s family of the run's EndpointSlice", ip, slice.AddressType)
	}
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}}}
	if _, err := eps.Update(ctx, slice, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("k8s: keep: update the run's EndpointSlice: %w", err)
	}
	return nil
}

// deleteUnmountedCfgSecret deletes name, the replacement config Secret a replace wrote, when the replace fails before
// its proxy pod is made: no pod mounts it and no later call would name it. It runs on a context of its own, so a
// replace its caller cut short still deletes it. A failed delete is logged; the Secret carries the run's label, and
// the run's teardown deletes it.
func (d *Driver) deleteUnmountedCfgSecret(ctx context.Context, ns, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := d.clientset.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
		slog.Warn("wardynd: k8s substrate: could not delete a failed replace's config Secret; the run's teardown deletes it",
			slog.String("namespace", ns), slog.String("secret", name), slog.String("error", err.Error()))
	}
}

// removeProxyPod deletes pod, a proxy pod of runID as read, under its UID, waits until it is gone, and then deletes the
// replacement config Secret it mounted (replacementCfgSecret). A pod made under its name since is not it: the UID
// precondition refuses that delete with a conflict, and the wait ends on the other UID. nil is no pod, and nothing is
// removed.
//
// The Secret is deleted by its name, never found by a read: the chart's Role grants no get, list or watch on Secrets.
// Each replace writes a Secret of its own, under a name no other takes, and only the proxy pod that replace makes mounts
// it, so once pod is gone no proxy needs it, and a newer revive's Secret, written before its pod exists, is never named.
// A failed delete is logged, not returned: a second try would not find pod to name it. A replace that fails before its
// new pod is made deletes the Secret it wrote (deleteUnmountedCfgSecret); one that delete missed carries the run's
// label, and the run's teardown deletes it.
func (d *Driver) removeProxyPod(ctx context.Context, ns string, runID uuid.UUID, pod *corev1.Pod) error {
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
	if name := replacementCfgSecret(runID, pod); name != "" {
		if err := d.clientset.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
			slog.Warn("wardynd: k8s substrate: could not delete a removed proxy pod's config Secret; the run's teardown deletes it",
				slog.String("namespace", ns), slog.String("secret", name), slog.String("error", err.Error()))
		}
	}
	return nil
}

// replacementCfgSecret is the replacement proxy config Secret pod mounts, "" for none: a nil pod, or one whose config
// volume names any Secret but one of runID's replacements, as the run's own Secret that a created proxy mounts.
func replacementCfgSecret(runID uuid.UUID, pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == proxyConfigSecretVolumeName && v.Secret != nil && strings.HasPrefix(v.Secret.SecretName, secretName(runID)+"-r") {
			return v.Secret.SecretName
		}
	}
	return ""
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
// read again, once, under the same generation rule, and removed with its config Secret before the create is tried again.
func (d *Driver) createReplacementPod(ctx context.Context, ns string, runID uuid.UUID, pod *corev1.Pod, gen int) error {
	_, err := d.createPod(ctx, runID, componentProxy, pod)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	taken, err := d.proxyNotNewer(ctx, ns, runID, gen, true)
	if err != nil {
		return err
	}
	if err := d.removeProxyPod(ctx, ns, runID, taken); err != nil {
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
