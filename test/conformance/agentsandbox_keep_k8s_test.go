// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// agentsandbox_keep_k8s_test.go holds the agent-sandbox backend's keep cases: a run whose work folder is a kept
// drive reaches its proxy through its own Service, can have its proxy replaced live, and can be ended and revived.
// They run under the same switches as agentsandbox_k8s_test.go. Every call below goes through the orchestrator over
// k8s.NewSubstrate, the type wardynd serves, and in the order wardynd makes them.

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	proxycfg "github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

var (
	keepRunnerOnce sync.Once
	keepOrch       *orchestrator.Orchestrator
	keepRunnerErr  error
)

// keepRunner is the orchestrator over the keep-on substrate, built once: the canary at boot is not repeated per case.
func keepRunner(t *testing.T, e *asEnv) *orchestrator.Orchestrator {
	t.Helper()
	keepRunnerOnce.Do(func() {
		image := os.Getenv("WARDYN_PROXY_IMAGE")
		if image == "" {
			keepRunnerErr = errors.New("WARDYN_PROXY_IMAGE must name the real wardyn-proxy build")
			return
		}
		sub, err := k8s.NewSubstrate(k8sSuiteConfig(k8s.Config{Namespaces: []string{e.ns}, ProxyImage: image, Record: true, AgentSandboxKeep: true}))
		if err != nil {
			keepRunnerErr = err
			return
		}
		keepOrch = orchestrator.New(sub)
	})
	if keepRunnerErr != nil {
		t.Fatalf("k8s.NewSubstrate with the keep switch: %v", keepRunnerErr)
	}
	return keepOrch
}

// keptRunMods makes the run one that can be kept: interactive, of an agent that is revived, with one drive as its work
// folder. The drive's claim outlives the run, so it is deleted at cleanup, after the run's own stop.
func (e *asEnv) keptRunMods(t *testing.T) func(*runner.SandboxSpec) {
	t.Helper()
	suffix := uuid.NewString()[:8]
	claim := "wardyn-conformance-keep-" + suffix
	t.Cleanup(func() {
		_ = e.cs.CoreV1().PersistentVolumeClaims(e.ns).Delete(context.Background(), claim, metav1.DeleteOptions{})
	})
	return func(s *runner.SandboxSpec) {
		s.Interactive = true
		maps.Copy(s.Labels, map[string]string{"wardyn.agent": "claude-code"})
		s.Drives = []types.DriveMount{{
			DriveID: uuid.New(), Backend: types.DriveBackendK8sPVC, ObjectName: claim,
			DriveName: "conformance-keep", HomeName: "conf-" + suffix, Target: runner.ScratchWorkPath, SizeMiB: 64,
		}}
	}
}

// keepExec runs script in the agent through ExecStream and returns its exit code and combined output.
func keepExec(ctx context.Context, t *testing.T, r runner.Runner, ref, script string) (int, string) {
	t.Helper()
	return keepExecArgv(ctx, t, r, ref, []string{"sh", "-c", script})
}

// keepExecArgv is keepExec for an argv.
func keepExecArgv(ctx context.Context, t *testing.T, r runner.Runner, ref string, argv []string) (int, string) {
	t.Helper()
	sess, err := r.ExecStream(ctx, ref, runner.ExecSpec{Argv: argv})
	if err != nil {
		t.Fatalf("ExecStream %q: %v", argv, err)
	}
	defer func() { _ = sess.Close() }()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	var stderr []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		stderr, _ = io.ReadAll(sess.Stderr)
	}()
	stdout, _ := io.ReadAll(sess.Stdout)
	<-done
	code, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait on %q: %v", argv, err)
	}
	return code, string(stdout) + string(stderr)
}

// keepProxyReachable is the proxy's own port answering from inside the agent: the request the suite's other proxy
// cases make first.
func keepProxyReachable(ctx context.Context, t *testing.T, r runner.Runner, ref string) bool {
	t.Helper()
	code, _ := keepExec(ctx, t, r, ref, "nc -z -w2 wardyn-proxy 3128")
	return code == 0
}

// keepWaitFor polls until cond holds or the timeout passes, and fails the case naming what it waited for.
func keepWaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); ; time.Sleep(time.Second) {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s never held", timeout, what)
		}
	}
}

func (e *asEnv) keepPod(ctx context.Context, t *testing.T, name string) *corev1.Pod {
	t.Helper()
	pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read pod %s: %v", name, err)
	}
	return pod
}

func TestAgentSandboxKeepProxyThroughService(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := keepRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), conformance.ASKeepServiceLimit)
	defer cancel()
	runID := uuid.New()
	if _, err := e.startRun(ctx, t, r, runID, e.keptRunMods(t)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	agent := e.keepPod(ctx, t, asPodName(runID))
	var pinned string
	for _, h := range agent.Spec.HostAliases {
		if len(h.Hostnames) == 1 && h.Hostnames[0] == "wardyn-proxy" {
			pinned = h.IP
		}
	}
	svc, err := e.cs.CoreV1().Services(e.ns).Get(ctx, "wardyn-proxy-svc-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's Service: %v", err)
	}
	if pinned == "" || pinned != svc.Spec.ClusterIP {
		t.Errorf("the agent pins wardyn-proxy to %q, want the Service's ClusterIP %q", pinned, svc.Spec.ClusterIP)
	}
	slice, err := e.cs.DiscoveryV1().EndpointSlices(e.ns).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's EndpointSlice: %v", err)
	}
	proxyPod := e.keepPod(ctx, t, "wardyn-proxy-"+runID.String())
	if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 || slice.Endpoints[0].Addresses[0] != proxyPod.Status.PodIP {
		t.Errorf("EndpointSlice endpoints = %+v, want the proxy pod's IP %s", slice.Endpoints, proxyPod.Status.PodIP)
	}
	if !keepProxyReachable(ctx, t, r, asPodName(runID)) {
		t.Error("the agent cannot reach the proxy through the Service")
	}
}

func TestAgentSandboxKeepEndpointSliceElsewhereIsDropped(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := keepRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), conformance.ASKeepSliceLimit)
	defer cancel()
	runID := uuid.New()
	ref := asPodName(runID)
	if _, err := e.startRun(ctx, t, r, runID, e.keptRunMods(t)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	decoy := e.startDecoy(ctx, t)
	name := "wardyn-proxy-svc-" + runID.String()
	slices := e.cs.DiscoveryV1().EndpointSlices(e.ns)
	original, err := slices.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's EndpointSlice: %v", err)
	}
	endpointsAre := func(endpoints []discoveryv1.Endpoint) {
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
	endpointsAre([]discoveryv1.Endpoint{{Addresses: []string{decoy}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}}})
	keepWaitFor(t, 10*time.Second, "the agent's connect to wardyn-proxy failing once the endpoint is elsewhere", func() bool {
		return !keepProxyReachable(ctx, t, r, ref)
	})
	endpointsAre(original.Endpoints)
	keepWaitFor(t, 30*time.Second, "the agent's connect to wardyn-proxy succeeding once the endpoint is restored", func() bool {
		return keepProxyReachable(ctx, t, r, ref)
	})
}

// startDecoy runs a plain pod, labelled unlike any proxy, that listens on the proxy's port, and returns its IP.
func (e *asEnv) startDecoy(ctx context.Context, t *testing.T) string {
	t.Helper()
	name := "wardyn-conformance-decoy-" + uuid.NewString()[:8]
	spec := asPodSpec(e.image, false)
	spec.Containers[0].Command = []string{"sh", "-c", "while true; do nc -l -p 3128 </dev/null >/dev/null 2>&1; done"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns, Labels: map[string]string{"app": "wardyn-conformance-decoy"}}, Spec: spec}
	if _, err := e.cs.CoreV1().Pods(e.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the decoy pod: %v", err)
	}
	t.Cleanup(func() {
		_ = e.cs.CoreV1().Pods(e.ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	var ip string
	keepWaitFor(t, conformance.ASKeepSliceDecoyWait, "the decoy pod running with an IP", func() bool {
		got, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
		ip = ""
		if err == nil && got.Status.Phase == corev1.PodRunning {
			ip = got.Status.PodIP
		}
		return ip != ""
	})
	return ip
}

// keepProxyConfig is the run's proxy config as the API marshals it.
func keepProxyConfig(t *testing.T, runID uuid.UUID) []byte {
	t.Helper()
	cfg, err := runner.BuildProxyConfig(runID, conformance.LoadableProxyConfig(), runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	return cfg
}

func TestAgentSandboxKeepLiveReplaceProxy(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := keepRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), conformance.ASKeepReplaceLimit)
	defer cancel()
	runID := uuid.New()
	ref := asPodName(runID)
	if _, err := e.startRun(ctx, t, r, runID, e.keptRunMods(t)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	proxyName := "wardyn-proxy-" + runID.String()
	proxyBefore, agentBefore := e.keepPod(ctx, t, proxyName).UID, e.keepPod(ctx, t, ref).UID
	if err := r.ReplaceProxy(ctx, ref, keepProxyConfig(t, runID)); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if got := e.keepPod(ctx, t, proxyName).UID; got == proxyBefore {
		t.Errorf("the proxy pod is still %s after the replace", got)
	}
	if got := e.keepPod(ctx, t, ref).UID; got != agentBefore {
		t.Errorf("the agent pod changed from %s to %s: a live replace leaves it alone", agentBefore, got)
	}
	keepWaitFor(t, conformance.ASKeepReplaceWait, "the agent reaching the new proxy through the Service", func() bool {
		return keepProxyReachable(ctx, t, r, ref)
	})
}

// TestAgentSandboxKeepAPISequence makes the calls wardynd makes, in wardynd's order: the lease end, its re-assert inside
// the controller's remake window, a third end, the status read, then the revive in run_revive.go's order.
func TestAgentSandboxKeepAPISequence(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := keepRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), conformance.ASKeepSequenceLimit)
	defer cancel()
	runID := uuid.New()
	ref := asPodName(runID)
	proof := runner.ScratchWorkPath + "/keep-proof"

	// 1. Create a run that can be kept; write the proof file.
	if _, err := e.startRun(ctx, t, r, runID, e.keptRunMods(t)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if code, out := keepExec(ctx, t, r, ref, "echo kept > "+proof); code != 0 {
		t.Fatalf("write the proof file: exit %d: %s", code, out)
	}
	original := e.keepPod(ctx, t, ref).UID

	// 2 and 3. The lease end, and the re-assert at once, inside the controller's remake window.
	for i, step := range []string{"the lease end", "the re-assert"} {
		if err := r.EndSandboxAt(ctx, ref, 1); err != nil {
			t.Fatalf("EndSandboxAt (%s, call %d): %v", step, i+1, err)
		}
	}

	// 4. The held pod appears: gated, no node. A third end leaves it alone.
	var held *corev1.Pod
	keepWaitFor(t, time.Minute, "the agent pod held behind the gate with no node", func() bool {
		pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, ref, metav1.GetOptions{})
		if err != nil || pod.UID == original || asNotHeld(pod) != "" {
			return false
		}
		held = pod
		return true
	})
	if err := r.EndSandboxAt(ctx, ref, 1); err != nil {
		t.Fatalf("EndSandboxAt (the third end): %v", err)
	}
	time.Sleep(conformance.ASKeepSequenceSettle)
	if got := e.keepPod(ctx, t, ref).UID; got != held.UID {
		t.Errorf("the held pod changed from %s to %s after a third end", held.UID, got)
	}

	// 5. Status reads kept.
	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status of the kept run: %v", err)
	}
	if st.State != types.RunStopped || st.Message != "kept: revive to start it again" {
		t.Errorf("Status = %+v, want stopped with the revive message", st)
	}

	// 6. The revive, in run_revive.go's order.
	cfg := keepProxyConfig(t, runID)
	if err := r.CanReplaceProxy(ctx, ref); err != nil {
		t.Fatalf("CanReplaceProxy: %v", err)
	}
	if err := r.EnsureProxyImage(ctx); err != nil {
		t.Fatalf("EnsureProxyImage: %v", err)
	}
	hash, err := r.ReplaceProxyHashedAt(ctx, ref, cfg, 2)
	if err != nil {
		t.Fatalf("ReplaceProxyHashedAt: %v", err)
	}
	if want := proxycfg.ConfigSHA256(cfg); hash != want {
		t.Errorf("config hash = %q, want %q", hash, want)
	}
	if err := r.StartSandbox(ctx, ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}

	// 7. The same name, a new UID, running and ungated; the proof and both markers read back.
	pod := e.keepPod(ctx, t, ref)
	if pod.UID == original || len(pod.Spec.SchedulingGates) != 0 || pod.Status.Phase != corev1.PodRunning {
		t.Errorf("revived pod = uid %s gates %v phase %s, want a new UID, no gate, Running (original %s)", pod.UID, pod.Spec.SchedulingGates, pod.Status.Phase, original)
	}
	if code, out := keepExec(ctx, t, r, ref, "cat "+proof); code != 0 || !strings.Contains(out, "kept") {
		t.Errorf("read the proof back: exit %d, %q", code, out)
	}
	if code, out := keepExec(ctx, t, r, ref, "test -f "+runner.ScratchWorkPath+"/.wardyn/booted-"+runID.String()); code != 0 {
		t.Errorf("the work folder's boot marker is missing: exit %d: %s", code, out)
	}
	homeMarker := []string{"sh", "-c", `test -f "${HOME:-/home/agent}/.wardyn/booted"`}
	if code, out := keepExecArgv(ctx, t, r, ref, homeMarker); code != 0 {
		t.Errorf("the home boot marker was not restored: exit %d: %s", code, out)
	}
	if st, err := r.Status(ctx, ref); err != nil || st.State != types.RunRunning {
		t.Errorf("Status after the revive = %+v, %v, want running", st, err)
	}

	// 8. A run that is not kept ends as before: unsupported, then the stop wardynd's fallback calls.
	e.endUnsupportedThenStop(ctx, t, r)
}

// endUnsupportedThenStop is step 8: a run that is not interactive and has no drive answers ErrEndUnsupported, and the
// teardown the API falls back to (StopSandbox, called by finalizeRunEffects for an ended run) leaves no pod of the run.
func (e *asEnv) endUnsupportedThenStop(ctx context.Context, t *testing.T, r *orchestrator.Orchestrator) {
	t.Helper()
	runID := uuid.New()
	sb, err := e.startRun(ctx, t, r, runID)
	if err != nil {
		t.Fatalf("CreateSandbox of a run that is not kept: %v", err)
	}
	if err := r.EndSandboxAt(ctx, sb.Ref, 1); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("EndSandboxAt of a run that is not kept = %v, want ErrEndUnsupported", err)
	}
	if err := r.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	keepWaitFor(t, conformance.ASKeepSequenceStop, "no pod of the run remaining", func() bool {
		pods, err := e.cs.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "wardyn.run-id=" + runID.String()})
		return err == nil && len(pods.Items) == 0 || apierrors.IsNotFound(err)
	})
}

// TestAgentSandboxKeepAdmissionBoundsServices drives the chart's two keep policies on this API server: wardynd may
// make only a run's own ClusterIP Service with no selector or external IPs, and EndpointSlices labelled for such a
// Service. Every create is a server-side dry run, as the suite's identity (wardynd), so nothing is left behind.
func TestAgentSandboxKeepAdmissionBoundsServices(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), conformance.ASKeepAdmissionLimit)
	defer cancel()

	self, err := e.cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("SelfSubjectReview: %v", err)
	}
	if me := self.Status.UserInfo.Username; !strings.HasPrefix(me, "system:serviceaccount:") {
		t.Fatalf("the suite runs as %q; it must run as the ServiceAccount the keep policies name as wardynd's (ci.yml renders it for wardyn/wardynd)", me)
	}
	id := uuid.New().String()
	own := "wardyn-proxy-svc-" + id
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	service := func(name string, mod func(*corev1.Service)) *corev1.Service {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.ServiceSpec{
				Type:  corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{{Port: 3128, Protocol: corev1.ProtocolTCP}},
			},
		}
		if mod != nil {
			mod(svc)
		}
		return svc
	}
	slice := func(labels map[string]string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{GenerateName: "wardyn-conformance-", Labels: labels},
			AddressType: discoveryv1.AddressTypeIPv4,
		}
	}
	const svcMsg = "wardynd may make only a run's own ClusterIP Service"
	const sliceMsg = "wardynd may write EndpointSlices only for a run's own Service"

	cases := []struct {
		name    string
		svc     *corev1.Service
		slice   *discoveryv1.EndpointSlice
		refused string
	}{
		{name: "own ClusterIP Service, no selector: allowed", svc: service(own, nil)},
		{name: "NodePort", svc: service(own, func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeNodePort }), refused: svcMsg},
		{name: "external IPs", svc: service(own, func(s *corev1.Service) { s.Spec.ExternalIPs = []string{"192.0.2.10"} }), refused: svcMsg},
		{name: "a selector", svc: service(own, func(s *corev1.Service) { s.Spec.Selector = map[string]string{"app": "x"} }), refused: svcMsg},
		{name: "a name that is not a run's", svc: service("wardyn-conformance-other-"+id[:8], nil), refused: svcMsg},
		{name: "own slice: allowed", slice: slice(map[string]string{discoveryv1.LabelServiceName: own})},
		{name: "slice of another Service", slice: slice(map[string]string{discoveryv1.LabelServiceName: "other"}), refused: sliceMsg},
		{name: "slice with no labels", slice: slice(nil), refused: sliceMsg},
	}
	for _, c := range cases {
		var err error
		if c.svc != nil {
			_, err = e.cs.CoreV1().Services(e.ns).Create(ctx, c.svc, dryRun)
		} else {
			_, err = e.cs.DiscoveryV1().EndpointSlices(e.ns).Create(ctx, c.slice, dryRun)
		}
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: refused, want allowed: %v", c.name, err)
		case c.refused != "" && err == nil:
			t.Errorf("%s: allowed, want refused", c.name)
		case c.refused != "" && !strings.Contains(err.Error(), c.refused):
			t.Errorf("%s: refused with %q, want a message containing %q", c.name, err, c.refused)
		}
	}
}
