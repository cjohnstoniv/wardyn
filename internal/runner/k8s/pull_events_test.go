// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	pullTestPod   = "wardyn-agent-cold-pull"
	pullTestUID   = k8stypes.UID("11111111-2222-3333-4444-555555555555")
	pullTestImage = "wardyn/agent-aws-sso:0.8.0"
)

// pullTestPodAt is the agent pod as the kubelet reports it: Running once
// running, ContainerCreating before.
func pullTestPodAt(ns string, running bool) *corev1.Pod {
	phase, state := corev1.PodPending, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	if running {
		phase, state = corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pullTestPod, Namespace: ns, UID: pullTestUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: mainContainerName, Image: pullTestImage}}},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{{Name: mainContainerName, State: state}},
		},
	}
}

// kubeletEvent is one Event as the kubelet writes it about the agent container.
func kubeletEvent(reason, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: pullTestPod + "." + reason},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: pullTestPod, UID: pullTestUID,
			FieldPath: "spec.containers{" + mainContainerName + "}",
		},
		Reason:  reason,
		Message: message,
		Source:  corev1.EventSource{Component: "kubelet"},
	}
}

// scriptPod answers every Get of the test pod with ContainerCreating for
// `creating` reads, then Running, and returns the count of Gets answered.
func scriptPod(cs *fake.Clientset, creating int32) *atomic.Int32 {
	var gets atomic.Int32
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || ga.GetName() != pullTestPod {
			return false, nil, nil
		}
		return true, pullTestPodAt(action.GetNamespace(), gets.Add(1) > creating), nil
	})
	return &gets
}

// TestWaitContainerRunning_ReportsImagePullFromEvents is #807: on Kubernetes the
// kubelet reports a cold pull as ContainerCreating, so the sign-in door's
// "Downloading" step never lit. The pod's Events carry the Pulling fact; the
// wait must report it in the Docker driver's exact words, and drop back to
// ContainerCreating once the kubelet says Pulled.
func TestWaitContainerRunning_ReportsImagePullFromEvents(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	// 10 reads of ContainerCreating (~2s at the 200ms poll), then Running.
	const creating = 10
	gets := scriptPod(cs, creating)
	var lists atomic.Int32
	cs.PrependReactor("list", "events", func(action clienttesting.Action) (bool, runtime.Object, error) {
		evs := []corev1.Event{kubeletEvent("Pulling", `Pulling image "`+pullTestImage+`"`)}
		if lists.Add(1) > 1 {
			evs = append(evs, kubeletEvent("Pulled", "Successfully pulled image"))
		}
		return true, &corev1.EventList{Items: evs}, nil
	})

	var seen []string
	if err := d.waitContainerRunning(context.Background(), pullTestPod, mainContainerName, func(detail string) {
		seen = append(seen, detail)
	}); err != nil {
		t.Fatalf("waitContainerRunning: %v", err)
	}
	// Compared up to the Running tick: that tick makes its own, pre-existing
	// report (waitingReason's "pod: Pending" fallback), which #807 does not touch.
	want := []string{"image: Pulling: " + pullTestImage, "agent: ContainerCreating"}
	if len(seen) < len(want) || !slices.Equal(seen[:len(want)], want) {
		t.Fatalf("OnWaiting calls = %q, want %q — a pull the kubelet reported as an Event must light the download step", seen, want)
	}
	// Throttled: the Events read must not ride every ContainerCreating pod read.
	// Counted against those reads, not the wall clock: a loaded box stretches the
	// polls and so adds lists, but only a read with no throttle lists on every one.
	if n, reads := lists.Load(), min(gets.Load(), creating); n >= reads {
		t.Errorf("events listed %d times over %d ContainerCreating pod reads, want fewer (at most once a second)", n, reads)
	}
}

// TestWaitContainerRunning_EventsForbiddenKeepsThePodsReason is the fail-closed
// half: an operator-written Role (k8s.rbac.create=false) that predates the
// `events: list` grant answers 403. The create must go on, report exactly what it
// reported before #807, and stop asking after the first refusal.
func TestWaitContainerRunning_EventsForbiddenKeepsThePodsReason(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	scriptPod(cs, 10)
	var lists atomic.Int32
	cs.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", nil)
	})

	var seen []string
	if err := d.waitContainerRunning(context.Background(), pullTestPod, mainContainerName, func(detail string) {
		seen = append(seen, detail)
	}); err != nil {
		t.Fatalf("waitContainerRunning must not fail on an Events read error: %v", err)
	}
	if want := []string{"agent: ContainerCreating"}; len(seen) == 0 || !slices.Equal(seen[:1], want) || slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, "Pulling") }) {
		t.Fatalf("OnWaiting calls = %q, want %q", seen, want)
	}
	if n := lists.Load(); n != 1 {
		t.Errorf("events listed %d times after a 403, want exactly 1", n)
	}
}

// TestPullingDetail_NotContainerCreatingNeverLights pins the containerCreating
// guard in pullingDetail: once the container's own status has moved past
// ContainerCreating — here to ErrImagePull, a reason already more specific
// than "Pulling" — a stale Pulling Event left over from an earlier pull must
// not re-light the download step, and the guard must stop the read before it
// ever asks the apiserver.
func TestPullingDetail_NotContainerCreatingNeverLights(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	pod := pullTestPodAt(testNamespace, false)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
	}
	var lists atomic.Int32
	cs.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return true, &corev1.EventList{Items: []corev1.Event{kubeletEvent("Pulling", "")}}, nil
	})

	if got := d.pullingDetail(context.Background(), pod, mainContainerName, &pullWatch{}); got != "" {
		t.Fatalf("pullingDetail = %q, want \"\" — ErrImagePull is not ContainerCreating, so a stale Pulling Event must not light the step", got)
	}
	if n := lists.Load(); n != 0 {
		t.Errorf("events listed %d times for a non-ContainerCreating pod, want 0 — the containerCreating guard must short-circuit before the read", n)
	}
}

// TestPullingFromEvents pins what may and may not light the step. Every
// negative row is a way an Event could claim a pull that is not this pod's, or
// not happening now.
func TestPullingFromEvents(t *testing.T) {
	pod := pullTestPodAt(testNamespace, false)
	want := "image: Pulling: " + pullTestImage
	otherUID := kubeletEvent("Pulling", "")
	otherUID.InvolvedObject.UID = "99999999-0000-0000-0000-000000000000"
	otherContainer := kubeletEvent("Pulling", "")
	otherContainer.InvolvedObject.FieldPath = "spec.containers{proxy}"
	notKubelet := kubeletEvent("Pulling", "")
	notKubelet.Source.Component = "someone-else"
	reportedByKubelet := kubeletEvent("Pulling", "")
	reportedByKubelet.Source.Component = ""
	reportedByKubelet.ReportingController = "kubelet"
	forged := kubeletEvent("Pulling", `Pulling image "evil" <script>`)

	cases := []struct {
		name string
		evs  []corev1.Event
		want string
	}{
		{"no events", nil, ""},
		{"pulling", []corev1.Event{kubeletEvent("Pulling", "")}, want},
		{"events.k8s.io reporter", []corev1.Event{reportedByKubelet}, want},
		{"message never echoed", []corev1.Event{forged}, want},
		{"pulled after pulling", []corev1.Event{kubeletEvent("Pulling", ""), kubeletEvent("Pulled", "")}, ""},
		{"failed after pulling", []corev1.Event{kubeletEvent("Pulling", ""), kubeletEvent("Failed", "")}, ""},
		{"started, no pull event", []corev1.Event{kubeletEvent("Started", "")}, ""},
		{"a same-named earlier pod", []corev1.Event{otherUID}, ""},
		{"another container", []corev1.Event{otherContainer}, ""},
		{"not the kubelet", []corev1.Event{notKubelet}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pullingFromEvents(pod, mainContainerName, c.evs); got != c.want {
				t.Errorf("pullingFromEvents = %q, want %q", got, c.want)
			}
		})
	}

	noUID := pod.DeepCopy()
	noUID.UID = ""
	blank := kubeletEvent("Pulling", "")
	blank.InvolvedObject.UID = ""
	if got := pullingFromEvents(noUID, mainContainerName, []corev1.Event{blank}); got != "" {
		t.Errorf("a pod with no UID matched an Event: %q", got)
	}
}
