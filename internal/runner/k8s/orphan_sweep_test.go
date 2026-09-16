// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
