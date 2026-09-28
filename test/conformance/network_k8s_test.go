// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// conformanceClientset builds a clientset the way the substrate's own config
// does (in-cluster first, then the default kubeconfig loading rules), plus the
// namespace runs land in (k8s.Config.withDefaults' own fallback when unset).
func conformanceClientset(t *testing.T) (kubernetes.Interface, string) {
	t.Helper()
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			t.Fatalf("kubeconfig: %v", err)
		}
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	ns := os.Getenv("WARDYN_K8S_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return cs, ns
}

// probeTarget is one destination the reachability probe dials. dns is a
// resolver address to ask for kubeName ("" = the pod's own resolv.conf);
// otherwise host:port is a TCP connect.
type probeTarget struct {
	name, host, port string
	dns              bool
}

// kubeName resolves on any cluster through its DNS service. A resolved name is
// the verdict: the query reached a resolver that forwards off the pod, which is
// a channel whatever the answer (the k8s form of TestL0_NoDNSExfil).
const kubeName = "kubernetes.default.svc.cluster.local"

// reachabilityScript reports every target as name=REACHED|BLOCKED, preceded by
// the tool and environment lines that make a BLOCKED mean something: a missing
// nc or an unset KUBERNETES_SERVICE_HOST blocks everything too.
func reachabilityScript(targets []probeTarget) string {
	var b strings.Builder
	b.WriteString("command -v nc >/dev/null 2>&1 && echo tool_nc=ok\n")
	b.WriteString("command -v nslookup >/dev/null 2>&1 && echo tool_nslookup=ok\n")
	b.WriteString("echo env_apiserver=$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT\n")
	for _, tg := range targets {
		cmd := fmt.Sprintf("nc -z -w3 %s %s", tg.host, tg.port)
		if tg.dns {
			cmd = "timeout 10 nslookup " + kubeName + " " + tg.host
		}
		fmt.Fprintf(&b, "if %s >/dev/null 2>&1; then echo %s=REACHED; else echo %s=BLOCKED; fi\n", cmd, tg.name, tg.name)
	}
	b.WriteString("echo done=1\n")
	return b.String()
}

// testAgentCannotReachAnythingButTheProxy is the kubernetes form of the docker
// L0 negatives (TestL0_MetadataUnreachable, TestL0_ProxyIsSoleEgressPath,
// TestL0_NoDNSExfil). This substrate proves L1, not L0 — the agent pod HAS a
// default route and its NetworkPolicy is what stops it — so each negative is a
// real dial rather than a routing-table read:
//
//	proxy        its own run's proxy on runner.ProxyListenPort — the POSITIVE
//	             control: if the one allowed peer is unreachable, every BLOCKED
//	             below proves nothing
//	apiserver    the control-plane Service (KUBERNETES_SERVICE_HOST:PORT)
//	metadata     169.254.169.254:80
//	public       1.1.1.1:80
//	peer_run     another run's proxy pod, on the port its own agent may use
//	cluster_pod  an ordinary pod in the same namespace listening on 8080
//	dns_cluster  the cluster DNS Service, asked for kubeName
//	dns_own      whatever resolver the agent pod is configured with
//
// Non-vacuous by construction: an UNCONFINED control pod in the same namespace
// runs the same probe first. apiserver, cluster_pod and both DNS targets must
// be REACHED from it (else the cluster cannot prove anything and the case
// fails); metadata and public are proven only where the control reaches them,
// and logged as unproven otherwise. The peer run's own agent must reach its
// proxy, so peer_run is a live listener.
func testAgentCannotReachAnythingButTheProxy(t *testing.T, r runner.Runner, agentImage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cs, ns := conformanceClientset(t)

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	create := func(what string) (runner.Sandbox, uuid.UUID) {
		spec := runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            agentImage,
			ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
			Labels:           map[string]string{"wardyn.conformance": "true"},
			// A LISTENING proxy: see testAgentCannotReachAPIServer's spec.
			ProxyConfig: runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "conformance"},
			// Small, because two runs are up at once: at the 2-CPU default the
			// second one does not fit a hosted runner's one-node cluster.
			Resources: runner.Resources{CPUMillis: 250, MemoryMiB: 128},
		}
		sb, err := r.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("CreateSandbox (%s): %v", what, err)
		}
		t.Cleanup(func() {
			stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			_ = r.StopSandbox(stopCtx, sb.Ref)
		})
		return sb, spec.RunID
	}
	subject, _ := create("subject")
	peer, peerRunID := create("peer run")

	peerProxy, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "wardyn.run-id=" + peerRunID.String() + ",wardyn.component=proxy",
	})
	if err != nil || len(peerProxy.Items) != 1 || peerProxy.Items[0].Status.PodIP == "" {
		t.Fatalf("the peer run's proxy pod and its IP (err %v, %d pods)", err, len(peerProxy.Items))
	}
	peerIP := peerProxy.Items[0].Status.PodIP
	dnsSvc, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil || dnsSvc.Spec.ClusterIP == "" {
		t.Fatalf("the cluster DNS Service kube-system/kube-dns: %v", err)
	}
	dnsIP := dnsSvc.Spec.ClusterIP
	port := fmt.Sprint(runner.ProxyListenPort)

	// ── the unconfined control ─────────────────────────────────────────────
	ctlTargets := []probeTarget{
		{name: "apiserver", host: "$KUBERNETES_SERVICE_HOST", port: "$KUBERNETES_SERVICE_PORT"},
		{name: "metadata", host: "169.254.169.254", port: "80"},
		{name: "public", host: "1.1.1.1", port: "80"},
		{name: "cluster_pod", host: "127.0.0.1", port: "8080"},
		{name: "dns_cluster", host: dnsIP, dns: true},
		{name: "dns_own", host: "", dns: true},
	}
	ctlScript := "mkdir -p /tmp/www && echo ok > /tmp/www/index.html && httpd -p 8080 -h /tmp/www || exit 1\n" +
		reachabilityScript(ctlTargets) + "exec sleep 3600\n"
	ctlIP, ctl := startControlPod(t, ctx, cs, ns, agentImage, ctlScript)
	for _, k := range []string{"apiserver", "cluster_pod", "dns_cluster", "dns_own"} {
		if ctl[k] != "REACHED" {
			t.Fatalf("an unconfined pod in namespace %s could not reach %s (%s) — on this cluster the agent's BLOCKED there would prove nothing", ns, k, ctl[k])
		}
	}

	// ── the peer's listener is live ────────────────────────────────────────
	peerCtl := conformance.ExecKeyValues(t, ctx, r, peer.Ref, reachabilityScript([]probeTarget{{name: "proxy", host: "wardyn-proxy", port: port}}))
	if peerCtl["proxy"] != "REACHED" {
		t.Fatalf("the peer run's own agent could not reach its proxy (%s), so its proxy is not a live listener to be blocked from", peerCtl["proxy"])
	}

	// ── the agent ──────────────────────────────────────────────────────────
	got := conformance.ExecKeyValues(t, ctx, r, subject.Ref, reachabilityScript([]probeTarget{
		{name: "proxy", host: "wardyn-proxy", port: port},
		{name: "apiserver", host: "$KUBERNETES_SERVICE_HOST", port: "$KUBERNETES_SERVICE_PORT"},
		{name: "metadata", host: "169.254.169.254", port: "80"},
		{name: "public", host: "1.1.1.1", port: "80"},
		{name: "peer_run", host: peerIP, port: port},
		{name: "cluster_pod", host: ctlIP, port: "8080"},
		{name: "dns_cluster", host: dnsIP, dns: true},
		{name: "dns_own", host: "", dns: true},
	}))
	if got["done"] != "1" || got["tool_nc"] != "ok" || got["tool_nslookup"] != "ok" || got["env_apiserver"] == ":" {
		t.Fatalf("the probe could not run (done=%q tool_nc=%q tool_nslookup=%q env_apiserver=%q) — an environment problem, not a verdict either way",
			got["done"], got["tool_nc"], got["tool_nslookup"], got["env_apiserver"])
	}
	if got["proxy"] != "REACHED" {
		t.Fatalf("the agent could not reach its own proxy on %s (%s) — the positive control: with the one allowed peer unreachable, the BLOCKED verdicts below prove nothing", port, got["proxy"])
	}
	for _, k := range []string{"apiserver", "metadata", "public", "peer_run", "cluster_pod", "dns_cluster", "dns_own"} {
		switch {
		case got[k] != "BLOCKED":
			t.Errorf("the agent reached %s (%s) — L1 breach: its NetworkPolicy must allow the run's own proxy and nothing else", k, got[k])
		case (k == "metadata" || k == "public") && ctl[k] != "REACHED":
			t.Logf("%s: BLOCKED, but the unconfined control could not reach it either, so this cluster does not prove it", k)
		}
	}
}

// startControlPod runs script in an UNCONFINED pod (no wardyn labels, so no
// run NetworkPolicy selects it; PSS-restricted security context, so it is
// admitted where runs are) and returns its IP and the key=value lines the
// script logged up to done=1.
func startControlPod(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns, image, script string) (string, map[string]string) {
	t.Helper()
	name := "wardyn-conformance-netctl-" + uuid.NewString()[:8]
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			Containers: []corev1.Container{{
				Name:            "control",
				Image:           image,
				Command:         []string{"sh", "-c", script},
				SecurityContext: restrictedContainer(1000),
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the control pod: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	})
	var logs string
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get the control pod: %v", err)
		}
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			t.Fatalf("the control pod exited (%s) before reporting", p.Status.Phase)
		}
		if p.Status.Phase == corev1.PodRunning {
			b, _ := cs.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{Container: "control"}).DoRaw(ctx)
			if logs = string(b); strings.Contains(logs, "done=1") {
				t.Logf("control pod %s (%s):\n%s", name, p.Status.PodIP, logs)
				return p.Status.PodIP, parseLines(logs)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the control pod never reported: %v\n%s", ctx.Err(), logs)
		case <-time.After(2 * time.Second):
		}
	}
}

// parseLines is the key=value reader for a pod log (the same rules as
// conformance.ExecKeyValues').
func parseLines(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k != "" && !strings.ContainsAny(k, " \t") {
			out[k] = v
		}
	}
	return out
}

// restrictedContainer is a container security context the PSS "restricted"
// profile admits, running as uid.
func restrictedContainer(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptr(uid),
		RunAsNonRoot:             ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func ptr[T any](v T) *T { return &v }
