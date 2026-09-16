// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The whole point of this file, asserted at COMPILE time: the control plane
// reaches its orphan sweep by type assertion on the Runner it holds
// (internal/api/reconcile.go's SandboxOrphanSweeper, forwarded per-substrate by
// the orchestrator), so a driver that does not satisfy that exact shape makes
// the sweep a silent no-op rather than a build failure. Before this, the k8s
// driver did not satisfy it and NOTHING said so — this line is what makes a
// future signature drift red here instead of silently dropping the k8s
// substrate back out of the sweep.
var _ api.SandboxOrphanSweeper = (*Driver)(nil)

// agePodsOfRun backdates every pod of runID past the sweep's minAge gate. The
// fake clientset's tracker stamps no CreationTimestamp, and a zero timestamp
// would pass the gate by accident rather than on purpose — so each arm below
// states which side of the gate it is testing.
func agePodsOfRun(t *testing.T, cs *fake.Clientset, runID uuid.UUID, age time.Duration) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(testNamespace).List(context.Background(),
		metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()})
	if err != nil {
		t.Fatalf("list pods of run %s: %v", runID, err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("run %s has no pods to age — the fixture did not create what the sweep keys on", runID)
	}
	for i := range pods.Items {
		setPodCreation(t, cs, pods.Items[i].Name, time.Now().Add(-age))
	}
}

func setPodCreation(t *testing.T, cs *fake.Clientset, name string, at time.Time) {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, name)
	if err != nil {
		t.Fatalf("tracker get %q: %v", name, err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	pod.CreationTimestamp = metav1.NewTime(at)
	if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatalf("tracker update %q: %v", name, err)
	}
}

// createdSandbox runs the real CreateSandbox sequence and returns the spec, so
// each arm below starts from the genuine object set (agent pod, proxy pod, two
// NetworkPolicies, the per-run Secret) rather than a hand-rolled subset.
func createdSandbox(t *testing.T, d *Driver, cs *fake.Clientset) runner.SandboxSpec {
	t.Helper()
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	spec.SecretEnv = map[string]string{"GIT_TOKEN": "ghp_secret"}
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return spec
}

func alwaysOrphan(uuid.UUID) bool { return true }

// TestSweepOrphanedSandboxes_ReclaimsAnEvictedRunsProxyAndSecret is the 0.7.2
// regression: disk_mib is now the agent container's ephemeral-storage limit, so
// the kubelet evicts the agent pod on a path no Wardyn code is on. Both shapes
// of the aftermath must be reclaimed — the evicted pod still sitting there
// Failed, and the pod already reaped by the kubelet's terminated-pod GC with
// only the credential-bearing proxy pod and Secret left standing.
func TestSweepOrphanedSandboxes_ReclaimsAnEvictedRunsProxyAndSecret(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gcdAgent bool
	}{
		{"evicted agent pod still present", false},
		{"evicted agent pod already GC'd, proxy + secret stranded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			spec := createdSandbox(t, d, cs)

			// The kubelet's verdict: phase Failed, reason Evicted (what
			// statusFromPod's failureDetail reads back).
			setPodStatus(t, cs, testNamespace, agentPodName(spec.RunID), func(st *corev1.PodStatus) {
				st.Phase = corev1.PodFailed
				st.Reason = "Evicted"
				st.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 10Mi"
			})
			if tc.gcdAgent {
				if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), agentPodName(spec.RunID), metav1.DeleteOptions{}); err != nil {
					t.Fatalf("simulate terminated-pod GC: %v", err)
				}
			}
			agePodsOfRun(t, cs, spec.RunID, time.Hour)

			swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
			if err != nil {
				t.Fatalf("SweepOrphanedSandboxes: %v", err)
			}
			if swept != 1 {
				t.Errorf("swept = %d, want 1 (the evicted run, counted once however many of its pods survived)", swept)
			}
			// The proxy pod, both NetworkPolicies and the per-run Secret —
			// RunToken, MITM CA key, injected git token — must all be gone.
			assertRunObjectsGone(t, cs, spec.RunID)
		})
	}
}

// TestSweepOrphanedSandboxes_LeavesLiveAndYoungRunsAlone pins the two guards
// that keep the sweep from eating working runs: the control plane's isOrphan
// verdict (only api can read run rows), and the minAge gate that protects a
// dispatch still in flight — one that has created its pods but not yet written
// its sandbox_ref, and so looks exactly like an orphan from here.
func TestSweepOrphanedSandboxes_LeavesLiveAndYoungRunsAlone(t *testing.T) {
	d, cs := newTestDriver(t, Config{})

	liveSpec := createdSandbox(t, d, cs)
	agePodsOfRun(t, cs, liveSpec.RunID, time.Hour) // old, but NOT an orphan

	youngSpec := testSandboxSpec()
	youngSpec.SecretEnv = map[string]string{"GIT_TOKEN": "ghp_secret"}
	if _, err := d.CreateSandbox(context.Background(), youngSpec); err != nil {
		t.Fatalf("CreateSandbox (young): %v", err)
	}
	agePodsOfRun(t, cs, youngSpec.RunID, 5*time.Second) // an orphan, but in flight

	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute,
		func(id uuid.UUID) bool { return id != liveSpec.RunID })
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 (a live run is not an orphan, and a young one is still dispatching)", swept)
	}
	assertRunObjectsPresent(t, cs, liveSpec.RunID, "a live, ref-tracked run")
	assertRunObjectsPresent(t, cs, youngSpec.RunID, "a run younger than minAge")
}

// TestSweepOrphanedSandboxes_NeverTouchesADriveClaim is the one object that
// must survive the sweep by construction: a user drive's PVC outlives every run
// that mounts it, so reclaiming a departed person's claim is an operator
// command and the chart grants wardynd no claim delete verb at all. Belt and
// braces here, because the sweep is the only code path that deletes by label
// without a ref to key on.
func TestSweepOrphanedSandboxes_NeverTouchesADriveClaim(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)

	// A claim labelled as aggressively as a real one can be — including the
	// swept run's own id, which drives.go never stamps but which is the label a
	// careless future edit would add.
	const claimName = "wardyn-drive-shared-src"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: testNamespace,
			Labels: map[string]string{
				labelManaged: "true",
				labelRun:     spec.RunID.String(),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(1<<30, resource.BinarySI)},
			},
		},
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create drive claim: %v", err)
	}
	agePodsOfRun(t, cs, spec.RunID, time.Hour)

	if _, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan); err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)

	if _, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), claimName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the orphan sweep deleted a drive claim (%v) — a drive outlives every run that mounts it, and the chart grants no claim delete verb", err)
	}
}

func assertRunObjectsPresent(t *testing.T, cs *fake.Clientset, runID uuid.UUID, why string) {
	t.Helper()
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}
	pods, err := cs.CoreV1().Pods(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Errorf("the sweep tore down the pods of %s (%s)", runID, why)
	}
	secrets, err := cs.CoreV1().Secrets(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) == 0 {
		t.Errorf("the sweep deleted the per-run Secret of %s (%s)", runID, why)
	}
}

// ageRunSecretsAndNetPols backdates a run's Secret and NetworkPolicies past the
// sweep's minAge gate — the sibling of agePodsOfRun for the arm where no pod is
// left to age. The fake clientset stamps no CreationTimestamp, so saying it
// explicitly is how each arm states which side of the gate it is testing.
func ageRunSecretsAndNetPols(t *testing.T, cs *fake.Clientset, runID uuid.UUID, age time.Duration) {
	t.Helper()
	at := metav1.NewTime(time.Now().Add(-age))
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}

	secrets, err := cs.CoreV1().Secrets(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list secrets of run %s: %v", runID, err)
	}
	if len(secrets.Items) == 0 {
		t.Fatalf("run %s has no Secret to age — the fixture did not create what the sweep keys on", runID)
	}
	for i := range secrets.Items {
		sec := secrets.Items[i].DeepCopy()
		sec.CreationTimestamp = at
		if _, err := cs.CoreV1().Secrets(testNamespace).Update(context.Background(), sec, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("age secret %q: %v", sec.Name, err)
		}
	}
	netpols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list netpols of run %s: %v", runID, err)
	}
	for i := range netpols.Items {
		np := netpols.Items[i].DeepCopy()
		np.CreationTimestamp = at
		if _, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Update(context.Background(), np, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("age netpol %q: %v", np.Name, err)
		}
	}
}

// TestSweepOrphanedSandboxes_ReclaimsARunWhoseBothPodsAreGone is the B9-F5
// rider on B9-F1: the sweep listed PODS only, so a run reachable solely through
// its Secret and NetworkPolicies was invisible to it — permanently. Both pods
// gone before teardown is an ordinary aftermath (a deleted node takes them
// together; an eviction plus terminated-pod GC gets there on its own), and what
// survives is the object that carries every SecretEnv value verbatim. Any fix
// that leaves the Secret reachable only via a pod label repeats the bug, so the
// sweep lists the Secret and NetworkPolicy labels too.
func TestSweepOrphanedSandboxes_ReclaimsARunWhoseBothPodsAreGone(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)

	for _, name := range []string{agentPodName(spec.RunID), proxyPodName(spec.RunID)} {
		if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("simulate a deleted node taking %q: %v", name, err)
		}
	}
	ageRunSecretsAndNetPols(t, cs, spec.RunID, time.Hour)

	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (the run is still reachable by its Secret and NetworkPolicy labels)", swept)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

// TestSweepOrphanedSandboxes_LeavesAnInFlightDispatchsSecretAlone is the
// negative control for the arm above: CreateSandbox writes the Secret and both
// NetworkPolicies BEFORE either pod exists, so a dispatch in flight looks
// exactly like a run whose pods are gone. The minAge gate has to apply to those
// objects' own age, or the sweep deletes the Secret out from under a run that
// is seconds from starting.
func TestSweepOrphanedSandboxes_LeavesAnInFlightDispatchsSecretAlone(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)

	for _, name := range []string{agentPodName(spec.RunID), proxyPodName(spec.RunID)} {
		if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %q: %v", name, err)
		}
	}
	ageRunSecretsAndNetPols(t, cs, spec.RunID, 5*time.Second) // younger than minAge

	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 (a Secret younger than minAge belongs to a dispatch still in flight)", swept)
	}
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + spec.RunID.String()}
	secrets, err := cs.CoreV1().Secrets(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) == 0 {
		t.Error("the sweep deleted the Secret of a dispatch that has written it but not yet its pods")
	}
	netpols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list netpols: %v", err)
	}
	if len(netpols.Items) != 2 {
		t.Errorf("the sweep left %d NetworkPolic(ies) of an in-flight dispatch, want both", len(netpols.Items))
	}
}

// TestSweepOrphanedSandboxes_NeverTouchesACanarysNetPol pins the one object the
// widened selector could newly reach by accident: a boot canary's deny-all
// NetworkPolicy carries labelManaged and a labelRun that IS a parseable uuid
// (canary.go's M2 per-invocation suffix). Only labelComponent tells it apart
// from a run's, so the sweep selects on agent/proxy exactly as the pod list does.
func TestSweepOrphanedSandboxes_NeverTouchesACanarysNetPol(t *testing.T) {
	d, cs := newTestDriver(t, Config{})

	const canaryNetPol = "wardyn-egress-canary-netpol-stranded"
	suffix := uuid.New().String()
	if _, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Create(context.Background(), &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      canaryNetPol,
			Namespace: testNamespace,
			Labels:    map[string]string{labelManaged: "true", labelComponent: componentCanary, labelRun: suffix},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create stranded canary netpol: %v", err)
	}

	if _, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan); err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Get(context.Background(), canaryNetPol, metav1.GetOptions{}); err != nil {
		t.Fatalf("the sweep deleted a canary NetworkPolicy (%v) — a canary's labelRun is its own invocation uuid, not a run id", err)
	}
}

// forbidNetPolList installs a reactor that answers every clientset "list
// networkpolicies" with the 403 a Role without the `list` verb produces — what
// an operator running their own pre-0.7.4 Role (k8s.rbac.create=false) gets on
// upgrade day. Returns a function that lifts it, so the test's own assertions
// can read the policies back afterwards.
//
// NetworkPolicies, not Secrets: the sweep never lists a Secret (W6-S4 — `list`
// returns its body, so no configuration of the chart grants it).
func forbidNetPolList(cs *fake.Clientset) (lift func()) {
	forbidden := true
	cs.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !forbidden {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "networkpolicies"}, "",
			errors.New(`networkpolicies.networking.k8s.io is forbidden: User "system:serviceaccount:wardyn:wardyn" cannot list resource "networkpolicies"`))
	})
	return func() { forbidden = false }
}

// TestSweepOrphanedSandboxes_StillReclaimsWhenNetPolListIsForbidden is the
// half of the widened sweep that has to survive contact with a real cluster.
//
// Listing NetworkPolicies is the ONE new privilege this release asks for
// (W6-S4: `list` on secrets is not granted in any configuration — it returns
// the body). An operator who writes their own Role (k8s.rbac.create=false) has
// a pre-0.7.4 one, and the chart cannot upgrade it for them, so on upgrade day
// that list 403s. If a 403 aborts sweepCandidates, the whole sweep dies with
// it, including the evicted-agent reclaim that worked BEFORE this branch: a
// partial credential leak would be traded for a total one. The list is
// therefore best-effort — a 403 degrades to the pod-only candidate set and the
// sweep still reclaims everything it could reclaim in 0.7.3.
func TestSweepOrphanedSandboxes_StillReclaimsWhenNetPolListIsForbidden(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)
	lift := forbidNetPolList(cs)
	agePodsOfRun(t, cs, spec.RunID, time.Hour)

	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes with no `list` on networkpolicies: %v, want nil (best-effort)", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 — a Role without the new verb must still get the 0.7.3 reclaim", swept)
	}

	// The DELETE side is a different verb and is still granted, so the run's
	// objects must be gone even though the sweep could not LIST the policies.
	lift()
	assertRunObjectsGone(t, cs, spec.RunID)
}

// TestSweepOrphanedSandboxes_ListErrorsByResourceAndKind is the table R-08
// (runner-k8s-docker's re-review) asked for: a Forbidden on the NetworkPolicy
// list degrades to the pod-only candidate set (err == nil) while any other list
// error still fails the sweep honestly (err != nil).
//
// The two `secrets` rows are the W6-S4 pin in table form: the sweep does not
// list Secrets AT ALL, so neither a 403 nor an etcd outage on that call can
// reach it. They are wantErr=false for the reason the netpol rows are not —
// the call is never made.
func TestSweepOrphanedSandboxes_ListErrorsByResourceAndKind(t *testing.T) {
	cases := []struct {
		name     string
		resource string
		err      error
		wantErr  bool
	}{
		{"a forbidden secrets list is inert (never called)", "secrets", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("nope")), false},
		{"an erroring secrets list is inert (never called)", "secrets", apierrors.NewInternalError(errors.New("etcd down")), false},
		{"networkpolicies forbidden degrades", "networkpolicies", apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, "", errors.New("nope")), false},
		{"networkpolicies internal error fails", "networkpolicies", apierrors.NewInternalError(errors.New("etcd down")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			spec := createdSandbox(t, d, cs)
			agePodsOfRun(t, cs, spec.RunID, time.Hour)
			listErr := tc.err
			cs.PrependReactor("list", tc.resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, listErr
			})

			_, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SweepOrphanedSandboxes with %s listing failing (%v): err = %v, wantErr %v", tc.resource, listErr, err, tc.wantErr)
			}
		})
	}
}

// TestSweepOrphanedSandboxes_SeesAPodCreatedAfterTheNetPolList is the ordering
// guard: hasPod decides which NetworkPolicy entries defer to a pod entry, so it
// must be built from the LATEST snapshot of the two lists, not the earliest. A
// pod created in the window between the lists is otherwise invisible, and its
// run's (older) NetworkPolicy then decides the run's fate on its own age —
// reclaiming a dispatch that had just come up.
//
// The reactor is the window: it fires on the "list networkpolicies" call and
// creates the agent pod as a side effect, through the TRACKER (never the typed
// clientset, which would re-enter Fake's non-reentrant lock and deadlock — see
// installCanaryReactor's doc). The pod is stamped young, so listing pods LAST
// makes the run defer to a pod entry that the age gate then skips: swept == 0.
func TestSweepOrphanedSandboxes_SeesAPodCreatedAfterTheNetPolList(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)

	// Start from "both pods gone", the shape the NetworkPolicy entry exists for,
	// with the Secret and netpols old enough to be swept on their own.
	for _, name := range []string{agentPodName(spec.RunID), proxyPodName(spec.RunID)} {
		if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %q: %v", name, err)
		}
	}
	ageRunSecretsAndNetPols(t, cs, spec.RunID, time.Hour)

	cs.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		// Re-create the agent pod, freshly stamped, as the dispatch would.
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:              agentPodName(spec.RunID),
			Namespace:         testNamespace,
			Labels:            wardynLabels(spec.RunID, componentAgent, nil),
			CreationTimestamp: metav1.NewTime(time.Now()),
		}}
		if _, err := cs.Tracker().Get(podsGVR, testNamespace, pod.Name); err != nil {
			_ = cs.Tracker().Add(pod)
		}
		return false, nil, nil // fall through to the real list
	})

	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 — a pod that appeared after the Secret list must still be seen, "+
			"or its run's older Secret decides the run's fate alone", swept)
	}
}
