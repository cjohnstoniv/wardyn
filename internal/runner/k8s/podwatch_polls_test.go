// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// steadyGets starts the watch on testNamespace, runs wait on a goroutine, waits for its first pod read, and returns
// how many more reads it made over steady while the pod did not change.
func steadyGets(t *testing.T, cs *fake.Clientset, w *podWatch, steady time.Duration, wait func()) int {
	t.Helper()
	w.start(testNamespace)
	awaitPodActions(t, cs, "watch", 1, time.Second)
	before := len(podActions(cs, "get"))
	go wait()
	awaitPodActions(t, cs, "get", before+1, time.Second)
	time.Sleep(steady)
	return len(podActions(cs, "get")) - before - 1
}

// The image check reads its unchanged check pod once and then waits on the pod watch; the verdict comes on the
// change's event (#4124).
func TestImageCheckWaitsOnThePodWatch(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	pod := managedPod("wardyn-imagecheck-" + uuid.NewString()[:8])
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: imageCheckContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	verdict := make(chan string, 1)
	extra := steadyGets(t, cs, w, 1500*time.Millisecond, func() {
		code, _ := d.waitImageCheck(context.Background(), pod.Name)
		verdict <- code
	})
	if extra != 0 {
		t.Errorf("the image check read its unchanged pod %d more times in 1.5s, want 0 (it waits on the pod watch)", extra)
	}
	begin := time.Now()
	setPodStatus(t, cs, testNamespace, pod.Name, func(st *corev1.PodStatus) {
		st.ContainerStatuses = []corev1.ContainerStatus{{Name: imageCheckContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	})
	select {
	case code := <-verdict:
		if code != runner.ImageCheckPassed {
			t.Fatalf("verdict = %q, want %q", code, runner.ImageCheckPassed)
		}
		if took := time.Since(begin); took >= podWatchResync {
			t.Errorf("the verdict came %s after the pod changed, want it on the change's event (resync is %s)", took, podWatchResync)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no verdict 5s after the check pod finished")
	}
}

// The relay source check reads its unchanged probe pod once and then waits on the pod watch; the IP is seen on the
// change's event (#4124).
func TestRelayProbeIPWaitsOnThePodWatch(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	pod := managedPod("wardyn-probe-" + uuid.NewString()[:8])
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan string, 1)
	extra := steadyGets(t, cs, w, 1500*time.Millisecond, func() { d.watchProbeIP(ctx, testNamespace, pod.Name, seen) })
	if extra != 0 {
		t.Errorf("the relay source check read its unchanged probe pod %d more times in 1.5s, want 0", extra)
	}
	begin := time.Now()
	setPodStatus(t, cs, testNamespace, pod.Name, func(st *corev1.PodStatus) { st.PodIP = "10.1.2.3" })
	select {
	case ip := <-seen:
		if ip != "10.1.2.3" {
			t.Fatalf("seen %q, want 10.1.2.3", ip)
		}
		if took := time.Since(begin); took >= podWatchResync {
			t.Errorf("the IP was seen %s after the pod changed, want it on the change's event", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no IP 5s after the probe pod got one")
	}
}

// Output recovery reads the agent pod once while its exec container has not started and then waits on the pod
// watch; the log is read on the change's event (#4124).
func TestRecoverOutputWaitsOnThePodWatch(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	extra := steadyGets(t, cs, w, 2500*time.Millisecond, func() { done <- d.RecoverOutput(context.Background(), ref, &out) })
	if extra != 0 {
		t.Errorf("output recovery read its unchanged pod %d more times in 2.5s, want 0", extra)
	}
	begin := time.Now()
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: execContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RecoverOutput: %v", err)
		}
		if took := time.Since(begin); took >= execWaitPollInterval {
			t.Errorf("the log was read %s after the exec container started, want it on the change's event", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecoverOutput did not return 5s after the exec container finished")
	}
}
