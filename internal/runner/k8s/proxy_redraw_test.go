// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// draw is the address and node the n-th proxy pod create of a run is given.
type draw struct{ ip, node string }

// drawLog records what the create reactor saw.
type drawLog struct {
	mu           sync.Mutex
	proxies      []*corev1.Pod // each proxy pod as it was sent to create, by draw
	agentCreates int
	redrawingAt  []bool // d.redrawing at each proxy create
	fencesAt     []int  // len(h.fences) at each proxy create
}

func (l *drawLog) proxyCreates() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.proxies)
}

// drawReactor binds each proxy pod the run creates to the next of draws, as a kubelet and a CNI would. A
// proxy given an address in f runs and never listens (the relay refuses its first renew), so it is not
// Ready.
func (h *runnerHarness) drawReactor(t *testing.T, f fenceSet, draws ...draw) *drawLog {
	t.Helper()
	nodeIP := map[string]string{"n1": "10.0.0.5", "n2": "10.0.0.6"}
	log := &drawLog{}
	h.cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		if strings.HasPrefix(pod.Name, "wardyn-agent-") {
			log.agentCreates++
			return false, nil, nil
		}
		if !strings.HasPrefix(pod.Name, "wardyn-proxy-") {
			return false, nil, nil
		}
		if aff := pod.Spec.Affinity; aff != nil && aff.NodeAffinity != nil {
			for _, term := range aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				for _, r := range term.Preference.MatchFields {
					if len(r.Values) != 1 {
						// The API's rule, which the fake does not apply.
						t.Errorf("a matchFields requirement %s %s has %d values, want 1", r.Key, r.Operator, len(r.Values))
					}
				}
			}
		}
		log.proxies = append(log.proxies, pod.DeepCopy())
		n := len(log.proxies)
		_, redrawing := h.d.redrawing.Load(h.runID)
		log.redrawingAt = append(log.redrawingAt, redrawing)
		log.fencesAt = append(log.fencesAt, len(h.fences))
		if n > len(draws) {
			return true, nil, fmt.Errorf("proxy pod create %d, but only %d draws are scripted", n, len(draws))
		}
		dr := draws[n-1]
		_, fenced := f[dr.ip]
		p := pod.DeepCopy()
		p.UID = k8stypes.UID(fmt.Sprintf("uid-proxy-%d", n))
		p.ResourceVersion = "10"
		p.Spec.NodeName = dr.node
		p.Status.Phase = corev1.PodRunning
		p.Status.PodIP = dr.ip
		p.Status.HostIP = nodeIP[dr.node]
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: proxyContainerName, ContainerID: fmt.Sprintf("containerd://p%d", n),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: !fenced,
		}}
		if err := h.cs.Tracker().Add(p); err != nil {
			return true, nil, err
		}
		return true, p, nil
	})
	return log
}

// redrawHarness is the runner harness shared by tests 1-4: two nodes, a fenced address held for another
// run, a kubelet that stops each deleted pod, and the stand-ins a runner-mode CreateSandbox needs.
func redrawHarness(t *testing.T, draws ...draw) (*runnerHarness, fenceSet, uuid.UUID, *drawLog) {
	t.Helper()
	h := newRunnerHarness(t)
	f := fenceSet{}
	f.wire(h)
	other := uuid.New()
	f[testProxyIP] = other
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5")
	h.node(t, "n2", "uid-n2", "m2", "s2", "10.0.0.6")
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installAgentRunningReactor(t, h.cs)
	log := h.drawReactor(t, f, draws...)
	h.onDelete = func(p *corev1.Pod) { terminate(p, p.Status.ContainerStatuses[0].ContainerID) }
	return h, f, other, log
}

func redrawSpec(h *runnerHarness) runner.SandboxSpec {
	spec := testSandboxSpec()
	spec.RunID = h.runID
	return spec
}

// notInNodes returns, per requirement of pod's preferred node-affinity terms, the node it names in a
// metadata.name NotIn.
func notInNodes(pod *corev1.Pod) []string {
	var out []string
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	for _, term := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, r := range term.Preference.MatchFields {
			if r.Key == "metadata.name" && r.Operator == corev1.NodeSelectorOpNotIn {
				out = append(out, r.Values...)
			}
		}
	}
	return out
}

// TestRunnerCreateSandbox_AFencedDrawIsReplaced: a proxy pod given an address another run's fence refuses
// is retired with completion evidence and replaced, and the run starts on the second draw. The check
// sits before Ready: the first draw runs and never becomes Ready.
func TestRunnerCreateSandbox_AFencedDrawIsReplaced(t *testing.T) {
	h, f, other, log := redrawHarness(t, draw{testProxyIP, "n1"}, draw{"10.244.2.9", "n2"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, redrawSpec(h)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got := log.proxyCreates(); got != 2 {
		t.Fatalf("proxy creates = %d, want 2", got)
	}
	if got := notInNodes(log.proxies[0]); len(got) != 0 {
		t.Errorf("the first draw avoids %v, want no node", got)
	}
	if got := notInNodes(log.proxies[1]); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("the second draw avoids %v, want [n1]", got)
	}
	agentObj, err := h.cs.Tracker().Get(podsGVR, testNamespace, agentPodName(h.runID))
	if err != nil {
		t.Fatalf("agent pod: %v", err)
	}
	agent := agentObj.(*corev1.Pod)
	if len(agent.OwnerReferences) == 0 || agent.OwnerReferences[0].UID != "uid-proxy-2" {
		t.Errorf("agent owner references = %+v, want the second draw's UID", agent.OwnerReferences)
	}
	if len(agent.Spec.HostAliases) == 0 || agent.Spec.HostAliases[0].IP != "10.244.2.9" {
		t.Errorf("agent host aliases = %+v, want the second draw's address", agent.Spec.HostAliases)
	}
	ev := h.ev.get(t, h.runID)
	if p := ev.Pods[componentProxy]; p == nil || p.UID != "uid-proxy-2" {
		t.Fatalf("recorded proxy = %+v, want uid-proxy-2", p)
	}
	if len(ev.Redrawn) != 1 || ev.Redrawn[0].UID != "uid-proxy-1" || ev.Redrawn[0].Witness == nil || !ev.Redrawn[0].FinalizerOff {
		t.Errorf("redrawn = %+v, want the first draw closed on its witness with its finalizer off", ev.Redrawn)
	}
	if ev.Fence != nil {
		t.Errorf("a fence record is still held after the draw closed: %+v", ev.Fence)
	}
	if len(h.fences) != 0 {
		t.Errorf("the relay was told to fence the run: %v", h.fences)
	}
	if f[testProxyIP] != other {
		t.Errorf("the other run's refusal of %s was released: %v", testProxyIP, f[testProxyIP])
	}
	if len(log.redrawingAt) != 2 || log.redrawingAt[0] || !log.redrawingAt[1] {
		t.Errorf("redrawing at each proxy create = %v, want [false true]", log.redrawingAt)
	}
	if _, ok := h.d.redrawing.Load(h.runID); ok {
		t.Error("the run is still marked redrawing after CreateSandbox returned")
	}
}

// TestRunnerCreateSandbox_EveryDrawFencedFailsRelayIPFenced: three fenced draws fail the run with the
// relay_ip_fenced error, naming the count, and the relay is told nothing until the rollback contains the
// last one.
func TestRunnerCreateSandbox_EveryDrawFencedFailsRelayIPFenced(t *testing.T) {
	h, _, _, log := redrawHarness(t, draw{testProxyIP, "n1"}, draw{testProxyIP, "n2"}, draw{testProxyIP, "n1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.d.CreateSandbox(ctx, redrawSpec(h))
	if !errors.Is(err, runner.ErrRelayIPFenced) || !strings.Contains(err.Error(), "each of its 3 draws") {
		t.Fatalf("CreateSandbox error = %v, want relay_ip_fenced naming 3 draws", err)
	}
	if got := log.proxyCreates(); got != 3 {
		t.Fatalf("proxy creates = %d, want 3", got)
	}
	if log.agentCreates != 0 {
		t.Errorf("agent creates = %d, want 0", log.agentCreates)
	}
	if got := notInNodes(log.proxies[2]); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("the third draw avoids %v, want [n1 n2], one requirement each", got)
	}
	if got := h.ev.get(t, h.runID).Redrawn; len(got) != 2 {
		t.Errorf("redrawn = %d records, want 2", len(got))
	}
	if len(log.fencesAt) != 3 || log.fencesAt[2] != 0 {
		t.Errorf("relay fences at each proxy create = %v, want none while redrawing", log.fencesAt)
	}
	if !slices.Contains(h.fences, testProxyIP) {
		t.Errorf("the rollback did not fence %s: %v", testProxyIP, h.fences)
	}
}

// TestRunnerCreateSandbox_AFencedDrawWithNoEvidenceFencesTheRunAndFails pins the fallback: a draw that
// never stops cannot be replaced, so the run is fenced and fails as at v0.10.2, and a restart replays it.
func TestRunnerCreateSandbox_AFencedDrawWithNoEvidenceFencesTheRunAndFails(t *testing.T) {
	h, _, _, log := redrawHarness(t, draw{testProxyIP, "n1"}, draw{"10.244.2.9", "n2"})
	h.onDelete = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.d.CreateSandbox(ctx, redrawSpec(h))
	if !errors.Is(err, runner.ErrRelayIPFenced) || !strings.Contains(err.Error(), "could not be replaced") {
		t.Fatalf("CreateSandbox error = %v, want relay_ip_fenced, could not be replaced", err)
	}
	if got := log.proxyCreates(); got != 1 {
		t.Errorf("proxy creates = %d, want 1", got)
	}
	if log.agentCreates != 0 {
		t.Errorf("agent creates = %d, want 0", log.agentCreates)
	}
	if !slices.Contains(h.fences, testProxyIP) {
		t.Fatalf("the run was not fenced: %v", h.fences)
	}
	restarted := fenceSet{}
	restarted.wire(h)
	if err := h.d.ReplayFences([]uuid.UUID{h.runID}); err != nil || restarted[testProxyIP] != h.runID {
		t.Errorf("replay: %v, refusals %v, want %s fenced for the run", err, restarted, testProxyIP)
	}
}

// TestRunnerCreateSandbox_ALateReadOfARetiredDrawIsNotRecorded: a read of the retired draw, as a 30 s
// list taken before the retire returns it, is refused and never written over the next draw's record.
func TestRunnerCreateSandbox_ALateReadOfARetiredDrawIsNotRecorded(t *testing.T) {
	h, _, _, log := redrawHarness(t, draw{testProxyIP, "n1"}, draw{"10.244.2.9", "n2"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, redrawSpec(h)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	first := log.proxies[0].DeepCopy()
	first.UID = "uid-proxy-1"
	first.Spec.NodeName = "n1"
	first.Status.PodIP = testProxyIP
	if _, err := h.d.observePod(ctx, h.runID, componentProxy, first); !errors.Is(err, errRedrawnPod) {
		t.Fatalf("observePod of the retired draw: %v, want errRedrawnPod", err)
	}
	if p := h.ev.get(t, h.runID).Pods[componentProxy]; p == nil || p.UID != "uid-proxy-2" {
		t.Fatalf("recorded proxy = %+v, want uid-proxy-2", p)
	}
	h.cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: []corev1.Pod{*first}}, nil
	})
	seen, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
	if err != nil {
		t.Fatalf("ObserveProxies: %v", err)
	}
	if o, ok := seen[h.runID]; ok {
		t.Errorf("the run was observed as %q from a retired draw", o)
	}
	if p := h.ev.get(t, h.runID).Pods[componentProxy]; p == nil || p.UID != "uid-proxy-2" {
		t.Errorf("recorded proxy after the tick = %+v, want uid-proxy-2", p)
	}
}

// TestRetireFencedDraw_TheFenceRecordIsPersistedNotApplied: the drawn pod's fence record is saved before
// its delete without being applied in the relay, and a restart replays it.
func TestRetireFencedDraw_TheFenceRecordIsPersistedNotApplied(t *testing.T) {
	h := newRunnerHarness(t)
	f := fenceSet{}
	f.wire(h)
	h.boundProxy(t)
	if err := h.d.recordDrawFence(h.runID); err != nil {
		t.Fatalf("recordDrawFence: %v", err)
	}
	if len(h.fences) != 0 {
		t.Errorf("the relay was told to fence: %v", h.fences)
	}
	fence := h.ev.get(t, h.runID).Fence
	if fence == nil || fence.ProxyIP != testProxyIP || fence.ProxyUID != "uid-proxy-1" {
		t.Fatalf("fence record = %+v, want %s of uid-proxy-1", fence, testProxyIP)
	}
	if err := h.d.recordDrawFence(h.runID); err == nil {
		t.Error("a second recordDrawFence succeeded, want it refused as not a fresh draw")
	}
	restarted := fenceSet{}
	restarted.wire(h)
	if err := h.d.ReplayFences([]uuid.UUID{h.runID}); err != nil || restarted[testProxyIP] != h.runID {
		t.Errorf("replay: %v, refusals %v, want %s fenced for the run", err, restarted, testProxyIP)
	}
}

// TestRelayIPFencedRedrawErrorsCrossTheWireAsTheirCode: both new error shapes keep the relay_ip_fenced
// code across the wire.
func TestRelayIPFencedRedrawErrorsCrossTheWireAsTheirCode(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("k8s: proxy pod %s: %w (each of its 3 draws was given a fenced address)", testProxyIP, errRelayIPFenced),
		fmt.Errorf("k8s: proxy pod %s: %w (it could not be replaced: %v)", testProxyIP, errRelayIPFenced, errTerminationUnknown),
	} {
		wire := runnerwire.ErrorFor(err)
		if wire.Code != runnerwire.CodeRelayIPFenced || !errors.Is(wire.Err(), runner.ErrRelayIPFenced) {
			t.Errorf("%v crossed as %+v", err, wire)
		}
	}
}

// moveToRedrawn is what closeDraw and the next draw's create do to the run's record, without a clientset call.
func moveToRedrawn(h *runnerHarness, next string) error {
	_, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		ev.Redrawn = append(ev.Redrawn, ev.Pods[componentProxy])
		delete(ev.Pods, componentProxy)
		if next != "" {
			ev.Pods[componentProxy] = &podEvidence{Name: proxyPodName(h.runID), UID: next}
		}
		return true, nil
	})
	return err
}

// TestRunnerObserveProxies_ARunReplacingAFencedDrawIsLeftOut: the 30 s observation reports nothing for a
// run that is replacing a draw, so a retired draw never becomes a durable ProxyExited obligation.
func TestRunnerObserveProxies_ARunReplacingAFencedDrawIsLeftOut(t *testing.T) {
	ctx := context.Background()
	t.Run("marked redrawing", func(t *testing.T) {
		h := newRunnerHarness(t)
		pod := h.boundProxy(t)
		terminate(pod, "containerd://p1")
		if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
			t.Fatal(err)
		}
		seen, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
		if err != nil || seen[h.runID] != runner.ProxySucceeded {
			t.Fatalf("precondition: %q %v, want succeeded", seen[h.runID], err)
		}
		h.d.redrawing.Store(h.runID, struct{}{})
		defer h.d.redrawing.Delete(h.runID)
		seen, err = h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
		if o, ok := seen[h.runID]; err != nil || ok {
			t.Errorf("a redrawing run was observed as %q (%v)", o, err)
		}
	})
	t.Run("retired under the list", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.boundProxy(t)
		var once sync.Once
		var moved error
		h.cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			once.Do(func() { moved = moveToRedrawn(h, "uid-proxy-2") })
			return true, &corev1.PodList{}, nil
		})
		seen, err := h.d.ObserveProxies(ctx, []uuid.UUID{h.runID})
		if moved != nil {
			t.Fatal(moved)
		}
		if o, ok := seen[h.runID]; err != nil || ok {
			t.Errorf("a run whose draw was retired under the list was observed as %q (%v)", o, err)
		}
	})
}

// TestNameListAfterItsRecordClosedRecordsNothing: a node list that straddles closeDraw neither
// dereferences the closed record nor writes the old node's list into the next draw's.
func TestNameListAfterItsRecordClosedRecordsNothing(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	var once sync.Once
	var moved error
	h.cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		once.Do(func() { moved = moveToRedrawn(h, "") })
		return false, nil, nil
	})
	_, err := h.d.closingList(context.Background(), h.runID, componentProxy)
	if moved != nil {
		t.Fatal(moved)
	}
	if !errors.Is(err, errRedrawnPod) {
		t.Fatalf("closingList: %v, want errRedrawnPod", err)
	}
}
