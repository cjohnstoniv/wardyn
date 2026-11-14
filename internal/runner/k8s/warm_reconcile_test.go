// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const warmTestImage = "registry.example/agent:1"

var netPolsGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}

// honourPodDeletePreconditions makes the fake clientset refuse a pod delete whose UID or resourceVersion precondition
// does not match the stored pod, as the API server does.
func honourPodDeletePreconditions(cs *fake.Clientset) {
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		del := a.(clienttesting.DeleteAction)
		obj, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), del.GetName())
		if err != nil {
			return true, nil, err
		}
		cur := obj.(*corev1.Pod)
		if p := del.GetDeleteOptions().Preconditions; p != nil &&
			((p.UID != nil && *p.UID != cur.UID) || (p.ResourceVersion != nil && *p.ResourceVersion != cur.ResourceVersion)) {
			return true, nil, apierrors.NewConflict(podsGVR.GroupResource(), del.GetName(), errors.New("precondition failed"))
		}
		return false, nil, nil
	})
}

// admittedWithAnEnv stands in for a mutating webhook and for the scheduler's bind: the stored placeholder gains an env
// var, and its resourceVersion has already moved past the one the create returns.
func admittedWithAnEnv(cs *fake.Clientset) {
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pod := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID = "warm-uid-1"
		pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "INJECTED", Value: "x"}}
		stored := pod.DeepCopy()
		stored.ResourceVersion = "2"
		if err := cs.Tracker().Create(podsGVR, stored, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		pod.ResourceVersion = "1"
		return true, pod, nil
	})
}

// admittedChanged stands in for a mutating webhook: change rewrites the placeholder before it is stored, and the create
// returns what was stored, as the API server does.
func admittedChanged(cs *fake.Clientset, change func(*corev1.Pod)) {
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pod := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID = "warm-uid-5"
		pod.ResourceVersion = "1"
		change(pod)
		if err := cs.Tracker().Create(podsGVR, pod, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, pod.DeepCopy(), nil
	})
}

// takenBetweenListAndCreate stands in for another replica: its placeholder, as built, is stored under the name just
// before this replica's create, which then answers AlreadyExists.
func takenBetweenListAndCreate(cs *fake.Clientset) {
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pod := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID = "warm-uid-7"
		if err := cs.Tracker().Create(podsGVR, pod, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(podsGVR.GroupResource(), pod.Name)
	})
}

func warmTestPool(name string, size int) WarmPool {
	return WarmPool{Name: name, Image: warmTestImage, Class: "CC1", CPU: "500m", Memory: "1Gi", Size: size}
}

// warmTestDriver is a driver over the fake clientset with pools configured and the canary's actions forgotten.
func warmTestDriver(t *testing.T, pools ...WarmPool) (*Driver, *fake.Clientset) {
	t.Helper()
	d, cs := newTestDriver(t, Config{WarmPools: pools})
	cs.ClearActions()
	return d, cs
}

// seedDeny stores the correct deny policy.
func seedDeny(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	if err := cs.Tracker().Add(warmDenyPolicyObject(testNamespace)); err != nil {
		t.Fatalf("seed the deny policy: %v", err)
	}
}

// seedPlaceholder stores a placeholder built as the builder builds it, then edited by edit.
func seedPlaceholder(t *testing.T, d *Driver, cs *fake.Clientset, p WarmPool, i int, edit func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	pod, err := d.warmPlaceholderPod(context.Background(), p, testNamespace, i)
	if err != nil {
		t.Fatalf("build a placeholder: %v", err)
	}
	pod.CreationTimestamp = metav1.NewTime(d.now())
	pod.Status.Phase = corev1.PodRunning
	if edit != nil {
		edit(pod)
	}
	if err := cs.Tracker().Add(pod); err != nil {
		t.Fatalf("seed a placeholder: %v", err)
	}
	return pod
}

// warmSeq is every action the fake clientset saw, as "verb resource".
func warmSeq(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
	}
	return out
}

// requireInOrder fails unless want is a subsequence of got.
func requireInOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	at := 0
	for _, g := range got {
		if at < len(want) && g == want[at] {
			at++
		}
	}
	if at != len(want) {
		t.Fatalf("actions %v do not hold %v in order", got, want)
	}
}

// requireAdjacent fails unless the first `first` is followed at once by `second`.
func requireAdjacent(t *testing.T, got []string, first, second string) {
	t.Helper()
	i := slices.Index(got, first)
	if i < 0 || i+1 >= len(got) || got[i+1] != second {
		t.Fatalf("actions %v: %q does not follow %q at once", got, second, first)
	}
}

func countOf(got []string, action string) int {
	n := 0
	for _, g := range got {
		if g == action {
			n++
		}
	}
	return n
}

// deletesOf is the delete actions on resource, in order.
func deletesOf(cs *fake.Clientset, resource string) []clienttesting.DeleteAction {
	var out []clienttesting.DeleteAction
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == resource {
			out = append(out, a.(clienttesting.DeleteAction))
		}
	}
	return out
}

// requireUIDOnly fails unless the delete named uid and no resourceVersion.
func requireUIDOnly(t *testing.T, del clienttesting.DeleteAction, uid string) {
	t.Helper()
	p := del.GetDeleteOptions().Preconditions
	if p == nil || p.UID == nil || string(*p.UID) != uid || p.ResourceVersion != nil {
		t.Fatalf("delete of %s has preconditions %+v, want the UID %s alone", del.GetName(), p, uid)
	}
}

func requirePodGone(t *testing.T, cs *fake.Clientset, name string) {
	t.Helper()
	if _, err := cs.Tracker().Get(podsGVR, testNamespace, name); !apierrors.IsNotFound(err) {
		t.Fatalf("pod %s: got %v, want NotFound", name, err)
	}
}

func TestWarmPlaceholderHoldsNothing(t *testing.T) {
	d, _ := warmTestDriver(t)
	p := warmTestPool("a", 1)
	pod, err := d.warmPlaceholderPod(context.Background(), p, testNamespace, 3)
	if err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if pod.Name != "wardyn-warm-a-3" || pod.Namespace != testNamespace {
		t.Errorf("pod is %s/%s", pod.Namespace, pod.Name)
	}
	if err := placeholderHoldsNothing(pod, p.Image); err != nil {
		t.Errorf("the built placeholder is refused: %v", err)
	}
	s := pod.Spec
	if len(s.Volumes) != 0 || len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 || len(s.Containers) != 1 {
		t.Errorf("volumes %d, init %d, ephemeral %d, containers %d", len(s.Volumes), len(s.InitContainers), len(s.EphemeralContainers), len(s.Containers))
	}
	c := s.Containers[0]
	if len(c.Env) != 0 || len(c.EnvFrom) != 0 || len(c.VolumeMounts) != 0 || c.Image != warmTestImage {
		t.Errorf("container carries env %d, envFrom %d, mounts %d, image %q", len(c.Env), len(c.EnvFrom), len(c.VolumeMounts), c.Image)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Error("the service account token may be mounted")
	}
	if _, ok := pod.Labels[labelRun]; ok {
		t.Error("the placeholder carries a run label")
	}
	if pod.Labels[labelComponent] != componentWarm || pod.Labels[labelWarmPool] != "a" {
		t.Errorf("labels %v lack the component or pool label", pod.Labels)
	}
	proxy := proxyResources(false)
	wantCPU, wantMem := resourceSum("500m", proxy.Requests.Cpu().String()), resourceSum("1Gi", proxy.Requests.Memory().String())
	for name, list := range map[string]corev1.ResourceList{"requests": c.Resources.Requests, "limits": c.Resources.Limits} {
		if list.Cpu().Cmp(wantCPU) != 0 || list.Memory().Cmp(wantMem) != 0 {
			t.Errorf("%s are cpu %s, memory %s; want %s and %s", name, list.Cpu(), list.Memory(), &wantCPU, &wantMem)
		}
	}
	if s.TerminationGracePeriodSeconds == nil || *s.TerminationGracePeriodSeconds != 1 {
		t.Errorf("grace is %v, want 1", s.TerminationGracePeriodSeconds)
	}
	if s.DNSPolicy != corev1.DNSNone {
		t.Errorf("DNS policy is %q, want None", s.DNSPolicy)
	}
}

func TestWarmPlaceholderTakesPriorityClass(t *testing.T) {
	d, _ := newTestDriver(t, Config{WarmPriorityClass: "wardyn-warm"})
	pod, err := d.warmPlaceholderPod(context.Background(), warmTestPool("a", 1), testNamespace, 0)
	if err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if pod.Spec.PriorityClassName != "wardyn-warm" {
		t.Errorf("priority class is %q, want wardyn-warm", pod.Spec.PriorityClassName)
	}
}

func TestWarmPlaceholderCC2TakesThePinnedRuntimeClass(t *testing.T) {
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "gvisor"}})
	mustCreateRuntimeClass(t, cs, "gvisor", "runsc")
	cc2 := warmTestPool("a", 1)
	cc2.Class = "CC2"
	pod, err := d.warmPlaceholderPod(context.Background(), cc2, testNamespace, 0)
	if err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if rc := pod.Spec.RuntimeClassName; rc == nil || *rc != "gvisor" {
		t.Errorf("CC2 runtime class is %v, want gvisor", rc)
	}
	pod, err = d.warmPlaceholderPod(context.Background(), warmTestPool("b", 1), testNamespace, 0)
	if err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("CC1 runtime class is %q, want none", *pod.Spec.RuntimeClassName)
	}
}

func TestWarmPlaceholderCC2WithNoRuntimeClassIsRefusedAndNothingIsCreated(t *testing.T) {
	cc2 := warmTestPool("a", 1)
	cc2.Class = "CC2"
	d, cs := warmTestDriver(t, cc2)
	if _, err := d.warmPlaceholderPod(context.Background(), cc2, testNamespace, 0); !errors.Is(err, errRuntimeClassUnavailable) {
		t.Fatalf("warmPlaceholderPod error %v, want errRuntimeClassUnavailable", err)
	}
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("a CC2 pool with no RuntimeClass made %d create calls, want none", n)
	}
	if !d.warmBackedOff("a") {
		t.Fatal("the pool is not backed off after the refused placeholder")
	}
}

func TestWarmPlaceholderCarriesThePullSecret(t *testing.T) {
	d, _ := newTestDriver(t, Config{ImagePullSecret: "regcred"})
	pod, err := d.warmPlaceholderPod(context.Background(), warmTestPool("a", 1), testNamespace, 0)
	if err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if s := pod.Spec.ImagePullSecrets; len(s) != 1 || s[0].Name != "regcred" {
		t.Errorf("pull secrets %v, want regcred alone", s)
	}
	bare, _ := newTestDriver(t, Config{})
	if pod, err = bare.warmPlaceholderPod(context.Background(), warmTestPool("a", 1), testNamespace, 0); err != nil {
		t.Fatalf("warmPlaceholderPod: %v", err)
	}
	if s := pod.Spec.ImagePullSecrets; len(s) != 0 {
		t.Errorf("pull secrets %v with none configured, want none", s)
	}
}

func TestWarmReconcileDenyPolicyBeforeFirstPlaceholder(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	d.reconcileWarmPools(context.Background())
	requireInOrder(t, warmSeq(cs), "create networkpolicies", "create pods")
}

func TestWarmReconcileWrongPolicyDeletesPlaceholdersFirst(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	honourPodDeletePreconditions(cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) { pod.UID, pod.ResourceVersion = "warm-uid-3", "5" })
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.UID, wrong.ResourceVersion = "np-old", "3"
	wrong.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if err := cs.Tracker().Add(wrong); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	requireInOrder(t, warmSeq(cs), "delete pods", "list pods", "delete networkpolicies", "create networkpolicies", "list networkpolicies")
	requireUIDOnly(t, deletesOf(cs, "pods")[0], "warm-uid-3")
	pre := deletesOf(cs, "networkpolicies")[0].GetDeleteOptions().Preconditions
	if pre == nil || pre.UID == nil || *pre.UID != "np-old" || pre.ResourceVersion == nil || *pre.ResourceVersion != "3" {
		t.Fatalf("policy delete preconditions %+v, want the wrong policy's UID and resourceVersion", pre)
	}
}

func TestWarmReconcileOneCreatePerPass(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 3))
	d.reconcileWarmPools(context.Background())
	var made []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			made = append(made, a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).Name)
		}
	}
	if !slices.Equal(made, []string{"wardyn-warm-a-0"}) {
		t.Fatalf("pods made %v, want wardyn-warm-a-0 alone", made)
	}
}

func TestWarmReconcileSurplusHighestFirst(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedDeny(t, cs)
	for i := 0; i < 3; i++ {
		seedPlaceholder(t, d, cs, p, i, nil)
	}
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	var gone []string
	for _, del := range deletesOf(cs, "pods") {
		gone = append(gone, del.GetName())
	}
	if !slices.Equal(gone, []string{"wardyn-warm-a-2", "wardyn-warm-a-1"}) {
		t.Fatalf("deleted %v, want 2 then 1", gone)
	}
	if countOf(warmSeq(cs), "create pods") != 0 {
		t.Error("a pod was made beside a full pool")
	}
}

func TestWarmReconcileDeletesFailedStaleAndUnknown(t *testing.T) {
	p := warmTestPool("a", 4)
	d, cs := warmTestDriver(t, p)
	seedDeny(t, cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed })
	seedPlaceholder(t, d, cs, p, 1, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded })
	seedPlaceholder(t, d, cs, p, 2, func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodPending
		pod.CreationTimestamp = metav1.NewTime(d.now().Add(-d.cfg.StartTimeout - time.Minute))
	})
	seedPlaceholder(t, d, cs, p, 3, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodPending }) // not yet stale: kept
	seedPlaceholder(t, d, cs, warmTestPool("gone", 1), 0, nil)
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	var gone []string
	for _, del := range deletesOf(cs, "pods") {
		gone = append(gone, del.GetName())
	}
	slices.Sort(gone)
	want := []string{"wardyn-warm-a-0", "wardyn-warm-a-1", "wardyn-warm-a-2", "wardyn-warm-gone-0"}
	if !slices.Equal(gone, want) {
		t.Fatalf("deleted %v, want %v", gone, want)
	}
}

func TestWarmReconcileTerminatingPlaceholderIsNotLiveAndKeepsItsName(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedDeny(t, cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) {
		gone := metav1.NewTime(d.now())
		pod.DeletionTimestamp = &gone
	})
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	var made []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			made = append(made, a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).Name)
		}
	}
	if !slices.Equal(made, []string{"wardyn-warm-a-1"}) {
		t.Fatalf("pods made %v, want wardyn-warm-a-1 alone (the stopping one is not live, and holds its name)", made)
	}
	if n := len(deletesOf(cs, "pods")); n != 0 {
		t.Fatalf("%d pod deletes, want none for a placeholder already stopping", n)
	}
}

func TestWarmReconcileKeepsAnOldRunningPlaceholder(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedDeny(t, cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) {
		pod.CreationTimestamp = metav1.NewTime(d.now().Add(-d.cfg.StartTimeout - time.Minute))
	})
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := len(deletesOf(cs, "pods")); n != 0 {
		t.Fatalf("%d pod deletes, want none: only a Pending placeholder goes stale", n)
	}
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d pods made beside a full pool", n)
	}
}

func TestWarmReconcileNoPoolsDeletesAll(t *testing.T) {
	d, cs := warmTestDriver(t)
	p := warmTestPool("a", 1)
	seedPlaceholder(t, d, cs, p, 0, nil)
	seedPlaceholder(t, d, cs, p, 1, nil)
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := len(deletesOf(cs, "pods")); n != 2 {
		t.Fatalf("%d pods deleted, want 2", n)
	}
	if seq := warmSeq(cs); countOf(seq, "create pods") != 0 || slices.Contains(seq, "create networkpolicies") {
		t.Errorf("with no pools the pass made objects: %v", seq)
	}
}

func TestWarmReconcileBacksOffAfterCreateError(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	cs.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 1 {
		t.Fatalf("first pass made %d create calls, want 1", n)
	}
	if !d.warmBackedOff("a") {
		t.Fatal("the pool is not backed off after a failed create")
	}
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("second pass made %d create calls, want none", n)
	}
}

func TestWarmReconcileCreatesResumeAfterTheBackoff(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	clock := time.Now().Add(-time.Hour) // off the wall clock: a backoff set or read on time.Now fails below
	d.now = func() time.Time { return clock }
	failed := false
	cs.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.New("quota exceeded")
	})
	d.reconcileWarmPools(context.Background())
	if !d.warmBackedOff("a") {
		t.Fatal("the pool is not backed off after a failed create")
	}
	clock = clock.Add(warmBackoff - time.Second)
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d creates before the backoff ended, want none", n)
	}
	clock = clock.Add(2 * time.Second)
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 1 {
		t.Fatalf("%d creates after the backoff ended, want 1", n)
	}
}

// refuseNewSandboxes puts the live canary verdict into the state netPolRefusal refuses.
func refuseNewSandboxes(d *Driver) {
	d.netPol.Store(&netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: netPolNotProven}}})
}

func TestWarmReconcileNoCreateWhileCanaryRefuses(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	seedDeny(t, cs)
	refuseNewSandboxes(d)
	if d.netPolRefusal(testNamespace) == nil {
		t.Fatal("the canary does not refuse")
	}
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d pods made while the canary refuses", n)
	}
}

func TestWarmReconcileDenyCheckedWhileCanaryRefuses(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) { pod.UID = "warm-uid-8" })
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if err := cs.Tracker().Add(wrong); err != nil {
		t.Fatal(err)
	}
	refuseNewSandboxes(d)
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	seq := warmSeq(cs)
	requireInOrder(t, seq, "delete pods", "delete networkpolicies", "create networkpolicies")
	if countOf(seq, "create pods") != 0 {
		t.Error("a placeholder was made while the canary refuses")
	}
	np, err := d.readWarmDeny(context.Background(), testNamespace)
	if err != nil || np == nil || !warmDenyHolds(np) {
		t.Fatalf("the deny policy after the pass: %v, %v", np, err)
	}
}

// wrongDenyIsReplaced seeds wrong as the stored policy, runs one pass with one pool of size 1, and expects the wrong
// policy deleted by its UID and the right one created before any placeholder.
func wrongDenyIsReplaced(t *testing.T, wrong *networkingv1.NetworkPolicy) {
	t.Helper()
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	if err := cs.Tracker().Add(wrong); err != nil {
		t.Fatal(err)
	}
	d.reconcileWarmPools(context.Background())
	dels := deletesOf(cs, "networkpolicies")
	if len(dels) != 1 || dels[0].GetDeleteOptions().Preconditions == nil || *dels[0].GetDeleteOptions().Preconditions.UID != wrong.UID {
		t.Fatalf("policy deletes %v, want one carrying UID %s", dels, wrong.UID)
	}
	requireInOrder(t, warmSeq(cs), "delete networkpolicies", "create networkpolicies", "create pods")
	np, err := d.readWarmDeny(context.Background(), testNamespace)
	if err != nil || np == nil || !warmDenyHolds(np) {
		t.Fatalf("the deny policy after the pass: %v, %v", np, err)
	}
}

func TestWarmDenyExtraMatchExpressionIsWrong(t *testing.T) {
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.UID = "np-expr"
	wrong.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: labelRun, Operator: metav1.LabelSelectorOpExists}}
	wrongDenyIsReplaced(t, wrong)
}

func TestWarmDenyExtraLabelIsWrong(t *testing.T) {
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.UID = "np-label"
	wrong.Spec.PodSelector.MatchLabels = map[string]string{labelComponent: componentWarm, "extra": "x"}
	wrongDenyIsReplaced(t, wrong)
}

func TestWarmDenySameNameWithoutTheLabelIsSeen(t *testing.T) {
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.UID = "np-nolabel"
	wrong.Labels = nil
	wrong.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	wrongDenyIsReplaced(t, wrong)
}

func TestWarmDenyAlreadyExistsIsReadBack(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	wrong := warmDenyPolicyObject(testNamespace)
	wrong.UID = "np-wrong"
	wrong.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if err := cs.Tracker().Add(wrong); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(netPolsGVR.GroupResource(), warmDenyPolicy, errors.New("changed"))
	})
	if err := d.ensureWarmDeny(context.Background(), testNamespace, nil); err == nil {
		t.Fatal("ensureWarmDeny accepted a wrong policy that the create's AlreadyExists hid")
	}
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d pods made behind a wrong deny policy", n)
	}
}

func TestWarmDenyBeingDeletedIsWrong(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	held := warmDenyPolicyObject(testNamespace)
	held.UID, held.Finalizers = "p0", []string{"example.com/hold"}
	now := metav1.NewTime(d.now())
	held.DeletionTimestamp = &now
	if err := cs.Tracker().Add(held); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // the finalizer holds the object
	})
	d.reconcileWarmPools(context.Background())
	dels := deletesOf(cs, "networkpolicies")
	if len(dels) != 1 || dels[0].GetDeleteOptions().Preconditions == nil || *dels[0].GetDeleteOptions().Preconditions.UID != "p0" {
		t.Fatalf("policy deletes %v, want one carrying UID p0", dels)
	}
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d pods made behind a policy that is being deleted", n)
	}
	got, err := cs.Tracker().Get(netPolsGVR, testNamespace, warmDenyPolicy)
	if err != nil || got.(*networkingv1.NetworkPolicy).UID != "p0" {
		t.Fatalf("stored policy %v, %v; want UID p0", got, err)
	}
}

func TestWarmDenyStaleRepairSparesANewerPolicy(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	newer := warmDenyPolicyObject(testNamespace)
	newer.UID = "p1"
	if err := cs.Tracker().Add(newer); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("delete", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		del := a.(clienttesting.DeleteAction)
		obj, err := cs.Tracker().Get(netPolsGVR, a.GetNamespace(), del.GetName())
		if err != nil {
			return true, nil, err
		}
		if p := del.GetDeleteOptions().Preconditions; p != nil && p.UID != nil && *p.UID != obj.(*networkingv1.NetworkPolicy).UID {
			return true, nil, apierrors.NewConflict(netPolsGVR.GroupResource(), del.GetName(), errors.New("precondition failed"))
		}
		return false, nil, nil
	})
	stale := warmDenyPolicyObject(testNamespace)
	stale.UID, stale.ResourceVersion = "p0", "1"
	stale.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if err := d.replaceWarmDeny(context.Background(), testNamespace, stale, nil); err != nil {
		t.Fatalf("replaceWarmDeny: %v", err)
	}
	dels := deletesOf(cs, "networkpolicies")
	if len(dels) != 1 || *dels[0].GetDeleteOptions().Preconditions.UID != "p0" {
		t.Fatalf("policy deletes %v, want one carrying UID p0", dels)
	}
	got, err := cs.Tracker().Get(netPolsGVR, testNamespace, warmDenyPolicy)
	if err != nil || got.(*networkingv1.NetworkPolicy).UID != "p1" {
		t.Fatalf("stored policy %v, %v; want the newer one, UID p1", got, err)
	}
}

func TestWarmReconcileDeletesAPlaceholderChangedAtAdmission(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	honourPodDeletePreconditions(cs)
	admittedWithAnEnv(cs)
	d.reconcileWarmPools(context.Background())
	seq := warmSeq(cs)
	if n := countOf(seq, "delete pods"); n != 1 {
		t.Fatalf("%d pod deletes, want 1: %v", n, seq)
	}
	requireAdjacent(t, seq, "create pods", "delete pods")
	requireUIDOnly(t, deletesOf(cs, "pods")[0], "warm-uid-1")
	requirePodGone(t, cs, "wardyn-warm-a-0")
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("second pass made %d creates, want none (backoff)", n)
	}
}

// deletedAsChanged seeds one live placeholder edited by edit with the given UID and resourceVersion, runs one pass, and
// expects it deleted by UID alone with no create for the pool.
func deletedAsChanged(t *testing.T, uid, rv string, edit func(*corev1.Pod)) {
	t.Helper()
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	honourPodDeletePreconditions(cs)
	seedDeny(t, cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) {
		pod.UID, pod.ResourceVersion = k8stypes.UID(uid), rv
		edit(pod)
	})
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	dels := deletesOf(cs, "pods")
	if len(dels) != 1 {
		t.Fatalf("%d pod deletes, want 1: %v", len(dels), warmSeq(cs))
	}
	requireUIDOnly(t, dels[0], uid)
	requirePodGone(t, cs, "wardyn-warm-a-0")
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("%d creates, want none (backoff)", n)
	}
}

func TestWarmReconcileDeletesAListedPlaceholderThatHoldsSomething(t *testing.T) {
	deletedAsChanged(t, "warm-uid-2", "7", func(pod *corev1.Pod) { pod.Spec.Volumes = []corev1.Volume{{Name: "v"}} })
}

func TestWarmReconcileDeletesAListedPlaceholderOfAnotherImage(t *testing.T) {
	deletedAsChanged(t, "warm-uid-4", "8", func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "other.example/agent:1" })
}

func TestWarmReconcilePlainPodOnTheAgentSandboxBackend(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{WarmPools: []WarmPool{warmTestPool("a", 1)}}, controllerMaking(t, nil))
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	var made []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			made = append(made, a.(clienttesting.CreateAction).GetObject().(*corev1.Pod).Name)
		}
	}
	if !slices.Equal(made, []string{"wardyn-warm-a-0"}) {
		t.Fatalf("pods made %v, want wardyn-warm-a-0", made)
	}
	if n := sandboxActions(dyn, "create"); n != 0 {
		t.Fatalf("%d Sandbox creates, want none", n)
	}
}

func TestWarmReconcileDeletesAPlaceholderStrippedOfItsWarmLabelAtAdmission(t *testing.T) {
	changedAtAdmissionIsDeleted(t, func(p *corev1.Pod) { delete(p.Labels, labelComponent) })
}

func TestWarmReconcileDeletesAPlaceholderWhosePoolLabelChangedAtAdmission(t *testing.T) {
	changedAtAdmissionIsDeleted(t, func(p *corev1.Pod) { p.Labels[labelWarmPool] = "other" })
}

// changedAtAdmissionIsDeleted runs one pass on an empty cluster where a webhook applies change to the stored pod, and
// expects the pod deleted by UID alone right after its create, and a second pass to make no create.
func changedAtAdmissionIsDeleted(t *testing.T, change func(*corev1.Pod)) {
	t.Helper()
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	honourPodDeletePreconditions(cs)
	admittedChanged(cs, change)
	d.reconcileWarmPools(context.Background())
	seq := warmSeq(cs)
	if n := countOf(seq, "delete pods"); n != 1 {
		t.Fatalf("%d pod deletes, want 1: %v", n, seq)
	}
	requireAdjacent(t, seq, "create pods", "delete pods")
	requireUIDOnly(t, deletesOf(cs, "pods")[0], "warm-uid-5")
	requirePodGone(t, cs, "wardyn-warm-a-0")
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("second pass made %d creates, want none (backoff)", n)
	}
}

func TestWarmReconcileJudgesThePodHoldingAFreeName(t *testing.T) {
	p := warmTestPool("a", 1)
	d, cs := warmTestDriver(t, p)
	honourPodDeletePreconditions(cs)
	seedDeny(t, cs)
	seedPlaceholder(t, d, cs, p, 0, func(pod *corev1.Pod) {
		pod.UID, pod.ResourceVersion = "warm-uid-6", "9"
		delete(pod.Labels, labelComponent) // the list does not see it
	})
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	seq := warmSeq(cs)
	requireInOrder(t, seq, "create pods", "get pods", "delete pods")
	dels := deletesOf(cs, "pods")
	if len(dels) != 1 {
		t.Fatalf("%d pod deletes, want 1: %v", len(dels), seq)
	}
	requireUIDOnly(t, dels[0], "warm-uid-6")
	requirePodGone(t, cs, "wardyn-warm-a-0")
	cs.ClearActions()
	d.reconcileWarmPools(context.Background())
	if n := countOf(warmSeq(cs), "create pods"); n != 0 {
		t.Fatalf("second pass made %d creates, want none (backoff)", n)
	}
}

func TestWarmReconcileKeepsAnotherReplicasPlaceholder(t *testing.T) {
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	takenBetweenListAndCreate(cs)
	d.reconcileWarmPools(context.Background())
	if n := len(deletesOf(cs, "pods")); n != 0 {
		t.Fatalf("%d pod deletes, want none", n)
	}
	if d.warmBackedOff("a") {
		t.Error("the pool is backed off after another replica's create")
	}
	got, err := cs.Tracker().Get(podsGVR, testNamespace, "wardyn-warm-a-0")
	if err != nil || got.(*corev1.Pod).UID != "warm-uid-7" {
		t.Fatalf("stored pod %v, %v; want UID warm-uid-7", got, err)
	}
}

// resourceSum is the sum of two quantities written as strings.
func resourceSum(a, b string) resource.Quantity {
	q := resource.MustParse(a)
	q.Add(resource.MustParse(b))
	return q
}
