// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A k8s runner that lists workload namespaces (WARDYN_RUNNERD_WORKLOAD_NAMESPACES, design T3 §2.5)
// watches WardynWorkload custom resources there and reports what it sees to the organisation over its
// session. The organisation decides what a resource means; the runner only reports it, and writes the
// organisation's answer back to the resource's status. Nothing here reads a Secret.

// workloadGVR is the custom resource the chart's CRD defines (crds/wardyn.dev_wardynworkloads.yaml).
var workloadGVR = schema.GroupVersionResource{Group: "wardyn.dev", Version: "v1alpha1", Resource: "wardynworkloads"}

const (
	// workloadResync is how often every namespace is reported in full, which also catches a delete
	// missed while runnerd was down and retries a generation the organisation refused.
	workloadResync = 10 * time.Minute
	// workloadSendTimeout bounds one event, so a stalled session cannot hold the informers.
	workloadSendTimeout = 30 * time.Second
)

// workloadPeer is the session's event sender, a *runnerwire.Peer.
type workloadPeer interface {
	Event(context.Context, runnerwire.Event) error
}

// workloadWatcher is one dynamic informer per listed namespace and the reports they drive.
type workloadWatcher struct {
	ctx    context.Context
	client dynamic.Interface
	resync time.Duration
	now    func() time.Time
	// names lists the namespaces in the order given; informers and synced are keyed by them and
	// never change once the watcher is built.
	names     []string
	informers map[string]cache.SharedIndexInformer
	// handlers holds each informer's handler registration, whose HasSynced is true only once the
	// informer has listed the namespace and the handler has seen everything that list held.
	handlers map[string]cache.ResourceEventHandlerRegistration
	// synced is set once a namespace's informer has listed it. Until then its events are ignored:
	// the full report that follows the sync sends everything the store holds.
	synced map[string]*atomic.Bool

	mu   sync.Mutex // guards peer
	peer workloadPeer
	// kick asks run for a full report now: the session came up, and anything seen while it was down
	// was dropped.
	kick chan struct{}
	// sendMu orders the events of one report, and keeps a live event from landing inside it.
	sendMu sync.Mutex
}

// newClusterWorkloadWatcher watches namespaces through runnerd's own ServiceAccount; with none it
// returns nil, which every method below that wires the watcher in treats as off.
func newClusterWorkloadWatcher(ctx context.Context, namespaces []string) (*workloadWatcher, error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("workload watcher: %w", err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("workload watcher: %w", err)
	}
	return newWorkloadWatcher(ctx, client, namespaces, workloadResync)
}

func newWorkloadWatcher(ctx context.Context, client dynamic.Interface, namespaces []string, resync time.Duration) (*workloadWatcher, error) {
	w := &workloadWatcher{ctx: ctx, client: client, resync: resync, now: time.Now, names: namespaces,
		informers: map[string]cache.SharedIndexInformer{}, handlers: map[string]cache.ResourceEventHandlerRegistration{}, synced: map[string]*atomic.Bool{}, kick: make(chan struct{}, 1)}
	// An absent CRD makes every informer 404 forever with boot green, so it refuses boot here like a
	// missing grant (CheckRunnerAccess).
	for _, ns := range namespaces {
		if _, err := client.Resource(workloadGVR).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("workload watcher: the WardynWorkload CRD (%s) is not installed: apply the chart's crds/wardyn.dev_wardynworkloads.yaml: %w", workloadGVR.GroupResource(), err)
			}
			return nil, fmt.Errorf("workload watcher: list %s in namespace %s: %w", workloadGVR.Resource, ns, err)
		}
	}
	for _, ns := range namespaces {
		inf := dynamicinformer.NewFilteredDynamicInformer(client, workloadGVR, ns, 0, cache.Indexers{}, nil).Informer()
		reg, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) { w.applied(ns, obj) },
			// A status write or a resync is an update that leaves metadata.generation alone. A new uid
			// is a delete and re-create the informer saw as one update, and starts at the same generation.
			UpdateFunc: func(old, cur any) {
				if workloadGeneration(old) != workloadGeneration(cur) || workloadUID(old) != workloadUID(cur) {
					w.applied(ns, cur)
				}
			},
			DeleteFunc: func(obj any) { w.deleted(ns, obj) },
		})
		if err != nil {
			return nil, fmt.Errorf("workload watcher: %w", err)
		}
		w.informers[ns], w.handlers[ns], w.synced[ns] = inf, reg, &atomic.Bool{}
	}
	return w, nil
}

func workloadGeneration(obj any) int64 {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GetGeneration()
	}
	return 0
}

func workloadUID(obj any) k8stypes.UID {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GetUID()
	}
	return ""
}

// attach wires the watcher into a runtime: it learns the session from the Online and StreamOffline
// hooks, and runs beside the runtime's own loop.
func (w *workloadWatcher) attach(rt runnerRuntime) runnerRuntime {
	if w == nil {
		return rt
	}
	online, offline, run := rt.hooks.Online, rt.hooks.StreamOffline, rt.run
	rt.hooks.Online = func(p *runnerwire.Peer, resumed bool) {
		online(p, resumed)
		w.setPeer(p)
	}
	rt.hooks.StreamOffline = func(p *runnerwire.Peer) {
		w.dropPeer(p)
		offline(p)
	}
	rt.run = func(ctx context.Context) error {
		go w.run(ctx)
		return run(ctx)
	}
	return rt
}

// statusFunc is the dispatcher's workload_status adapter; nil, which refuses the call, when off.
func (w *workloadWatcher) statusFunc() func(context.Context, runnerwire.WorkloadStatusArgs) error {
	if w == nil {
		return nil
	}
	return w.status
}

// setPeer makes p the session events go to and asks for a full report over it.
func (w *workloadWatcher) setPeer(p workloadPeer) {
	w.mu.Lock()
	w.peer = p
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// dropPeer forgets p, when it is still the session: events seen while offline are dropped, and the
// next session's full report carries them.
func (w *workloadWatcher) dropPeer(p workloadPeer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peer == p {
		w.peer = nil
	}
}

func (w *workloadWatcher) currentPeer() workloadPeer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peer
}

// run starts the informers and reports each namespace in full once it has synced, again whenever a
// session comes up, and on every resync, until ctx ends.
func (w *workloadWatcher) run(ctx context.Context) {
	// A session that came up before this run is covered by the full report each namespace sends once synced.
	select {
	case <-w.kick:
	default:
	}
	for _, ns := range w.names {
		inf := w.informers[ns]
		go inf.Run(ctx.Done())
		go func() {
			if !cache.WaitForCacheSync(ctx.Done(), w.handlers[ns].HasSynced) {
				return
			}
			slog.InfoContext(ctx, "workload informer synced", "namespace", ns)
			w.synced[ns].Store(true)
			w.report(ns)
		}()
	}
	tick := time.NewTicker(w.resync)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-w.kick:
		}
		for _, ns := range w.names {
			w.report(ns)
		}
	}
}

// applied reports a resource that appeared or whose spec changed.
func (w *workloadWatcher) applied(ns string, obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || !w.synced[ns].Load() {
		return
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if p := w.currentPeer(); p != nil {
		_ = w.emit(p, applyReport(u))
	}
}

// deleted reports a resource that is gone.
func (w *workloadWatcher) deleted(ns string, obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || !w.synced[ns].Load() {
		return
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if p := w.currentPeer(); p != nil {
		_ = w.emit(p, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpDelete, Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID())})
	}
}

// report sends every resource the namespace's store holds, then the names it holds, so the
// organisation prunes what the list omits. It sends nothing before the namespace has synced or
// while no session is up.
func (w *workloadWatcher) report(ns string) {
	if !w.synced[ns].Load() {
		return
	}
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	p := w.currentPeer()
	if p == nil {
		return
	}
	var items []*unstructured.Unstructured
	for _, o := range w.informers[ns].GetStore().List() {
		if u, ok := o.(*unstructured.Unstructured); ok {
			items = append(items, u)
		}
	}
	slices.SortFunc(items, func(a, b *unstructured.Unstructured) int { return cmp.Compare(a.GetName(), b.GetName()) })
	var names []string
	for _, u := range items {
		// A resource the report cannot carry (an oversize spec) still counts as held: leaving its name
		// out of the list would make the organisation delete a workload it already runs.
		if runnerwire.ValidWorkloadName(u.GetName()) {
			names = append(names, u.GetName())
		}
		if w.emit(p, applyReport(u)) != nil {
			return
		}
	}
	if len(names) > runnerwire.MaxWorkloadSyncNames {
		slog.WarnContext(w.ctx, "workload sync not sent: the namespace holds more resources than one sync names", "namespace", ns, "count", len(names), "max", runnerwire.MaxWorkloadSyncNames)
		return
	}
	_ = w.emit(p, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpSync, Namespace: ns, Names: names})
}

// applyReport is the apply for u: its spec as written.
func applyReport(u *unstructured.Unstructured) runnerwire.WorkloadReport {
	spec := json.RawMessage("{}")
	if s, ok := u.Object["spec"]; ok && s != nil {
		if b, err := json.Marshal(s); err == nil {
			spec = b
		}
	}
	return runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpApply, Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()), Generation: u.GetGeneration(), Spec: spec}
}

// emit sends one report with sendMu held. A report outside the wire's bounds is logged and not sent
// (nil: nothing to retry). A failed send is logged and returned: the next full report carries what
// it held.
func (w *workloadWatcher) emit(p workloadPeer, rep runnerwire.WorkloadReport) error {
	if err := rep.Validate(); err != nil {
		slog.WarnContext(w.ctx, "workload not reported", "namespace", rep.Namespace, "name", rep.Name, "op", rep.Op, "error", err)
		return nil
	}
	data, err := runnerwire.Marshal(rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(w.ctx, workloadSendTimeout)
	defer cancel()
	if err := p.Event(ctx, runnerwire.Event{Kind: runnerwire.EventWorkload, Data: data}); err != nil {
		slog.WarnContext(w.ctx, "workload report failed; the next full report will carry it", "namespace", rep.Namespace, "name", rep.Name, "op", rep.Op, "error", err)
		return err
	}
	return nil
}

// workloadStatusAttempts bounds the read-then-write rounds of one status: a write that meets a newer
// resourceVersion is read again, and one still losing after this many is dropped (the org's next push
// carries it).
const workloadStatusAttempts = 3

// status writes the organisation's answer to the resource's status subresource and nowhere else, and
// only to the resource the answer is for: the one whose metadata.uid is a.UID and whose
// metadata.generation is a.ObservedGeneration. A late answer for a resource since deleted and recreated
// under the name, or for a spec since changed, is dropped. The write carries the resourceVersion it was
// checked at, so the apiserver refuses it if the resource changed in between; a deleted resource has
// nothing to write to. Neither is an error: the org's next push answers what the resource is now. A write
// that keeps losing to changes is one, so the org sends it again.
func (w *workloadWatcher) status(ctx context.Context, a runnerwire.WorkloadStatusArgs) error {
	if _, ok := w.informers[a.Namespace]; !ok {
		return runnerwire.Refuse("workload namespace %q is not one this runner watches", a.Namespace)
	}
	ready := metav1.ConditionFalse
	if a.Phase == runnerwire.WorkloadPhaseRunning {
		ready = metav1.ConditionTrue
	}
	res := w.client.Resource(workloadGVR).Namespace(a.Namespace)
	for range workloadStatusAttempts {
		u, err := res.Get(ctx, a.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if string(u.GetUID()) != a.UID || u.GetGeneration() != a.ObservedGeneration {
			slog.DebugContext(ctx, "workload status dropped: it answers another resource or generation", "namespace", a.Namespace, "name", a.Name,
				"uid", a.UID, "generation", a.ObservedGeneration, "current_uid", u.GetUID(), "current_generation", u.GetGeneration())
			return nil
		}
		// The transition time moves only when Ready does, so a repeated answer is not a new transition.
		since := w.now().UTC().Format(time.RFC3339)
		if st, at := readyCondition(u); st == string(ready) && at != "" {
			since = at
		}
		body, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": a.UID, "resourceVersion": u.GetResourceVersion()},
			"status": map[string]any{
				"observedGeneration": a.ObservedGeneration,
				"phase":              a.Phase,
				"runID":              a.RunID,
				"reason":             a.Reason,
				"message":            a.Message,
				"conditions": []map[string]any{{
					"type": "Ready", "status": string(ready), "reason": a.Phase, "lastTransitionTime": since,
				}},
			}})
		if err != nil {
			return err
		}
		_, err = res.Patch(ctx, a.Name, k8stypes.MergePatchType, body, metav1.PatchOptions{}, "status")
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return nil
		case !apierrors.IsConflict(err):
			return err
		}
	}
	// An error, so the org keeps the status unsent and its next sweep writes it again.
	return fmt.Errorf("workload %s/%s kept changing under the status write", a.Namespace, a.Name)
}

// readyCondition is the Ready condition's status and lastTransitionTime on u, "" when it has none.
func readyCondition(u *unstructured.Unstructured) (status, at string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			status, _ = m["status"].(string)
			at, _ = m["lastTransitionTime"].(string)
		}
	}
	return status, at
}
