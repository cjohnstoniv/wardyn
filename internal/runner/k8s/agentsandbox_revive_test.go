// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// replaceCfg is the proxy config the API would pass: only the fields the new proxy pod is built from matter here.
var replaceCfg = []byte(`{"control_plane_url":"http://wardynd:8080"}`)

// keptRun is a keep-on rig with a run that can be kept created, its writes not yet recorded.
func keptRun(t *testing.T) (*keepDriver, *fake.Clientset, *dynamicfake.FakeDynamicClient, string) {
	t.Helper()
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	sb, err := d.CreateSandbox(context.Background(), keptSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return &keepDriver{Driver: d}, cs, dyn, sb.Ref
}

func TestKeepDriverIsTheProxyReviver(t *testing.T) {
	var _ runner.ProxyReviver = (*keepDriver)(nil)
	var _ runner.ProxyReviverHashed = (*keepDriver)(nil)
	off, _ := newTestDriver(t, Config{})
	if _, ok := any(off).(runner.ProxyReviver); ok {
		t.Error("a plain *Driver is a runner.ProxyReviver; only the keep-on substrate may be")
	}
	if _, ok := served(off).(*Driver); !ok {
		t.Errorf("served with keep off = %T, want *Driver", served(off))
	}
	on, _, _ := newSandboxRig(t, Config{AgentSandboxKeep: true}, controllerMaking(t, nil))
	if _, ok := served(on).(*keepDriver); !ok {
		t.Errorf("served with keep on = %T, want *keepDriver", served(on))
	}
}

func TestCanReplaceProxyRefusesARunNotKept(t *testing.T) {
	k, _, _, ref := keptRun(t)
	if err := k.CanReplaceProxy(context.Background(), ref); err != nil {
		t.Errorf("CanReplaceProxy of a kept run = %v, want nil", err)
	}
	d, _, _ := keepRig(t, Config{AgentSandboxKeep: true})
	spec := keptSpec()
	spec.Interactive = false
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := (&keepDriver{Driver: d}).CanReplaceProxy(context.Background(), sb.Ref); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("CanReplaceProxy of a run not kept = %v, want ErrReviveUnsupported", err)
	}
}

func TestReplaceProxyOrder(t *testing.T) {
	k, cs, dyn, ref := keptRun(t)
	// The replacement reads another IP than the proxy it replaces, so writing back any IP read before the swap fails.
	created := false
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Name == proxyPodName(asRunID) {
			created = true
		}
		return false, nil, nil
	})
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if !created || a.(clienttesting.GetAction).GetName() != proxyPodName(asRunID) {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), proxyPodName(asRunID))
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.PodIP = "10.244.0.9"
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: true}}
		return true, pod, nil
	})
	order := writesInOrder(cs, dyn)
	if err := k.ReplaceProxy(context.Background(), ref, replaceCfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	// The created proxy mounts the run's own Secret, which a replace never deletes.
	want := []string{"create secrets", "update endpointslices", "delete pods", "create pods", "update endpointslices"}
	if !slices.Equal(*order, want) {
		t.Errorf("writes = %v\nwant %v: the agent Sandbox and pod see no write", *order, want)
	}
	if got := sliceIPs(keepSlice(t, cs, asRunID)); !slices.Equal(got, []string{"10.244.0.9"}) {
		t.Errorf("endpoints after the replace = %v, want the new proxy pod's IP 10.244.0.9, not the old 10.244.0.7", got)
	}
}

func TestReplaceProxyUsesTheNewSecretAndLabels(t *testing.T) {
	k, cs, _, ref := keptRun(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := k.ReplaceProxy(ctx, ref, replaceCfg); err != nil {
			t.Fatalf("ReplaceProxy %d: %v", i, err)
		}
	}
	names := cfgSecretNames(t, cs)
	if len(names) != 1 {
		t.Fatalf("replacement Secrets = %v, want only the newest", names)
	}
	obj, err := cs.Tracker().Get(secretsGVR, testNamespace, names[0])
	if err != nil {
		t.Fatal(err)
	}
	newest := obj.(*corev1.Secret)
	if !strings.HasPrefix(newest.Name, secretName(asRunID)+"-r") || string(newest.Data[proxyConfigSecretKey]) != string(replaceCfg) {
		t.Errorf("replacement Secret %q holds %q, want the passed config under a -r<stamp> name", newest.Name, newest.Data[proxyConfigSecretKey])
	}
	if !secretStored(t, cs, secretName(asRunID)) {
		t.Error("the run's own Secret is gone")
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(ctx, proxyPodName(asRunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the new proxy pod: %v", err)
	}
	named := slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Secret != nil && v.Secret.SecretName == newest.Name })
	if !named {
		t.Errorf("the new proxy pod's volumes = %+v, want one from Secret %s", pod.Spec.Volumes, newest.Name)
	}
	if pod.Labels["team"] != "test" {
		t.Errorf("proxy pod labels = %v, want the run's extra label team=test", pod.Labels)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Error("the new proxy pod has no active deadline; the run's own was 2h")
	}
}

// Every server run has a Service now, so the run here is a kept one whose Service is gone: kept-ness alone does not let
// a replace start.
func TestReplaceProxyNoServiceIsUnsupported(t *testing.T) {
	k, cs, dyn, ref := keptRun(t)
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("services"), testNamespace, keepServiceName(runID)); err != nil {
		t.Fatalf("remove the run's Service: %v", err)
	}
	order := writesInOrder(cs, dyn)
	if err := k.ReplaceProxy(context.Background(), ref, replaceCfg); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Fatalf("ReplaceProxy of a run with no Service = %v, want ErrReviveUnsupported", err)
	}
	if len(*order) != 0 {
		t.Errorf("writes = %v, want none", *order)
	}
}

// A server run that is not kept has its own Service like every server run, and is still no run a replace may touch: it
// answers revive_unsupported and nothing is written, so its proxy pod, and with it the agent, stays.
func TestReplaceProxyRefusesANotKeptRunWithItsService(t *testing.T) {
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	spec := keptSpec()
	spec.Drives = nil
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := cs.CoreV1().Services(testNamespace).Get(context.Background(), keepServiceName(spec.RunID), metav1.GetOptions{}); err != nil {
		t.Fatalf("the run's Service: %v, want it made", err)
	}
	k := &keepDriver{Driver: d}
	order := writesInOrder(cs, dyn)
	if err := k.CanReplaceProxy(context.Background(), sb.Ref); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("CanReplaceProxy of a run not kept, with its Service = %v, want ErrReviveUnsupported", err)
	}
	if err := k.ReplaceProxy(context.Background(), sb.Ref, replaceCfg); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ReplaceProxy of a run not kept, with its Service = %v, want ErrReviveUnsupported", err)
	}
	if len(*order) != 0 {
		t.Errorf("writes = %v, want none", *order)
	}
}

func TestReplaceProxyFailsAfterDeleteWrapsReplaceFailed(t *testing.T) {
	k, cs, _, ref := keptRun(t)
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "wardyn-proxy-") {
			return true, nil, errors.New("proxy pod create refused")
		}
		return false, nil, nil
	})
	err := k.ReplaceProxy(context.Background(), ref, replaceCfg)
	if !errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy = %v, want ErrProxyReplaceFailed", err)
	}
	if got := sliceIPs(keepSlice(t, cs, asRunID)); len(got) != 0 {
		t.Errorf("endpoints after the failed replace = %v, want none", got)
	}
	if names := cfgSecretNames(t, cs); len(names) != 0 {
		t.Errorf("replacement config Secrets after the refused create = %v, want none: no pod mounts the one it wrote", names)
	}
}

func TestReplaceProxyBeforeAnyChangeIsNotReplaceFailed(t *testing.T) {
	k, cs, dyn, ref := keptRun(t)
	order := writesInOrder(cs, dyn)
	err := k.ReplaceProxy(context.Background(), ref, []byte("not json"))
	if err == nil || errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy of a bad config = %v, want a plain error: the old proxy still runs", err)
	}
	if len(*order) != 0 {
		t.Errorf("writes = %v, want none", *order)
	}
}

func TestReplaceProxyHashedReturnsTheConfigHash(t *testing.T) {
	k, _, _, ref := keptRun(t)
	got, err := k.ReplaceProxyHashed(context.Background(), ref, replaceCfg)
	if err != nil {
		t.Fatalf("ReplaceProxyHashed: %v", err)
	}
	if want := proxy.ConfigSHA256(replaceCfg); got != want {
		t.Errorf("hash = %q, want %q", got, want)
	}
}
