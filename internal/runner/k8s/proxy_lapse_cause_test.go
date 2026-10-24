// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
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
