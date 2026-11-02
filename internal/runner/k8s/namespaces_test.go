// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// otherNamespace is a second runs namespace beside testNamespace (design T5 §2.7).
const otherNamespace = "wardyn-test-b"

// TestCreateSandbox_InTheNamespaceTheSpecNames: every object of a run naming the second namespace
// is created there, and the run is found there again after the map is lost (a restart), by Status
// and by the teardown, which removes it there.
func TestCreateSandbox_InTheNamespaceTheSpecNames(t *testing.T) {
	d, cs := newTestDriver(t, Config{Namespaces: []string{testNamespace, otherNamespace}})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	cs.ClearActions()
	ctx := context.Background()

	spec := testSandboxSpec()
	spec.Namespace = otherNamespace
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	creates := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			creates++
			if a.GetNamespace() != otherNamespace {
				t.Errorf("%s created in %q, want %q", a.GetResource().Resource, a.GetNamespace(), otherNamespace)
			}
		}
	}
	if creates != 5 {
		t.Fatalf("%d creates, want the run's 5 objects", creates)
	}

	d.runNamespaces.Delete(spec.RunID) // a restarted driver has no map
	st, err := d.Status(ctx, sb.Ref)
	if err != nil || st.State == types.RunStopped {
		t.Fatalf("Status after the map was lost = %+v, %v; want the run found in %s, not gone", st, err, otherNamespace)
	}
	if ns, _ := d.runNamespaces.Load(spec.RunID); ns != otherNamespace {
		t.Errorf("the search noted %v, want %s", ns, otherNamespace)
	}

	d.runNamespaces.Delete(spec.RunID)
	if err := d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + spec.RunID.String()}
	pods, _ := cs.CoreV1().Pods(otherNamespace).List(ctx, sel)
	pols, _ := cs.NetworkingV1().NetworkPolicies(otherNamespace).List(ctx, sel)
	secs, _ := cs.CoreV1().Secrets(otherNamespace).List(ctx, sel)
	if len(pods.Items)+len(pols.Items)+len(secs.Items) != 0 {
		t.Errorf("teardown left %d pods, %d NetworkPolicies, %d Secrets in %s", len(pods.Items), len(pols.Items), len(secs.Items), otherNamespace)
	}
}

// TestCreateSandbox_RefusesAnUnservedNamespace: a spec naming a namespace the driver does not
// serve is refused, by name, before anything is created.
func TestCreateSandbox_RefusesAnUnservedNamespace(t *testing.T) {
	d, cs := newTestDriver(t, Config{Namespaces: []string{testNamespace, otherNamespace}})
	cs.ClearActions()
	spec := testSandboxSpec()
	spec.Namespace = "team-elsewhere"
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errRunsNamespaceNotServed) || !strings.Contains(err.Error(), "team-elsewhere") {
		t.Fatalf("CreateSandbox in an unserved namespace: %v, want runs_namespace_not_served naming it", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d API calls before the refusal, want none: %v", n, cs.Actions())
	}
	if _, ok := d.runNamespaces.Load(spec.RunID); ok {
		t.Error("a refused run was noted")
	}
}

// TestRunNamespace_OneNamespaceReadsNothing: with one namespace a run's namespace is answered
// without an API call, so a single-namespace driver makes exactly the calls it always made.
func TestRunNamespace_OneNamespaceReadsNothing(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()
	ns, err := d.runNamespace(context.Background(), uuid.New())
	if err != nil || ns != testNamespace {
		t.Fatalf("runNamespace = %q, %v; want %s", ns, err, testNamespace)
	}
	if ns, err := d.refNamespace(context.Background(), "not-a-ref"); err != nil || ns != testNamespace {
		t.Fatalf("refNamespace of a non-ref = %q, %v; want %s", ns, err, testNamespace)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d API calls, want none: %v", n, cs.Actions())
	}
}

// TestRunNamespace_Map: the namespace comes from the map, then the run's record without a list,
// then a search of each namespace; a run found nowhere answers the default namespace.
func TestRunNamespace_Map(t *testing.T) {
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	ctx := context.Background()
	h.cs.ClearActions()

	noted := uuid.New()
	h.d.noteNamespace(noted, otherNamespace)
	recorded := uuid.New()
	if _, err := h.d.mutate(recorded, func(ev *runEvidence) (bool, error) { ev.Namespace = otherNamespace; return true, nil }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{noted, recorded} {
		if ns, err := h.d.runNamespace(ctx, id); err != nil || ns != otherNamespace {
			t.Fatalf("runNamespace(%s) = %q, %v; want %s", id, ns, err, otherNamespace)
		}
	}
	if n := len(h.cs.Actions()); n != 0 {
		t.Errorf("the map and the record answered with %d API calls, want none", n)
	}

	h.cs.ClearActions()
	if ns, err := h.d.runNamespace(ctx, uuid.New()); err != nil || ns != testNamespace {
		t.Fatalf("an unknown run = %q, %v; want the default %s", ns, err, testNamespace)
	}
	lists := map[string]bool{}
	for _, a := range h.cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			lists[a.GetNamespace()] = true
		}
	}
	if !lists[testNamespace] || !lists[otherNamespace] {
		t.Errorf("the miss searched %v, want both namespaces", lists)
	}
}

// TestRunnerOpsReachTheRunsNamespace: a run in the second namespace is listed by the proxy tick,
// found by the relay's IP lookup, and proved against its pods there.
func TestRunnerOpsReachTheRunsNamespace(t *testing.T) {
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	ctx := context.Background()
	pod := h.proxyPod("n1")
	pod.Namespace = otherNamespace
	if _, err := h.cs.CoreV1().Pods(otherNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5")
	obs, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
	if _, ok := obs[h.runID]; !ok {
		t.Fatalf("ObserveProxies = %v, %v; want the run's proxy in %s", obs, err, otherNamespace)
	}
	id, ok, err := h.d.ProxyRunByIP(ctx, netip.MustParseAddr(pod.Status.PodIP))
	if err != nil || !ok || id != h.runID {
		t.Fatalf("ProxyRunByIP = %s, %v, %v; want the run in %s", id, ok, err, otherNamespace)
	}
	if err := h.d.ProveRunnerSandbox(ctx, agentPodName(h.runID), "another-runner"); !errors.Is(err, runner.ErrNotRunnerSandbox) {
		t.Fatalf("ProveRunnerSandbox for another runner: %v; the proxy in %s was not read", err, otherNamespace)
	}
}

// TestRunnerCreate_RefusesAnUnservedNamespace: the runner refuses before its own preflight.
func TestRunnerCreate_RefusesAnUnservedNamespace(t *testing.T) {
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	if _, err := h.d.CreateSandbox(context.Background(), runner.SandboxSpec{RunID: h.runID, Namespace: "team-elsewhere"}); !errors.Is(err, errRunsNamespaceNotServed) {
		t.Fatalf("CreateSandbox: %v, want runs_namespace_not_served", err)
	}
}

// TestCheckRunNamespaces: boot refuses a live run recorded in a namespace no longer listed, naming
// it, and notes the listed namespaces' runs from this runner's pods there.
func TestCheckRunNamespaces(t *testing.T) {
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	ctx := context.Background()
	gone := uuid.New()
	if _, err := h.d.mutate(gone, func(ev *runEvidence) (bool, error) { ev.Namespace = "team-removed"; return true, nil }); err != nil {
		t.Fatal(err)
	}
	err := h.d.CheckRunNamespaces(ctx, []uuid.UUID{h.runID, gone})
	if !errors.Is(err, errRunsNamespaceUnlisted) || !strings.Contains(err.Error(), gone.String()+" in team-removed") {
		t.Fatalf("CheckRunNamespaces: %v, want runs_namespace_unlisted naming the run", err)
	}

	pod := h.proxyPod("n1")
	pod.Namespace = otherNamespace
	if _, err := h.cs.CoreV1().Pods(otherNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.CheckRunNamespaces(ctx, []uuid.UUID{h.runID}); err != nil {
		t.Fatalf("CheckRunNamespaces with every namespace listed: %v", err)
	}
	if ns, _ := h.d.runNamespaces.Load(h.runID); ns != otherNamespace {
		t.Errorf("boot noted %v for the run, want %s", ns, otherNamespace)
	}
}

// TestRunnerBootChecksEachNamespace: the access review, the foreign-object check and the canary
// each cover every listed namespace, and a refusal names the namespace it came from.
func TestRunnerBootChecksEachNamespace(t *testing.T) {
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	ctx := context.Background()

	h.cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sar := a.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		ra := sar.Spec.ResourceAttributes
		sar.Status.Allowed = !(ra.Namespace == otherNamespace && ra.Resource == "pods" && ra.Verb == "patch")
		return true, sar, nil
	})
	if err := h.d.CheckRunnerAccess(ctx, "wardyn-runner"); !errors.Is(err, errRunnerAccessMissing) || !strings.Contains(err.Error(), otherNamespace+"/pods:patch") {
		t.Fatalf("CheckRunnerAccess: %v, want the verb missing in %s named", err, otherNamespace)
	}

	foreign := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "server-run", Namespace: otherNamespace, Labels: map[string]string{labelManaged: "true"}}}
	if _, err := h.cs.NetworkingV1().NetworkPolicies(otherNamespace).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.CheckForeignObjects(ctx); !errors.Is(err, errRunnerForeignObjects) || !strings.Contains(err.Error(), "namespace "+otherNamespace) {
		t.Fatalf("CheckForeignObjects: %v, want the second namespace's object refused", err)
	}

	// The canary: run in each namespace, and one unenforced namespace refuses the runner.
	cs := fake.NewClientset()
	installCanaryReactorByNamespace(t, cs, map[string]int32{testNamespace: 1, otherNamespace: 0})
	cfg := Config{Namespaces: []string{testNamespace, otherNamespace}, ProxyImage: "wardyn/wardyn-proxy:test"}
	_, err := newWithClient(ctx, cs, testRestConfig(), cfg)
	if !errors.Is(err, errNetworkPolicyUnenforced) || !strings.Contains(err.Error(), "namespace "+otherNamespace) {
		t.Fatalf("newWithClient with %s unenforced: %v, want the refusal naming it", otherNamespace, err)
	}
	canaries := map[string]int{}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			canaries[a.GetNamespace()]++
		}
	}
	if canaries[testNamespace] != 2 || canaries[otherNamespace] != 2 {
		t.Errorf("canary pods per namespace = %v, want both phases in each", canaries)
	}
}

// installCanaryReactorByNamespace is installCanaryReactor with phase B's exit code per namespace.
func installCanaryReactorByNamespace(t *testing.T, cs *fake.Clientset, phaseBExit map[string]int32) {
	t.Helper()
	seen := map[string]bool{}
	phases := map[string]int{}
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-egress-canary-") {
			return false, nil, nil
		}
		ns := action.GetNamespace()
		if !seen[ga.GetName()] {
			seen[ga.GetName()] = true
			phases[ns]++
		}
		exitCode := int32(0)
		if phases[ns] >= 2 {
			exitCode = phaseBExit[ns]
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ns},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  canaryContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
			}}},
		}, nil
	})
}
