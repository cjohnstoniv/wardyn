// Copyright 2026 The Wardyn Authors
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// runnerKill is a runner-mode kill of the harness run (grace 0), with the teardown's waits set to wait.
func (h *runnerHarness) runnerKill(t *testing.T, ctx context.Context, wait time.Duration) error {
	t.Helper()
	prev := teardownWaitFor
	teardownWaitFor = func(*int64) time.Duration { return wait }
	t.Cleanup(func() { teardownWaitFor = prev })
	zero := int64(0)
	return h.d.teardownRunner(ctx, h.runID, &zero)
}

// agentPod creates the harness run's running agent pod on n1, under the stop-proof finalizer.
func (h *runnerHarness) agentPod(t *testing.T) {
	t.Helper()
	agent := h.proxyPod("n1")
	agent.Name, agent.UID = agentPodName(h.runID), "uid-agent-1"
	agent.Labels = wardynLabels(h.runID, componentAgent, map[string]string{runner.LabelRunnerID: testRunnerID})
	agent.Status.ContainerStatuses[0].ContainerID = "containerd://a1"
	if _, err := h.cs.CoreV1().Pods(testNamespace).Create(context.Background(), agent, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func noPoliciesDropped(t *testing.T, h *runnerHarness) {
	t.Helper()
	if n := countActions(h.cs, "delete-collection", "networkpolicies") + countActions(h.cs, "delete-collection", "secrets"); n != 0 {
		t.Fatalf("%d collections dropped before both pods were proven stopped and gone", n)
	}
}

// R2: the proxy is proven stopped, the agent was deleted and has no evidence yet, and no delete bound has
// passed (the clock stands still). The plain held answer whose only cause is "no evidence yet" is
// runner.ErrTeardownConfirming, and it is not the typed containment-pending answer.
func TestRunnerKillStillStoppingIsConfirming(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.agentPod(t)
	fixed := h.clock
	h.d.now = func() time.Time { return fixed }
	h.onDelete = func(p *corev1.Pod) {
		if p.Name == proxyPodName(h.runID) {
			terminate(p, "containerd://p1")
		}
	}
	err := h.runnerKill(t, context.Background(), 0)
	if !errors.Is(err, runner.ErrTeardownConfirming) || errors.As(err, new(*runner.ContainmentPendingError)) {
		t.Fatalf("kill with the agent still stopping = %v, want ErrTeardownConfirming and no typed hold", err)
	}
	if !strings.Contains(err.Error(), "held, NetworkPolicies kept") {
		t.Errorf("kill = %v, want the held answer kept in the message", err)
	}
	noPoliciesDropped(t, h)
}

// R3: the agent is gone, the proxy has no evidence: agent_only, typed as before and also confirming.
// A kill whose ctx is done keeps the plain typed answer.
func TestRunnerKillAgentOnlyIsConfirming(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	err := h.runnerKill(t, context.Background(), 25*time.Second)
	var pending *runner.ContainmentPendingError
	if !errors.Is(err, runner.ErrTeardownConfirming) || !errors.As(err, &pending) || pending.Kind != runner.ContainmentAgentOnly {
		t.Fatalf("agent_only kill = %v, want ErrTeardownConfirming that still carries the agent_only hold", err)
	}
	noPoliciesDropped(t, h)

	h2 := newRunnerHarness(t)
	h2.boundProxy(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h2.runnerKill(t, ctx, 25*time.Second); err == nil || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("agent_only kill on a done ctx = %v, want the plain answer", err)
	}
}

// R4: both delete bounds passed without evidence: pending_delete, the hold, keeps its plain typed answer.
func TestRunnerKillPendingDeleteIsNotConfirming(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.agentPod(t)
	err := h.runnerKill(t, context.Background(), 25*time.Second)
	var pending *runner.ContainmentPendingError
	if !errors.As(err, &pending) || pending.Kind != runner.ContainmentPendingDelete || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("pending_delete kill = %v, want the typed hold and not ErrTeardownConfirming", err)
	}
	noPoliciesDropped(t, h)
}

// R5: both pods are proven stopped, but the proxy's object stays (another finalizer) past the wait. Every
// pod left is marked for deletion: confirming. A labelled pod that is not marked keeps today's error.
func TestRunnerKillPodsNotYetGone(t *testing.T) {
	for name, stray := range map[string]bool{"all marked": false, "one not marked": true} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			pod := h.boundProxy(t)
			pod.Finalizers = append(pod.Finalizers, "example.com/hold")
			if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
				t.Fatal(err)
			}
			h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
			if stray {
				s := h.proxyPod("n1")
				s.Name, s.UID, s.Finalizers = "stray", "uid-stray", nil
				if _, err := h.cs.CoreV1().Pods(testNamespace).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			err := h.runnerKill(t, context.Background(), time.Second)
			if err == nil || !strings.Contains(err.Error(), "pods") {
				t.Fatalf("kill = %v, want the pods' wait to have run out", err)
			}
			if got := errors.Is(err, runner.ErrTeardownConfirming); got == stray {
				t.Fatalf("kill = %v: confirming %v, want %v", err, got, !stray)
			}
			noPoliciesDropped(t, h)
		})
	}
}

// R8 and a vanished pod: a read error, and a held pod that went without a witness (a stripped finalizer),
// keep their answers; neither is confirming.
func TestRunnerKillErrorsAreNotConfirming(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd is down"))
	})
	if err := h.runnerKill(t, context.Background(), 25*time.Second); err == nil || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("kill with a read error = %v, want a plain error", err)
	}
	noPoliciesDropped(t, h)

	h2 := newRunnerHarness(t)
	pod := h2.boundProxy(t)
	if err := h2.cs.Tracker().Delete(podsGVR, testNamespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	err := h2.runnerKill(t, context.Background(), 25*time.Second)
	if !errors.As(err, new(*runner.ContainmentPendingError)) || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("kill of a vanished pod = %v, want the typed hold and not ErrTeardownConfirming", err)
	}
	noPoliciesDropped(t, h2)
}

// A final verdict (a reused node name, a changed node record tuple, a different execution, the pod GC's
// mark, a closing anchor on another Node, a pod whose main containers never started with no causal node)
// is released only by a person's record or an operator's taint, so no later status read proves it: the
// agent_only hold it leaves keeps its plain typed answer and is not confirming.
func TestRunnerKillFinalVerdictsAreNotConfirming(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *runnerHarness){
		"node name reused": func(t *testing.T, h *runnerHarness) {
			h.evidence(t, func(ev *runEvidence) {
				ev.Reuse = append(ev.Reuse, runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7", InternalIP: "10.0.0.9"})
			})
		},
		"node record tuple changed": func(t *testing.T, h *runnerHarness) {
			h.evidence(t, func(ev *runEvidence) { ev.Record = &runner.NodeRecord{Tuple: runner.NodeTuple{RunID: h.runID}} })
		},
		"different execution": func(t *testing.T, h *runnerHarness) {
			h.evidence(t, func(ev *runEvidence) {
				ev.Pods[componentProxy].Containers[proxyContainerName].Latched = []string{"containerd://p2"}
			})
			h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
		},
		"pod GC mark": func(t *testing.T, h *runnerHarness) {
			h.onDelete = func(p *corev1.Pod) {
				terminate(p, "containerd://p1")
				p.Status.Phase = corev1.PodFailed
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reasonDeletionByPodGC})
			}
		},
		"closing anchor on another Node": func(t *testing.T, h *runnerHarness) {
			h.evidence(t, func(ev *runEvidence) {
				ev.Pods[componentProxy].Closing = &nodeHolder{Node: runner.NodeSample{Name: "n1", UID: "uid-n1-other", MachineID: "m1", SystemUUID: "s1", InternalIP: "10.0.0.5"}}
			})
			h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
		},
		"main never started, no causal node": func(t *testing.T, h *runnerHarness) {
			pod, err := h.cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(h.runID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
			if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
				t.Fatal(err)
			}
			h.evidence(t, func(ev *runEvidence) {
				pe := ev.Pods[componentProxy]
				pe.Assoc = assocU3
				delete(pe.Containers, proxyContainerName)
			})
			h.onDelete = func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionFalse})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.boundProxy(t)
			setup(t, h)
			err := h.runnerKill(t, context.Background(), 25*time.Second)
			var pending *runner.ContainmentPendingError
			if !errors.As(err, &pending) || pending.Kind != runner.ContainmentAgentOnly || errors.Is(err, runner.ErrTeardownConfirming) {
				t.Fatalf("kill = %v, want the agent_only hold and not ErrTeardownConfirming", err)
			}
			if !errors.As(err, new(unprovableError)) {
				t.Errorf("kill = %v, want the final verdict's unprovableError in the chain", err)
			}
			noPoliciesDropped(t, h)
		})
	}
}

// One pod still unknown and the other with a real error (a refused delete): the answer is plain, because
// not every cause is "no evidence yet".
func TestRunnerKillUnknownBesideARealErrorIsNotConfirming(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.agentPod(t)
	h.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.DeleteActionImpl).GetName() == agentPodName(h.runID) {
			return true, nil, apierrors.NewForbidden(podsGVR.GroupResource(), agentPodName(h.runID), errors.New("refused"))
		}
		return false, nil, nil
	})
	err := h.runnerKill(t, context.Background(), 25*time.Second)
	if err == nil || errors.Is(err, runner.ErrTeardownConfirming) || !errors.Is(err, errTerminationUnknown) {
		t.Fatalf("kill = %v, want a plain answer that still carries the proxy's unknown", err)
	}
	noPoliciesDropped(t, h)
}

// evidence edits the harness run's stored evidence.
func (h *runnerHarness) evidence(t *testing.T, edit func(*runEvidence)) {
	t.Helper()
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) { edit(ev); return true, nil }); err != nil {
		t.Fatal(err)
	}
}
