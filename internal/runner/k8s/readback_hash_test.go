// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A runner's create rewrites the proxy config (runnerProxyConfig) before the Secret: the hash it
// answers is of the Secret's bytes, which the proxy stages unchanged and loads, not of the built
// config; the proxy address it answers is the agent's wardyn-proxy host alias.
func TestRunnerCreateSandbox_AnswersTheSecretBytesHashAndProxyAddr(t *testing.T) {
	px := httptest.NewServer(http.NotFoundHandler())
	defer px.Close()
	h := newRunnerHarness(t)
	h.d.cfg.ProxyImage = "registry.example/wardyn-proxy:pinned"
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	spec := testSandboxSpec()
	spec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sb, err := h.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := h.cs.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var loaded []byte
	for _, s := range secrets.Items {
		if s.Name == secretName(spec.RunID) {
			loaded = s.Data[proxyConfigSecretKey]
		}
	}
	if len(loaded) == 0 || sb.ProxyConfigSHA256 != proxy.ConfigSHA256(loaded) {
		t.Fatalf("create hash %q, want the hash of the %d Secret bytes the proxy loads", sb.ProxyConfigSHA256, len(loaded))
	}
	built, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if sb.ProxyConfigSHA256 == proxy.ConfigSHA256(built) {
		t.Fatal("the create hash is of the built config, before runnerProxyConfig rewrote it")
	}

	pods, err := h.cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pinned := ""
	for _, p := range pods.Items {
		if p.Name == agentPodName(spec.RunID) {
			for _, ha := range p.Spec.HostAliases {
				for _, n := range ha.Hostnames {
					if n == "wardyn-proxy" {
						pinned = ha.IP
					}
				}
			}
		}
	}
	if pinned == "" || sb.ProxyAddr != pinned {
		t.Fatalf("create proxy address %q, want the agent's wardyn-proxy alias %q", sb.ProxyAddr, pinned)
	}
}
