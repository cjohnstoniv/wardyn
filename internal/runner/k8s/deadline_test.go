// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	runnertypes "github.com/cjohnstoniv/wardyn/internal/types"
)

// installPodUIDReactor gives every created pod a UID, as a real API server does. The fake
// clientset assigns none, and the agent's ownerReference needs the proxy's.
func installPodUIDReactor(cs *fake.Clientset) {
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if ca, ok := action.(clienttesting.CreateAction); ok {
			if pod, ok := ca.GetObject().(*corev1.Pod); ok && pod.UID == "" {
				pod.UID = types.UID("uid-" + pod.Name)
			}
		}
		return false, nil, nil
	})
}

func getPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %q: %v", name, err)
	}
	return pod
}

// TestCreateSandbox_PodDeadlinesFollowMaxAge: with a max age both run pods carry
// activeDeadlineSeconds = max age + grace, and without one neither does.
func TestCreateSandbox_PodDeadlinesFollowMaxAge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		maxAge time.Duration
		want   *int64
	}{
		{"max age set", 2 * time.Hour, ptrInt64(int64((2*time.Hour + podDeadlineGrace) / time.Second))},
		{"max age off", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{RunMaxAge: tc.maxAge})
			spec := createdSandbox(t, d, cs)
			for _, name := range []string{proxyPodName(spec.RunID), agentPodName(spec.RunID)} {
				got := getPod(t, cs, name).Spec.ActiveDeadlineSeconds
				switch {
				case tc.want == nil && got != nil:
					t.Errorf("%s: activeDeadlineSeconds = %d, want unset", name, *got)
				case tc.want != nil && (got == nil || *got != *tc.want):
					t.Errorf("%s: activeDeadlineSeconds = %v, want %d", name, got, *tc.want)
				}
			}
		})
	}
}

// TestCreateSandbox_AgentIsOwnedByTheProxyPod: the agent is created with an ownerReference to
// the proxy pod's UID, so deleting the proxy by any route garbage-collects the agent. The
// proxy carries no reference to the agent, and the runner needs no `patch` verb to set one.
func TestCreateSandbox_AgentIsOwnedByTheProxyPod(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installPodUIDReactor(cs)
	cs.ClearActions()
	spec := createdSandbox(t, d, cs)

	proxy := getPod(t, cs, proxyPodName(spec.RunID))
	agent := getPod(t, cs, agentPodName(spec.RunID))
	if len(agent.OwnerReferences) != 1 {
		t.Fatalf("agent ownerReferences = %+v, want exactly one, to the proxy pod", agent.OwnerReferences)
	}
	ref := agent.OwnerReferences[0]
	if ref.Kind != "Pod" || ref.Name != proxy.Name || ref.UID != proxy.UID || ref.UID == "" {
		t.Errorf("agent ownerReference = %+v, want the proxy pod %s (uid %q)", ref, proxy.Name, proxy.UID)
	}
	if ref.BlockOwnerDeletion != nil || ref.Controller != nil {
		t.Errorf("ownerReference sets blockOwnerDeletion/controller (%+v): blockOwnerDeletion needs an extra RBAC permission", ref)
	}
	if len(proxy.OwnerReferences) != 0 {
		t.Errorf("proxy ownerReferences = %+v, want none", proxy.OwnerReferences)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" {
			t.Errorf("CreateSandbox issued a %s on %s; the runner Role has no patch verb", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// TestDeadlineExceededRunIsReclaimedByTheReaper: a pod the kubelet failed for activeDeadlineSeconds
// reads as a terminal agent status, with the reason kept, and the teardown the reconciler runs
// from that verdict (StopSandbox, which finalizing a run calls) removes the Secret and both
// NetworkPolicies as well as the pods. activeDeadlineSeconds deletes nothing itself.
func TestDeadlineExceededRunIsReclaimedByTheReaper(t *testing.T) {
	d, cs := newTestDriver(t, Config{RunMaxAge: time.Hour})
	spec := createdSandbox(t, d, cs)
	ref := agentPodName(spec.RunID)
	execID, err := d.Exec(context.Background(), ref, []string{"agent-run", "task"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for _, name := range []string{ref, proxyPodName(spec.RunID)} {
		setPodStatus(t, cs, testNamespace, name, func(st *corev1.PodStatus) {
			st.Phase = corev1.PodFailed
			st.Reason = "DeadlineExceeded"
			st.Message = "Pod was active on the node longer than the specified deadline"
		})
	}

	st, err := d.AgentStatus(context.Background(), ref, execID)
	if err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if st.State != runnertypes.RunFailed || !strings.Contains(st.Message, "DeadlineExceeded") {
		t.Fatalf("AgentStatus = %+v, want a terminal failure naming DeadlineExceeded", st)
	}
	if st.ExitCode != nil && *st.ExitCode == 0 {
		t.Errorf("AgentStatus fabricated a successful exit: %+v", st)
	}

	if err := d.StopSandbox(context.Background(), ref); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

// TestACrashBetweenProxyAndAgentCreateIsReclaimed: wardynd dies after the proxy pod exists and
// before the agent pod does, so no rollback runs. The proxy pod already carries its deadline,
// and the orphan sweep reclaims it with the Secret and both NetworkPolicies.
func TestACrashBetweenProxyAndAgentCreateIsReclaimed(t *testing.T) {
	d, cs := newTestDriver(t, Config{RunMaxAge: time.Hour})
	installProxyIPReactor(t, cs, "10.244.0.9")
	crashed := true
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok || !crashed {
			return false, nil, nil
		}
		if pod, ok := ca.GetObject().(*corev1.Pod); ok && strings.HasPrefix(pod.Name, "wardyn-agent-") {
			return true, nil, errors.New("wardynd died before creating the agent pod")
		}
		return false, nil, nil
	})
	// The rollback CreateSandbox runs on a failed create would reclaim everything; a crash runs none.
	cs.PrependReactor("delete-collection", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		if crashed {
			return true, nil, errors.New("wardynd is gone")
		}
		return false, nil, nil
	})

	spec := testSandboxSpec()
	spec.SecretEnv = map[string]string{"GIT_TOKEN": "ghp_secret"}
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox succeeded though the agent pod create failed")
	}
	proxy := getPod(t, cs, proxyPodName(spec.RunID))
	if proxy.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the stranded proxy pod has no activeDeadlineSeconds, so nothing but the sweep would ever end it")
	}

	crashed = false
	agePodsOfRun(t, cs, spec.RunID, time.Hour)
	ageRunSecretsAndNetPols(t, cs, spec.RunID, time.Hour)
	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil || swept != 1 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 1, nil", swept, err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}
