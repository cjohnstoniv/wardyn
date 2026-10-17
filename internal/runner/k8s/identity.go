// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Recorded identity and causal node association for a k8s runner's pods (design K1 §3.1,
// R8-M1, r9 M1, Fable r9 L1, Codex R10-L1, owner #8).
//
// The read contract: every read the evidence rests on (the before-list, the unbound pod read P0,
// the first bound read P1, the after-list, anchor, closing, post-status, taint and acceptance lists)
// is a direct API request with resourceVersion unset, never a resourceVersion=0 read, an informer,
// a lister or the fit advisory's cached node list, and each is sent only after its predecessor's
// response arrived: these functions are synchronous and called in that order.

// Bracket cadence while a pod is unbound (Fable r10 L1).
const (
	bracketFastCadence = 5 * time.Second
	bracketSlowCadence = 15 * time.Second
	bracketBackoffIdle = 2 * time.Minute
)

func (d *Driver) runLock(id uuid.UUID) func() {
	v, _ := d.runLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// mutate loads the run's record, applies f, and saves when f reports a change. A failed save is
// returned: nothing that depends on the change may follow it (Codex R8).
func (d *Driver) mutate(runID uuid.UUID, f func(ev *runEvidence) (bool, error)) (*runEvidence, error) {
	unlock := d.runLock(runID)
	defer unlock()
	b, err := d.cfg.Evidence.LoadEvidence(runID)
	if err != nil {
		return nil, fmt.Errorf("k8s: load run evidence: %w", err)
	}
	ev, err := decodeEvidence(b)
	if err != nil {
		return nil, fmt.Errorf("k8s: decode run evidence: %w", err)
	}
	changed, err := f(ev)
	if err != nil {
		return ev, err
	}
	if !changed {
		return ev, nil
	}
	if err := d.saveEvidence(runID, ev); err != nil {
		return nil, err
	}
	return ev, nil
}

func (d *Driver) saveEvidence(runID uuid.UUID, ev *runEvidence) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := d.cfg.Evidence.SaveEvidence(runID, b); err != nil {
		return fmt.Errorf("k8s: save run evidence: %w", err)
	}
	return nil
}

func (d *Driver) loadEvidence(runID uuid.UUID) (*runEvidence, error) {
	return d.mutate(runID, func(*runEvidence) (bool, error) { return false, nil })
}

// nodeSampleOf is a Node's UID and published machine identity.
func nodeSampleOf(n *corev1.Node) runner.NodeSample {
	s := runner.NodeSample{Name: n.Name, UID: string(n.UID), MachineID: n.Status.NodeInfo.MachineID, SystemUUID: n.Status.NodeInfo.SystemUUID}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			s.InternalIP = a.Address
			break
		}
	}
	return s
}

// listNodesScoped is a before-list: the nodes the pod's own nodeSelector selects, as a direct
// request with resourceVersion unset.
func (d *Driver) listNodesScoped(ctx context.Context, selector map[string]string) (*nodeMap, error) {
	list, err := d.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String()})
	if err != nil {
		return nil, fmt.Errorf("k8s: scoped node list: %w", err)
	}
	m := &nodeMap{Version: list.ResourceVersion, Names: make(map[string]string, len(list.Items))}
	for i := range list.Items {
		m.Names[list.Items[i].Name] = string(list.Items[i].UID)
	}
	return m, nil
}

// nodeHolderByName is a name-scoped node list (fieldSelector metadata.name=N), which makes the
// holder exact. Like every evidence read it is direct, with resourceVersion unset.
func (d *Driver) nodeHolderByName(ctx context.Context, name string) (*nodeHolder, error) {
	list, err := d.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return nil, fmt.Errorf("k8s: name-scoped node list: %w", err)
	}
	h := &nodeHolder{At: d.now().UTC(), None: true}
	for i := range list.Items {
		n := &list.Items[i]
		if n.Name != name {
			continue
		}
		h.None, h.Node = false, nodeSampleOf(n)
		for _, t := range n.Spec.Taints {
			if t.Key == corev1.TaintNodeOutOfService {
				h.OutOfService = true
			}
		}
	}
	if h.None {
		h.Node = runner.NodeSample{Name: name}
	}
	return h, nil
}

// associate completes a pod's bracket with the after-list's holder h of its node name.
func (pe *podEvidence) associate(h *nodeHolder) {
	pe.PostBind = h
	if pe.Before == nil {
		pe.Assoc = assocU3
		return
	}
	before, held := pe.Before.Names[pe.NodeName]
	switch {
	case !held:
		pe.Assoc = assocU1
	case !h.None && h.Node.UID == before:
		pe.Assoc = assocCausal
	default:
		pe.Assoc, pe.BeforeHolderUID = assocU2, before
	}
}

var errPodUIDChanged = errors.New("k8s: the pod under this name is not the recorded pod UID")

// observePod records what one read of a run's pod shows, in the read contract's order: the UID on
// a first read of a pod with no record (r6 L3), the after-list right after the first bound read,
// the pod and host IPs and container IDs, an anchor list right after a read that recorded a new
// baseline, and the closing list right after the first read showing a deletion mark. Every new
// reuse generation is persisted before this returns.
func (d *Driver) observePod(ctx context.Context, runID uuid.UUID, component string, pod *corev1.Pod) (*runEvidence, error) {
	var needAfter, needAnchor, needClosing bool
	ev, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		pe := ev.pod(component, pod.Name)
		changed := false
		if pe.UID == "" {
			pe.UID, changed = string(pod.UID), true
		}
		if string(pod.UID) != pe.UID {
			return false, errPodUIDChanged
		}
		if pe.NodeName == "" && pod.Spec.NodeName != "" {
			pe.NodeName, changed = pod.Spec.NodeName, true
			needAfter = true
		}
		nb, c := pe.recordIdentity(pod)
		changed = changed || c
		needAnchor = pe.NodeName != "" && (nb || hasUnanchored(pe))
		needClosing = pod.DeletionTimestamp != nil && pe.Closing == nil && pe.NodeName != ""
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	pe := ev.Pods[component]
	if needAfter {
		h, err := d.nodeHolderByName(ctx, pe.NodeName)
		if err != nil {
			return nil, err
		}
		if ev, err = d.mutate(runID, func(ev *runEvidence) (bool, error) {
			p := ev.Pods[component]
			if p.PostBind != nil {
				return false, nil
			}
			p.associate(h)
			return true, nil
		}); err != nil {
			return nil, err
		}
		pe = ev.Pods[component]
	}
	if needAnchor && pe.PostBind != nil {
		if ev, err = d.nameList(ctx, runID, component, func(p *podEvidence, h *nodeHolder) bool { return p.anchorBaselines(h) }); err != nil {
			return nil, err
		}
	}
	if needClosing {
		if ev, err = d.closingList(ctx, runID, component); err != nil {
			return nil, err
		}
	}
	return ev, nil
}

func hasUnanchored(pe *podEvidence) bool {
	for _, c := range pe.Containers {
		if c.Baseline != "" && !c.Anchored {
			return true
		}
	}
	return false
}

// nameList sends one name-scoped list for the pod's node name, latches any reuse it shows, and
// applies use; the record is saved before the caller compares anything with it.
func (d *Driver) nameList(ctx context.Context, runID uuid.UUID, component string, use func(*podEvidence, *nodeHolder) bool) (*runEvidence, error) {
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return nil, err
	}
	pe := ev.Pods[component]
	if pe == nil || pe.NodeName == "" {
		return ev, nil
	}
	h, err := d.nodeHolderByName(ctx, pe.NodeName)
	if err != nil {
		return nil, err
	}
	return d.mutate(runID, func(ev *runEvidence) (bool, error) {
		p := ev.Pods[component]
		changed := ev.latchReuse(p, h)
		if use != nil && use(p, h) {
			changed = true
		}
		return changed, nil
	})
}

// closingList takes the pod's closing anchor (Codex R11-M1): a name-scoped list sent after
// runnerd's own delete returned or after the first read showing a deletion mark, persisted before
// any evidence is read.
func (d *Driver) closingList(ctx context.Context, runID uuid.UUID, component string) (*runEvidence, error) {
	return d.nameList(ctx, runID, component, func(p *podEvidence, h *nodeHolder) bool {
		if p.Closing != nil {
			return false
		}
		p.Closing = h
		return true
	})
}

// bracket tracks one pod's unbound reads during CreateSandbox's start wait: a scoped before-list
// every 5 s (15 s after 2 min with no node-set change) ahead of the pod read, the version an unbound
// read followed persisted when it changes, and the after-list on the first bound read.
type bracket struct {
	d         *Driver
	runID     uuid.UUID
	component string
	selector  map[string]string
	last      *nodeMap
	lastAt    time.Time
	changedAt time.Time
	since     time.Time
	bound     bool
}

func (b *bracket) cadence(now time.Time) time.Duration {
	if now.Sub(b.since) >= bracketBackoffIdle && now.Sub(b.changedAt) >= bracketBackoffIdle {
		return bracketSlowCadence
	}
	return bracketFastCadence
}

// before takes the bracket's lower end ahead of the create request and persists it there.
func (b *bracket) before(ctx context.Context, name string) error {
	m, err := b.d.listNodesScoped(ctx, b.selector)
	if err != nil {
		return err
	}
	now := b.d.now()
	b.last, b.lastAt, b.changedAt, b.since = m, now, now, now
	_, err = b.d.mutate(b.runID, func(ev *runEvidence) (bool, error) {
		pe := ev.pod(b.component, name)
		pe.Before = m
		return true, nil
	})
	return err
}

// created records the create response's pod UID.
func (b *bracket) created(pod *corev1.Pod) error {
	_, err := b.d.mutate(b.runID, func(ev *runEvidence) (bool, error) {
		pe := ev.pod(b.component, pod.Name)
		pe.UID = string(pod.UID)
		return true, nil
	})
	return err
}

// get is the start wait's pod read under the bracket.
func (b *bracket) get(ctx context.Context, name string) (*corev1.Pod, error) {
	if now := b.d.now(); !b.bound && now.Sub(b.lastAt) >= b.cadence(now) {
		m, err := b.d.listNodesScoped(ctx, b.selector)
		if err != nil {
			return nil, err
		}
		if !m.sameNames(b.last) {
			b.changedAt = now
		}
		b.last, b.lastAt = m, now
	}
	pod, err := b.d.clientset.CoreV1().Pods(b.d.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if pod.Spec.NodeName == "" {
		last := b.last
		if _, err := b.d.mutate(b.runID, func(ev *runEvidence) (bool, error) {
			pe := ev.pod(b.component, name)
			if pe.UID != "" && string(pod.UID) != pe.UID {
				return false, errPodUIDChanged
			}
			if last == nil || last.sameNames(pe.Before) {
				return false, nil
			}
			pe.Before = last
			return true, nil
		}); err != nil {
			return nil, err
		}
	} else {
		b.bound = true
	}
	if _, err := b.d.observePod(ctx, b.runID, b.component, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// classifyProxy maps a listed proxy pod to its observation.
func (d *Driver) classifyProxy(pod *corev1.Pod) runner.ProxyObservation {
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return runner.ProxyFailed
	case corev1.PodSucceeded:
		return runner.ProxySucceeded
	case corev1.PodRunning:
		return runner.ProxyRunning
	}
	if d.now().Sub(pod.CreationTimestamp.Time) > d.cfg.StartTimeout {
		return runner.ProxyPendingPastStart
	}
	return runner.ProxyPending
}

// ObserveProxies is the runner's 30 s pod list (one call; the Role grants pods: list). It reports
// each asked run's proxy, records every listed pod's identity, and lists each node name a held run
// uses, so a reuse is caught on the tick (owner #8). A run whose proxy pod is missing reports
// NotFound, which records the obligation but is never completion evidence.
func (d *Driver) ObserveProxies(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]runner.ProxyObservation, error) {
	if !d.runnerMode() {
		return nil, runner.ErrEndUnsupported
	}
	pods, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: d.runnerSelector("")})
	if err != nil {
		return nil, fmt.Errorf("k8s: list runner pods: %w", err)
	}
	byName := map[string]*corev1.Pod{}
	for i := range pods.Items {
		byName[pods.Items[i].Name] = &pods.Items[i]
	}
	out := make(map[uuid.UUID]runner.ProxyObservation, len(runIDs))
	var errs []error
	for _, id := range runIDs {
		if p := byName[proxyPodName(id)]; p != nil {
			out[id] = d.classifyProxy(p)
		} else if held, err := d.proxyCreated(id); err != nil {
			errs = append(errs, err)
		} else if held {
			// NotFound speaks only for a proxy that was created: a run still inside CreateSandbox
			// (its Ref stored before any pod exists) is left out, not reported gone.
			out[id] = runner.ProxyNotFound
		}
		for comp, name := range map[string]string{componentProxy: proxyPodName(id), componentAgent: agentPodName(id)} {
			pod := byName[name]
			if pod == nil {
				continue
			}
			if _, err := d.observePod(ctx, id, comp, pod); err != nil {
				errs = append(errs, err)
			}
		}
		if err := d.tickNames(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// proxyCreated reports whether the run's evidence records a proxy pod UID from its create response.
func (d *Driver) proxyCreated(runID uuid.UUID) (bool, error) {
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return false, err
	}
	pe := ev.Pods[componentProxy]
	return pe != nil && pe.UID != "", nil
}

// tickNames lists each node name a run's unreleased pods hold, latching any reuse.
func (d *Driver) tickNames(ctx context.Context, runID uuid.UUID) error {
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return err
	}
	for comp, pe := range ev.Pods {
		if pe.NodeName == "" || pe.Witness != nil || pe.PostBind == nil {
			continue
		}
		if _, err := d.nameList(ctx, runID, comp, nil); err != nil {
			return err
		}
	}
	return nil
}

// runnerSelector selects this runner's managed pods, optionally of one run.
func (d *Driver) runnerSelector(runID string) string {
	sel := labelManaged + "=true," + runner.LabelRunnerID + "=" + d.cfg.RunnerID + "," + labelComponent + " in (" + componentAgent + "," + componentProxy + ")"
	if runID != "" {
		sel += "," + labelRun + "=" + runID
	}
	return sel
}
