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

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// TestRunnerCreateSandbox_NoAgentPodWhileItsProxyRefuses pins the cause of a short k8s runner run
// losing its recording. A runner's proxy (self_lapse) binds its listener and then refuses every
// agent request with 503 until its first renew succeeds; this proxy stand-in is that window held
// open, its listener bound and every answer a 503 (TestSelfLapse_ClosedBeforeFirstRenew pins the
// real proxy's). A kubelet stand-in marks the proxy pod Ready by the readiness probe CreateSandbox
// declares on it. The agent pod must not be created while the proxy refuses: an agent started there
// has its requests refused, a short run's recording upload among them.
func TestRunnerCreateSandbox_NoAgentPodWhileItsProxyRefuses(t *testing.T) {
	var refused atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused.Add(1)
		http.Error(w, "wardyn: this run's egress is closed", http.StatusServiceUnavailable)
	}))
	defer px.Close()

	h := newRunnerHarness(t)
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
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
		ready := probeSucceeds(px, pod.Spec.Containers[0].ReadinessProbe)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: ready,
		}}
		return true, pod, nil
	})
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
		t.Fatalf("the agent pod was created while its proxy answered every request 503 (CreateSandbox: %v)", err)
	}
	if err == nil {
		t.Fatal("CreateSandbox succeeded while its proxy refused every request")
	}
}

// TestRunnerCreateSandbox_OlderProxyImageKeepsTheTCPProbe: a runner whose proxy image is a known
// release older than proxyReadyRelease answers proxy.ReadyPath 404, so probing there would fail
// every run at the start deadline. Its pods keep the TCP probe, and the driver warns once naming
// the image and the fix. A current release, a digest pin and a development tag are probed on
// proxy.ReadyPath.
func TestRunnerCreateSandbox_OlderProxyImageKeepsTheTCPProbe(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	const repo = "ghcr.io/cjohnstoniv/wardyn-proxy"
	for _, tc := range []struct {
		image string
		http  bool
	}{
		{repo + ":0.10.0-rc.1", false},
		{repo + ":0.9.8", false},
		{repo + ":" + proxyReadyRelease, true},
		{repo + ":0.10.0", true},
		{repo + "@sha256:" + strings.Repeat("a", 64), true},
		{"wardyn/wardyn-proxy:dev", true},
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
			probe := pod.Spec.Containers[0].ReadinessProbe
			if gotHTTP := probe.HTTPGet != nil && probe.HTTPGet.Path == proxy.ReadyPath; gotHTTP != tc.http || !tc.http && probe.TCPSocket == nil {
				t.Fatalf("%s: readiness probe %+v, want HTTP on the ready path = %v (else TCP)", tc.image, probe.ProbeHandler, tc.http)
			}
		}
		warned := strings.Count(buf.String(), "the proxy image is older than this runner")
		if tc.http && warned != 0 || !tc.http && (warned != 1 || !strings.Contains(buf.String(), tc.image)) {
			t.Fatalf("%s: warned %d times, want %d naming the image:\n%s", tc.image, warned, map[bool]int{true: 0, false: 1}[tc.http], buf.String())
		}
	}
}

// probeSucceeds runs probe as a kubelet does against the proxy stand-in: a TCP probe passes once
// the listener accepts, an HTTP probe only on a 2xx or 3xx answer at its path.
func probeSucceeds(px *httptest.Server, probe *corev1.Probe) bool {
	addr := px.Listener.Addr().String()
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
