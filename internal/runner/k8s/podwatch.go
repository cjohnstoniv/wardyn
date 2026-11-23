// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// The pod watch is a doorbell for the pod waits, never a source of truth. One watch per runs
// namespace, on Wardyn's own pods, rings each wait whose pod changed; the wait then reads the pod (or
// lists the run's pods) directly, as it does on its timer. So every decision still rests on a direct
// read: the bracket's reads (identity.go's read contract), Wait's restart-safe read and teardown's H3
// proof. A missed event only delays a wait until its resync.
//
// Without `watch` on pods (a Role written by hand under k8s.rbac.create=false, or rbac.create=false
// on the runner chart) the watch is refused and every wait polls on its own interval, as it always
// did. The refusal is retried every podWatchForbiddenRetry, so a Role fixed later takes effect
// without a restart.
const (
	podWatchResync         = 2 * time.Second  // bounded waits (start, teardown, canary) while the watch is live
	podWatchLongResync     = 30 * time.Second // Wait and the exec log follower, which last as long as the run
	podWatchForbiddenRetry = 10 * time.Minute
	podWatchBackoffMax     = 30 * time.Second
	podWatchTimeoutSeconds = int64(300) // the apiserver ends each watch then; it resumes from the last resourceVersion
)

// errPodWatchEndedAtOnce is a watch that ended cleanly within a second of opening: retried with
// back-off, so an apiserver or proxy that ends every watch at once cannot spin the loop.
var errPodWatchEndedAtOnce = errors.New("pod watch ended at once")

// podWatch holds one namespace watch per runs namespace. New starts one for each configured namespace.
type podWatch struct {
	cs     kubernetes.Interface
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu  sync.Mutex
	nss map[string]*nsPodWatch
}

// nsPodWatch is one namespace's watch: whether it is live, and the waits it rings.
type nsPodWatch struct {
	started bool // set by start, under podWatch.mu
	live    atomic.Bool
	mu      sync.Mutex
	subs    map[*podBell]struct{}
}

// podBell is one wait's subscription. ring receives when a pod match accepts changes, and when the
// watch ends, since a change may have been missed then.
type podBell struct {
	ns    *nsPodWatch
	match func(*corev1.Pod) bool
	ring  chan struct{}
}

func newPodWatch(cs kubernetes.Interface) *podWatch {
	ctx, cancel := context.WithCancel(context.Background())
	return &podWatch{cs: cs, ctx: ctx, cancel: cancel, nss: map[string]*nsPodWatch{}}
}

// close ends every namespace watch and waits for each. Nil-safe. A bell still open then polls.
func (w *podWatch) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
}

// entry returns ns's watch, making it if need be. w.mu must be held.
func (w *podWatch) entry(ns string) *nsPodWatch {
	nw, ok := w.nss[ns]
	if !ok {
		nw = &nsPodWatch{subs: map[*podBell]struct{}{}}
		w.nss[ns] = nw
	}
	return nw
}

// start opens ns's watch, once. New calls it for each configured namespace, so the boot log says
// whether that namespace's waits ring or poll. After close it does nothing.
func (w *podWatch) start(ns string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() != nil {
		return
	}
	nw := w.entry(ns)
	if nw.started {
		return
	}
	nw.started = true
	w.wg.Add(1)
	go w.run(ns, nw)
}

// bell subscribes a wait to changes of the pods match accepts in ns. It starts no watch: a bell in a
// namespace whose watch never started, or after close, only polls, as does every bell of a nil
// podWatch (a driver newWithClient built, as unit tests do). The wait must call stop when it ends.
func (w *podWatch) bell(ns string, match func(*corev1.Pod) bool) *podBell {
	b := &podBell{match: match, ring: make(chan struct{}, 1)}
	if w == nil {
		return b
	}
	w.mu.Lock()
	nw := w.entry(ns)
	w.mu.Unlock()
	nw.mu.Lock()
	nw.subs[b] = struct{}{}
	nw.mu.Unlock()
	b.ns = nw
	return b
}

// podBell rings when the pod named name in ns changes.
func (d *Driver) podBell(ns, name string) *podBell {
	return d.podWatch.bell(ns, func(p *corev1.Pod) bool { return p.Name == name })
}

// stop unsubscribes the wait. Calling it again does nothing.
func (b *podBell) stop() {
	if b.ns == nil {
		return
	}
	b.ns.mu.Lock()
	delete(b.ns.subs, b)
	b.ns.mu.Unlock()
}

// sleep returns when ctx ends (with ctx.Err()), the bell rings, or the timer runs out: resync while
// the namespace's watch is live, fallback (the wait's own poll interval) otherwise.
func (b *podBell) sleep(ctx context.Context, fallback, resync time.Duration) error {
	d := fallback
	if b.ns != nil && b.ns.live.Load() {
		d = resync
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ring:
		return nil
	case <-t.C:
		return nil
	}
}

// pollUntil is wait.PollUntilContextTimeout(ctx, interval, timeout, true, cond) that also wakes on
// b: cond runs at once and after each wake, and its error, its true, or the end of ctx (the timeout
// gives context.DeadlineExceeded) ends the wait. ctx is checked again after each wake, so a ring
// that races the deadline never runs cond on a dead context.
func pollUntil(ctx context.Context, b *podBell, interval, timeout time.Duration, cond func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if done, err := cond(ctx); err != nil || done {
			return err
		}
		if err := b.sleep(ctx, interval, podWatchResync); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// ring rings every wait whose pod p is, or every wait when p is nil. It never blocks: one pending
// ring is enough, since the wait re-reads everything it needs.
func (nw *nsPodWatch) ring(p *corev1.Pod) {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	for b := range nw.subs {
		if p == nil || b.match(p) {
			select {
			case b.ring <- struct{}{}:
			default:
			}
		}
	}
}

// run keeps ns's watch open until close. An end, an error or a refusal marks it not live and rings
// every wait, which then polls on its own interval until the watch is open again.
func (w *podWatch) run(ns string, nw *nsPodWatch) {
	defer w.wg.Done()
	rv, backoff, refused, announced := "", time.Second, false, false
	for {
		timeout := podWatchTimeoutSeconds
		opened := time.Now()
		asked := rv
		wi, err := w.cs.CoreV1().Pods(ns).Watch(w.ctx, metav1.ListOptions{LabelSelector: labelManaged + "=true", ResourceVersion: rv, TimeoutSeconds: &timeout, AllowWatchBookmarks: true})
		if err == nil {
			nw.live.Store(true)
			refused = false
			if !announced {
				announced = true
				slog.Info("wardynd: k8s substrate: pod waits ring on a pod watch", slog.String("namespace", ns))
			}
			rv, err = drainPodWatch(w.ctx, wi, nw, rv)
			wi.Stop()
			nw.live.Store(false)
			nw.ring(nil)
			if err == nil && time.Since(opened) < time.Second {
				err = errPodWatchEndedAtOnce
			}
			if err == nil {
				backoff = time.Second
			}
		}
		var pause time.Duration
		switch {
		case w.ctx.Err() != nil:
			return
		case err == nil:
		case apierrors.IsForbidden(err):
			if !refused {
				refused = true
				slog.Warn("wardynd: k8s substrate: the runs Role grants no watch on pods here, so pod waits poll; add watch to its pods rule",
					slog.String("namespace", ns), slog.Any("err", err))
			}
			pause = podWatchForbiddenRetry
		case apierrors.IsResourceExpired(err) || apierrors.IsGone(err):
			if asked == "" { // a 410 to a watch that asked for no resourceVersion is never free
				pause, backoff = backoff, min(backoff*2, podWatchBackoffMax)
			}
			rv = ""
		default:
			pause, backoff = backoff, min(backoff*2, podWatchBackoffMax)
		}
		if pause > 0 {
			t := time.NewTimer(pause)
			select {
			case <-w.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// drainPodWatch rings the waits for each event's pod until the watch ends. A BOOKMARK only moves the
// resourceVersion on, so a resumed label-selected watch is not answered 410 on a busy namespace; it
// rings nothing. It returns the last resourceVersion seen and the error that ended it: nil for a
// plain end, the server's status for an ERROR event (410 Expired among them), ctx.Err() at close.
func drainPodWatch(ctx context.Context, wi watch.Interface, nw *nsPodWatch, rv string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return rv, ctx.Err()
		case ev, ok := <-wi.ResultChan():
			if !ok {
				return rv, nil
			}
			if ev.Type == watch.Error {
				return rv, apierrors.FromObject(ev.Object)
			}
			if p, ok := ev.Object.(*corev1.Pod); ok {
				rv = p.ResourceVersion
				if ev.Type != watch.Bookmark {
					nw.ring(p)
				}
			}
		}
	}
}
