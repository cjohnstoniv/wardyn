// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// unschedulablePod is the retry's agent pod in #1182: Pending, with the
// scheduler saying why in PodScheduled.
func unschedulablePod(name string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: "0/1 nodes are available: 1 Insufficient cpu.",
			}},
		},
	}
}

// TestWaitContainerRunningThrottledDeadlineNamesPodReason: x/time/rate refuses
// a Wait whose token would land after the deadline, with an error that does
// not wrap context.DeadlineExceeded. Passed through, that refusal became the
// run's whole failure ("client rate limiter Wait returned an error") and the
// pod's own reason was dropped. Counted as "not yet", the wait ends on its
// deadline and names the pod's reason.
//
// Red on the unfixed tree: the first throttled Get ends the poll with the
// limiter's text, and the enrichment never runs.
func TestWaitContainerRunningThrottledDeadlineNamesPodReason(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	pod := unschedulablePod("wardyn-agent-throttled")
	var gets atomic.Int32
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) == 1 {
			return true, pod, nil
		}
		// client-go's own wrapping of the refusal (rest.Request.tryThrottleWithInfo).
		return true, nil, fmt.Errorf("client rate limiter Wait returned an error: %w",
			errors.New("rate: Wait(n=1) would exceed context deadline"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := d.waitContainerRunning(ctx, pod.Name, mainContainerName, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout (context.DeadlineExceeded)", err)
	}
	if !strings.Contains(err.Error(), "Unschedulable") {
		t.Fatalf("err = %q, want the pod's own reason (Unschedulable …)", err)
	}
}

// TestTwoConcurrentStartsKeepThePodReason is #1182's shape end to end on a
// real clientset: two agent pods waiting at once, each poller asking every
// k8sPollInterval, against the rate limit New actually builds. On client-go's
// 5/10 default the two pollers drain the burst in about 2s and the last Gets
// are refused; with the fixed limit (and a refusal counted as "not yet") both
// waits end on their deadline and name the pod's reason.
func TestTwoConcurrentStartsKeepThePodReason(t *testing.T) {
	pod := unschedulablePod("wardyn-agent-pending")
	body, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	apiserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"+testNamespace+"/pods/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer apiserver.Close()

	cs, err := newClientset(&rest.Config{Host: apiserver.URL})
	if err != nil {
		t.Fatalf("newClientset: %v", err)
	}
	// The limit is what New ships, and it is ONE limiter for every API group,
	// not client-go's per-group 5/10 bucket.
	lim := cs.CoreV1().RESTClient().GetRateLimiter()
	if lim == nil || lim.QPS() != clientQPS || cs.AppsV1().RESTClient().GetRateLimiter() != lim {
		t.Errorf("clientset rate limiter = %v, want one shared %v QPS limiter across API groups", lim, clientQPS)
	}
	d := &Driver{clientset: cs, cfg: Config{Namespace: testNamespace}}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.waitContainerRunning(ctx, pod.Name, mainContainerName, nil)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Unschedulable") {
			t.Errorf("start %d: err = %v, want a timeout naming the pod's reason (Unschedulable …)", i, err)
		}
	}
}
