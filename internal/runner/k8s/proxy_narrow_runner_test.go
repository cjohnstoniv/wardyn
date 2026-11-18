// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// runnerCreateHarness is a runner harness set up so CreateSandbox succeeds against a stand-in proxy.
func runnerCreateHarness(t *testing.T) (*runnerHarness, runner.SandboxSpec) {
	t.Helper()
	px := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(px.Close)
	h := newRunnerHarness(t)
	h.d.cfg.ProxyImage = "registry.example/wardyn-proxy:pinned"
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	spec := testSandboxSpec()
	spec.RunID = h.runID
	return h, spec
}

// A runner's proxy pod gets the live deny env, a read-write mount and a memory-backed 1Mi emptyDir.
func TestProxyPod_RunnerLiveDenyVolumeIsMemory(t *testing.T) {
	h, spec := runnerCreateHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("runner CreateSandbox: %v", err)
	}
	pod, err := h.cs.CoreV1().Pods(testNamespace).Get(ctx, proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get runner proxy pod: %v", err)
	}
	c := containerByName(pod.Spec.Containers, proxyContainerName)
	if c == nil {
		t.Fatalf("proxy pod has no %q container", proxyContainerName)
	}
	envs, mounts := 0, 0
	for _, e := range c.Env {
		if e.Name == proxy.LiveDenyDirEnv && e.Value == proxyLiveDenyMountDir {
			envs++
		}
	}
	for _, m := range c.VolumeMounts {
		if m.Name == proxyLiveDenyVolumeName && m.MountPath == proxyLiveDenyMountDir && !m.ReadOnly {
			mounts++
		}
	}
	if envs != 1 || mounts != 1 {
		t.Errorf("live deny env x%d, mount x%d, want one each: %+v %+v", envs, mounts, c.Env, c.VolumeMounts)
	}
	vol, ok := findVolume(pod.Spec.Volumes, proxyLiveDenyVolumeName)
	if !ok || vol.EmptyDir == nil {
		t.Fatalf("no emptyDir volume %q: %+v", proxyLiveDenyVolumeName, pod.Spec.Volumes)
	}
	if vol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("medium = %q, want memory: the proxy never restarts on a runner", vol.EmptyDir.Medium)
	}
	if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.String() != "1Mi" {
		t.Errorf("size limit = %v, want 1Mi", vol.EmptyDir.SizeLimit)
	}
}

// Create records the base of the config the proxy is handed before any pod exists.
func TestCreateSandbox_RunnerRecordsProxyBaseBeforeThePod(t *testing.T) {
	h, spec := runnerCreateHarness(t)
	var atPodCreate string
	podCreates := 0
	h.cs.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		podCreates++
		if podCreates == 1 {
			atPodCreate = h.ev.get(t, h.runID).ProxyBaseSHA256
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("runner CreateSandbox: %v", err)
	}
	if atPodCreate == "" {
		t.Fatal("no ProxyBaseSHA256 was recorded when the first pod was created")
	}
	secrets, err := h.cs.CoreV1().Secrets(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, s := range secrets.Items {
		if cfg, ok := s.Data[proxyConfigSecretKey]; ok {
			if want, err = proxy.LiveDenyBaseSHA256(cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	if want == "" || atPodCreate != want {
		t.Errorf("recorded base %q, want the base of the proxy's config Secret %q", atPodCreate, want)
	}
}

// A failed save of the base fails the create, and no pod exists.
func TestCreateSandbox_RunnerBaseSaveFailureFailsTheCreate(t *testing.T) {
	h, spec := runnerCreateHarness(t)
	h.ev.failSave = func(b []byte) bool { return strings.Contains(string(b), "proxy_base_sha256") }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("CreateSandbox succeeded although the base could not be saved")
	}
	pods, err := h.cs.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("%d pods exist after a failed create, want none", len(pods.Items))
	}
}

// recordBase stands in for create: it saves base as the run's recorded proxy base.
func recordBase(t *testing.T, h *runnerHarness, base string) {
	t.Helper()
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.ProxyBaseSHA256 = base
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func answerWithBase(t *testing.T, base string, changed bool) string {
	t.Helper()
	state := "current"
	if changed {
		state = "applied"
	}
	return narrowJSON(t, proxy.NarrowAnswer{State: state, Changed: changed, ConfigSHA256: "cfg", BaseSHA256: base, LiveDenies: []string{"a.example"}})
}

// With no record the probe answers Unsupported, and NarrowProxy stops at its probe: one exec, no apply.
func TestNarrowProxy_RunnerNoRecordIsUnsupportedBeforeTheApply(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	ex := &narrowExecutor{stdout: answerWithBase(t, "base", false)}
	h.d.execFactory = ex.execFactory
	ref := agentPodName(h.runID)
	if _, err := h.d.ProxyNarrowState(context.Background(), ref); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("ProxyNarrowState with no record = %v, want ErrNarrowUnsupported", err)
	}
	ex.calls, ex.gotStdin = 0, ""
	if _, err := h.d.NarrowProxy(context.Background(), ref, []string{"a.example"}); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("NarrowProxy with no record = %v, want ErrNarrowUnsupported", err)
	}
	if ex.calls != 1 || ex.gotStdin != `{"deny":[]}` {
		t.Errorf("%d execs, last stdin %q; want the probe only (one exec, an empty list)", ex.calls, ex.gotStdin)
	}
}

// A base that differs from the record is Unsupported before the apply on both calls.
func TestNarrowProxy_RunnerBaseMismatchBeforeTheApply(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	recordBase(t, h, "recorded")
	ex := &narrowExecutor{stdout: answerWithBase(t, "other", false)}
	h.d.execFactory = ex.execFactory
	if _, err := h.d.NarrowProxy(context.Background(), agentPodName(h.runID), []string{"a.example"}); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("NarrowProxy with a mismatched base = %v, want ErrNarrowUnsupported", err)
	}
	if ex.calls != 1 {
		t.Errorf("%d execs, want the probe only", ex.calls)
	}
}

// A base that matches on the probe and differs on the apply answer is uncertain: the list may be applied.
func TestNarrowProxy_RunnerBaseMismatchOnTheApplyIsUncertain(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	recordBase(t, h, "recorded")
	seq := &seqExecutor{outs: []string{answerWithBase(t, "recorded", false), answerWithBase(t, "other", true)}}
	h.d.execFactory = seq.execFactory
	_, err := h.d.NarrowProxy(context.Background(), agentPodName(h.runID), []string{"a.example"})
	if !errors.Is(err, runner.ErrNarrowUncertain) {
		t.Fatalf("NarrowProxy = %v, want ErrNarrowUncertain", err)
	}
	if seq.calls != 2 {
		t.Errorf("%d execs, want the probe and the apply", seq.calls)
	}
}

// A matching base gives BaseChecked on the probe and on the apply.
func TestNarrowProxy_RunnerMatchIsBaseChecked(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	recordBase(t, h, "recorded")
	ref := agentPodName(h.runID)
	probe := &narrowExecutor{stdout: answerWithBase(t, "recorded", false)}
	h.d.execFactory = probe.execFactory
	st, err := h.d.ProxyNarrowState(context.Background(), ref)
	if err != nil || !st.BaseChecked {
		t.Errorf("ProxyNarrowState = %+v, %v; want BaseChecked", st, err)
	}
	seq := &seqExecutor{outs: []string{answerWithBase(t, "recorded", false), answerWithBase(t, "recorded", true)}}
	h.d.execFactory = seq.execFactory
	st, err = h.d.NarrowProxy(context.Background(), ref, []string{"a.example"})
	if err != nil || !st.BaseChecked || !st.Changed || st.ConfigSHA256 != "cfg" {
		t.Errorf("NarrowProxy = %+v, %v; want BaseChecked, Changed and the answered hash", st, err)
	}
}

// A probe failure that is not Unsupported or NotListening, and a read error before any exec, answer
// NotSent and never Uncertain, in runner mode and in server mode.
func TestNarrowProxy_NothingSentWhenNoExecRan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner bool
	}{{"runner", true}, {"server", false}} {
		t.Run(tc.name+" no proxy pod", func(t *testing.T) {
			var d *Driver
			var ref string
			if tc.runner {
				h := newRunnerHarness(t)
				d, ref = h.d, agentPodName(h.runID)
			} else {
				d, _ = newTestDriver(t, Config{})
				ref = agentPodName(uuid.New())
			}
			ex := &narrowExecutor{}
			d.execFactory = ex.execFactory
			for name, call := range map[string]func() error{
				"probe":  func() error { _, err := d.ProxyNarrowState(context.Background(), ref); return err },
				"narrow": func() error { _, err := d.NarrowProxy(context.Background(), ref, []string{"a.example"}); return err },
			} {
				err := call()
				if !errors.Is(err, runner.ErrNarrowNotSent) || errors.Is(err, runner.ErrNarrowUncertain) {
					t.Errorf("%s = %v, want ErrNarrowNotSent and not Uncertain", name, err)
				}
			}
			if ex.calls != 0 {
				t.Errorf("%d execs, want none", ex.calls)
			}
		})
	}
}

// A probe that fails after the exec starts (not Unsupported, not NotListening) inside NarrowProxy is
// NotSent: an empty list applies nothing. A NotListening probe keeps its answer.
func TestNarrowProxy_RunnerProbeFailureIsNotSent(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	recordBase(t, h, "recorded")
	ex := &narrowExecutor{err: errors.New("websocket: close 1006")}
	h.d.execFactory = ex.execFactory
	_, err := h.d.NarrowProxy(context.Background(), agentPodName(h.runID), []string{"a.example"})
	if !errors.Is(err, runner.ErrNarrowNotSent) || errors.Is(err, runner.ErrNarrowUncertain) {
		t.Errorf("NarrowProxy with a failed probe = %v, want ErrNarrowNotSent and not Uncertain", err)
	}
	if ex.calls != 1 {
		t.Errorf("%d execs, want the probe only", ex.calls)
	}
}

// Server mode never sets BaseChecked and does not probe before the apply.
func TestNarrowProxy_ServerModeNeverProbesFirst(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	narrowProxyPod(t, cs, runID, true, true)
	ex := &narrowExecutor{stdout: answerWithBase(t, "anything", true)}
	d.execFactory = ex.execFactory
	st, err := d.NarrowProxy(context.Background(), agentPodName(runID), []string{"a.example"})
	if err != nil || st.BaseChecked {
		t.Errorf("NarrowProxy = %+v, %v; want no error and BaseChecked false", st, err)
	}
	if ex.calls != 1 || ex.gotStdin != `{"deny":["a.example"]}` {
		t.Errorf("%d execs, stdin %q; want one exec carrying the list", ex.calls, ex.gotStdin)
	}
	st, err = d.ProxyNarrowState(context.Background(), agentPodName(runID))
	if err != nil || st.BaseChecked {
		t.Errorf("ProxyNarrowState = %+v, %v; want BaseChecked false", st, err)
	}
}
