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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// lostServerRun is a server-mode run on the plain backend as its create leaves it: its Service, its EndpointSlice
// naming the proxy pod, the proxy pod (UID uid-<name>), and the agent pod owned by that proxy pod. Its namespace's
// verdict is proven, and the actions of the create are cleared.
func lostServerRun(t *testing.T) (*Driver, *fake.Clientset, string, uuid.UUID) {
	t.Helper()
	d, cs, spec, _ := serverRun(t, Config{}, readyAt7)
	installPodUIDReactor(cs)
	spec.Interactive = true
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if !d.netPolProvenIn(testNamespace) {
		t.Fatal("the rig's namespace verdict is not proven")
	}
	cs.ClearActions()
	return d, cs, sb.Ref, spec.RunID
}

// orphanGC does for a pod deleted with orphan propagation what the garbage collector does: it removes that pod from
// the owners of runID's agent pod and, with dyn, of its Sandbox. The fake clientset has no garbage collector.
func orphanGC(t *testing.T, cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient, runID uuid.UUID) {
	t.Helper()
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteActionImpl)
		if p := da.DeleteOptions.PropagationPolicy; p == nil || *p != metav1.DeletePropagationOrphan {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), da.GetName())
		if err != nil {
			return false, nil, nil
		}
		gone := obj.(*corev1.Pod).UID
		keep := func(refs []metav1.OwnerReference) []metav1.OwnerReference {
			return slices.DeleteFunc(slices.Clone(refs), func(r metav1.OwnerReference) bool { return r.UID == gone })
		}
		if agent, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), agentPodName(runID)); err == nil {
			p := agent.(*corev1.Pod).DeepCopy()
			p.OwnerReferences = keep(p.OwnerReferences)
			if err := cs.Tracker().Update(podsGVR, p, a.GetNamespace()); err != nil {
				t.Errorf("orphanGC: update the agent pod: %v", err)
			}
		}
		if dyn != nil {
			if obj, err := dyn.Tracker().Get(sandboxesGVR, a.GetNamespace(), agentPodName(runID)); err == nil {
				sb := obj.(*unstructured.Unstructured).DeepCopy()
				sb.SetOwnerReferences(keep(sb.GetOwnerReferences()))
				if err := dyn.Tracker().Update(sandboxesGVR, sb, a.GetNamespace()); err != nil {
					t.Errorf("orphanGC: update the Sandbox: %v", err)
				}
			}
		}
		return false, nil, nil
	})
}

// podDeletes is every pod delete the fake received, in order.
func podDeletes(cs *fake.Clientset) []clienttesting.DeleteActionImpl {
	var out []clienttesting.DeleteActionImpl
	for _, a := range cs.Actions() {
		if da, ok := a.(clienttesting.DeleteActionImpl); ok && da.GetResource().Resource == "pods" {
			out = append(out, da)
		}
	}
	return out
}

func isOrphanDelete(da clienttesting.DeleteActionImpl) bool {
	p := da.DeleteOptions.PropagationPolicy
	return p != nil && *p == metav1.DeletePropagationOrphan
}

// dynWrites is every dynamic-client action but a read, as "<verb> <resource>".
func dynWrites(dyn *dynamicfake.FakeDynamicClient) []string {
	var out []string
	for _, a := range dyn.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
	}
	return out
}

func getAgentPod(t *testing.T, cs *fake.Clientset, runID uuid.UUID) *corev1.Pod {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, agentPodName(runID))
	if err != nil {
		t.Fatalf("the agent pod: %v", err)
	}
	return obj.(*corev1.Pod)
}

func proxyPodGone(cs *fake.Clientset, runID uuid.UUID) bool {
	_, err := cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(runID))
	return apierrors.IsNotFound(err)
}

func TestStopLostProxy_NilOnlyAfterEndpointsEmptyAndPodGone(t *testing.T) {
	t.Run("the proxy pod goes", func(t *testing.T) {
		d, cs, ref, runID := lostServerRun(t)
		proxyUID := podUID(t, cs, proxyPodName(runID))
		agentUID := getAgentPod(t, cs, runID).UID
		if err := d.StopLostProxyAt(context.Background(), ref, 0); err != nil {
			t.Fatalf("StopLostProxyAt = %v, want nil", err)
		}
		if got := sliceIPs(keepSlice(t, cs, runID)); len(got) != 0 {
			t.Errorf("endpoints after the stop = %v, want none", got)
		}
		if !proxyPodGone(cs, runID) {
			t.Error("the proxy pod is still there after a nil answer")
		}
		if got := getAgentPod(t, cs, runID).UID; got != agentUID {
			t.Errorf("agent pod UID = %s, want %s: the agent is kept", got, agentUID)
		}
		w := writes(cs)
		if i, j := slices.Index(w, "update endpointslices"), slices.Index(w, "delete pods"); i < 0 || j < 0 || i > j {
			t.Errorf("writes = %v, want the EndpointSlice emptied before the proxy pod is deleted", w)
		}
		dels := podDeletes(cs)
		if len(dels) != 1 || dels[0].GetName() != proxyPodName(runID) {
			t.Fatalf("pod deletes = %+v, want only the proxy pod's", dels)
		}
		o := dels[0].DeleteOptions
		if g := o.GracePeriodSeconds; g == nil || *g != 1 {
			t.Errorf("proxy delete grace %v, want 1", g)
		}
		if !isOrphanDelete(dels[0]) {
			t.Errorf("proxy delete propagation %v, want Orphan", o.PropagationPolicy)
		}
		if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != proxyUID {
			t.Errorf("proxy delete preconditions %+v, want the UID read, %s", o.Preconditions, proxyUID)
		}
	})
	t.Run("a proxy pod that stays is never a nil answer", func(t *testing.T) {
		d, cs, ref, runID := lostServerRun(t)
		cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
			return a.(clienttesting.DeleteActionImpl).GetName() == proxyPodName(runID), nil, nil // accepted, never gone
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := d.StopLostProxyAt(ctx, ref, 0)
		if err == nil || errors.Is(err, runner.ErrEndUnsupported) {
			t.Fatalf("StopLostProxyAt with the proxy pod never gone = %v, want a retryable error", err)
		}
		if proxyPodGone(cs, runID) {
			t.Fatal("the reactor did not hold the proxy pod")
		}
	})
}

// podUID is the stored UID of pod name.
func podUID(t *testing.T, cs *fake.Clientset, name string) k8stypes.UID {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, name)
	if err != nil {
		t.Fatalf("pod %s: %v", name, err)
	}
	return obj.(*corev1.Pod).UID
}

func TestStopLostProxy_UnprovenNamespaceIsEndUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict *netPolVerdict
	}{
		{"acknowledged", &netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: netPolAcknowledged}}}},
		{"unenforced", &netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: netPolUnenforced}}}},
		{"not proven", &netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: netPolNotProven}}}},
		{"never checked", &netPolVerdict{byNS: map[string]nsVerdict{}}},
		{"no verdict", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, ref, runID := lostServerRun(t)
			d.cfg.AllowUnenforcedNetPol, d.cfg.AckAmbientDefaultDeny = true, true
			d.netPol.Store(tc.verdict)
			err := d.StopLostProxyAt(context.Background(), ref, 0)
			if !errors.Is(err, runner.ErrEndUnsupported) {
				t.Errorf("StopLostProxyAt = %v, want runner.ErrEndUnsupported", err)
			}
			if w := writes(cs); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
			if proxyPodGone(cs, runID) {
				t.Error("the proxy pod went on a namespace that is not proven")
			}
		})
	}
}

func TestStopLostProxy_PreUpgradeRunIsEndUnsupported(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, cs *fake.Clientset, runID uuid.UUID)
	}{
		{"no Service", func(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
			if err := cs.Tracker().Delete(servicesGVR, testNamespace, keepServiceName(runID)); err != nil {
				t.Fatal(err)
			}
		}},
		{"agent owned by another pod", func(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
			changeOwners(t, cs, runID, func(o []metav1.OwnerReference) []metav1.OwnerReference {
				o[0].UID = "another-pod-uid"
				return o
			})
		}},
		{"agent owned by its proxy pod, which is gone", func(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
			if err := cs.Tracker().Delete(podsGVR, testNamespace, proxyPodName(runID)); err != nil {
				t.Fatal(err)
			}
		}},
		{"agent with two owners", func(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
			changeOwners(t, cs, runID, func(o []metav1.OwnerReference) []metav1.OwnerReference {
				return append(o, metav1.OwnerReference{APIVersion: "v1", Kind: "Service", Name: keepServiceName(runID), UID: "svc-uid-default"})
			})
		}},
		{"agent pod gone", func(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
			if err := cs.Tracker().Delete(podsGVR, testNamespace, agentPodName(runID)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, ref, runID := lostServerRun(t)
			tc.change(t, cs, runID)
			cs.ClearActions()
			if err := d.StopLostProxyAt(ctx, ref, 0); !errors.Is(err, runner.ErrEndUnsupported) {
				t.Errorf("StopLostProxyAt = %v, want runner.ErrEndUnsupported (torn down)", err)
			}
			if w := writes(cs); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// changeOwners rewrites the stored agent pod's owner references.
func changeOwners(t *testing.T, cs *fake.Clientset, runID uuid.UUID, f func([]metav1.OwnerReference) []metav1.OwnerReference) {
	t.Helper()
	p := getAgentPod(t, cs, runID).DeepCopy()
	if len(p.OwnerReferences) != 1 {
		t.Fatalf("agent owners = %+v, want the proxy pod alone", p.OwnerReferences)
	}
	p.OwnerReferences = f(p.OwnerReferences)
	if err := cs.Tracker().Update(podsGVR, p, testNamespace); err != nil {
		t.Fatal(err)
	}
}

// setSandboxOwners rewrites the stored Sandbox's owner references.
func setSandboxOwners(t *testing.T, dyn *dynamicfake.FakeDynamicClient, runID uuid.UUID, owners []metav1.OwnerReference) {
	t.Helper()
	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, agentPodName(runID))
	if err != nil {
		t.Fatal(err)
	}
	sb := obj.(*unstructured.Unstructured).DeepCopy()
	sb.SetOwnerReferences(owners)
	if err := dyn.Tracker().Update(sandboxesGVR, sb, testNamespace); err != nil {
		t.Fatal(err)
	}
}

func sandboxOwners(t *testing.T, dyn *dynamicfake.FakeDynamicClient, runID uuid.UUID) []metav1.OwnerReference {
	t.Helper()
	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, agentPodName(runID))
	if err != nil {
		t.Fatalf("the Sandbox: %v", err)
	}
	return obj.(*unstructured.Unstructured).GetOwnerReferences()
}

func TestStopLostProxy_AgentSandboxReadsSandboxOwner(t *testing.T) {
	ctx := context.Background()
	t.Run("owned by the proxy pod read", func(t *testing.T) {
		d, cs, dyn, ref := asAdmitted(t)
		if o := metav1.GetControllerOf(getAgentPod(t, cs, asRunID)); o == nil || o.Kind != "Sandbox" {
			t.Fatalf("the agent pod's controller = %+v, want its Sandbox", o)
		}
		if o := sandboxOwners(t, dyn, asRunID); len(o) != 1 || o[0].Kind != "Pod" || o[0].UID != asProxyUID {
			t.Fatalf("Sandbox owners = %+v, want the proxy pod", o)
		}
		cs.ClearActions()
		dyn.ClearActions()
		if err := d.StopLostProxyAt(ctx, ref, 0); err != nil {
			t.Fatalf("StopLostProxyAt = %v, want nil", err)
		}
		if w := dynWrites(dyn); len(w) != 0 {
			t.Errorf("Sandbox writes = %v, want none: the agent is kept", w)
		}
		if dels := podDeletes(cs); len(dels) != 1 || dels[0].GetName() != proxyPodName(asRunID) || !isOrphanDelete(dels[0]) {
			t.Errorf("pod deletes = %+v, want the proxy pod's, with orphan propagation", dels)
		}
	})
	t.Run("owned by another pod", func(t *testing.T) {
		d, cs, dyn, ref := asAdmitted(t)
		setSandboxOwners(t, dyn, asRunID, []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: proxyPodName(asRunID), UID: "another-pod-uid"}})
		cs.ClearActions()
		dyn.ClearActions()
		if err := d.StopLostProxyAt(ctx, ref, 0); !errors.Is(err, runner.ErrEndUnsupported) {
			t.Errorf("StopLostProxyAt = %v, want runner.ErrEndUnsupported", err)
		}
		if w, dw := writes(cs), dynWrites(dyn); len(w)+len(dw) != 0 {
			t.Errorf("writes = %v, Sandbox writes = %v, want none", w, dw)
		}
	})
}

// failProxyDelete answers every delete of runID's proxy pod with a server error.
func failProxyDelete(cs *fake.Clientset, runID uuid.UUID) {
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.DeleteActionImpl).GetName() != proxyPodName(runID) {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New("test: the proxy pod delete fails"))
	})
}

func TestStopLostProxy_UnconfirmedProxyStopsAgent(t *testing.T) {
	ctx := context.Background()
	t.Run("plain", func(t *testing.T) {
		d, cs, ref, runID := lostServerRun(t)
		failProxyDelete(cs, runID)
		err := d.StopLostProxyAt(ctx, ref, 0)
		if err == nil || errors.Is(err, runner.ErrEndUnsupported) {
			t.Fatalf("StopLostProxyAt = %v, want a retryable error", err)
		}
		var agentDeleted bool
		for _, da := range podDeletes(cs) {
			agentDeleted = agentDeleted || da.GetName() == agentPodName(runID)
		}
		if !agentDeleted {
			t.Error("no delete of the agent pod: an unconfirmed proxy must stop the agent")
		}
		if _, err := cs.Tracker().Get(podsGVR, testNamespace, agentPodName(runID)); !apierrors.IsNotFound(err) {
			t.Errorf("the agent pod after the stop: %v, want it gone", err)
		}
		if err := d.StopLostProxyAt(ctx, ref, 0); !errors.Is(err, runner.ErrEndUnsupported) {
			t.Errorf("the next pass = %v, want runner.ErrEndUnsupported (torn down)", err)
		}
	})
	t.Run("agent-sandbox", func(t *testing.T) {
		d, cs, dyn, ref := asAdmitted(t)
		failProxyDelete(cs, asRunID)
		dyn.PrependReactor("delete", "sandboxes", func(a clienttesting.Action) (bool, runtime.Object, error) {
			_ = cs.Tracker().Delete(podsGVR, a.GetNamespace(), agentPodName(asRunID)) // the controller's pod goes with it
			return false, nil, nil
		})
		dyn.ClearActions()
		err := d.StopLostProxyAt(ctx, ref, 0)
		if err == nil || errors.Is(err, runner.ErrEndUnsupported) {
			t.Fatalf("StopLostProxyAt = %v, want a retryable error", err)
		}
		if w := dynWrites(dyn); !slices.Contains(w, "delete sandboxes") {
			t.Errorf("Sandbox writes = %v, want the Sandbox deleted: the controller makes a deleted pod again", w)
		}
		if _, err := cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID)); !apierrors.IsNotFound(err) {
			t.Errorf("the agent pod after the stop: %v, want it gone", err)
		}
	})
}

func TestStopLostProxyAt_NewerGenerationRefused(t *testing.T) {
	d, cs, ref, runID := lostServerRun(t)
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(runID))
	if err != nil {
		t.Fatal(err)
	}
	p := obj.(*corev1.Pod).DeepCopy()
	p.Labels[labelProxyGeneration] = "5"
	if err := cs.Tracker().Update(podsGVR, p, testNamespace); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	if err := d.StopLostProxyAt(context.Background(), ref, 3); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Errorf("StopLostProxyAt(3) under a generation 5 proxy = %v, want runner.ErrProxyGenerationNewer", err)
	}
	if w := writes(cs); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if err := d.StopLostProxyAt(context.Background(), ref, 5); err != nil {
		t.Errorf("StopLostProxyAt(5) = %v, want nil", err)
	}
}

func TestServerK8sStopProxyStillUnsupported(t *testing.T) {
	d, cs, ref, runID := lostServerRun(t)
	stopper, ok := served(d).(runner.ProxyStopper)
	if !ok {
		t.Fatalf("served = %T, not a runner.ProxyStopper", served(d))
	}
	if err := stopper.StopProxy(context.Background(), ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StopProxy in server mode = %v, want runner.ErrEndUnsupported", err)
	}
	if w := writes(cs); len(w) != 0 || proxyPodGone(cs, runID) {
		t.Errorf("writes = %v, want none and the proxy pod kept", w)
	}
}

func TestStopLostProxy_OrphanOnlyOnKeepPath(t *testing.T) {
	ctx := context.Background()
	d, cs, ref, runID := lostServerRun(t)
	if err := d.StopLostProxyAt(ctx, ref, 0); err != nil {
		t.Fatalf("StopLostProxyAt = %v", err)
	}
	var orphans []string
	for _, da := range podDeletes(cs) {
		if isOrphanDelete(da) {
			orphans = append(orphans, da.GetName())
		}
	}
	if !slices.Equal(orphans, []string{proxyPodName(runID)}) {
		t.Errorf("orphan deletes = %v, want only the proxy pod's", orphans)
	}

	miss, mcs, mref, _ := lostServerRun(t)
	miss.netPol.Store(&netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: netPolAcknowledged}}})
	if err := miss.StopLostProxyAt(ctx, mref, 0); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("StopLostProxyAt on a gate miss = %v", err)
	}
	if dels := podDeletes(mcs); len(dels) != 0 {
		t.Errorf("pod deletes on a gate miss = %+v, want none", dels)
	}

	k, kcs, _, kref := keptRun(t)
	if err := k.ReplaceProxy(ctx, kref, replaceCfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	ended := newKeptRig(t, nil)
	ended.end(t)
	for _, da := range append(podDeletes(kcs), podDeletes(ended.cs)...) {
		if da.DeleteOptions.PropagationPolicy != nil {
			t.Errorf("keep-path delete of %s carries propagation %v, want none", da.GetName(), *da.DeleteOptions.PropagationPolicy)
		}
	}
}

// Every removeProxyPod call passes false but StopLostProxyAt's.
func TestRemoveProxyPodOrphansOnlyForTheLostStop(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "removeProxyPod" || len(call.Args) != 5 {
					return true
				}
				calls++
				arg, _ := call.Args[4].(*ast.Ident)
				want := "false"
				if fn.Name.Name == "StopLostProxyAt" {
					want = "true"
				}
				if arg == nil || arg.Name != want {
					t.Errorf("%s: %s calls removeProxyPod with orphanAgent %v, want %s", path, fn.Name.Name, call.Args[4], want)
				}
				return true
			})
		}
	}
	if calls != 4 {
		t.Errorf("removeProxyPod calls = %d, want 4 (StopLostProxyAt, swapProxy, stopKeptProxy, createReplacementPod)", calls)
	}
}

func TestServerCreate_AgentStillOwnedByProxy(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		_, cs, _, runID := lostServerRun(t)
		proxyUID := podUID(t, cs, proxyPodName(runID))
		o := getAgentPod(t, cs, runID).OwnerReferences
		if len(o) != 1 || o[0].Kind != "Pod" || o[0].Name != proxyPodName(runID) || o[0].UID != proxyUID {
			t.Errorf("agent owners = %+v, want exactly the proxy pod it was created with (%s)", o, proxyUID)
		}
	})
	t.Run("agent-sandbox", func(t *testing.T) {
		_, _, dyn, _ := asAdmitted(t)
		o := sandboxOwners(t, dyn, asRunID)
		if len(o) != 1 || o[0].Kind != "Pod" || o[0].Name != proxyPodName(asRunID) || o[0].UID != asProxyUID {
			t.Errorf("Sandbox owners = %+v, want exactly the proxy pod it was created with", o)
		}
	})
}
