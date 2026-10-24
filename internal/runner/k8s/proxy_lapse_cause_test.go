// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// TestRunnerCreateSandbox_NoAgentPodWhileItsProxyRefuses pins the cause of a short k8s runner run
// losing its recording. A runner's proxy (self_lapse) refuses every agent request until its first
// renew succeeds, and so listens only from then (TestSelfLapse_ListensOnlyOnceOpen pins the real
// proxy's); this proxy stand-in is that window held open, nothing listening at its address. A
// kubelet stand-in marks the proxy pod Ready by the readiness probe CreateSandbox declares on it.
// The agent pod must not be created before the proxy serves: an agent started there has its
// requests refused, a short run's recording upload among them.
func TestRunnerCreateSandbox_NoAgentPodWhileItsProxyRefuses(t *testing.T) {
	h := newRunnerHarness(t)
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, unusedAddr(t), nil)
	installAgentRunningReactor(t, h.cs)
	var agentCreated atomic.Bool
	h.cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "wardyn-agent-") {
			agentCreated.Store(true)
		}
		return false, nil, nil
	})

	spec := testSandboxSpec()
	spec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := h.d.CreateSandbox(ctx, spec)
	if agentCreated.Load() {
		t.Fatalf("the agent pod was created before its proxy served (CreateSandbox: %v)", err)
	}
	if err == nil {
		t.Fatal("CreateSandbox succeeded while its proxy never served")
	}
}

// TestRunnerCreateSandbox_PinnedOlderProxyStarts: an older proxy named by digest or by a tag that
// is not a release number starts as it did before this release. The runner cannot read its
// release; the proxy stand-in is such a proxy, open, which listens at once and knows no readiness
// route, answering 404 to every path. Its pod must read Ready and CreateSandbox succeed well inside
// the start deadline.
func TestRunnerCreateSandbox_PinnedOlderProxyStarts(t *testing.T) {
	px := httptest.NewServer(http.NotFoundHandler())
	defer px.Close()
	for _, image := range []string{"ghcr.io/cjohnstoniv/wardyn-proxy@sha256:" + strings.Repeat("b", 64), "registry.example/wardyn-proxy:pinned"} {
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = image
		h.d.cfg.Sink = &fakeSink{h: h}
		h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
		installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
		installAgentRunningReactor(t, h.cs)
		spec := testSandboxSpec()
		spec.RunID = h.runID
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := h.d.CreateSandbox(ctx, spec)
		cancel()
		if err != nil {
			t.Fatalf("%s: an older proxy pinned this way never started its run: %v", image, err)
		}
	}
}

// TestRunnerCreateSandbox_OlderProxyImageWarnsOnce: every proxy pod is probed by a connect, which
// this release's proxy passes only once it serves and an older one as soon as it listens, as
// before. A known release older than proxyReadyRelease also gets one warning naming the image and
// the fix; a current release, a digest pin and a development tag get none.
func TestRunnerCreateSandbox_OlderProxyImageWarnsOnce(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	const repo = "ghcr.io/cjohnstoniv/wardyn-proxy"
	for _, tc := range []struct {
		image string
		older bool
	}{
		{repo + ":0.10.0-rc.1", true},
		{repo + ":0.9.8", true},
		{repo + ":" + proxyReadyRelease, false},
		{repo + ":0.10.0", false},
		{repo + "@sha256:" + strings.Repeat("a", 64), false},
		{"wardyn/wardyn-proxy:dev", false},
	} {
		buf.Reset()
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = tc.image
		h.d.cfg.Sink = &fakeSink{h: h}
		h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
		installProxyIPReactor(t, h.cs, "10.244.0.7")
		installAgentRunningReactor(t, h.cs)
		for range 2 {
			spec := testSandboxSpec()
			if _, err := h.d.CreateSandbox(context.Background(), spec); err != nil {
				t.Fatalf("%s: CreateSandbox: %v", tc.image, err)
			}
			pod, err := h.cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if probe := pod.Spec.Containers[0].ReadinessProbe; probe == nil || probe.TCPSocket == nil || probe.HTTPGet != nil {
				t.Fatalf("%s: readiness probe %+v, want a TCP connect", tc.image, probe)
			}
		}
		warned := strings.Count(buf.String(), "the proxy image is older than this runner")
		if !tc.older && warned != 0 || tc.older && (warned != 1 || !strings.Contains(buf.String(), tc.image)) {
			t.Fatalf("%s: warned %d times, want %d naming the image:\n%s", tc.image, warned, map[bool]int{true: 1, false: 0}[tc.older], buf.String())
		}
	}
}

// installProbedProxyReactor is the kubelet stand-in: a get of the proxy pod reads it Running, and
// Ready only when the readiness probe CreateSandbox declares on it succeeds against addr. seen, when
// set, sees each probe's result.
func installProbedProxyReactor(h *runnerHarness, addr string, seen func(ready bool)) {
	h.cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga := action.(clienttesting.GetAction)
		if !strings.HasPrefix(ga.GetName(), "wardyn-proxy-") {
			return false, nil, nil
		}
		obj, err := h.cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.PodIP, pod.Status.Phase = "10.244.0.7", corev1.PodRunning
		ready := probeSucceeds(addr, pod.Spec.Containers[0].ReadinessProbe)
		if seen != nil {
			seen(ready)
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: ready,
		}}
		return true, pod, nil
	})
}

// unusedAddr is a loopback address nothing listens on.
func unusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// probeSucceeds runs probe as a kubelet does against the proxy stand-in at addr: a TCP probe passes
// once the listener accepts, an HTTP probe only on a 2xx or 3xx answer at its path.
func probeSucceeds(addr string, probe *corev1.Probe) bool {
	switch {
	case probe == nil:
		return true
	case probe.HTTPGet != nil:
		resp, err := http.Get("http://" + addr + probe.HTTPGet.Path)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	case probe.TCPSocket != nil:
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	return false
}
