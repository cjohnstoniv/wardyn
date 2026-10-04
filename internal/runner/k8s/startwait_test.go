// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func unschedulableStatus(msg string) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: msg,
	}}}
}

func proxyReadyStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
		Name: proxyContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
}

func agentRunningStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: mainContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
}

// podByAge serves podName with a status that depends on how long ago the reactor was installed.
func podByAge(cs interface {
	PrependReactor(verb, resource string, reaction clienttesting.ReactionFunc)
}, podName string, status func(age time.Duration) corev1.PodStatus) {
	begin := time.Now()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() != podName {
			return false, nil, nil
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: action.GetNamespace()},
			Status:     status(time.Since(begin)),
		}, nil
	})
}

func fastPoll(t *testing.T) {
	t.Helper()
	prev := capacityPollMax
	capacityPollMax = 50 * time.Millisecond
	t.Cleanup(func() { capacityPollMax = prev })
}

// The proxy pod stays unplaceable for several multiples of the start timeout, then lands: the run
// starts. Before this change the proxy failed at its own 90s IP bound and the whole run with it.
func TestWaitPodIP_UnplaceableProxyWaitsForRoomThenStarts(t *testing.T) {
	fastPoll(t)
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond, CapacityWait: 5 * time.Second})
	const podName = "wardyn-proxy-room"
	podByAge(cs, podName, func(age time.Duration) corev1.PodStatus {
		if age < 1500*time.Millisecond {
			return unschedulableStatus("0/1 nodes are available: 1 Insufficient cpu.")
		}
		return proxyReadyStatus()
	})
	ip, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	if err != nil || ip != "10.244.0.7" {
		t.Fatalf("waitPodIP = %q, %v; want the IP once room frees, well past StartTimeout", ip, err)
	}
}

// Only the agent contends: the proxy starts at once, the agent waits for room, and the shared clock
// still lets it start.
func TestWaitContainerRunning_AgentOnlyContention(t *testing.T) {
	fastPoll(t)
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond, CapacityWait: 5 * time.Second})
	const proxy, agent = "wardyn-proxy-a", "wardyn-agent-a"
	podByAge(cs, proxy, func(time.Duration) corev1.PodStatus { return proxyReadyStatus() })
	podByAge(cs, agent, func(age time.Duration) corev1.PodStatus {
		if age < 1200*time.Millisecond {
			return unschedulableStatus("0/1 nodes are available: 1 Insufficient memory.")
		}
		return agentRunningStatus()
	})
	clock := d.newStartClock()
	if _, err := d.waitPodIP(context.Background(), clock, proxy, nil); err != nil {
		t.Fatalf("proxy wait: %v", err)
	}
	if err := d.waitContainerRunning(context.Background(), clock, agent, mainContainerName, nil); err != nil {
		t.Fatalf("agent wait = %v; want the start once room frees", err)
	}
}

// With the capacity wait off (the default of a Config left zero, and WARDYN_SANDBOX_CAPACITY_WAIT=0),
// an unplaceable proxy fails at the start timeout, the proxy included, naming the scheduler's reason.
func TestWaitPodIP_CapacityWaitOffFailsAtStartTimeout(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: 400 * time.Millisecond})
	const podName = "wardyn-proxy-off"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus {
		return unschedulableStatus("0/1 nodes are available: 1 Insufficient cpu.")
	})
	start := time.Now()
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Insufficient cpu") {
		t.Fatalf("err = %v; want a deadline error carrying the scheduler's reason", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("failed after %s; want about the 400ms start timeout", elapsed)
	}
}

// A pod that has an IP but whose proxy container never gets Ready times out at the start bound, and the
// error names the container's own Waiting reason rather than the scheduler's.
func TestWaitPodIP_NotReadyFailsAtStartTimeoutNamingTheContainerReason(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond})
	const podName = "wardyn-proxy-creating"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}}
	})
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not start within") ||
		!strings.Contains(err.Error(), proxyContainerName+": ContainerCreating") {
		t.Fatalf("err = %v; want a start-timeout error naming %s: ContainerCreating", err, proxyContainerName)
	}
}

// The capacity wait itself has an end, and the error says it was room that never came.
func TestWaitPodIP_CapacityWaitExpires(t *testing.T) {
	fastPoll(t)
	d, cs := newTestDriver(t, Config{StartTimeout: 200 * time.Millisecond, CapacityWait: 600 * time.Millisecond})
	const podName = "wardyn-proxy-expire"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus { return unschedulableStatus("0/1 nodes are available") })
	start := time.Now()
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no machine had room") {
		t.Fatalf("err = %v; want the capacity-wait expiry", err)
	}
	if elapsed < 600*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("failed after %s; want about the 600ms capacity wait", elapsed)
	}
}

// Image-pull and other terminal errors still fail at once, deep inside a long capacity wait.
func TestWaitPodIP_TerminalErrorStillFailsFast(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: time.Minute, CapacityWait: time.Hour})
	const podName = "wardyn-proxy-pull"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
			Name:  proxyContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "simulated"}},
		}}}
	})
	start := time.Now()
	_, err := d.waitPodIP(context.Background(), d.newStartClock(), podName, nil)
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("err = %v; want the terminal pull error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s; a terminal error must not wait", time.Since(start))
	}
}

// Cancelling during the wait (a kill, a shutdown) ends it at once with the context's own error.
func TestWaitPodIP_CancellationDuringCapacityWait(t *testing.T) {
	fastPoll(t)
	d, cs := newTestDriver(t, Config{StartTimeout: time.Minute, CapacityWait: time.Hour})
	const podName = "wardyn-proxy-cancel"
	podByAge(cs, podName, func(time.Duration) corev1.PodStatus { return unschedulableStatus("0/1 nodes are available") })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := d.waitPodIP(ctx, d.newStartClock(), podName, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s to notice the cancel", time.Since(start))
	}
}

// startClock arithmetic on a fake clock: the budgets are absolute, a changed stuck message does not
// reset them, and time spent waiting for room does not eat the start budget.
func TestStartClock(t *testing.T) {
	t0 := time.Now()
	now := t0
	newClock := func(start, capacity time.Duration) *startClock {
		return &startClock{now: func() time.Time { return now }, begin: now, startTimeout: start, capacityWait: capacity}
	}
	pod := func(msg string) *corev1.Pod { return &corev1.Pod{Status: unschedulableStatus(msg)} }
	scheduled := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}

	t.Run("a changed stuck reason does not reset the capacity deadline", func(t *testing.T) {
		now = t0
		c := newClock(3*time.Minute, 15*time.Minute)
		c.observe(pod("1 Insufficient cpu"))
		now = now.Add(10 * time.Minute)
		c.observe(pod("1 Insufficient memory"))
		now = now.Add(4 * time.Minute)
		c.observe(pod("0/3 nodes are available"))
		if err := c.expired(); err != nil {
			t.Fatalf("14m waited of 15m: %v", err)
		}
		now = now.Add(2 * time.Minute)
		if err := c.expired(); err == nil || !strings.Contains(err.Error(), "no machine had room") {
			t.Fatalf("16m waited of 15m: err = %v; want the capacity expiry", err)
		}
	})
	t.Run("time waiting for room does not spend the start budget", func(t *testing.T) {
		now = t0
		c := newClock(3*time.Minute, 15*time.Minute)
		c.observe(pod("1 Insufficient cpu"))
		now = now.Add(10 * time.Minute)
		c.observe(scheduled)
		now = now.Add(2 * time.Minute)
		if err := c.expired(); err != nil {
			t.Fatalf("2m of start after a 10m wait: %v", err)
		}
		now = now.Add(2 * time.Minute)
		if err := c.expired(); err == nil || !strings.Contains(err.Error(), "did not start within") {
			t.Fatalf("4m of start: err = %v; want the start expiry", err)
		}
	})
	t.Run("capacity wait off counts the wait as ordinary start time", func(t *testing.T) {
		now = t0
		c := newClock(3*time.Minute, 0)
		c.observe(pod("1 Insufficient cpu"))
		now = now.Add(4 * time.Minute)
		if err := c.expired(); err == nil || !strings.Contains(err.Error(), "did not start within") {
			t.Fatalf("err = %v; want the start expiry", err)
		}
	})
}

// blockedAPIDriver is a Driver whose API server accepts a request and never answers it, with
// budgets far shorter than the pod read would otherwise block.
func blockedAPIDriver(t *testing.T) *Driver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(server.Close)
	client, err := newClientset(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return &Driver{clientset: client, cfg: Config{Namespace: "blocked", StartTimeout: 20 * time.Millisecond, CapacityWait: 20 * time.Millisecond}}
}

// A pod read the API server never answers still ends at the start budget: the poll bounds each
// read by what is left of it, so a hung connection cannot strand a run in STARTING. The bound is the
// remaining budget floored at one poll interval (200ms), hence the 2s allowance.
func TestWaitPodIP_BlockedGETFailsWithinBudgets(t *testing.T) {
	d := blockedAPIDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // frees a still-blocked read so the fake server can close
	done := make(chan error, 1)
	go func() { _, err := d.waitPodIP(ctx, d.newStartClock(), "blocked-proxy", nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not start within") {
			t.Fatalf("err = %v; want a deadline error naming the start timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pod read still blocked after 2s against 20ms budgets")
	}
}

func TestWaitContainerRunning_BlockedGETFailsWithinBudgets(t *testing.T) {
	d := blockedAPIDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.waitContainerRunning(ctx, d.newStartClock(), "blocked-agent", "agent", nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not start within") {
			t.Fatalf("err = %v; want a deadline error naming the start timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pod read still blocked after 2s against 20ms budgets")
	}
}

func TestStartClock_Remaining(t *testing.T) {
	t0 := time.Now()
	now := t0
	c := &startClock{now: func() time.Time { return now }, begin: t0, startTimeout: 3 * time.Minute, capacityWait: 15 * time.Minute}
	if got := c.remaining(); got != 3*time.Minute {
		t.Fatalf("fresh clock: remaining = %s, want the 3m start budget", got)
	}
	c.observe(&corev1.Pod{Status: unschedulableStatus("1 Insufficient cpu")})
	now = now.Add(14 * time.Minute)
	if got := c.remaining(); got != time.Minute {
		t.Fatalf("14m blocked of 15m: remaining = %s, want the 1m capacity budget (start budget untouched)", got)
	}
	c.observe(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}})
	now = now.Add(time.Hour)
	if got := c.remaining(); got != k8sPollInterval {
		t.Fatalf("spent clock: remaining = %s, want the %s floor", got, k8sPollInterval)
	}
}
