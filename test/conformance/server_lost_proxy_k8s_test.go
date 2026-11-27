// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// server_lost_proxy_k8s_test.go holds the server cluster's lost-run cases: a run lost in an outage keeps its agent
// with its proxy stopped and no network, its owner's revive starts a new proxy behind the same Service, and only the
// kept run's agent outlives a proxy pod deleted by hand. They run under WARDYN_TEST_K8S=1, on the namespace the suite
// uses, and every call goes through the orchestrator over k8s.NewSubstrate, the type wardynd serves.

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// The limits of the lost-run cases.
const (
	serverLostLimit      = 8 * time.Minute  // each case's context
	serverLostReachable  = 2 * time.Minute  // the agent reaching its proxy through the Service, at start or after a revive
	serverLostCutWait    = 10 * time.Second // a connect failing once the proxy is gone
	serverLostStreamWait = 10 * time.Second // an open stream ending once StopLostProxyAt has returned
	serverLostGCWait     = 60 * time.Second // a running run's agent going with its proxy pod
	serverLostStopWait   = 2 * time.Minute  // the agent gone after the run's teardown
)

type serverLostEnv struct {
	r          *orchestrator.Orchestrator
	restCfg    *rest.Config
	cs         *kubernetes.Clientset
	ns         string
	agentImage string
}

var (
	serverLostOnce sync.Once
	serverLost     *serverLostEnv
	serverLostErr  error
)

// requireServerLost is the orchestrator over the served substrate in server mode, built once.
func requireServerLost(t *testing.T) *serverLostEnv {
	t.Helper()
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping the server cluster's lost-run cases")
	}
	serverLostOnce.Do(func() {
		proxyImage, agentImage := os.Getenv("WARDYN_PROXY_IMAGE"), os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
		if proxyImage == "" || agentImage == "" {
			serverLostErr = errors.New("WARDYN_PROXY_IMAGE and WARDYN_TEST_K8S_AGENT_IMAGE must be set, as for TestConformanceK8s")
			return
		}
		restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			serverLostErr = err
			return
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			serverLostErr = err
			return
		}
		ns := cmp.Or(os.Getenv("WARDYN_K8S_NAMESPACE"), "default")
		sub, err := k8s.NewSubstrate(k8sSuiteConfig(k8s.Config{
			Namespaces: []string{os.Getenv("WARDYN_K8S_NAMESPACE")},
			ProxyImage: proxyImage,
			Record:     true,
			ControlPlanePeer: &k8s.ControlPlanePeer{
				Namespace: ns,
				PodLabels: map[string]string{standInLabelKey: standInLabelValue},
			},
		}))
		if err != nil {
			serverLostErr = err
			return
		}
		serverLost = &serverLostEnv{r: orchestrator.New(sub), restCfg: restCfg, cs: cs, ns: ns, agentImage: agentImage}
	})
	if serverLostErr != nil {
		t.Fatalf("the server cluster's substrate: %v", serverLostErr)
	}
	return serverLost
}

// startRun creates a server run and waits for its agent to reach its proxy. The run is torn down at the case's end.
func (e *serverLostEnv) startRun(ctx context.Context, t *testing.T) (uuid.UUID, string) {
	t.Helper()
	caps, err := e.r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	runID := uuid.New()
	sb, err := e.r.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            runID,
		Image:            e.agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:      runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "conformance"},
		Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), serverLostStopWait)
		defer stop()
		_ = e.r.StopSandbox(stopCtx, sb.Ref)
	})
	keepWaitFor(t, serverLostReachable, "the agent reaching its proxy through the Service", func() bool {
		return keepProxyReachable(ctx, t, e.r, sb.Ref)
	})
	return runID, sb.Ref
}

func (e *serverLostEnv) pod(ctx context.Context, t *testing.T, name string) *corev1.Pod {
	t.Helper()
	pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read pod %s: %v", name, err)
	}
	return pod
}

func (e *serverLostEnv) clusterIP(ctx context.Context, t *testing.T, runID uuid.UUID) string {
	t.Helper()
	svc, err := e.cs.CoreV1().Services(e.ns).Get(ctx, "wardyn-proxy-svc-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's Service: %v", err)
	}
	return svc.Spec.ClusterIP
}

// stopLost is the lost-run stop wardynd's lease sweep makes, through the orchestrator.
func (e *serverLostEnv) stopLost(ctx context.Context, t *testing.T, ref string) {
	t.Helper()
	if err := e.r.StopLostProxyAt(ctx, ref, 0); err != nil {
		t.Fatalf("StopLostProxyAt: %v", err)
	}
}

// offline waits until a connect from the agent to each address on the proxy port fails.
func (e *serverLostEnv) offline(ctx context.Context, t *testing.T, ref string, addrs ...string) {
	t.Helper()
	for _, addr := range addrs {
		keepWaitFor(t, serverLostCutWait, "the agent's connect to "+addr+":3128 failing", func() bool {
			code, _ := keepExec(ctx, t, e.r, ref, "nc -z -w2 "+addr+" 3128")
			return code != 0
		})
	}
}

// kept asserts the agent pod is still uid, has no owner, and is not being deleted.
func (e *serverLostEnv) kept(ctx context.Context, t *testing.T, ref string, uid string) {
	t.Helper()
	agent := e.pod(ctx, t, ref)
	if string(agent.UID) != uid || agent.DeletionTimestamp != nil {
		t.Errorf("agent pod UID %s, deleting %v; want %s kept", agent.UID, agent.DeletionTimestamp != nil, uid)
	}
	if len(agent.OwnerReferences) != 0 && !agentSandboxOn() {
		t.Errorf("agent pod owners = %+v, want none once its proxy pod went", agent.OwnerReferences)
	}
}

// revive starts a new proxy for the kept run, with a config whose first renew a hop fake answers.
func (e *serverLostEnv) revive(ctx context.Context, t *testing.T, runID uuid.UUID, ref string) {
	t.Helper()
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		t.Fatal(err)
	}
	host, token := "wardyn-hopfake-"+uuid.NewString()[:8], "lost-renew-token"
	startHopFakeK8s(t, e.restCfg, e.cs, e.ns, e.agentImage, ca, host, uuid.New(), token)
	pc := conformance.LoadableProxyConfig()
	pc.ControlPlaneURL, pc.ControlPlaneCAPEM, pc.RunToken = "https://"+host+":8443", string(ca.CertPEM), token
	cfg, err := runner.BuildProxyConfig(runID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	if err := e.r.CanReplaceProxy(ctx, ref); err != nil {
		t.Fatalf("CanReplaceProxy of the kept run: %v", err)
	}
	if _, err := e.r.ReplaceProxyHashedAt(ctx, ref, cfg, 0); err != nil {
		t.Fatalf("ReplaceProxyHashedAt: %v", err)
	}
	keepWaitFor(t, serverLostReachable, "the agent reaching the new proxy through the same Service", func() bool {
		return keepProxyReachable(ctx, t, e.r, ref)
	})
}

// TestK8sServerStopLostProxyKeepsAgentNoNetwork: the lost-run stop leaves the agent pod running with no network. A
// connect to the Service and to the old proxy pod's address fails, and a stream open through the proxy has ended.
func TestK8sServerStopLostProxyKeepsAgentNoNetwork(t *testing.T) {
	e := requireServerLost(t)
	ctx, cancel := context.WithTimeout(context.Background(), serverLostLimit)
	defer cancel()
	runID, ref := e.startRun(ctx, t)
	oldIP := e.pod(ctx, t, "wardyn-proxy-"+runID.String()).Status.PodIP
	agentUID := string(e.pod(ctx, t, ref).UID)
	if e.r.CanReplaceProxy(ctx, ref) == nil {
		t.Fatal("CanReplaceProxy of a running run = nil, want it refused")
	}

	sess, err := e.r.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", "echo open; sleep 600 | nc wardyn-proxy 3128; echo closed"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer func() { _ = sess.Close() }()
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(sess.Stdout)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
		close(lines)
	}()
	if l := <-lines; l != "open" {
		t.Fatalf("the stream's first line = %q, want open", l)
	}
	time.Sleep(2 * time.Second) // the connect is made

	e.stopLost(ctx, t, ref)
	select {
	case l := <-lines:
		if l != "closed" {
			t.Errorf("the stream printed %q, want closed", l)
		}
	case <-time.After(serverLostStreamWait):
		t.Errorf("the stream through the proxy was still open %s after StopLostProxyAt returned", serverLostStreamWait)
	}
	if _, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, "wardyn-proxy-"+runID.String(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the proxy pod after the stop: %v, want it gone", err)
	}
	e.kept(ctx, t, ref, agentUID)
	e.offline(ctx, t, ref, e.clusterIP(ctx, t, runID), oldIP)
}

// TestK8sServerReviveRestoresNetwork: a revive of the kept run reaches the agent through the same Service, and the
// agent pod is the same one.
func TestK8sServerReviveRestoresNetwork(t *testing.T) {
	e := requireServerLost(t)
	ctx, cancel := context.WithTimeout(context.Background(), serverLostLimit)
	defer cancel()
	runID, ref := e.startRun(ctx, t)
	agentUID := string(e.pod(ctx, t, ref).UID)
	e.stopLost(ctx, t, ref)
	e.revive(ctx, t, runID, ref)
	e.kept(ctx, t, ref, agentUID)
}

// TestK8sServerRunningProxyDeleteTakesAgent: a running run's proxy pod deleted out of band takes its agent with it.
func TestK8sServerRunningProxyDeleteTakesAgent(t *testing.T) {
	e := requireServerLost(t)
	ctx, cancel := context.WithTimeout(context.Background(), serverLostLimit)
	defer cancel()
	runID, ref := e.startRun(ctx, t)
	agentUID := e.pod(ctx, t, ref).UID
	if err := e.cs.CoreV1().Pods(e.ns).Delete(ctx, "wardyn-proxy-"+runID.String(), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the proxy pod: %v", err)
	}
	keepWaitFor(t, serverLostGCWait, "the agent pod going with its proxy pod", func() bool {
		pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, ref, metav1.GetOptions{})
		return apierrors.IsNotFound(err) || err == nil && (pod.UID != agentUID || pod.DeletionTimestamp != nil)
	})
}

// TestK8sServerKeptRunProxyDeleteLeavesAgentOffline: once a run is kept and revived, its new proxy pod deleted out of
// band leaves the agent pod, with no network, until the run's teardown deletes it.
func TestK8sServerKeptRunProxyDeleteLeavesAgentOffline(t *testing.T) {
	e := requireServerLost(t)
	ctx, cancel := context.WithTimeout(context.Background(), serverLostLimit)
	defer cancel()
	runID, ref := e.startRun(ctx, t)
	agentUID := string(e.pod(ctx, t, ref).UID)
	e.stopLost(ctx, t, ref)
	e.revive(ctx, t, runID, ref)
	proxyName := "wardyn-proxy-" + runID.String()
	newIP := e.pod(ctx, t, proxyName).Status.PodIP
	if err := e.cs.CoreV1().Pods(e.ns).Delete(ctx, proxyName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the new proxy pod: %v", err)
	}
	e.offline(ctx, t, ref, e.clusterIP(ctx, t, runID), newIP)
	time.Sleep(serverLostCutWait) // past the garbage collector's reaction to the delete
	e.kept(ctx, t, ref, agentUID)

	if err := e.r.StopSandbox(ctx, ref); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	keepWaitFor(t, serverLostStopWait, "the run's teardown deleting the agent pod", func() bool {
		_, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, ref, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}
