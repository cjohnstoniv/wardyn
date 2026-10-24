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

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// TestRunnerCreateSandbox_ShortRunRecordingLandsOnceTheProxyServes: a k8s runner's proxy binds
// its listener at once but refuses every agent request until its first renew succeeds, which can
// be a retry later. The agent pod must not exist before the proxy serves: a short run ends inside
// that window, and wardyn-rec's recording upload, its last act, is refused and lost.
//
// A kubelet stand-in marks the proxy pod Ready by the readiness probe CreateSandbox declares on it,
// against a proxy stand-in that answers as wardyn-proxy does (TestSelfLapse_ReadyPathFollowsTheGate
// pins those answers on the real proxy): 503 to everything while closed, 204 on proxy.ReadyPath
// once open. The short run's upload is sent the moment CreateSandbox returns, as the agent's start.
func TestRunnerCreateSandbox_ShortRunRecordingLandsOnceTheProxyServes(t *testing.T) {
	var open atomic.Bool
	var landed atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !open.Load():
			http.Error(w, "wardyn: this run's egress is closed", http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && r.URL.Path == proxy.ReadyPath:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/wardyn/v1/recordings/"):
			landed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
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

	// The first renew failed; its retry opens the proxy a little later.
	go func() {
		time.Sleep(300 * time.Millisecond)
		open.Store(true)
	}()
	spec := testSandboxSpec()
	spec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, px.URL+"/wardyn/v1/recordings/"+spec.RunID.String(), strings.NewReader("{\"version\": 2}\n"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if landed.Load() != 1 {
		t.Fatalf("the short run's recording upload got %d and no recording landed: the agent started before its proxy served", resp.StatusCode)
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
