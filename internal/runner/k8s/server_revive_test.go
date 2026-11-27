// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// replaceRefused asserts that every replacement method of rv refuses ref with runner.ErrReviveUnsupported.
func replaceRefused(t *testing.T, rv runner.ProxyReviverAt, ref string) {
	t.Helper()
	ctx := context.Background()
	full := rv.(interface {
		runner.ProxyReviver
		runner.ProxyReviverHashed
	})
	if err := full.CanReplaceProxy(ctx, ref); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("CanReplaceProxy = %v, want runner.ErrReviveUnsupported", err)
	}
	if err := full.ReplaceProxy(ctx, ref, replaceCfg); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ReplaceProxy = %v, want runner.ErrReviveUnsupported", err)
	}
	if _, err := full.ReplaceProxyHashed(ctx, ref, replaceCfg); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ReplaceProxyHashed = %v, want runner.ErrReviveUnsupported", err)
	}
	if _, err := rv.ReplaceProxyHashedAt(ctx, ref, replaceCfg, 4); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ReplaceProxyHashedAt = %v, want runner.ErrReviveUnsupported", err)
	}
}

func serverDriverOf(t *testing.T, d *Driver) *serverDriver {
	t.Helper()
	s, ok := served(d).(*serverDriver)
	if !ok {
		t.Fatalf("served in server mode with keep off = %T, want *serverDriver", served(d))
	}
	return s
}

// keptLostRun is lostServerRun after StopLostProxyAt kept it, with the garbage collector's release of the agent.
func keptLostRun(t *testing.T) (*serverDriver, *fake.Clientset, string, *corev1.Pod) {
	t.Helper()
	d, cs, ref, runID := lostServerRun(t)
	orphanGC(t, cs, nil, runID)
	if err := d.StopLostProxyAt(context.Background(), ref, 0); err != nil {
		t.Fatalf("StopLostProxyAt = %v", err)
	}
	agent := getAgentPod(t, cs, runID)
	if len(agent.OwnerReferences) != 0 {
		t.Fatalf("agent owners after the stop = %+v, want none", agent.OwnerReferences)
	}
	cs.ClearActions()
	return serverDriverOf(t, d), cs, ref, agent
}

func TestServerDriver_RunningRunNotReplaceable(t *testing.T) {
	d, cs, ref, _ := lostServerRun(t)
	s := serverDriverOf(t, d)
	replaceRefused(t, s, ref)
	if w := writes(cs); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if err := s.EnsureProxyImage(context.Background()); err != nil {
		t.Errorf("EnsureProxyImage = %v, want nil", err)
	}
}

func TestServerDriver_CrashedProxyNotReplaceable(t *testing.T) {
	s, cs, ref, agent := keptLostRun(t)
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		t.Fatal(err)
	}
	crashed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(runID), Namespace: testNamespace, UID: "crashed-proxy-uid", Labels: wardynLabels(runID, componentProxy, nil)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	if err := cs.Tracker().Create(podsGVR, crashed, testNamespace); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	replaceRefused(t, s, ref)
	if w := writes(cs); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if got := getAgentPod(t, cs, runID).UID; got != agent.UID {
		t.Errorf("agent pod UID = %s, want %s", got, agent.UID)
	}
}

func TestServerDriver_StoppedLostRunReplaceable(t *testing.T) {
	ctx := context.Background()
	s, cs, ref, agent := keptLostRun(t)
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CanReplaceProxy(ctx, ref); err != nil {
		t.Fatalf("CanReplaceProxy of a kept lost run = %v, want nil", err)
	}
	if _, err := s.ReplaceProxyHashedAt(ctx, ref, replaceCfg, 0); err != nil {
		t.Fatalf("ReplaceProxyHashedAt = %v, want nil", err)
	}
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(runID))
	if err != nil {
		t.Fatalf("the new proxy pod: %v", err)
	}
	if o := obj.(*corev1.Pod).OwnerReferences; len(o) != 0 {
		t.Errorf("new proxy pod owners = %+v, want none", o)
	}
	if got := sliceIPs(keepSlice(t, cs, runID)); !slices.Equal(got, []string{"10.244.0.7"}) {
		t.Errorf("endpoints after the revive = %v, want the new proxy pod's IP", got)
	}
	a := getAgentPod(t, cs, runID)
	if a.UID != agent.UID || len(a.OwnerReferences) != 0 {
		t.Errorf("agent pod after the revive: UID %s owners %+v, want UID %s and no owner", a.UID, a.OwnerReferences, agent.UID)
	}
	if err := s.CanReplaceProxy(ctx, ref); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("CanReplaceProxy once the new proxy runs = %v, want runner.ErrReviveUnsupported", err)
	}
}

func TestKeepDriverDeclaresStopLostProxyAt(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "agentsandbox_revive.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "StopLostProxyAt" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "keepDriver" {
				return
			}
		}
	}
	t.Error("agentsandbox_revive.go declares no (*keepDriver).StopLostProxyAt: the keep backend's answer must be its own")
}

func TestKeepDriverStopLostProxyAt_KeepAndNonKeepRuns(t *testing.T) {
	ctx := context.Background()
	t.Run("keep run", func(t *testing.T) {
		k, cs, dyn, ref := keptRun(t)
		before := sandboxOwners(t, dyn, asRunID)
		if len(before) != 1 || before[0].Kind != "Service" {
			t.Fatalf("keep run's Sandbox owners = %+v, want its Service", before)
		}
		cs.ClearActions()
		dyn.ClearActions()
		if err := k.StopLostProxyAt(ctx, ref, 0); err != nil {
			t.Fatalf("StopLostProxyAt = %v, want nil", err)
		}
		if w := dynWrites(dyn); len(w) != 0 {
			t.Errorf("Sandbox writes = %v, want none", w)
		}
		if after := sandboxOwners(t, dyn, asRunID); !slices.Equal(after, before) {
			t.Errorf("Sandbox owners = %+v, want them unchanged (%+v)", after, before)
		}
		if !proxyPodGone(cs, asRunID) {
			t.Error("the proxy pod is still there")
		}
	})
	t.Run("non-keep run on a keep install", func(t *testing.T) {
		k, cs, dyn, ref := nonKeepRunOnKeepInstall(t)
		dyn.ClearActions()
		if err := k.StopLostProxyAt(ctx, ref, 0); err != nil {
			t.Fatalf("StopLostProxyAt = %v, want nil", err)
		}
		if dels := podDeletes(cs); len(dels) != 1 || dels[0].GetName() != proxyPodName(asRunID) || !isOrphanDelete(dels[0]) {
			t.Errorf("pod deletes = %+v, want the proxy pod's, with orphan propagation", dels)
		}
		if w := dynWrites(dyn); len(w) != 0 {
			t.Errorf("Sandbox writes = %v, want none", w)
		}
	})
}

// nonKeepRunOnKeepInstall is a live run that is not kept at its end, on an install with keep on: its Sandbox is owned
// by its proxy pod.
func nonKeepRunOnKeepInstall(t *testing.T) (*keepDriver, *fake.Clientset, *dynamicfake.FakeDynamicClient, string) {
	t.Helper()
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	spec := keptSpec()
	spec.Interactive = false
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if o := sandboxOwners(t, dyn, asRunID); len(o) != 1 || o[0].Kind != "Pod" {
		t.Fatalf("non-keep run's Sandbox owners = %+v, want its proxy pod", o)
	}
	cs.ClearActions()
	return &keepDriver{Driver: d}, cs, dyn, sb.Ref
}

func TestKeepDriver_NonKeepRunningRunNotReplaceable(t *testing.T) {
	ctx := context.Background()
	k, cs, dyn, ref := nonKeepRunOnKeepInstall(t)
	dyn.ClearActions()
	replaceRefused(t, k, ref)
	if w, dw := writes(cs), dynWrites(dyn); len(w)+len(dw) != 0 {
		t.Errorf("writes = %v, Sandbox writes = %v, want none", w, dw)
	}

	live, _, _, liveRef := keptRun(t)
	if err := live.CanReplaceProxy(ctx, liveRef); err != nil {
		t.Errorf("CanReplaceProxy of a live keep run = %v, want nil", err)
	}

	if err := dyn.Tracker().Delete(sandboxesGVR, testNamespace, agentPodName(asRunID)); err != nil {
		t.Fatal(err)
	}
	if err := k.CanReplaceProxy(ctx, ref); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("CanReplaceProxy with the Sandbox gone = %v, want runner.ErrReviveUnsupported (through the keep-off gates)", err)
	}

	k2, _, dyn2, ref2 := nonKeepRunOnKeepInstall(t)
	dyn2.PrependReactor("get", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("test: the Sandbox read fails"))
	})
	err := k2.CanReplaceProxy(ctx, ref2)
	if err == nil || errors.Is(err, runner.ErrReviveUnsupported) || !strings.Contains(err.Error(), "read the agent's Sandbox") {
		t.Errorf("CanReplaceProxy with the Sandbox read failing = %v, want a plain read error", err)
	}
}
