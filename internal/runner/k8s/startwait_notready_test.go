// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// notReadyLine is the fragment proxyNotReadyDetail adds; the readiness probe is named in it.
const notReadyLine = "readiness probe"

// A proxy container that is Running but never Ready when the start deadline passes gets the extra
// line naming the readiness probe and the image.
func TestWaitPodIP_RunningButNotReadyNamesTheReadinessProbe(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond, ProxyImage: "ghcr.io/cjohnstoniv/wardyn-proxy@sha256:abc"})
	const podName = "wardyn-proxy-running"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
			Name: proxyContainerName, Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}}
	})
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), notReadyLine) ||
		!strings.Contains(err.Error(), "wardyn-proxy@sha256:abc") {
		t.Fatalf("err = %v; want a start-timeout error naming the readiness probe and the proxy image", err)
	}
}

// The line is only for Running and not Ready: a Ready proxy, a Waiting one and a missing one get none.
func TestProxyNotReadyDetailOnlyForRunningNotReady(t *testing.T) {
	status := func(cs ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: cs}}
	}
	running := &corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want bool
	}{
		"running, not ready": {status(corev1.ContainerStatus{Name: proxyContainerName, State: *running}), true},
		"running, ready":     {status(corev1.ContainerStatus{Name: proxyContainerName, Ready: true, State: *running}), false},
		"waiting": {status(corev1.ContainerStatus{Name: proxyContainerName, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}), false},
		"terminated": {status(corev1.ContainerStatus{Name: proxyContainerName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}), false},
		"another container running, not ready": {status(corev1.ContainerStatus{Name: "other", State: *running}), false},
		"no container status":                  {status(), false},
	} {
		got := proxyNotReadyDetail(tc.pod, "img")
		if (got != "") != tc.want || (tc.want && !strings.Contains(got, notReadyLine)) {
			t.Errorf("%s: detail = %q, want a line: %v", name, got, tc.want)
		}
	}
}

// A proxy that is not Running keeps the existing detail and gains no readiness-probe line.
func TestWaitPodIP_WaitingProxyHasNoReadinessProbeLine(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond})
	const podName = "wardyn-proxy-waiting"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}}
	})
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), notReadyLine) ||
		!strings.Contains(err.Error(), proxyContainerName+": ContainerCreating") {
		t.Fatalf("err = %v; want the container reason and no readiness-probe line", err)
	}
}
