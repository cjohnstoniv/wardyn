// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const heldPodUID = k8stypes.UID("held-pod-uid")

// claudeKeptSpec is a run that can be kept, of an agent that is revived.
func claudeKeptSpec() runner.SandboxSpec {
	spec := keptSpec()
	spec.Labels = map[string]string{"team": "test", labelAgent: "claude-code"}
	return spec
}

type keptRig struct {
	k   *keepDriver
	cs  *fake.Clientset
	dyn *dynamicfake.FakeDynamicClient
	ref string
}

// newKeptRig is a keep-on rig with a run created from claudeKeptSpec after mutate. Its recorded actions are cleared.
func newKeptRig(t *testing.T, mutate func(*runner.SandboxSpec)) keptRig {
	t.Helper()
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	spec := claudeKeptSpec()
	if mutate != nil {
		mutate(&spec)
	}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	cs.ClearActions()
	return keptRig{k: &keepDriver{Driver: d}, cs: cs, dyn: dyn, ref: sb.Ref}
}

func (r keptRig) end(t *testing.T) {
	t.Helper()
	if err := r.k.EndSandbox(context.Background(), r.ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
}

// holdPod makes the pod the controller makes again after an end: from the Sandbox's template, gated, under a new UID.
func (r keptRig) holdPod(t *testing.T, mutate func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	pod, err := r.makeHeldPod(t, mutate)
	if err != nil {
		t.Fatal(err)
	}
	return pod
}

// makeHeldPod is holdPod for a goroutine other than the test's: it returns its error.
func (r keptRig) makeHeldPod(t *testing.T, mutate func(*corev1.Pod)) (*corev1.Pod, error) {
	obj, err := r.dyn.Tracker().Get(sandboxesGVR, testNamespace, r.ref)
	if err != nil {
		return nil, fmt.Errorf("get the Sandbox: %w", err)
	}
	_, pod := controllerMaking(t, func(p *corev1.Pod) {
		p.UID = heldPodUID
		if mutate != nil {
			mutate(p)
		}
	})(obj.(*unstructured.Unstructured).DeepCopy())
	if err := r.cs.Tracker().Create(podsGVR, pod, testNamespace); err != nil {
		return nil, fmt.Errorf("create the held pod: %w", err)
	}
	return pod, nil
}

// editSandbox rewrites the stored Sandbox, as something outside Wardyn would.
func (r keptRig) editSandbox(t *testing.T, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	obj, err := r.dyn.Tracker().Get(sandboxesGVR, testNamespace, r.ref)
	if err != nil {
		t.Fatalf("get the Sandbox: %v", err)
	}
	sb := obj.(*unstructured.Unstructured).DeepCopy()
	mutate(sb)
	if err := r.dyn.Tracker().Update(sandboxesGVR, sb, testNamespace); err != nil {
		t.Fatalf("update the Sandbox: %v", err)
	}
}

// editSlice rewrites the stored EndpointSlice of the run.
func (r keptRig) editSlice(t *testing.T, mutate func(*discoveryv1.EndpointSlice)) {
	t.Helper()
	es := keepSlice(t, r.cs, asRunID)
	mutate(es)
	if _, err := r.cs.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), es, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update the EndpointSlice: %v", err)
	}
}

func (r keptRig) dropRecord(t *testing.T) {
	t.Helper()
	r.editSlice(t, func(es *discoveryv1.EndpointSlice) { delete(es.Annotations, annotationKeptSandbox) })
}

func (r keptRig) setGeneration(t *testing.T, g int64) {
	t.Helper()
	r.editSandbox(t, func(sb *unstructured.Unstructured) { sb.SetGeneration(g) })
}

// forgeSandbox makes the Sandbox one Wardyn did not create: the same name and labels at generation 1, under a new UID.
func (r keptRig) forgeSandbox(t *testing.T, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	r.editSandbox(t, func(sb *unstructured.Unstructured) {
		sb.SetUID("forged-uid")
		sb.SetGeneration(1)
		if mutate != nil {
			mutate(sb)
		}
	})
}

func (r keptRig) status(t *testing.T) runner.Status {
	t.Helper()
	st, err := r.k.Status(context.Background(), r.ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st
}

func agentPodDeletes(cs *fake.Clientset) []clienttesting.DeleteAction {
	var out []clienttesting.DeleteAction
	for _, a := range cs.Actions() {
		if da, ok := a.(clienttesting.DeleteAction); ok && a.GetResource().Resource == "pods" && da.GetName() == agentPodName(asRunID) {
			out = append(out, da)
		}
	}
	return out
}

func sliceServerTimeout(cs *fake.Clientset) {
	cs.PrependReactor("get", "endpointslices", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServerTimeout(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "get", 1)
	})
}

// assertNoWrites fails when any write was recorded.
func assertNoWrites(t *testing.T, order *[]string) {
	t.Helper()
	if len(*order) != 0 {
		t.Errorf("writes = %v, want none", *order)
	}
}

func TestKeepDriverEndsOnlyKeptRuns(t *testing.T) {
	for name, mutate := range map[string]func(*runner.SandboxSpec){
		"not interactive":           func(s *runner.SandboxSpec) { s.Interactive = false },
		"interactive with no drive": func(s *runner.SandboxSpec) { s.Drives = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := newKeptRig(t, mutate)
			order := writesInOrder(r.cs, r.dyn)
			if err := r.k.EndSandbox(context.Background(), r.ref); !errors.Is(err, runner.ErrEndUnsupported) {
				t.Fatalf("EndSandbox = %v, want ErrEndUnsupported", err)
			}
			assertNoWrites(t, order)
		})
	}
}

func TestKeepEndSandboxOrder(t *testing.T) {
	r := newKeptRig(t, nil)
	order := writesInOrder(r.cs, r.dyn)
	r.end(t)
	want := []string{"update endpointslices", "delete pods", "delete pods"}
	if !slices.Equal(*order, want) {
		t.Errorf("writes = %v\nwant %v: the slice emptied, the proxy pod, the agent pod; no Secret, as the created proxy mounts the run's own; nothing on the Sandbox", *order, want)
	}
	deletes := agentPodDeletes(r.cs)
	if len(deletes) != 1 {
		t.Fatalf("agent pod deletes = %d, want 1", len(deletes))
	}
	if p := deletes[0].GetDeleteOptions().Preconditions; p == nil || p.UID == nil || *p.UID != asPodUID {
		t.Errorf("agent pod delete preconditions = %+v, want UID %s", p, asPodUID)
	}
	if es := keepSlice(t, r.cs, asRunID); len(es.Endpoints) != 0 || es.Annotations[annotationKeptSandbox] != string(asSandboxUID) {
		t.Errorf("slice after the end = %+v, want no endpoint and the recorded Sandbox UID kept", es)
	}
	if !secretStored(t, r.cs, secretName(asRunID)) {
		t.Error("the run's Secret is gone: the created proxy mounts it, and an end deletes only a replacement's")
	}
}

func TestKeepEndSandboxRepeatsWhileThePodIsRemade(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.end(t)
	r.holdPod(t, nil)
	r.end(t)
	if n := len(agentPodDeletes(r.cs)); n != 1 {
		t.Errorf("agent pod deletes over three ends = %d, want 1: a repeated end deletes nothing, held or not yet made", n)
	}
}

func TestKeepEndSandboxChangedSandboxIsUnsupported(t *testing.T) {
	r := newKeptRig(t, nil)
	r.setGeneration(t, 2)
	order := writesInOrder(r.cs, r.dyn)
	if err := r.k.EndSandbox(context.Background(), r.ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported", err)
	}
	assertNoWrites(t, order)
}

func TestKeepEndSandboxNoKeepLabelIsUnsupported(t *testing.T) {
	r := newKeptRig(t, nil)
	r.editSandbox(t, func(sb *unstructured.Unstructured) {
		unstructured.RemoveNestedField(sb.Object, "spec", "podTemplate", "metadata", "labels", labelKeep)
	})
	order := writesInOrder(r.cs, r.dyn)
	if err := r.k.EndSandbox(context.Background(), r.ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported", err)
	}
	assertNoWrites(t, order)
}

func TestKeepEndSandboxKeepLabelMustBeTrue(t *testing.T) {
	r := newKeptRig(t, nil)
	r.editSandbox(t, func(sb *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(sb.Object, "false", "spec", "podTemplate", "metadata", "labels", labelKeep); err != nil {
			t.Fatal(err)
		}
	})
	order := writesInOrder(r.cs, r.dyn)
	if err := r.k.EndSandbox(context.Background(), r.ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported", err)
	}
	assertNoWrites(t, order)
}

func TestKeepEndSandboxDeletingSandboxIsUnsupported(t *testing.T) {
	r := newKeptRig(t, nil)
	r.editSandbox(t, func(sb *unstructured.Unstructured) {
		now := metav1.Now()
		sb.SetDeletionTimestamp(&now)
	})
	order := writesInOrder(r.cs, r.dyn)
	if err := r.k.EndSandbox(context.Background(), r.ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported", err)
	}
	assertNoWrites(t, order)
}

func TestKeepStartSandboxRevivesAHeldPod(t *testing.T) {
	r := newKeptRig(t, nil)
	ctx := context.Background()
	r.end(t)
	r.holdPod(t, nil)
	reviver, ok := served(r.k.Driver).(runner.ProxyReviver)
	if !ok {
		t.Fatal("the keep-on substrate is not a ProxyReviver")
	}
	if err := reviver.ReplaceProxy(ctx, r.ref, replaceCfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	r.cs.ClearActions()
	if err := r.k.StartSandbox(ctx, r.ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	if n := len(podPatches(r.cs)); n != 1 {
		t.Errorf("pod patches = %d, want the one that removes the gate", n)
	}
	pod, err := r.cs.CoreV1().Pods(testNamespace).Get(ctx, r.ref, metav1.GetOptions{})
	if err != nil || agentPodGated(pod) || pod.UID != heldPodUID {
		t.Errorf("agent pod after the start = %+v (err %v), want the held pod ungated", pod, err)
	}
	if got := keepSlice(t, r.cs, asRunID).Annotations[annotationKeptSandbox]; got != string(asSandboxUID) {
		t.Errorf("recorded Sandbox UID after the replace and start = %q, want %q", got, asSandboxUID)
	}
	if err := r.k.EndSandbox(ctx, r.ref); err != nil {
		t.Errorf("a second EndSandbox = %v, want nil", err)
	}
}

func TestKeepStartSandboxRefusesAChangedSandbox(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.holdPod(t, nil)
	r.setGeneration(t, 2)
	r.cs.ClearActions()
	err := r.k.StartSandbox(context.Background(), r.ref)
	if err == nil || !strings.Contains(err.Error(), "was changed after the run started; revive refused") {
		t.Fatalf("StartSandbox = %v, want the refusal", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none: the gate stays", n)
	}
}

func TestKeepStartSandboxWaitsForAMissingPod(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.cs.ClearActions()
	made := make(chan error, 1)
	timer := time.AfterFunc(300*time.Millisecond, func() {
		_, err := r.makeHeldPod(t, nil)
		made <- err
	})
	defer timer.Stop()
	if err := r.k.StartSandbox(context.Background(), r.ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	if err := <-made; err != nil {
		t.Fatalf("the controller's pod: %v", err)
	}
	if n := len(podPatches(r.cs)); n != 1 {
		t.Errorf("pod patches = %d, want the one that removes the gate", n)
	}
}

func TestKeepStartSandboxAlreadyRunningIsANoOp(t *testing.T) {
	r := newKeptRig(t, nil)
	if err := r.k.StartSandbox(context.Background(), r.ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none for a running agent", n)
	}
}

func TestKeepStartSandboxRefusesAPodStillBeingDeleted(t *testing.T) {
	r := newKeptRig(t, nil)
	obj, err := r.cs.Tracker().Get(podsGVR, testNamespace, r.ref)
	if err != nil {
		t.Fatal(err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	if err := r.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
	err = r.k.StartSandbox(context.Background(), r.ref)
	if err == nil || !strings.Contains(err.Error(), "still being deleted") {
		t.Fatalf("StartSandbox = %v, want the refusal: a pod the end is deleting still shows a running container", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none", n)
	}
}

func TestKeptPodReadsStopped(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.holdPod(t, nil)
	if st := r.status(t); st.State != types.RunStopped || st.Message != "kept: revive to start it again" {
		t.Errorf("Status = %+v, want stopped with the revive message", st)
	}
}

func TestKeptPodWithItsProxyReadsReplaced(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.holdPod(t, nil)
	proxyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(asRunID), Namespace: testNamespace}}
	if err := r.cs.Tracker().Create(podsGVR, proxyPod, testNamespace); err != nil {
		t.Fatal(err)
	}
	if st := r.status(t); st.State != types.RunFailed || st.Message != agentPodReplacedMessage {
		t.Errorf("Status = %+v, want failed as replaced: a live run's pod deleted outside Wardyn", st)
	}
}

func TestGatedPodWithoutKeepLabelStillReadsReplaced(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.holdPod(t, func(p *corev1.Pod) { delete(p.Labels, labelKeep) })
	if st := r.status(t); st.State != types.RunFailed || st.Message != agentPodReplacedMessage {
		t.Errorf("Status = %+v, want failed as replaced", st)
	}
}

// mainCommand is the created Sandbox's main container command.
func mainCommand(t *testing.T, r keptRig) []string {
	t.Helper()
	_, tmpl := sandboxTemplate(t, r.dyn, r.ref)
	for _, c := range tmpl.Spec.Containers {
		if c.Name == mainContainerName {
			return c.Command
		}
	}
	t.Fatal("the Sandbox's template has no main container")
	return nil
}

func TestKeepRunGetsTheScript(t *testing.T) {
	script := keepIdleScript(asRunID.String())
	if cmd := mainCommand(t, newKeptRig(t, nil)); !strings.HasSuffix(strings.Join(cmd, "\x00"), strings.Join(script, "\x00")) {
		t.Errorf("main command = %q, want it to end with the keep script", cmd)
	}
	other := newKeptRig(t, func(s *runner.SandboxSpec) { s.Labels[labelAgent] = "aws-sso" })
	cmd := mainCommand(t, other)
	if len(cmd) < 2 || !slices.Equal(cmd[len(cmd)-2:], []string{"agent-run", "--idle"}) || slices.Contains(cmd, script[2]) {
		t.Errorf("main command of an aws-sso run = %q, want agent-run --idle and not the keep script", cmd)
	}
}

func TestKeptPodStatusTerms(t *testing.T) {
	proxyTimeout := func(r keptRig) {
		r.cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.(clienttesting.GetAction).GetName() != proxyPodName(asRunID) {
				return false, nil, nil
			}
			return true, nil, apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "get", 1)
		})
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, r keptRig, pod *corev1.Pod)
		wantErr bool
	}{
		{"keep off", func(_ *testing.T, r keptRig, _ *corev1.Pod) { r.k.cfg.AgentSandboxKeep = false }, false},
		{"a wrong run label", func(_ *testing.T, _ keptRig, p *corev1.Pod) { p.Labels[labelRun] = uuid.NewString() }, false},
		{"another agent", func(_ *testing.T, _ keptRig, p *corev1.Pod) { p.Labels[labelAgent] = "aws-sso" }, false},
		{"a Sandbox UID that was not recorded", func(_ *testing.T, _ keptRig, p *corev1.Pod) { p.OwnerReferences[0].UID = "forged-uid" }, false},
		{"no record on the slice", func(t *testing.T, r keptRig, _ *corev1.Pod) { r.dropRecord(t) }, false},
		{"a proxy read error", func(_ *testing.T, r keptRig, _ *corev1.Pod) { proxyTimeout(r) }, true},
		{"a slice read error", func(_ *testing.T, r keptRig, _ *corev1.Pod) { sliceServerTimeout(r.cs) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newKeptRig(t, nil)
			r.end(t)
			pod := r.holdPod(t, nil)
			if _, kept, err := r.k.keptPodStatus(context.Background(), testNamespace, pod); !kept || err != nil {
				t.Fatalf("before the change keptPodStatus = %v, %v, want a kept pod", kept, err)
			}
			tc.setup(t, r, pod)
			_, kept, err := r.k.keptPodStatus(context.Background(), testNamespace, pod)
			if kept {
				t.Errorf("keptPodStatus read the pod as kept")
			}
			if (err != nil) != tc.wantErr || errors.Is(err, errKeptSandboxNotOurs) {
				t.Errorf("err = %v, want an error: %v, and never errKeptSandboxNotOurs", err, tc.wantErr)
			}
		})
	}
}

func TestKeepEndSandboxSliceReadErrorIsReturned(t *testing.T) {
	r := newKeptRig(t, nil)
	sliceServerTimeout(r.cs)
	order := writesInOrder(r.cs, r.dyn)
	err := r.k.EndSandbox(context.Background(), r.ref)
	if err == nil || errors.Is(err, runner.ErrEndUnsupported) || errors.Is(err, errKeptSandboxNotOurs) {
		t.Fatalf("EndSandbox = %v, want the read error: a transient error never tears a kept run down", err)
	}
	assertNoWrites(t, order)
}

func TestKeepEndSandboxOtherAgentIsUnsupported(t *testing.T) {
	r := newKeptRig(t, func(s *runner.SandboxSpec) { s.Labels[labelAgent] = "aws-sso" })
	order := writesInOrder(r.cs, r.dyn)
	err := r.k.EndSandbox(context.Background(), r.ref)
	if !errors.Is(err, runner.ErrEndUnsupported) || !strings.HasSuffix(err.Error(), `k8s: this run is not kept: only claude-code and codex-cli runs are revived on Kubernetes, and its agent is "aws-sso"`) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported with the reason", err)
	}
	assertNoWrites(t, order)
}

func TestKeepStartSandboxRefusesAnotherAgent(t *testing.T) {
	r := newKeptRig(t, func(s *runner.SandboxSpec) { s.Labels[labelAgent] = "aws-sso" })
	for _, name := range []string{agentPodName(asRunID), proxyPodName(asRunID)} {
		if err := r.cs.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	r.holdPod(t, nil)
	r.cs.ClearActions()
	err := r.k.StartSandbox(context.Background(), r.ref)
	if err == nil || !strings.HasSuffix(err.Error(), `its agent is "aws-sso"; revive refused`) {
		t.Fatalf("StartSandbox = %v, want the agent refusal", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none", n)
	}
}

func TestCreateSandboxRecordsTheKeptSandbox(t *testing.T) {
	r := newKeptRig(t, nil)
	if got := keepSlice(t, r.cs, asRunID).Annotations[annotationKeptSandbox]; got != string(asSandboxUID) {
		t.Errorf("recorded Sandbox UID = %q, want %q", got, asSandboxUID)
	}
	d, cs, _ := keepRig(t, Config{AgentSandboxKeep: true})
	spec := claudeKeptSpec()
	spec.Drives = nil
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for _, a := range cs.Actions() {
		if ua, ok := a.(clienttesting.UpdateAction); ok && a.GetResource().Resource == "endpointslices" {
			if _, kept := ua.GetObject().(*discoveryv1.EndpointSlice).Annotations[annotationKeptSandbox]; kept {
				t.Errorf("an EndpointSlice update records a kept Sandbox for a run that is not kept")
			}
		}
	}
}

func TestKeepEndSandboxRecreatedSandboxIsUnsupported(t *testing.T) {
	r := newKeptRig(t, nil)
	r.forgeSandbox(t, nil)
	order := writesInOrder(r.cs, r.dyn)
	err := r.k.EndSandbox(context.Background(), r.ref)
	if !errors.Is(err, runner.ErrEndUnsupported) || !errors.Is(err, errKeptSandboxNotOurs) {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported wrapping errKeptSandboxNotOurs", err)
	}
	assertNoWrites(t, order)
}

func TestKeepEndSandboxWithoutTheRecordIsUnsupported(t *testing.T) {
	r := newKeptRig(t, nil)
	r.dropRecord(t)
	order := writesInOrder(r.cs, r.dyn)
	err := r.k.EndSandbox(context.Background(), r.ref)
	if !errors.Is(err, runner.ErrEndUnsupported) || !strings.Contains(err.Error(), "does not record the agent's Sandbox as the one Wardyn created for this run") {
		t.Fatalf("EndSandbox = %v, want ErrEndUnsupported saying the record is missing", err)
	}
	assertNoWrites(t, order)
}

// forgeTemplate is what a Sandbox made again by someone else could carry: another image, command and Secret key.
func forgeTemplate(t *testing.T) func(*unstructured.Unstructured) {
	return func(sb *unstructured.Unstructured) {
		path := []string{"spec", "podTemplate", "spec", "containers"}
		containers, _, err := unstructured.NestedSlice(sb.Object, path...)
		if err != nil || len(containers) == 0 {
			t.Fatalf("read the template's containers: %v", err)
		}
		c := containers[0].(map[string]any)
		c["image"], c["command"] = "other/image:1", []any{"sh", "-c", "other"}
		env, _ := c["env"].([]any)
		c["env"] = append(env, map[string]any{"name": "OTHER", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "other-secret", "key": "k"}}})
		containers[0] = c
		if err := unstructured.SetNestedSlice(sb.Object, containers, path...); err != nil {
			t.Fatalf("write the template's containers: %v", err)
		}
	}
}

func TestKeepStartSandboxRefusesARecreatedSandbox(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.forgeSandbox(t, forgeTemplate(t))
	r.holdPod(t, nil)
	r.cs.ClearActions()
	if err := r.k.StartSandbox(context.Background(), r.ref); !errors.Is(err, errKeptSandboxNotOurs) {
		t.Fatalf("StartSandbox = %v, want errKeptSandboxNotOurs", err)
	}
	if n := len(podPatches(r.cs)); n != 0 {
		t.Errorf("pod patches = %d, want none: the forged template is never ungated", n)
	}
}

func TestKeptPodOfARecreatedSandboxReadsReplaced(t *testing.T) {
	r := newKeptRig(t, nil)
	r.end(t)
	r.forgeSandbox(t, forgeTemplate(t))
	r.holdPod(t, nil)
	if st := r.status(t); st.State != types.RunFailed || st.Message != agentPodReplacedMessage {
		t.Errorf("Status = %+v, want failed as replaced, not stopped as kept", st)
	}
}

func TestCheckKeptSandbox(t *testing.T) {
	runID := uuid.New()
	slice := func(annotations map[string]string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: keepServiceName(runID), Namespace: testNamespace, Annotations: annotations},
			AddressType: discoveryv1.AddressTypeIPv4,
		}
	}
	recorded := map[string]string{annotationKeptSandbox: "sandbox-uid"}
	for _, tc := range []struct {
		name    string
		slice   *discoveryv1.EndpointSlice // nil: the run has no slice
		uid     k8stypes.UID
		notOurs bool
	}{
		{"the recorded UID", slice(recorded), "sandbox-uid", false},
		{"another UID", slice(recorded), "forged-uid", true},
		{"no record on the slice", slice(nil), "sandbox-uid", true},
		{"no slice", nil, "sandbox-uid", true},
		{"an empty UID and no record", slice(nil), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			if tc.slice != nil {
				if err := cs.Tracker().Add(tc.slice); err != nil {
					t.Fatal(err)
				}
			}
			err := d.checkKeptSandbox(context.Background(), testNamespace, runID, tc.uid)
			if tc.notOurs && !errors.Is(err, errKeptSandboxNotOurs) {
				t.Fatalf("err = %v, want errKeptSandboxNotOurs", err)
			}
			if !tc.notOurs && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
	t.Run("a slice read error is returned, never read as not ours", func(t *testing.T) {
		d, cs := newTestDriver(t, Config{})
		if err := cs.Tracker().Add(slice(recorded)); err != nil {
			t.Fatal(err)
		}
		cs.PrependReactor("get", "endpointslices", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServerTimeout(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "get", 1)
		})
		err := d.checkKeptSandbox(context.Background(), testNamespace, runID, "sandbox-uid")
		if err == nil || errors.Is(err, errKeptSandboxNotOurs) {
			t.Fatalf("err = %v, want the read error, not errKeptSandboxNotOurs", err)
		}
	})
}
