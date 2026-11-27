// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// testExtraContainerReachesOnlyTheProxy proves that an extra container in a run's sandbox is held like the
// agent. It is built like testAgentCannotReachAnythingButTheProxy: an unconfined control pod first, then the
// same targets and verdict words, but the probe runs as the extra container's command and its verdicts are
// read from that container's log. The extra also serves 127.0.0.1:8081 and writes a file in the work folder,
// and the case proves the agent reaches that listener and reads that file.
func testExtraContainerReachesOnlyTheProxy(t *testing.T, r runner.Runner, agentImage string) {
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
	if !caps.ExtraContainers {
		t.Skip("the runner does not run extra containers")
	}
	create := func(what string, extras []runner.ExtraContainer) (runner.Sandbox, uuid.UUID) {
		spec := runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            agentImage,
			ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
			Labels:           map[string]string{"wardyn.conformance": "true"},
			ProxyConfig:      runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "conformance"},
			// Small, because two runs are up at once and the extra's share comes out of the subject's.
			Resources:  runner.Resources{CPUMillis: 750, MemoryMiB: 512},
			Containers: extras,
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

	dnsSvc, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil || dnsSvc.Spec.ClusterIP == "" {
		t.Fatalf("the cluster DNS Service kube-system/kube-dns: %v", err)
	}
	dnsIP := dnsSvc.Spec.ClusterIP
	port := fmt.Sprint(runner.ProxyListenPort)

	// The peer run, whose proxy the extra must not reach.
	peer, peerRunID := create("peer run", nil)
	peerProxy, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "wardyn.run-id=" + peerRunID.String() + ",wardyn.component=proxy",
	})
	if err != nil || len(peerProxy.Items) != 1 || peerProxy.Items[0].Status.PodIP == "" {
		t.Fatalf("the peer run's proxy pod and its IP (err %v, %d pods)", err, len(peerProxy.Items))
	}
	peerIP := peerProxy.Items[0].Status.PodIP

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
	ctlIP, ctl := startControlPod(t, ctx, cs, ns, agentImage, ctlScript, nil)
	for _, k := range []string{"apiserver", "cluster_pod", "dns_cluster", "dns_own"} {
		if ctl[k] != "REACHED" {
			t.Fatalf("an unconfined pod in namespace %s could not reach %s (%s) — on this cluster the extra container's BLOCKED there would prove nothing", ns, k, ctl[k])
		}
	}

	// ── the peer's listener is live ────────────────────────────────────────
	peerCtl := conformance.ExecKeyValues(t, ctx, r, peer.Ref, reachabilityScript([]probeTarget{{name: "proxy", host: "wardyn-proxy", port: port}}))
	if peerCtl["proxy"] != "REACHED" {
		t.Fatalf("the peer run's own agent could not reach its proxy (%s), so its proxy is not a live listener to be blocked from", peerCtl["proxy"])
	}

	// ── the subject: the probe is the extra container's command ────────────
	targets := []probeTarget{
		{name: "proxy", host: "wardyn-proxy", port: port},
		{name: "apiserver", host: "$KUBERNETES_SERVICE_HOST", port: "$KUBERNETES_SERVICE_PORT"},
		{name: "metadata", host: "169.254.169.254", port: "80"},
		{name: "public", host: "1.1.1.1", port: "80"},
		{name: "peer_run", host: peerIP, port: port},
		{name: "cluster_pod", host: ctlIP, port: "8080"},
		{name: "dns_cluster", host: dnsIP, dns: true},
		{name: "dns_own", host: "", dns: true},
	}
	extraScript := "mkdir -p /tmp/www && echo ok > /tmp/www/index.html && (httpd -p 127.0.0.1:8081 -h /tmp/www || httpd -p 8081 -h /tmp/www)\n" +
		"echo extra > /home/agent/work/mc-proof\n" + reachabilityScript(targets) + "exec sleep 3600\n"
	subject, subjectRunID := create("subject", []runner.ExtraContainer{{
		Name: "probe", Image: agentImage, CPUMillis: 200, MemoryMiB: 128,
		Command: []string{"sh", "-c", extraScript},
	}})

	agentPods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "wardyn.run-id=" + subjectRunID.String() + ",wardyn.component=agent",
	})
	if err != nil || len(agentPods.Items) != 1 {
		t.Fatalf("the subject run's agent pod (err %v, %d pods)", err, len(agentPods.Items))
	}
	agentPod := agentPods.Items[0].Name

	var logs string
	var got map[string]string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		b, _ := cs.CoreV1().Pods(ns).GetLogs(agentPod, &corev1.PodLogOptions{Container: "probe"}).DoRaw(ctx)
		if logs = string(b); strings.Contains(logs, "done=1") {
			got = parseLines(logs)
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("the extra container never reported done=1 in its log:\n%s", logs)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("extra container probe (%s):\n%s", agentPod, logs)

	if got["done"] != "1" || got["tool_nc"] != "ok" || got["tool_nslookup"] != "ok" || got["env_apiserver"] == ":" {
		t.Fatalf("the probe could not run (done=%q tool_nc=%q tool_nslookup=%q env_apiserver=%q) — an environment problem, not a verdict either way",
			got["done"], got["tool_nc"], got["tool_nslookup"], got["env_apiserver"])
	}
	if got["proxy"] != "REACHED" {
		t.Fatalf("the extra container could not reach its run's proxy on %s (%s) — the positive control: with the one allowed peer unreachable, the BLOCKED verdicts below prove nothing", port, got["proxy"])
	}
	for _, k := range []string{"apiserver", "metadata", "public", "peer_run", "cluster_pod", "dns_cluster", "dns_own"} {
		switch {
		case got[k] != "BLOCKED":
			t.Errorf("the extra container reached %s (%s) — it must be held like the agent: its pod's NetworkPolicy allows the run's own proxy and nothing else", k, got[k])
		case (k == "metadata" || k == "public") && ctl[k] != "REACHED":
			t.Logf("%s: BLOCKED, but the unconfined control could not reach it either, so this cluster does not prove it", k)
		}
	}

	// ── what the two share: the loopback and the work folder ───────────────
	agentScript := `for i in $(seq 1 30); do [ -s /home/agent/work/mc-proof ] && break; sleep 1; done
if wget -q -T 5 -O - http://127.0.0.1:8081/ 2>/dev/null | grep -q '^ok$'; then echo loopback=REACHED; else echo loopback=BLOCKED; fi
echo "shared=$(cat /home/agent/work/mc-proof 2>/dev/null)"
echo done=1
`
	shared := conformance.ExecKeyValues(t, ctx, r, subject.Ref, agentScript)
	if shared["loopback"] != "REACHED" {
		t.Errorf("the agent could not reach the extra container's listener on 127.0.0.1:8081 (%s)", shared["loopback"])
	}
	if shared["shared"] != "extra" {
		t.Errorf("the agent read %q from the file the extra container wrote in the work folder, want %q", shared["shared"], "extra")
	}
}
