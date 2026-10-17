// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const testRunnerID = "runner-k2"

var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// memEvidence is an in-memory EvidenceStore; failSave, when set, refuses a save it matches.
type memEvidence struct {
	mu       sync.Mutex
	m        map[uuid.UUID][]byte
	failSave func([]byte) bool
}

func (s *memEvidence) LoadEvidence(id uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id], nil
}

func (s *memEvidence) SaveEvidence(id uuid.UUID, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave != nil && s.failSave(b) {
		return errors.New("disk full")
	}
	s.m[id] = append([]byte(nil), b...)
	return nil
}

func (s *memEvidence) get(t *testing.T, id uuid.UUID) *runEvidence {
	t.Helper()
	b, _ := s.LoadEvidence(id)
	ev, err := decodeEvidence(b)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

type runnerHarness struct {
	d      *Driver
	cs     *fake.Clientset
	ev     *memEvidence
	runID  uuid.UUID
	fences []string
	// onDelete runs when a held pod is marked for deletion, standing in for its kubelet.
	onDelete func(*corev1.Pod)
	clock    time.Time
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	h := &runnerHarness{ev: &memEvidence{m: map[uuid.UUID][]byte{}}, runID: uuid.New(), clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	cfg := Config{RunnerID: testRunnerID, Evidence: h.ev, HardDeny: &HardDeny{Suffixes: []string{"svc", "cluster.local"}},
		Fence: func(_ uuid.UUID, ip string) { h.fences = append(h.fences, ip) }}
	h.d, h.cs = newTestDriver(t, cfg)
	// Each read of the clock moves it a minute on, so a bounded evidence wait ends after one pass.
	h.d.now = func() time.Time { h.clock = h.clock.Add(time.Minute); return h.clock }
	h.installFinalizerSemantics(t)
	return h
}

// installFinalizerSemantics makes the fake honour finalizers as the API does: a delete of a pod
// holding one only marks it, and the JSON patch's test ops answer 422 when they do not hold. A
// pod marked for deletion goes once its last finalizer is removed.
func (h *runnerHarness) installFinalizerSemantics(t *testing.T) {
	t.Helper()
	tr := h.cs.Tracker()
	h.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteActionImpl)
		obj, err := tr.Get(podsGVR, da.GetNamespace(), da.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if pre := da.DeleteOptions.Preconditions; pre != nil && pre.UID != nil && *pre.UID != pod.UID {
			return true, nil, apierrors.NewConflict(podsGVR.GroupResource(), pod.Name, errors.New("UID precondition failed"))
		}
		if len(pod.Finalizers) == 0 {
			return true, nil, tr.Delete(podsGVR, pod.Namespace, pod.Name)
		}
		if pod.DeletionTimestamp == nil {
			now := metav1.NewTime(h.clock)
			pod.DeletionTimestamp = &now
		}
		if h.onDelete != nil {
			h.onDelete(pod)
		}
		return true, nil, tr.Update(podsGVR, pod, pod.Namespace)
	})
	h.cs.PrependReactor("patch", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		if pa.PatchType != k8stypes.JSONPatchType {
			return false, nil, nil
		}
		obj, err := tr.Get(podsGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		var ops []jsonPatchOp
		if err := json.Unmarshal(pa.Patch, &ops); err != nil {
			return true, nil, err
		}
		invalid := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pod.Name, nil)
		for _, op := range ops {
			switch {
			case op.Op == "test" && op.Path == "/metadata/uid":
				if op.Value != string(pod.UID) {
					return true, nil, invalid
				}
			case op.Op == "test" && op.Path == "/metadata/resourceVersion":
				if op.Value != pod.ResourceVersion {
					return true, nil, invalid
				}
			case strings.HasPrefix(op.Path, "/metadata/finalizers/"):
				var i int
				fmt.Sscanf(strings.TrimPrefix(op.Path, "/metadata/finalizers/"), "%d", &i)
				if i >= len(pod.Finalizers) || (op.Op == "test" && pod.Finalizers[i] != op.Value) {
					return true, nil, invalid
				}
				if op.Op == "remove" {
					pod.Finalizers = append(pod.Finalizers[:i:i], pod.Finalizers[i+1:]...)
				}
			default:
				return true, nil, fmt.Errorf("unexpected patch op %+v", op)
			}
		}
		if pod.DeletionTimestamp != nil && len(pod.Finalizers) == 0 {
			return true, pod, tr.Delete(podsGVR, pod.Namespace, pod.Name)
		}
		return true, pod, tr.Update(podsGVR, pod, pod.Namespace)
	})
}

func (h *runnerHarness) node(t *testing.T, name, uid, machine, system, ip string, taints ...corev1.Taint) {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID(uid), Labels: map[string]string{"pool": "runs"}},
		Spec: corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{MachineID: machine, SystemUUID: system, KubeletVersion: "v1.31.2"},
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
	tr := h.cs.Tracker()
	if _, err := tr.Get(nodesGVR, "", name); err == nil {
		if err := tr.Delete(nodesGVR, "", name); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Add(n); err != nil {
		t.Fatal(err)
	}
}

func (h *runnerHarness) dropNode(t *testing.T, name string) {
	t.Helper()
	if err := h.cs.Tracker().Delete(nodesGVR, "", name); err != nil {
		t.Fatal(err)
	}
}

// proxyPod returns a running, bound proxy pod of the harness run under the stop-proof finalizer.
func (h *runnerHarness) proxyPod(node string) *corev1.Pod {
	name := proxyPodName(h.runID)
	l := wardynLabels(h.runID, componentProxy, nil)
	l[runner.LabelRunnerID] = testRunnerID
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "uid-proxy-1", ResourceVersion: "10", Labels: l, Finalizers: []string{stopProofFinalizer}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: proxyContainerName}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.7", HostIP: "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{{Name: proxyContainerName, ContainerID: "containerd://p1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
}

func terminate(pod *corev1.Pod, id string) {
	pod.Status.Phase = corev1.PodSucceeded
	pod.ResourceVersion = "11"
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", ContainerID: id}}
	}
}

// boundProxy runs the create-side bracket for a proxy on node n1 and records its identity as the
// start wait would: before-list, create, a bound read with its after-list and anchor list.
func (h *runnerHarness) boundProxy(t *testing.T) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5")
	pod := h.proxyPod("n1")
	b := &bracket{d: h.d, runID: h.runID, component: componentProxy}
	if err := b.before(ctx, pod.Name); err != nil {
		t.Fatal(err)
	}
	created, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.created(created); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.observePod(ctx, h.runID, componentProxy, created); err != nil {
		t.Fatal(err)
	}
	pe := h.ev.get(t, h.runID).Pods[componentProxy]
	if pe.Assoc != assocCausal || !pe.Containers[proxyContainerName].Anchored {
		t.Fatalf("setup: assoc %q, containers %+v", pe.Assoc, pe.Containers)
	}
	return created
}

func countActions(cs *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// TestRunnerStopProxyTier1Completes is the normal stop: fence, delete, closing anchor, terminal
// status bound by tier 1, witness, finalizer removed.
func TestRunnerStopProxyTier1Completes(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	ev := h.ev.get(t, h.runID)
	pe := ev.Pods[componentProxy]
	if pe.Witness == nil || pe.Witness.Arm != armTerminal || pe.Witness.Containers[0].Tier != tier1 || !pe.FinalizerOff {
		t.Fatalf("witness %+v finalizerOff %v", pe.Witness, pe.FinalizerOff)
	}
	if pe.Witness.PostStatus == nil || pe.Witness.PostStatus.Node.UID != "uid-n1" {
		t.Fatalf("tier witness lacks its post-status observation: %+v", pe.Witness.PostStatus)
	}
	if _, err := h.cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(h.runID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the proxy pod was not released: %v", err)
	}
	for _, a := range h.cs.Actions() {
		if da, ok := a.(clienttesting.DeleteActionImpl); ok && da.GetResource().Resource == "pods" && da.GetName() == proxyPodName(h.runID) {
			if g := da.DeleteOptions.GracePeriodSeconds; g == nil || *g != 1 {
				t.Fatalf("proxy delete grace %v, want 1", g)
			}
			if p := da.DeleteOptions.PropagationPolicy; p == nil || *p != metav1.DeletePropagationOrphan {
				t.Fatalf("proxy delete propagation %v, want Orphan", p)
			}
		}
	}
}

// TestRunnerFenceBeforeFirstDelete: the fence is persisted, and applied in the relay, before
// runnerd's first delete of the pod (R4-M1).
func TestRunnerFenceBeforeFirstDelete(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	var fencedAtDelete, appliedAtDelete bool
	h.onDelete = func(p *corev1.Pod) {
		ev := h.ev.get(t, h.runID)
		fencedAtDelete = ev.Fence != nil && ev.Fence.ProxyIP == "10.244.1.7" && ev.Fence.ProxyUID == "uid-proxy-1"
		appliedAtDelete = len(h.fences) == 1 && h.fences[0] == "10.244.1.7"
		terminate(p, "containerd://p1")
	}
	if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err != nil {
		t.Fatal(err)
	}
	if !fencedAtDelete || !appliedAtDelete {
		t.Fatalf("delete preceded the fence: persisted %v applied %v", fencedAtDelete, appliedAtDelete)
	}
}

// TestRunnerWitnessBeforeFinalizer: the witness is persisted before the finalizer patch is sent
// (R7-M1), and the patch is the guarded JSON patch.
func TestRunnerWitnessBeforeFinalizer(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	var witnessed bool
	var patch []byte
	h.cs.PrependReactor("patch", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		patch = a.(clienttesting.PatchActionImpl).Patch
		witnessed = h.ev.get(t, h.runID).Pods[componentProxy].Witness != nil
		return false, nil, nil
	})
	if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err != nil {
		t.Fatal(err)
	}
	if !witnessed {
		t.Fatal("the finalizer patch was sent before the witness was persisted")
	}
	want := `[{"op":"test","path":"/metadata/uid","value":"uid-proxy-1"},{"op":"test","path":"/metadata/resourceVersion","value":"11"},{"op":"test","path":"/metadata/finalizers/0","value":"wardyn.dev/stop-proof"},{"op":"remove","path":"/metadata/finalizers/0"}]`
	if string(patch) != want {
		t.Fatalf("patch %s", patch)
	}
}

// TestRunnerFailedSaveStopsSequence: a failed witness save stops the sequence; no patch follows.
func TestRunnerFailedSaveStopsSequence(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	h.ev.failSave = func(b []byte) bool { return strings.Contains(string(b), `"witness"`) }
	if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err == nil {
		t.Fatal("StopProxy completed with no persisted witness")
	}
	if n := countActions(h.cs, "patch", "pods"); n != 0 {
		t.Fatalf("%d finalizer patches followed a failed witness save", n)
	}
	if h.ev.get(t, h.runID).Pods[componentProxy].FinalizerOff {
		t.Fatal("finalizer recorded off")
	}
}

// TestRunnerStaleWitnessPatchRetries: a 409 or 422 on the patch is not a refusal: the next pass
// re-reads, re-checks the arm, refreshes the witness and retries.
func TestRunnerStaleWitnessPatchRetries(t *testing.T) {
	for _, code := range []string{"conflict", "invalid"} {
		t.Run(code, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.boundProxy(t)
			h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
			calls := 0
			h.cs.PrependReactor("patch", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls > 1 {
					return false, nil, nil
				}
				if code == "conflict" {
					return true, nil, apierrors.NewConflict(podsGVR.GroupResource(), "p", errors.New("changed"))
				}
				return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "p", nil)
			})
			if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err != nil {
				t.Fatalf("StopProxy: %v", err)
			}
			if calls != 2 || !h.ev.get(t, h.runID).Pods[componentProxy].FinalizerOff {
				t.Fatalf("patch calls %d", calls)
			}
		})
	}
}

// TestRunnerNotFoundIsNeverEvidence: a missing pod completes only from a persisted witness.
func TestRunnerNotFoundIsNeverEvidence(t *testing.T) {
	h := newRunnerHarness(t)
	pod := h.boundProxy(t)
	ctx := context.Background()
	if err := h.cs.Tracker().Delete(podsGVR, testNamespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	done, err := h.d.checkOnce(ctx, h.runID, componentProxy, pod.Name)
	if done || !errors.Is(err, errTerminationUnknown) {
		t.Fatalf("NotFound with no witness: done %v err %v", done, err)
	}
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.Pods[componentProxy].Witness = &witness{Arm: armTerminal, PodUID: "uid-proxy-1"}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if done, err := h.d.checkOnce(ctx, h.runID, componentProxy, pod.Name); !done || err != nil {
		t.Fatalf("NotFound with a witness: done %v err %v", done, err)
	}
}

// TestRunnerNameReuseLatchHolds: a reuse a tick saw stays latched although the name later shows
// its first holder again, and it refuses every automatic arm (owner #8).
func TestRunnerNameReuseLatchHolds(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	ctx := context.Background()
	h.node(t, "n1", "uid-n1", "m1-reimaged", "s1", "10.0.0.5") // the same Node object, reimaged
	if _, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID}); err != nil {
		t.Fatal(err)
	}
	if r := h.ev.get(t, h.runID).Reuse; len(r) != 1 || r[0].MachineID != "m1-reimaged" {
		t.Fatalf("reuse not latched on the tick: %+v", r)
	}
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5") // the first holder again: the latch still holds
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	err := h.d.StopProxy(ctx, agentPodName(h.runID))
	var pending *runner.ContainmentPendingError
	if !errors.As(err, &pending) && (err == nil || !strings.Contains(err.Error(), "node name reused")) {
		t.Fatalf("StopProxy after a reuse: %v", err)
	}
	ev := h.ev.get(t, h.runID)
	if ev.Pods[componentProxy].Witness != nil || len(ev.Reuse) != 1 {
		t.Fatalf("a reused name released the run: witness %+v reuse %+v", ev.Pods[componentProxy].Witness, ev.Reuse)
	}
	if n := countActions(h.cs, "patch", "pods"); n != 0 {
		t.Fatalf("%d finalizer patches after a reuse", n)
	}
}

func TestRunnerLatchReuseGenerations(t *testing.T) {
	pb := runner.NodeSample{Name: "n1", UID: "u1", MachineID: "m1", SystemUUID: "s1", InternalIP: "10.0.0.5"}
	ev := &runEvidence{}
	pe := &podEvidence{PostBind: &nodeHolder{Node: pb}}
	at := func(s runner.NodeSample) *nodeHolder { return &nodeHolder{Node: s} }
	ipOnly := pb
	ipOnly.InternalIP = "10.0.0.9"
	newMachine := pb
	newMachine.MachineID = "m2"
	newUID := pb
	newUID.UID = "u2"
	steps := []struct {
		h    *nodeHolder
		want bool
	}{
		{at(pb), false}, {at(ipOnly), false}, {&nodeHolder{None: true}, false},
		{at(newMachine), true}, {at(newMachine), false}, {at(newUID), true},
	}
	for i, s := range steps {
		if got := ev.latchReuse(pe, s.h); got != s.want {
			t.Fatalf("step %d: latched %v, want %v", i, got, s.want)
		}
	}
	if len(ev.Reuse) != 2 {
		t.Fatalf("generations %+v", ev.Reuse)
	}
}

// evidenceFixture is a causal pod with an anchored baseline and a closing anchor on its holder.
func evidenceFixture() (*runEvidence, *podEvidence, *corev1.Pod, runner.NodeSample) {
	n := runner.NodeSample{Name: "n1", UID: "uid-n1", MachineID: "m1", SystemUUID: "s1", InternalIP: "10.0.0.5"}
	pe := &podEvidence{Name: "p", UID: "uid-p", NodeName: "n1", Assoc: assocCausal, PostBind: &nodeHolder{Node: n},
		Before:     &nodeMap{Names: map[string]string{"n1": "uid-n1"}},
		Containers: map[string]*containerIDs{"c": {Baseline: "containerd://c1", Anchored: true}},
		Closing:    &nodeHolder{Node: n}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "uid-p", ResourceVersion: "7"},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, HostIP: "10.0.0.5", ContainerStatuses: []corev1.ContainerStatus{{Name: "c",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, ContainerID: "containerd://c1"}}}}}}
	return &runEvidence{Pods: map[string]*podEvidence{componentProxy: pe}}, pe, pod, n
}

// TestRunnerEvaluateTiers pins the terminal arm's tiers, its refusals, and tier 1's post-status
// list (Fable r12 M2).
func TestRunnerEvaluateTiers(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*runEvidence, *podEvidence, *corev1.Pod, *nodeHolder) *nodeHolder
		arm   string
		tier  string
		inWhy string
	}{
		{name: "tier1", arm: armTerminal, tier: tier1},
		{name: "tier1 with no Node after status", arm: armTerminal, tier: tier1,
			edit: func(_ *runEvidence, _ *podEvidence, _ *corev1.Pod, _ *nodeHolder) *nodeHolder {
				return &nodeHolder{None: true}
			}},
		{name: "tier1 refused when the post-status list shows another UID",
			edit: func(_ *runEvidence, _ *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				p := *post
				p.Node.UID = "uid-n1-replacement"
				return &p
			}},
		{name: "tier1 refused when the post-status list shows another machine",
			edit: func(_ *runEvidence, _ *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				p := *post
				p.Node.SystemUUID = "s2"
				return &p
			}},
		{name: "tier1 refused with no closing anchor",
			edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				pe.Closing = nil
				return post
			}},
		{name: "tier2 for an unanchored baseline", arm: armTerminal, tier: tier2,
			edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				pe.Containers["c"].Anchored = false
				return post
			}},
		{name: "tier2 refused when the host IP is not the holder's",
			edit: func(_ *runEvidence, pe *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pe.Containers["c"].Anchored = false
				pod.Status.HostIP = "10.0.0.99"
				return post
			}},
		{name: "a different execution", inWhy: "different execution",
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://c2"
				return post
			}},
		{name: "a latched ID", inWhy: "different execution",
			edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				pe.Containers["c"].Latched = []string{"containerd://c2"}
				return post
			}},
		{name: "the pod garbage collector's Failed",
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reasonDeletionByPodGC}}
				return post
			}},
		{name: "a running container",
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				return post
			}},
		{name: "another pod UID", inWhy: "recorded pod UID",
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.UID = "uid-other"
				return post
			}},
		{name: "a reuse refuses", inWhy: "node name reused",
			edit: func(ev *runEvidence, _ *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
				ev.Reuse = []runner.NodeSample{{Name: "n1", UID: "uid-x"}}
				return post
			}},
		{name: "the taint on the causal machine", arm: armTaint,
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.Status.Phase = corev1.PodRunning
				p := *post
				p.OutOfService = true
				return &p
			}},
		{name: "the taint on another machine",
			edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pod.Status.Phase = corev1.PodRunning
				p := *post
				p.OutOfService, p.Node.MachineID = true, "m9"
				return &p
			}},
		{name: "deleted before binding", arm: armUnbound,
			edit: func(_ *runEvidence, pe *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
				pe.NodeName, pod.Spec.NodeName = "", ""
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Status.Phase = corev1.PodPending
				return nil
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, pe, pod, n := evidenceFixture()
			post := &nodeHolder{Node: n}
			if c.edit != nil {
				post = c.edit(ev, pe, pod, post)
			}
			v := ev.evaluate(pe, pod, post)
			if v.arm != c.arm {
				t.Fatalf("arm %q (%s), want %q", v.arm, v.why, c.arm)
			}
			if c.tier != "" && v.tiers["c"] != c.tier {
				t.Fatalf("tier %q, want %q", v.tiers["c"], c.tier)
			}
			if c.inWhy != "" && !strings.Contains(v.why, c.inWhy) {
				t.Fatalf("why %q, want it to name %q", v.why, c.inWhy)
			}
		})
	}
}

// TestRunnerNeverInitializedArm: Failed and deleting with Initialized=False and every container
// waiting, bound to the causal node by tier 2's checks.
func TestRunnerNeverInitializedArm(t *testing.T) {
	setup := func() (*runEvidence, *podEvidence, *corev1.Pod, *nodeHolder) {
		ev, pe, pod, n := evidenceFixture()
		pe.Containers = nil
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionFalse}}
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
		return ev, pe, pod, &nodeHolder{Node: n}
	}
	ev, pe, pod, post := setup()
	if v := ev.evaluate(pe, pod, post); v.arm != armNeverInit {
		t.Fatalf("arm %q (%s)", v.arm, v.why)
	}
	ev, pe, pod, post = setup()
	pe.Containers = map[string]*containerIDs{"c": {Baseline: "containerd://c1"}}
	if v := ev.evaluate(pe, pod, post); v.ok() {
		t.Fatalf("a container that ever ran passed as never initialized: %q", v.arm)
	}
	ev, pe, pod, post = setup()
	pe.Assoc = assocU1
	if v := ev.evaluate(pe, pod, post); v.ok() {
		t.Fatalf("never initialized released with no causal association: %q", v.arm)
	}
}

// TestRunnerReadContract: every evidence read is a direct request with resourceVersion unset, and
// each status read's name-scoped list is sent after it.
func TestRunnerReadContract(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	h.cs.ClearActions()
	if err := h.d.StopProxy(context.Background(), agentPodName(h.runID)); err != nil {
		t.Fatal(err)
	}
	lastGet := -1
	for i, a := range h.cs.Actions() {
		switch act := a.(type) {
		case clienttesting.GetActionImpl:
			if act.GetResource().Resource == "pods" {
				if act.GetOptions.ResourceVersion != "" {
					t.Fatalf("pod read %d carries resourceVersion %q", i, act.GetOptions.ResourceVersion)
				}
				lastGet = i
			}
		case clienttesting.ListActionImpl:
			if act.GetResource().Resource == "nodes" {
				if act.ListOptions.ResourceVersion != "" {
					t.Fatalf("node list %d carries resourceVersion %q", i, act.ListOptions.ResourceVersion)
				}
				if lastGet < 0 {
					t.Fatalf("node list %d was sent before any pod read", i)
				}
			}
		}
	}
	if lastGet < 0 {
		t.Fatal("no pod read")
	}
}

// TestRunnerBracketBeforeCreate: the scoped before-list is persisted before the create request.
func TestRunnerBracketBeforeCreate(t *testing.T) {
	h := newRunnerHarness(t)
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5")
	var persisted bool
	h.cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pe := h.ev.get(t, h.runID).Pods[componentProxy]
		persisted = pe != nil && pe.Before != nil && pe.Before.Names["n1"] == "uid-n1"
		return false, nil, nil
	})
	pod := h.proxyPod("")
	pod.Spec.NodeSelector = map[string]string{"pool": "runs"}
	if _, err := h.d.createPod(context.Background(), h.runID, componentProxy, pod); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("the create request preceded the persisted before-list")
	}
	if pe := h.ev.get(t, h.runID).Pods[componentProxy]; pe.UID != "uid-proxy-1" {
		t.Fatalf("create response UID not recorded: %q", pe.UID)
	}
	if _, ok := h.d.brackets.Load(pod.Name); !ok {
		t.Fatal("the start wait does not read through the bracket")
	}
}

func TestRunnerBracketCadence(t *testing.T) {
	start := time.Unix(0, 0)
	b := &bracket{since: start, changedAt: start}
	if c := b.cadence(start.Add(time.Minute)); c != bracketFastCadence {
		t.Fatalf("cadence %s", c)
	}
	if c := b.cadence(start.Add(2 * time.Minute)); c != bracketSlowCadence {
		t.Fatalf("cadence after 2 min idle %s", c)
	}
	b.changedAt = start.Add(90 * time.Second)
	if c := b.cadence(start.Add(2 * time.Minute)); c != bracketFastCadence {
		t.Fatalf("cadence after a change %s", c)
	}
}

func nodeRecordFixture(h *runnerHarness, t *testing.T, att string, approvers ...string) runner.NodeRecord {
	t.Helper()
	rec := runner.NodeRecord{ID: uuid.New(), Attestation: att, Tuple: *h.ev.get(t, h.runID).tuple(h.runID)}
	for _, a := range approvers {
		rec.Approvers = append(rec.Approvers, runner.NodeRecordApprover{Principal: a, Email: a + "@example.com", Human: true})
	}
	return rec
}

// TestRunnerNodeRecordAcceptanceList: the acceptance list is sent at acceptance and latches what it
// shows, so a name taken by a new Node since approval voids the approvals (Fable r12 M3).
func TestRunnerNodeRecordAcceptanceList(t *testing.T) {
	ctx := context.Background()
	t.Run("accepted when the name is empty", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		h.dropNode(t, "n1")
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
		if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); err != nil {
			t.Fatal(err)
		}
		if ev := h.ev.get(t, h.runID); ev.Record == nil || ev.Record.ID != rec.ID {
			t.Fatal("record not kept")
		}
	})
	t.Run("refused when a new Node took the name", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		h.dropNode(t, "n1")
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice", "bob")
		h.node(t, "n1", "uid-n1-new", "m7", "s7", "10.0.0.5")
		err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec)
		if !errors.Is(err, runner.ErrNodeRecordRefused) {
			t.Fatalf("AcceptNodeRecord: %v", err)
		}
		if ev := h.ev.get(t, h.runID); ev.Record != nil || len(ev.Reuse) != 1 {
			t.Fatalf("record %+v reuse %+v", ev.Record, ev.Reuse)
		}
	})
	t.Run("refused while the holder is still there", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
		if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); !errors.Is(err, runner.ErrNodeRecordRefused) {
			t.Fatalf("AcceptNodeRecord: %v", err)
		}
	})
	t.Run("a token is never an approver", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		h.dropNode(t, "n1")
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
		rec.Approvers[0].Human = false
		if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); !errors.Is(err, runner.ErrNodeRecordRefused) {
			t.Fatalf("AcceptNodeRecord: %v", err)
		}
	})
	t.Run("four eyes needs two distinct humans", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		h.dropNode(t, "n1")
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice", "ALICE")
		rec.FourEyes = true
		if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); !errors.Is(err, runner.ErrNodeRecordRefused) {
			t.Fatalf("AcceptNodeRecord: %v", err)
		}
	})
	t.Run("an accepted record is the record arm", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		h.dropNode(t, "n1")
		rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
		if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); err != nil {
			t.Fatal(err)
		}
		if err := h.d.StopProxy(ctx, agentPodName(h.runID)); err != nil {
			t.Fatal(err)
		}
		if w := h.ev.get(t, h.runID).Pods[componentProxy].Witness; w == nil || w.Arm != armRecord || w.RecordID != rec.ID || w.Tuple == nil {
			t.Fatalf("witness %+v", w)
		}
	})
}

// TestRunnerContainmentPendingKinds: with no evidence the agent pod is deleted too and the run is
// held as pending_delete once both bounds passed, its NetworkPolicies kept.
func TestRunnerContainmentPendingKinds(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	agent := h.proxyPod("n1")
	agent.Name, agent.UID = agentPodName(h.runID), "uid-agent-1"
	agent.Labels = wardynLabels(h.runID, componentAgent, map[string]string{runner.LabelRunnerID: testRunnerID})
	agent.Labels[runner.LabelRunnerID] = testRunnerID
	agent.Status.ContainerStatuses[0].ContainerID = "containerd://a1"
	if _, err := h.cs.CoreV1().Pods(testNamespace).Create(context.Background(), agent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := h.d.StopProxy(context.Background(), agentPodName(h.runID))
	var pending *runner.ContainmentPendingError
	if !errors.As(err, &pending) || pending.Kind != runner.ContainmentPendingDelete || pending.Tuple == nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if !h.ev.get(t, h.runID).Hold {
		t.Fatal("pending_delete did not hold the NetworkPolicies")
	}
	if n := countActions(h.cs, "delete-collection", "networkpolicies"); n != 0 {
		t.Fatal("NetworkPolicies dropped without completion evidence")
	}
}

func TestRunnerServerModeUnchanged(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	if err := d.StopProxy(context.Background(), agentPodName(uuid.New())); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("server StopProxy: %v", err)
	}
	if d.podDeadline(time.Hour) == nil || *d.podDeadline(time.Hour) != *activeDeadline(time.Hour) {
		t.Fatal("server pod deadline changed")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}
	if err := d.runnerPodSpec(pod, componentProxy, "https://wardynd:8443"); err != nil || len(pod.Finalizers) != 0 || pod.Spec.RestartPolicy != "" || len(pod.Labels) != 0 {
		t.Fatalf("server pod spec changed: %+v %v", pod, err)
	}
	if rules := d.runnerdPeerRules(); rules != nil {
		t.Fatal("server proxy policy gained a peer")
	}
}

func TestRunnerPodSpec(t *testing.T) {
	h := newRunnerHarness(t)
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	h.d.cfg.MaxPodLifetime = 2 * time.Hour
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{runner.LabelRunnerID: "spoofed"}}}
	if err := h.d.runnerPodSpec(pod, componentProxy, "https://wardynd.wardyn.svc:8443/x"); err != nil {
		t.Fatal(err)
	}
	if pod.Labels[runner.LabelRunnerID] != testRunnerID || pod.Finalizers[0] != stopProofFinalizer || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("proxy pod %+v", pod)
	}
	if ha := pod.Spec.HostAliases; len(ha) != 1 || ha[0].IP != "10.96.0.40" || ha[0].Hostnames[0] != "wardynd.wardyn.svc" {
		t.Fatalf("hostAliases %+v", ha)
	}
	if got := *h.d.podDeadline(10 * time.Hour); got != 7200 {
		t.Fatalf("deadline %d, want the max pod lifetime", got)
	}
	if got := *h.d.podDeadline(0); got != 7200 {
		t.Fatalf("uncapped deadline %d", got)
	}
	if got, want := *h.d.podDeadline(time.Minute), *activeDeadline(time.Minute); got != want {
		t.Fatalf("short run deadline %d, want %d", got, want)
	}
}

func TestRunnerProxyConfigKeys(t *testing.T) {
	h := newRunnerHarness(t)
	ctx := context.Background()
	if _, err := h.d.CreateSandbox(ctx, runner.SandboxSpec{RunID: h.runID}); !errors.Is(err, errDecisionSinkUnwired) {
		t.Fatalf("CreateSandbox with no decision sink: %v", err)
	}
	h.d.cfg.DecisionSink = func(context.Context, uuid.UUID) (any, error) {
		return map[string]string{"url": "tls://10.96.0.40:9443"}, nil
	}
	b, err := h.d.runnerProxyConfig(ctx, h.runID, []byte(`{"run_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["self_lapse"] != true || got["decision_sink"] == nil || got["hard_deny"] == nil || got["run_id"] != "x" {
		t.Fatalf("proxy config %s", b)
	}
	hd := got["hard_deny"].(map[string]any)
	if fmt.Sprint(hd["hosts"]) != "[127.0.0.1]" {
		t.Fatalf("hard_deny lacks the apiserver: %s", b)
	}
}

func TestRunnerHardDeny(t *testing.T) {
	h, err := NewHardDeny("cluster.local.", []string{"10.96.0.40", "10.244.0.9", "10.96.0.40"}, []string{"10.244.0.0/16", "10.96.0.1/12"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(h.Suffixes, h.Hosts, h.CIDRs) != "[svc cluster.local] [10.244.0.9 10.96.0.40] [10.244.0.0/16 10.96.0.0/12]" {
		t.Fatalf("hard deny %+v", h)
	}
	for _, bad := range []struct{ domain, host, cidr string }{{"", "", ""}, {"cluster.local", "10.0.0.1:443", ""}, {"cluster.local", "", "10.0.0.0/33"}} {
		var hosts, cidrs []string
		if bad.host != "" {
			hosts = []string{bad.host}
		}
		if bad.cidr != "" {
			cidrs = []string{bad.cidr}
		}
		if _, err := NewHardDeny(bad.domain, hosts, cidrs); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	got, err := h.withAPIServer(context.Background(), "api.example.internal:6443", func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil })
	if err != nil || fmt.Sprint(got.Hosts) != "[10.244.0.9 10.96.0.40 192.0.2.10 api.example.internal]" {
		t.Fatalf("with apiserver %+v %v", got, err)
	}
}

func TestRunnerBootRefusesOverrides(t *testing.T) {
	for _, cfg := range []Config{
		{RunnerID: testRunnerID, Evidence: &memEvidence{}, HardDeny: &HardDeny{}, AllowUnenforcedNetPol: true},
		{RunnerID: testRunnerID, Evidence: &memEvidence{}, HardDeny: &HardDeny{}, AckAmbientDefaultDeny: true},
		{RunnerID: testRunnerID, HardDeny: &HardDeny{}},
		{RunnerID: testRunnerID, Evidence: &memEvidence{}},
	} {
		cs := fake.NewClientset()
		installCanaryReactor(t, cs, false)
		cfg.Namespace, cfg.ProxyImage = testNamespace, "wardyn/wardyn-proxy:test"
		if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err == nil {
			t.Fatalf("runner booted with %+v", cfg)
		}
	}
}

// TestRunnerTeardownHoldsPolicies: runner-mode teardown drops nothing without evidence, and drops
// pods, Secrets, then NetworkPolicies once both pods are proven stopped.
func TestRunnerTeardownHoldsPolicies(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	ctx := context.Background()
	if err := h.d.teardownByRunID(ctx, h.runID, nil); err == nil {
		t.Fatal("teardown completed with no completion evidence")
	}
	if n := countActions(h.cs, "delete-collection", "networkpolicies") + countActions(h.cs, "delete-collection", "secrets"); n != 0 {
		t.Fatalf("%d collections dropped without evidence", n)
	}
	h2 := newRunnerHarness(t)
	h2.boundProxy(t)
	h2.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
	if err := h2.d.teardownByRunID(ctx, h2.runID, nil); err != nil {
		t.Fatalf("teardown with evidence: %v", err)
	}
	var order []string
	for _, a := range h2.cs.Actions() {
		if a.GetVerb() == "delete-collection" {
			order = append(order, a.GetResource().Resource)
		}
	}
	if fmt.Sprint(order) != "[secrets networkpolicies]" {
		t.Fatalf("teardown order %v", order)
	}
}
