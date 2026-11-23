// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// withPodWatch turns the pod watch on for a newTestDriver driver. It starts no watch: each test calls
// start where its setup says, as New does for every configured namespace.
func withPodWatch(t *testing.T, d *Driver, cs *fake.Clientset) *podWatch {
	t.Helper()
	d.podWatch = newPodWatch(cs)
	t.Cleanup(d.podWatch.close)
	return d.podWatch
}

// podActions returns the recorded calls of verb on pods (no subresource), in order.
func podActions(cs *fake.Clientset, verb string) []clienttesting.Action {
	var out []clienttesting.Action
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "pods" && a.GetSubresource() == "" {
			out = append(out, a)
		}
	}
	return out
}

// awaitPodActions waits up to within for at least n recorded calls of verb on pods and returns them.
func awaitPodActions(t *testing.T, cs *fake.Clientset, verb string, n int, within time.Duration) []clienttesting.Action {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := podActions(cs, verb); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s calls = %d after %s, want at least %d", verb, len(podActions(cs, verb)), within, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// watchRestrictions reads a recorded pod Watch call's selector and resourceVersion.
func watchRestrictions(t *testing.T, a clienttesting.Action) clienttesting.WatchRestrictions {
	t.Helper()
	w, ok := a.(clienttesting.WatchAction)
	if !ok {
		t.Fatalf("action %T is not a watch action", a)
	}
	return w.GetWatchRestrictions()
}

// awaitRing fails the test unless b rings within the bound.
func awaitRing(t *testing.T, b *podBell, within time.Duration, what string) {
	t.Helper()
	select {
	case <-b.ring:
	case <-time.After(within):
		t.Fatalf("%s: no ring within %s", what, within)
	}
}

// drainRing empties b's pending ring, if it has one.
func drainRing(b *podBell) {
	select {
	case <-b.ring:
	default:
	}
}

// fakeWatches answers every pod Watch on cs with a new watch.NewFake, handed to the test in call order.
func fakeWatches(cs *fake.Clientset) <-chan *watch.FakeWatcher {
	ch := make(chan *watch.FakeWatcher, 64)
	cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFake()
		select {
		case ch <- fw:
		default:
		}
		return true, fw, nil
	})
	return ch
}

func nextWatcher(t *testing.T, ch <-chan *watch.FakeWatcher, within time.Duration) *watch.FakeWatcher {
	t.Helper()
	select {
	case fw := <-ch:
		return fw
	case <-time.After(within):
		t.Fatalf("no pod Watch call within %s", within)
		return nil
	}
}

func managedPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{labelManaged: "true"}}}
}

// sleepTook runs b.sleep and returns how long it took.
func sleepTook(t *testing.T, b *podBell, fallback, resync time.Duration) time.Duration {
	t.Helper()
	begin := time.Now()
	if err := b.sleep(context.Background(), fallback, resync); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	return time.Since(begin)
}

func TestPodBellRingsOnlyTheWaitWhosePodChanged(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		if _, err := cs.CoreV1().Pods(testNamespace).Create(ctx, managedPod(name), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", name, err)
		}
	}
	ba, bb := d.podBell(testNamespace, "a"), d.podBell(testNamespace, "b")
	defer ba.stop()
	defer bb.stop()
	w.start(testNamespace)
	acts := awaitPodActions(t, cs, "watch", 1, time.Second)
	// The fake replays the namespace as ADDED events first.
	awaitRing(t, ba, time.Second, "replayed a")
	awaitRing(t, bb, time.Second, "replayed b")
	drainRing(ba)
	drainRing(bb)

	pod, err := cs.CoreV1().Pods(testNamespace).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	if _, err := cs.CoreV1().Pods(testNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update a: %v", err)
	}
	awaitRing(t, ba, time.Second, "a after its update")
	select {
	case <-bb.ring:
		t.Fatal("b rang for a change of a")
	case <-time.After(300 * time.Millisecond):
	}
	if got, want := watchRestrictions(t, acts[0]).Labels.String(), labelManaged+"=true"; got != want {
		t.Errorf("watch label selector = %q, want %q", got, want)
	}
}

func TestPodWatchForbiddenPollsOnTheFallbackInterval(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no watch verb"))
	})
	b := d.podBell(testNamespace, "a")
	defer b.stop()
	begin := time.Now()
	w.start(testNamespace)
	awaitPodActions(t, cs, "watch", 1, time.Second)
	if took := sleepTook(t, b, 20*time.Millisecond, time.Hour); took > 500*time.Millisecond {
		t.Errorf("sleep with the watch refused took %s, want the 20ms fallback", took)
	}
	time.Sleep(time.Until(begin.Add(1500 * time.Millisecond)))
	if n := len(podActions(cs, "watch")); n != 1 {
		t.Errorf("pod Watch calls within 1s of a refusal = %d, want 1 (the retry is %s)", n, podWatchForbiddenRetry)
	}
}

func TestPodWatchEndRingsEveryWaitAndResumesFromLastResourceVersion(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	watchers := fakeWatches(cs)
	ba, bb := d.podBell(testNamespace, "a"), d.podBell(testNamespace, "b")
	defer ba.stop()
	defer bb.stop()
	w.start(testNamespace)
	fw := nextWatcher(t, watchers, time.Second)
	pod := managedPod("a")
	pod.ResourceVersion = "7"
	fw.Modify(pod)
	fw.Stop()
	awaitRing(t, ba, time.Second, "a")
	awaitRing(t, bb, time.Second, "b at the watch's end")
	acts := awaitPodActions(t, cs, "watch", 2, 3*time.Second)
	if rv := watchRestrictions(t, acts[0]).ResourceVersion; rv != "" {
		t.Errorf("first watch resourceVersion = %q, want \"\"", rv)
	}
	if rv := watchRestrictions(t, acts[1]).ResourceVersion; rv != "7" {
		t.Errorf("resumed watch resourceVersion = %q, want \"7\"", rv)
	}
}

func TestPodWatchExpiredResourceVersionRestartsFromNow(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	watchers := fakeWatches(cs)
	w.start(testNamespace)
	fw := nextWatcher(t, watchers, time.Second)
	pod := managedPod("a")
	pod.ResourceVersion = "7"
	fw.Modify(pod)
	fw.Stop()
	// The 410 answers a resumed watch: the at-once end above backs off 1 s, then the watch resumes at "7".
	fw2 := nextWatcher(t, watchers, 3*time.Second)
	acts := awaitPodActions(t, cs, "watch", 2, time.Second)
	if rv := watchRestrictions(t, acts[1]).ResourceVersion; rv != "7" {
		t.Fatalf("resumed watch resourceVersion = %q, want \"7\"", rv)
	}
	fw2.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired})
	acts = awaitPodActions(t, cs, "watch", 3, 500*time.Millisecond)
	if rv := watchRestrictions(t, acts[2]).ResourceVersion; rv != "" {
		t.Errorf("watch after 410 Expired resourceVersion = %q, want \"\" (restart from now)", rv)
	}
}

func TestPodWatchExpiredWithNoResourceVersionBacksOff(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewResourceExpired("too old resource version")
	})
	begin := time.Now()
	w.start(testNamespace)
	awaitPodActions(t, cs, "watch", 1, time.Second)
	time.Sleep(time.Until(begin.Add(700 * time.Millisecond)))
	if n := len(podActions(cs, "watch")); n > 2 {
		t.Errorf("pod Watch calls in 700ms of 410 answers to an empty resourceVersion = %d, want at most 2 (backed off)", n)
	}
}

func TestPodWatchBookmarkMovesResourceVersionAndRingsNothing(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	watchers := fakeWatches(cs)
	b := d.podBell(testNamespace, "a")
	defer b.stop()
	w.start(testNamespace)
	fw := nextWatcher(t, watchers, time.Second)
	mark := managedPod("a")
	mark.ResourceVersion = "9"
	fw.Action(watch.Bookmark, mark)
	select {
	case <-b.ring:
		t.Fatal("a BOOKMARK rang a wait")
	case <-time.After(300 * time.Millisecond):
	}
	fw.Stop()
	acts := awaitPodActions(t, cs, "watch", 2, 3*time.Second)
	if rv := watchRestrictions(t, acts[1]).ResourceVersion; rv != "9" {
		t.Errorf("watch after a BOOKMARK resourceVersion = %q, want \"9\"", rv)
	}
}

func TestPodWatchShortStreamBacksOff(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFake()
		fw.Stop()
		return true, fw, nil
	})
	begin := time.Now()
	w.start(testNamespace)
	awaitPodActions(t, cs, "watch", 1, time.Second)
	time.Sleep(time.Until(begin.Add(700 * time.Millisecond)))
	if n := len(podActions(cs, "watch")); n > 2 {
		t.Errorf("pod Watch calls in 700ms of streams that end at once = %d, want at most 2 (backed off)", n)
	}
}

func TestPodWatchNilBellPolls(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	if d.podWatch != nil {
		t.Fatal("newWithClient set a pod watch; want nil, so a driver it builds polls")
	}
	b := d.podBell(testNamespace, "a")
	if took := sleepTook(t, b, 20*time.Millisecond, time.Hour); took > 500*time.Millisecond {
		t.Errorf("nil-watch sleep took %s, want the 20ms fallback, never the resync", took)
	}
	if took := sleepTook(t, b, 100*time.Millisecond, time.Nanosecond); took < 100*time.Millisecond {
		t.Errorf("nil-watch sleep took %s, want the 100ms fallback, never the resync", took)
	}
	b.stop()
	b.stop()
}

func TestPodWatchEventIsNeverReadAsPodState(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	watchers := fakeWatches(cs)
	const name = "wardyn-proxy-event"
	pending := managedPod(name)
	pending.Status.Phase = corev1.PodPending
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pending, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create proxy pod: %v", err)
	}
	w.start(testNamespace)
	fw := nextWatcher(t, watchers, time.Second)

	type result struct {
		ip  string
		err error
	}
	done := make(chan result, 1)
	gets := len(podActions(cs, "get"))
	go func() {
		ip, err := d.waitPodIP(context.Background(), d.newStartClock(), testNamespace, name, nil)
		done <- result{ip, err}
	}()
	// The wait's bell is subscribed before its first read.
	awaitPodActions(t, cs, "get", gets+1, time.Second)

	ready := managedPod(name)
	ready.Status = proxyReadyStatus()
	ready.Status.PodIP = "10.9.9.9"
	fw.Modify(ready)
	select {
	case r := <-done:
		t.Fatalf("waitPodIP = %q, %v on a watch event alone; want it still waiting on the pod it reads", r.ip, r.err)
	case <-time.After(300 * time.Millisecond):
	}

	setPodStatus(t, cs, testNamespace, name, func(st *corev1.PodStatus) {
		*st = proxyReadyStatus()
		st.PodIP = "10.1.1.1"
	})
	fw.Modify(ready)
	select {
	case r := <-done:
		if r.err != nil || r.ip != "10.1.1.1" {
			t.Fatalf("waitPodIP = %q, %v; want the IP the pod read gives, 10.1.1.1", r.ip, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitPodIP did not return after the pod became ready")
	}
}

func TestWaitPodsGoneDecidesOnTheListNotTheDeleteEvent(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	watchers := fakeWatches(cs)
	runID := uuid.New()
	ref := createAgentPodFixture(t, cs, runID, "wardyn/agent-claude:local", nil)
	opts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}
	w.start(testNamespace)
	fw := nextWatcher(t, watchers, time.Second)

	done := make(chan error, 1)
	lists := len(podActions(cs, "list"))
	go func() { done <- d.waitPodsGone(context.Background(), testNamespace, opts, 30*time.Second) }()
	awaitPodActions(t, cs, "list", lists+1, time.Second)
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, ref)
	if err != nil {
		t.Fatalf("tracker get: %v", err)
	}
	fw.Delete(obj)
	select {
	case err := <-done:
		t.Fatalf("waitPodsGone = %v on a Deleted event while the List still finds the pod; want it still waiting", err)
	case <-time.After(300 * time.Millisecond):
	}

	// A timeout is still the error stopConfirming reads.
	if err := d.waitPodsGone(context.Background(), testNamespace, opts, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waitPodsGone timeout = %v, want context.DeadlineExceeded", err)
	}

	if err := cs.Tracker().Delete(podsGVR, testNamespace, ref); err != nil {
		t.Fatalf("tracker delete: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitPodsGone = %v once the pod is gone, want nil", err)
		}
	case <-time.After(podWatchResync + time.Second):
		t.Fatalf("waitPodsGone did not return within %s of the pod's delete", podWatchResync+time.Second)
	}
}

func TestExecWaitSeesExitOnTheWatchEvent(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  execContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
	})
	w.start(testNamespace)
	awaitPodActions(t, cs, "watch", 1, time.Second)
	getsBefore := len(podActions(cs, "get"))

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(200 * time.Millisecond)
		obj, err := cs.Tracker().Get(podsGVR, testNamespace, ref)
		if err != nil {
			t.Errorf("tracker get: %v", err)
			return
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  execContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
		}}
		if _, err := cs.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			t.Errorf("update status: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	begin := time.Now()
	code, err := d.Wait(ctx, ref)
	took := time.Since(begin)
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
	}
	if took >= execWaitPollInterval {
		t.Errorf("Wait took %s, want the exit seen on the watch event (resync is %s)", took, podWatchLongResync)
	}
	if gets := len(podActions(cs, "get")) - getsBefore; gets > 4 {
		t.Errorf("pod gets during Wait = %d, want at most 4 (one per read: first, replay, change)", gets)
	}
}

func TestPodWatchCloseEndsEveryNamespaceWatch(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	w := withPodWatch(t, d, cs)
	const other = testNamespace + "-2"
	b1, b2 := d.podBell(testNamespace, "a"), d.podBell(other, "a")
	defer b1.stop()
	defer b2.stop()
	w.start(testNamespace)
	w.start(other)
	awaitPodActions(t, cs, "watch", 2, time.Second)

	closed := make(chan struct{})
	go func() {
		w.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not return")
	}
	for _, b := range []*podBell{b1, b2} {
		drainRing(b)
		if b.ns.live.Load() {
			t.Error("a namespace watch is still live after close")
		}
		if took := sleepTook(t, b, 20*time.Millisecond, time.Hour); took > 500*time.Millisecond {
			t.Errorf("sleep after close took %s, want the 20ms fallback", took)
		}
	}

	n := len(podActions(cs, "watch"))
	const late = testNamespace + "-3"
	w.start(late)
	w.start(testNamespace)
	lb := w.bell(late, func(*corev1.Pod) bool { return true })
	defer lb.stop()
	if took := sleepTook(t, lb, 20*time.Millisecond, time.Hour); took > 500*time.Millisecond {
		t.Errorf("a bell made after close slept %s, want the 20ms fallback", took)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(podActions(cs, "watch")); got != n {
		t.Errorf("pod Watch calls after close = %d, want %d (start after close starts nothing)", got, n)
	}
}

func TestPodWatchRealClientForbiddenFallsBack(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/"+testNamespace+"/pods" && r.URL.Query().Get("watch") == "true" {
			calls.Add(1)
			mu.Lock()
			query = r.URL.Query()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"pods is forbidden"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cs, err := newClientset(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("newClientset: %v", err)
	}
	w := newPodWatch(cs)
	t.Cleanup(w.close) // runs before srv.Close
	b := w.bell(testNamespace, func(*corev1.Pod) bool { return true })
	defer b.stop()
	w.start(testNamespace)

	deadline := time.Now().Add(time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no watch request within 1s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	q := query
	mu.Unlock()
	for k, want := range map[string]string{"watch": "true", "labelSelector": labelManaged + "=true", "timeoutSeconds": "300", "allowWatchBookmarks": "true"} {
		if got := q.Get(k); got != want {
			t.Errorf("watch query %s = %q, want %q", k, got, want)
		}
	}
	if took := sleepTook(t, b, 20*time.Millisecond, time.Hour); took > 500*time.Millisecond {
		t.Errorf("sleep with the watch refused took %s, want the 20ms fallback", took)
	}
	if b.ns.live.Load() {
		t.Error("watch marked live after a 403")
	}
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("watch requests = %d, want 1 (the retry after a refusal is %s)", n, podWatchForbiddenRetry)
	}
}
