// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The limits of the per-run Service cases.
const (
	runServiceLimit     = 8 * time.Minute  // the RunService case's context, K1 to K4
	runServiceGoneWait  = 10 * time.Second // K2: the agent's connect failing once the endpoint is the decoy
	runServiceBackWait  = 30 * time.Second // K2, K3: it succeeding once the endpoint is restored
	runServiceStopWait  = 2 * time.Minute  // K4: every object of the run gone after StopSandbox
	firstPacketLimit    = 10 * time.Minute // K7, the control and its 20 attempts
	firstPacketAttempts = 20
	firstPacketWatch    = 3 * time.Second // K7: how long a probe is read after its first line
)

// testRunService pins the server run's own Service: the agent reaches its proxy through it from the first exec (K1),
// the EndpointSlice is the only route (K2, K3), and teardown leaves nothing of the run (K4). K7 measures whether the
// cluster's CNI isolates a pod from its first packet, which the agent pod now relies on.
func testRunService(t *testing.T, r runner.Runner, agentImage string) {
	cs, ns := conformanceClientset(t)
	ctx, cancel := context.WithTimeout(context.Background(), runServiceLimit)
	defer cancel()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:      runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "conformance"},
		Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			_ = r.StopSandbox(stopCtx, sb.Ref)
		}
	})
	// The first exec, straight after CreateSandbox: nothing may run in the agent before it.
	firstCode, firstOut := keepExec(ctx, t, r, sb.Ref, "nc -z -w2 wardyn-proxy 3128")
	name := "wardyn-proxy-svc-" + spec.RunID.String()
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's Service: %v", err)
	}
	slices := cs.DiscoveryV1().EndpointSlices(ns)
	original, err := slices.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's EndpointSlice: %v", err)
	}
	endpointsAre := func(t *testing.T, endpoints []discoveryv1.Endpoint) {
		t.Helper()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := slices.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			cur.Endpoints = endpoints
			_, err = slices.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			t.Fatalf("update the run's EndpointSlice: %v", err)
		}
	}
	restore := func(t *testing.T) {
		t.Helper()
		endpointsAre(t, original.Endpoints)
		keepWaitFor(t, runServiceBackWait, "the agent's connect to wardyn-proxy succeeding once the endpoint is restored", func() bool {
			return keepProxyReachable(ctx, t, r, sb.Ref)
		})
	}

	t.Run("K1", func(t *testing.T) {
		if firstCode != 0 {
			t.Errorf("the first exec's connect to wardyn-proxy:3128 exited %d: %s", firstCode, firstOut)
		}
		agent, err := cs.CoreV1().Pods(ns).Get(ctx, sb.Ref, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read the agent pod: %v", err)
		}
		var pinned string
		for _, h := range agent.Spec.HostAliases {
			for _, n := range h.Hostnames {
				if n == "wardyn-proxy" {
					pinned = h.IP
				}
			}
		}
		if pinned == "" || pinned != svc.Spec.ClusterIP {
			t.Errorf("the agent pins wardyn-proxy to %q, want the Service's ClusterIP %q", pinned, svc.Spec.ClusterIP)
		}
		proxy, err := cs.CoreV1().Pods(ns).Get(ctx, "wardyn-proxy-"+spec.RunID.String(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read the proxy pod: %v", err)
		}
		if e := original.Endpoints; len(e) != 1 || len(e[0].Addresses) != 1 || e[0].Addresses[0] != proxy.Status.PodIP {
			t.Errorf("EndpointSlice endpoints = %+v, want the proxy pod's IP %s", e, proxy.Status.PodIP)
		}
	})

	t.Run("K2", func(t *testing.T) {
		decoy, _ := startControlPod(t, ctx, cs, ns, agentImage,
			"echo done=1; while true; do nc -l -p 3128 </dev/null >/dev/null 2>&1; done", map[string]string{"app": "wardyn-conformance-decoy"})
		_, lines := startControlPod(t, ctx, cs, ns, agentImage,
			"for i in 1 2 3 4 5 6 7 8 9 10; do nc -z -w2 "+decoy+" 3128 && { echo reach=1; break; }; sleep 1; done; echo done=1; sleep 3600",
			map[string]string{"app": "wardyn-conformance-decoy-probe"})
		if lines["reach"] != "1" {
			t.Fatalf("the decoy %s does not accept on 3128 from a pod no policy selects, so a refused connect below would prove nothing", decoy)
		}
		endpointsAre(t, []discoveryv1.Endpoint{{Addresses: []string{decoy}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}}})
		keepWaitFor(t, runServiceGoneWait, "the agent's connect to wardyn-proxy failing once the endpoint is the decoy", func() bool {
			return !keepProxyReachable(ctx, t, r, sb.Ref)
		})
		restore(t)
	})

	t.Run("K3", func(t *testing.T) {
		// The proxy answers every lookup but its own name REFUSED; an unanswered one times out.
		lookup := "timeout 10 nslookup wardyn-conformance.invalid " + svc.Spec.ClusterIP + "; echo rc=$?"
		answered := func(out string) bool {
			return strings.Contains(out, "REFUSED") || strings.Contains(out, "can't find")
		}
		if _, out := keepExec(ctx, t, r, sb.Ref, lookup); !answered(out) {
			t.Fatalf("with the slice naming the proxy, a lookup at the ClusterIP was not answered REFUSED, so a failure below would prove nothing:\n%s", out)
		}
		endpointsAre(t, nil)
		for i := range 3 {
			if keepProxyReachable(ctx, t, r, sb.Ref) {
				t.Errorf("try %d: a connect to wardyn-proxy succeeded with the run's EndpointSlice empty", i+1)
			}
		}
		if _, out := keepExec(ctx, t, r, sb.Ref, lookup); answered(out) || strings.Contains(out, "rc=0") {
			t.Errorf("with the slice empty, a lookup at the ClusterIP was answered:\n%s", out)
		}
		restore(t)
	})

	t.Run("K4", func(t *testing.T) {
		stopCtx, stop := context.WithTimeout(context.Background(), runServiceStopWait)
		defer stop()
		if err := r.StopSandbox(stopCtx, sb.Ref); err != nil {
			t.Fatalf("StopSandbox: %v", err)
		}
		stopped = true
		var left []string
		keepWaitFor(t, runServiceStopWait, "no object of the run left after StopSandbox", func() bool {
			left = runObjectsLeft(ctx, t, cs, ns, spec.RunID)
			return len(left) == 0
		})
		if len(left) != 0 {
			t.Errorf("left after StopSandbox: %v", left)
		}
	})

	t.Run("K7", func(t *testing.T) { testFirstPacket(t, cs, agentImage) })
}

// runObjectsLeft names every Service, EndpointSlice, pod, Secret and NetworkPolicy of the run that still exists.
func runObjectsLeft(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string, runID uuid.UUID) []string {
	t.Helper()
	var left []string
	sel := metav1.ListOptions{LabelSelector: "wardyn.run-id=" + runID.String()}
	if _, err := cs.CoreV1().Services(ns).Get(ctx, "wardyn-proxy-svc-"+runID.String(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		left = append(left, "service (get err "+errString(err)+")")
	}
	lists := map[string]func() (int, error){
		"endpointslices": func() (int, error) {
			l, err := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, sel)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		},
		"pods": func() (int, error) {
			l, err := cs.CoreV1().Pods(ns).List(ctx, sel)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		},
		"secrets": func() (int, error) {
			l, err := cs.CoreV1().Secrets(ns).List(ctx, sel)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		},
		"networkpolicies": func() (int, error) {
			l, err := cs.NetworkingV1().NetworkPolicies(ns).List(ctx, sel)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		},
	}
	for kind, list := range lists {
		n, err := list()
		if err != nil {
			t.Fatalf("list %s: %v", kind, err)
		}
		if n != 0 {
			left = append(left, strconv.Itoa(n)+" "+kind)
		}
	}
	return left
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// testFirstPacket (K7) measures whether a NetworkPolicy created just before a pod holds from that pod's first packet:
// a property of the cluster's CNI, which the run's agent pod now relies on, since it is created moments after its
// policy. It runs in a scratch namespace that holds no policy but its own, and only with
// WARDYN_TEST_K8S_FIRST_PACKET=1.
func testFirstPacket(t *testing.T, cs kubernetes.Interface, image string) {
	if os.Getenv("WARDYN_TEST_K8S_FIRST_PACKET") != "1" {
		t.Skip("WARDYN_TEST_K8S_FIRST_PACKET=1 not set; skipping the first-packet measurement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), firstPacketLimit)
	defer cancel()
	ns := "wardyn-k7-" + uuid.NewString()[:8]
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the scratch namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})
	// A pod is refused until the namespace's default ServiceAccount exists, which its controller makes moments later.
	keepWaitFor(t, time.Minute, "the scratch namespace's default ServiceAccount", func() bool {
		_, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "default", metav1.GetOptions{})
		return err == nil
	})

	reached, ms := firstPacketProbe(ctx, t, cs, ns, image, "control", false)
	t.Logf("K7 control reached=%t create_to_first_ms=%d", reached, ms)
	if !reached {
		t.Fatal("K7: the control pod, under no policy, never reached the API server: a silent probe proves nothing")
	}
	for i := 1; i <= firstPacketAttempts; i++ {
		reached, ms := firstPacketProbe(ctx, t, cs, ns, image, "attempt-"+strconv.Itoa(i), true)
		t.Logf("K7 attempt=%d reached=%t create_to_first_ms=%d", i, reached, ms)
		if reached {
			t.Errorf("K7 attempt %d: the pod reached the API server before its deny-all egress NetworkPolicy held", i)
		}
	}
}

// firstPacketProbe creates, when deny is set, a deny-all egress NetworkPolicy for a fresh label and at once a probe
// pod with that label, reads the pod's log from its first probe line for firstPacketWatch, and deletes both. It
// reports whether any line was REACHED and the milliseconds from the pod create's return to the first line read.
func firstPacketProbe(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, image, name string, deny bool) (bool, int64) {
	t.Helper()
	labels := map[string]string{"wardyn.k7": uuid.NewString()[:8]}
	podName := "wardyn-k7-" + name
	if deny {
		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: labels},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      []networkingv1.NetworkPolicyEgressRule{},
			},
		}
		if _, err := cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
			t.Fatalf("K7 %s: create the deny-all policy: %v", name, err)
		}
		defer func() {
			_ = cs.NetworkingV1().NetworkPolicies(ns).Delete(context.Background(), podName, metav1.DeleteOptions{})
		}()
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"sh", "-c",
					`while true; do if nc -z -w1 "$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT"; then echo REACHED; else echo BLOCKED; fi; done`},
				SecurityContext: restrictedContainer(1000),
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("K7 %s: create the probe pod: %v", name, err)
	}
	created := time.Now()
	defer func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), podName, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	}()
	var first int64 = -1
	var logs string
	for {
		b, _ := cs.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{Container: "probe"}).DoRaw(ctx)
		logs = string(b)
		if first < 0 && (strings.Contains(logs, "REACHED") || strings.Contains(logs, "BLOCKED")) {
			first = time.Since(created).Milliseconds()
		}
		if first >= 0 && time.Since(created) >= time.Duration(first)*time.Millisecond+firstPacketWatch {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("K7 %s: the probe pod printed no probe line: %v\n%s", name, ctx.Err(), logs)
		case <-time.After(100 * time.Millisecond):
		}
	}
	firstPacketPulledLocally(ctx, t, cs, ns, podName)
	return strings.Contains(logs, "REACHED"), first
}

// firstPacketPulledLocally fails the attempt as vacuous unless the kubelet's Pulled event says the image was already
// on the node: a pull would sit between the pod's create and its first connect, and give the policy time to apply.
func firstPacketPulledLocally(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, pod string) {
	t.Helper()
	var msg string
	keepWaitFor(t, 30*time.Second, "the probe pod's Pulled event", func() bool {
		evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod + ",reason=Pulled"})
		if err != nil || len(evs.Items) == 0 {
			return false
		}
		msg = evs.Items[0].Message
		return true
	})
	if !strings.Contains(msg, "already present on machine") {
		t.Fatalf("K7 %s: the image was pulled (%q), so the probe proves nothing", pod, msg)
	}
}
