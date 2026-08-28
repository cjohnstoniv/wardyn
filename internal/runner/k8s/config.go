// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// serviceAccountNamespaceFile is where the kubelet projects a pod's own
// namespace when it mounts the default service account token — the standard
// in-cluster "what namespace am I in" source client-go consumers read
// directly (there is no dedicated client-go helper for it).
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// loadRestConfig is client-go's standard config loading: in-cluster config
// when running as a pod, else the kubeconfig loading rules (KUBECONFIG env,
// then ~/.kube/config) — the same chain `kubectl` and every other client-go
// consumer uses, so an operator's existing kubeconfig setup just works.
//
// L5: an out-of-cluster kubeconfig's server host may be unreachable from
// wherever wardynd actually runs (a laptop-local kind cluster, a VPN-only
// endpoint, ...) — in that case the boot-time egress canary (canary.go)
// never gets a verdict at all and construction fails with a PERMANENT
// canaryIndeterminate: no operator override restores boot, because there is
// no such thing as "opt out of an unreachable apiserver". This is a real,
// expected failure mode for out-of-cluster kubeconfig use, not a bug — the
// remedy is a reachable rest.Config (run in-cluster, or point kubeconfig at
// a cluster this process can actually dial), not a flag.
//
// Do not conflate that PERMANENT shape with the one canaryIndeterminate DOES
// have a partial override for (B1): a canary pod that reaches Running and
// then exits exactly 1 is NOT "never got a verdict" — the apiserver was
// reachable, the pod scheduled and ran, only its own connect attempt was
// refused, which is what an ambient (platform-applied) default-deny
// NetworkPolicy looks like from here. WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1
// (Config.AckAmbientDefaultDeny) acknowledges exactly that narrow shape and
// lets boot proceed anyway — see canary.go's runEgressCanary. An unreachable
// apiserver still has no such override; this doc's "no operator override
// restores boot" claim is scoped to that case, not to every indeterminate.
func loadRestConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: load kubeconfig (no in-cluster config either): %w", err)
	}
	return cfg, nil
}

// resolveNamespace implements WARDYN_K8S_NAMESPACE's documented fallback
// chain: an explicit env value wins; else the pod's own namespace via the
// serviceaccount projection; else "default" (e.g. running out-of-cluster
// against a kubeconfig with no namespace context).
func resolveNamespace(envVal string) string {
	if envVal != "" {
		return envVal
	}
	if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}

// apiserverHostPort extracts host:port from cfg.Host (e.g.
// "https://10.96.0.1:443") for the egress canary's dial target — the
// substrate's OWN rest.Config, never DNS, never a route off-cluster (see the
// package doc's canary contract).
func apiserverHostPort(cfg *rest.Config) (string, error) {
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return "", fmt.Errorf("k8s: parse apiserver host %q: %w", cfg.Host, err)
	}
	hostPort := u.Host
	if hostPort == "" {
		// A bare "host:port" with no scheme parses into u.Opaque/u.Path rather
		// than u.Host; rest.Config.Host is documented as a URL but tolerate the
		// bare form defensively rather than fail closed on a working config.
		hostPort = strings.TrimPrefix(cfg.Host, "//")
	}
	if hostPort == "" {
		return "", fmt.Errorf("k8s: apiserver host %q has no host:port component", cfg.Host)
	}
	if !strings.Contains(hostPort, ":") {
		// rest.Config.Host omits the port when it's the scheme default (443);
		// DialTimeout requires an explicit port.
		hostPort += ":443"
	}
	return hostPort, nil
}
