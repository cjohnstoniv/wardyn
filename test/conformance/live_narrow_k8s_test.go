// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// live_narrow_k8s_test.go proves a live egress edit's in-place narrowing on a real cluster: the proxy
// pod takes new denies through its own exec, drops open connections, keeps its identity, and keeps the
// list across a container restart. TestK8sNodeRestartKeepsLiveDenies is the node-restart half. It has a
// nightly job of its own, because the conformance job gates releases and a node reboot is slow.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// narrowPolicyHosts are the two hosts the sandbox's policy allows; the narrowing denies the first.
const (
	narrowedHost = "a.example"
	otherHost    = "b.example"
)

// deniedBy is the reason header a refused request carries.
const deniedBy = "x-wardyn-egress-reason: policy:denied"

type narrowSandbox struct {
	sb   runner.Sandbox
	spec runner.SandboxSpec
	pod  corev1.Pod // the proxy pod as created
	base []byte     // the config the proxy was started from
}

// newNarrowSandbox creates a sandbox whose policy allows both hosts, and records its proxy pod.
func newNarrowSandbox(t *testing.T, r runner.Runner, cs kubernetes.Interface, ns, agentImage string) narrowSandbox {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	pc := conformance.LoadableProxyConfig()
	pc.Policy.AllowedDomains = append(pc.Policy.AllowedDomains, narrowedHost, otherHost)
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:      pc,
		Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_ = r.KillSandbox(stopCtx, sb.Ref)
	})
	base, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, "wardyn-proxy-"+spec.RunID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	return narrowSandbox{sb: sb, spec: spec, pod: *pod, base: base}
}

// secretVersion is the resourceVersion of the run's proxy config Secret.
func secretVersion(t *testing.T, cs kubernetes.Interface, ns string, runID uuid.UUID) string {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), "wardyn-proxy-cfg-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy config Secret: %v", err)
	}
	return s.ResourceVersion
}

// probeDone is the line proxyAnswer's script prints after nc returns, whatever nc did.
const probeDone = "probe-done"

// proxyAnswer is what the agent gets back from the proxy for an https CONNECT to host, lower-cased,
// followed by probeDone.
func proxyAnswer(t *testing.T, r runner.Runner, ref, host string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := fmt.Sprintf(`printf 'CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n' | nc -w 5 wardyn-proxy 3128; echo %s`, host, host, probeDone)
	sess, err := r.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("ExecStream (ask the proxy for %s): %v", host, err)
	}
	defer func() { _ = sess.Close() }()
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	out, err := io.ReadAll(sess.Stdout)
	if err != nil {
		t.Fatalf("read the proxy's answer for %s: %v", host, err)
	}
	return strings.ToLower(string(out))
}

// holdConnection holds a connection from the agent to its proxy. gone closes on heldScript's first gone=
// line, and failed on its first observer-error= line. The end of the stream closes neither, so it never
// passes for a close. heldAt is when held=1 was read. Every line the script printed is logged at cleanup.
func holdConnection(t *testing.T, r runner.Runner, ref string) (gone, failed <-chan struct{}, heldAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	sess, err := r.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", heldScript}})
	if err != nil {
		cancel()
		t.Fatalf("ExecStream (hold a connection to the proxy): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	var mu sync.Mutex
	var lines []string
	held, goneCh, failedCh, ended := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		var sawHeld, sawGone, sawFailed bool
		sc := bufio.NewScanner(sess.Stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
			switch {
			case line == "held=1" && !sawHeld:
				sawHeld = true
				heldAt = time.Now()
				close(held)
			case strings.HasPrefix(line, "gone=") && !sawGone:
				sawGone = true
				close(goneCh)
			case strings.HasPrefix(line, "observer-error=") && !sawFailed:
				sawFailed = true
				close(failedCh)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			t.Logf("held-connection observer: %s", l)
		}
		select {
		case <-failedCh:
			t.Error("the held-connection observer failed (its lines are above)")
		default:
		}
	})
	select {
	case <-held:
	case <-failedCh:
		t.Fatal("the held-connection observer failed before the connection was held (its lines are logged below)")
	case <-ended:
		t.Fatal("the connection from the agent to its proxy did not stay up for 2 s: an environment problem, not a verdict")
	case <-time.After(time.Minute):
		t.Fatal("the held-connection script never reported: an environment problem, not a verdict")
	}
	return goneCh, failedCh, heldAt
}

func narrower(t *testing.T, r runner.Runner) runner.ProxyNarrower {
	t.Helper()
	n, ok := r.(runner.ProxyNarrower)
	if !ok {
		t.Fatalf("%T does not narrow a proxy in place", r)
	}
	return n
}

// liveProxyNarrow: the proxy of a live sandbox takes a deny in place. It keeps its pod, its address and
// its config Secret, refuses the host at once, drops the connections it held, and keeps the list across
// a restart of its container. A pod started without the live deny directory is refused, with no exec.
func liveProxyNarrow(t *testing.T, r runner.Runner, cs kubernetes.Interface, ns, agentImage string) {
	t.Helper()
	n := narrower(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 1. A sandbox that allows both hosts. Its proxy pod's identity is recorded.
	s := newNarrowSandbox(t, r, cs, ns, agentImage)
	uid, ip, version := s.pod.UID, s.pod.Status.PodIP, secretVersion(t, cs, ns, s.spec.RunID)

	// 2. A connection from the agent to the proxy, held open.
	closed, observerFailed, heldAt := holdConnection(t, r, s.sb.Ref)

	// 3. The proxy's state: the config it enforces is the one the driver handed it, with no list.
	st, err := n.ProxyNarrowState(ctx, s.sb.Ref)
	if err != nil {
		t.Fatalf("ProxyNarrowState: %v", err)
	}
	wantBase, err := proxy.LiveDenyBaseSHA256(s.base)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("narrow hashes: sandbox.ProxyConfigSHA256=%s probe.ConfigSHA256=%s probe.BaseSHA256=%s want.BaseSHA256=%s",
		s.sb.ProxyConfigSHA256, st.ConfigSHA256, st.BaseSHA256, wantBase)
	if st.ConfigSHA256 != s.sb.ProxyConfigSHA256 || st.BaseSHA256 != wantBase || len(st.LiveDenies) != 0 {
		t.Fatalf("probe = %+v; want the sandbox's config hash %s, base %s and no list", st, s.sb.ProxyConfigSHA256, wantBase)
	}

	// 4. Narrow it. The answer's hash is that of the launch config with the list. A connection already
	// gone, or an observer already failed, before the narrowing is never a pass.
	select {
	case <-closed:
		t.Fatal("held connection never held: the observer saw it go before the narrowing")
	case <-observerFailed:
		t.Fatal("the held-connection observer failed before the narrowing")
	default:
	}
	// The agent-facing http.Server that proxy.newServer builds has ReadHeaderTimeout: 30 * time.Second. It
	// closes a connection that has sent no request on its own, as the held one has not, and that close would
	// look like a narrowing's. So the narrowing must be sent well inside that window, or the check says nothing.
	sentAfter := time.Since(heldAt)
	t.Logf("narrow sent %s after hold", sentAfter)
	if sentAfter >= 20*time.Second {
		t.Fatalf("inconclusive: narrow sent %s after hold, inside the 30 s ReadHeaderTimeout window", sentAfter)
	}
	after, err := n.NarrowProxy(ctx, s.sb.Ref, []string{narrowedHost})
	t.Logf("narrow exec: answer=%+v err=%v", after, err)
	if err != nil {
		t.Fatalf("NarrowProxy: %v", err)
	}
	narrowed, err := proxy.WithLiveDenies(s.base, []string{narrowedHost})
	if err != nil {
		t.Fatal(err)
	}
	if after.ConfigSHA256 != proxy.ConfigSHA256(narrowed) || !slices.Equal(after.LiveDenies, []string{narrowedHost}) || !after.Changed {
		t.Fatalf("NarrowProxy = %+v; want hash %s, list [%s], changed", after, proxy.ConfigSHA256(narrowed), narrowedHost)
	}

	// 5. The held connection is gone within 2 s. This is checked first: the proxy drops a silent
	// connection on its own after 30 s, so a later check could pass with the narrowing closing nothing.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("the connection the agent held to its proxy was still up 2 s after the narrowing")
	}

	// 6. The agent is refused the narrowed host and not the other.
	assertNarrowed(t, r, s.sb.Ref)

	// 7. The pod, its address and its config Secret are as they were.
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, s.pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod again: %v", err)
	}
	if pod.UID != uid || pod.Status.PodIP != ip {
		t.Errorf("proxy pod uid/ip = %s/%s, was %s/%s: the proxy was replaced", pod.UID, pod.Status.PodIP, uid, ip)
	}
	if got := secretVersion(t, cs, ns, s.spec.RunID); got != version {
		t.Errorf("config Secret resourceVersion = %s, was %s", got, version)
	}

	t.Run("ProxyContainerRestart", func(t *testing.T) {
		cluster := os.Getenv("WARDYN_TEST_K8S_KIND_CLUSTER")
		if cluster == "" {
			t.Skip("set WARDYN_TEST_K8S_KIND_CLUSTER to the kind cluster name to restart the proxy container")
		}
		restartProxyContainer(t, cs, ns, s.pod.Name)
		again, err := n.ProxyNarrowState(ctx, s.sb.Ref)
		if err != nil {
			t.Fatalf("ProxyNarrowState after the restart: %v", err)
		}
		if !slices.Equal(again.LiveDenies, []string{narrowedHost}) || again.ConfigSHA256 != after.ConfigSHA256 {
			t.Errorf("after a container restart the proxy holds %+v; want the list %v at hash %s", again, []string{narrowedHost}, after.ConfigSHA256)
		}
		assertNarrowed(t, r, s.sb.Ref)
	})

	t.Run("PodWithoutTheDirectoryIsRefused", func(t *testing.T) {
		// A proxy pod an older server started has no live deny directory. The driver refuses it before
		// any exec, whatever state the pod is in.
		old := uuid.New()
		no := false
		yes := true
		uid65532 := int64(65532)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "wardyn-proxy-" + old.String(), Namespace: ns},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   &yes,
					RunAsUser:      &uid65532,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:    "wardyn-proxy",
					Image:   agentImage,
					Command: []string{"sleep", "3600"},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &no,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				}},
			},
		}
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create the stand-in proxy pod: %v", err)
		}
		t.Cleanup(func() { _ = cs.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}) })
		ref := "wardyn-agent-" + old.String()
		if _, err := n.ProxyNarrowState(ctx, ref); !errors.Is(err, runner.ErrNarrowUnsupported) {
			t.Errorf("ProxyNarrowState on a pod without the directory = %v, want ErrNarrowUnsupported", err)
		}
		if _, err := n.NarrowProxy(ctx, ref, []string{narrowedHost}); !errors.Is(err, runner.ErrNarrowUnsupported) {
			t.Errorf("NarrowProxy on a pod without the directory = %v, want ErrNarrowUnsupported", err)
		}
	})
}

// assertNarrowed: the narrowed host is refused by policy, the other is not.
func assertNarrowed(t *testing.T, r runner.Runner, ref string) {
	t.Helper()
	if got := proxyAnswer(t, r, ref, narrowedHost); !strings.Contains(got, deniedBy) {
		t.Errorf("the proxy's answer for %s lacks %q:\n%s", narrowedHost, deniedBy, got)
	}
	if got := proxyAnswer(t, r, ref, otherHost); strings.Contains(got, deniedBy) {
		t.Errorf("the proxy refused %s by policy, which the narrowing did not name:\n%s", otherHost, got)
	}
}

// restartProxyContainer stops the proxy's container on its node and waits for the kubelet to start it
// again and for the pod to be ready.
func restartProxyContainer(t *testing.T, cs kubernetes.Interface, ns, podName string) {
	t.Helper()
	cluster := os.Getenv("WARDYN_TEST_K8S_KIND_CLUSTER")
	pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	var id string
	var restarts int32
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == "wardyn-proxy" {
			id, restarts = strings.TrimPrefix(c.ContainerID, "containerd://"), c.RestartCount
		}
	}
	if id == "" || pod.Spec.NodeName == "" {
		t.Fatalf("the proxy container has no id or node yet: %+v", pod.Status.ContainerStatuses)
	}
	t.Logf("restarting the proxy container %s on node %s of cluster %s", id, pod.Spec.NodeName, cluster)
	if out, err := exec.Command("docker", "exec", pod.Spec.NodeName, "crictl", "stop", id).CombinedOutput(); err != nil {
		t.Fatalf("crictl stop: %v\n%s", err, out)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := cs.CoreV1().Pods(ns).Get(context.Background(), podName, metav1.GetOptions{})
		if err == nil && proxyReady(p) && proxyRestarts(p) > restarts {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("the proxy container did not restart and become ready in 3 minutes")
}

func proxyRestarts(p *corev1.Pod) int32 {
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == "wardyn-proxy" {
			return c.RestartCount
		}
	}
	return 0
}

func proxyReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == "wardyn-proxy" {
			return c.Ready
		}
	}
	return false
}

// TestK8sNodeRestartKeepsLiveDenies reboots the node a narrowed proxy runs on. WARDYN_TEST_K8S_NODE_RESTART
// names the kind cluster; without it the test skips, so the conformance job never runs it.
//
// It passes in exactly two cases, and logs which: the list is still enforced ("list kept"), or the proxy is
// back on the policy it was launched with ("list lost, proxy on its launch policy"). Anything else fails.
func TestK8sNodeRestartKeepsLiveDenies(t *testing.T) {
	cluster := os.Getenv("WARDYN_TEST_K8S_NODE_RESTART")
	if cluster == "" {
		t.Skip("set WARDYN_TEST_K8S_NODE_RESTART to the kind cluster name to restart its node")
	}
	proxyImage, agentImage := os.Getenv("WARDYN_PROXY_IMAGE"), os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if proxyImage == "" || agentImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE and WARDYN_TEST_K8S_AGENT_IMAGE must name the proxy and conformance agent images")
	}
	build := func() (runner.Runner, kubernetes.Interface, string, error) {
		sub, err := k8s.New(k8sSuiteConfig(k8s.Config{Namespaces: []string{os.Getenv("WARDYN_K8S_NAMESPACE")}, ProxyImage: proxyImage, Record: true}))
		if err != nil {
			return nil, nil, "", fmt.Errorf("k8s.New: %w", err)
		}
		cs, ns := conformanceClientset(t)
		return orchestrator.New(sub), cs, ns, nil
	}
	r, cs, ns, err := build()
	if err != nil {
		t.Fatal(err)
	}

	// 1. A narrowed sandbox. H is the hash it enforces, L the one it was launched with.
	s := newNarrowSandbox(t, r, cs, ns, agentImage)
	ctx := context.Background()
	after, err := narrower(t, r).NarrowProxy(ctx, s.sb.Ref, []string{narrowedHost})
	if err != nil {
		t.Fatalf("NarrowProxy: %v", err)
	}
	h, l := after.ConfigSHA256, s.sb.ProxyConfigSHA256
	uid, ip := s.pod.UID, s.pod.Status.PodIP
	before, err := narrower(t, r).ProxyNarrowState(ctx, s.sb.Ref)
	if err != nil || before.ConfigSHA256 != h {
		t.Fatalf("before the restart: %+v, %v; want hash %s", before, err, h)
	}
	node := s.pod.Spec.NodeName
	oldNodeIP := dockerNodeIP(t, node)

	// 2. Reboot the node with the pod still bound to it.
	if out, err := exec.Command("docker", "restart", node).CombinedOutput(); err != nil {
		t.Fatalf("docker restart %s: %v\n%s", node, err, out)
	}

	// 3. The node may come back on another address: point the client and the driver at it. The driver is
	// rebuilt inside the wait, since k8s.New runs its boot canary against an API that may not be up yet.
	rebuild := false
	if newIP := dockerNodeIP(t, node); newIP != oldNodeIP {
		t.Logf("node %s moved from %s to %s; rewriting the kubeconfig", node, oldNodeIP, newIP)
		pointKubeconfigAt(t, newIP, node)
		rebuild = true
	}
	lastBuildErr := ""
	waitFor(t, 5*time.Minute, "the API, the node and the proxy pod to be ready", func() bool {
		if rebuild {
			nr, ncs, nns, err := build()
			if err != nil {
				if err.Error() != lastBuildErr {
					lastBuildErr = err.Error()
					t.Logf("rebuild the driver after the restart: %v (retrying)", err)
				}
				return false
			}
			r, cs, ns, rebuild = nr, ncs, nns, false
		}
		n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil || !nodeReady(n) {
			return false
		}
		p, err := cs.CoreV1().Pods(ns).Get(ctx, s.pod.Name, metav1.GetOptions{})
		return err == nil && proxyReady(p)
	})
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, s.pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	if pod.UID != uid {
		t.Fatalf("proxy pod uid = %s, was %s: the pod was replaced, not rebooted", pod.UID, uid)
	}

	// 4. The proxy answers its probe, within the same five minutes.
	var st runner.ProxyNarrowState
	waitFor(t, 5*time.Minute, "the proxy to answer a probe", func() bool {
		st, err = narrower(t, r).ProxyNarrowState(ctx, s.sb.Ref)
		return err == nil
	})
	wantBase, _ := proxy.LiveDenyBaseSHA256(s.base)
	if st.BaseSHA256 != wantBase {
		t.Fatalf("base hash = %s after the restart, was %s", st.BaseSHA256, wantBase)
	}

	// 5 and 6. Exactly one of two cases passes, and the proxy's probe alone decides which.
	switch {
	case slices.Equal(st.LiveDenies, []string{narrowedHost}) && st.ConfigSHA256 == h:
		t.Log("node-restart: list kept")
	case len(st.LiveDenies) == 0 && st.ConfigSHA256 == l:
		t.Log("node-restart: list lost, proxy on its launch policy")
		return
	default:
		t.Fatalf("after a node restart the proxy holds %+v; want the list %v at hash %s, or no list at the launch hash %s", st, []string{narrowedHost}, h, l)
	}

	// 7. The agent-side check needs a live agent to exec into. The agent pod has RestartPolicy Never, so a
	// node reboot usually leaves it dead; the verdict above stands without it.
	agent, err := cs.CoreV1().Pods(ns).Get(ctx, "wardyn-agent-"+s.spec.RunID.String(), metav1.GetOptions{})
	if err != nil {
		t.Logf("agent-side check skipped: read the agent pod: %v", err)
		return
	}
	if agent.Status.Phase != corev1.PodRunning {
		t.Logf("agent-side check skipped: the agent pod is %s, not Running, after the node restart", agent.Status.Phase)
		return
	}
	if pod.Status.PodIP == ip {
		assertNarrowed(t, r, s.sb.Ref)
		return
	}
	// The agent's pinned address reaches no proxy now: neither host may get through. Each probe must
	// show it ran, so an exec that printed nothing does not pass.
	for _, host := range []string{narrowedHost, otherHost} {
		got := proxyAnswer(t, r, s.sb.Ref, host)
		switch {
		case !strings.Contains(got, probeDone):
			t.Errorf("the agent's probe for %s at the old address printed no end marker, so it proves nothing:\n%s", host, got)
		case strings.Contains(got, "http/1."):
			t.Errorf("the agent reached a proxy for %s at the old address: %s", host, got)
		}
	}
	t.Log("the proxy pod came back on a new address; the agent reaches none, which is narrower")
}

func waitFor(t *testing.T, bound time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out after %s waiting for %s", bound, what)
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// dockerNodeIP is the address of a kind node's container on its network.
func dockerNodeIP(t *testing.T, node string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", node).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", node, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		t.Fatalf("docker inspect %s: no address", node)
	}
	return f[0]
}

// pointKubeconfigAt writes a copy of the kubeconfig whose API server is ip, named node for TLS (the
// node's name is in the API server's certificate), and uses it from here on.
func pointKubeconfigAt(t *testing.T, ip, node string) {
	t.Helper()
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		t.Fatal("KUBECONFIG is not set, so the client cannot follow the node to a new address")
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	for _, c := range cfg.Clusters {
		c.Server = "https://" + ip + ":6443"
		c.TLSServerName = node
	}
	moved := t.TempDir() + "/kubeconfig"
	if err := clientcmd.WriteToFile(*cfg, moved); err != nil {
		t.Fatalf("write %s: %v", moved, err)
	}
	t.Setenv("KUBECONFIG", moved)
}
