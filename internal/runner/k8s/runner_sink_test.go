// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// fakeSink records each sink call in the harness's event log.
type fakeSink struct {
	h          *runnerHarness
	events     []string
	registered map[uuid.UUID][32]byte
	keys       int
}

func (f *fakeSink) NewKey() (string, [32]byte, error) {
	f.keys++
	key := fmt.Sprintf("key-%d", f.keys)
	return key, sha256.Sum256([]byte(key)), nil
}

func (f *fakeSink) Register(id uuid.UUID, h [32]byte) error {
	if f.registered == nil {
		f.registered = map[uuid.UUID][32]byte{}
	}
	persisted := false
	if f.h != nil {
		b, _ := f.h.ev.LoadEvidence(id)
		ev, err := decodeEvidence(b)
		persisted = err == nil && ev.SinkKeyHash == hex.EncodeToString(h[:])
	}
	f.events = append(f.events, fmt.Sprintf("register(persisted=%v)", persisted))
	f.registered[id] = h
	return nil
}

func (f *fakeSink) ProxyConfig(key string) any {
	return map[string]string{"url": "tls://10.96.0.40:9443", "spki_sha256": "pin", "key": key}
}

func (f *fakeSink) Drain(context.Context, uuid.UUID) error {
	f.events = append(f.events, "drain")
	return nil
}

func (f *fakeSink) ProducerGone(context.Context, uuid.UUID) error {
	f.events = append(f.events, "producer_gone")
	return nil
}

func (f *fakeSink) count(e string) int {
	n := 0
	for _, x := range f.events {
		if x == e {
			n++
		}
	}
	return n
}

func sinkHarness(t *testing.T) (*runnerHarness, *fakeSink) {
	h := newRunnerHarness(t)
	sink := &fakeSink{h: h}
	h.d.cfg.Sink = sink
	h.cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sink.events = append(sink.events, "delete "+a.(clienttesting.DeleteActionImpl).GetName())
		return false, nil, nil
	})
	return h, sink
}

// TestRunnerSinkKeyHashPersistedBeforeRegister: the key's hash is in the run's record before
// Register, and a failed save registers nothing.
func TestRunnerSinkKeyHashPersistedBeforeRegister(t *testing.T) {
	h, sink := sinkHarness(t)
	cfg, err := h.d.openSink(h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sink.events) != "[register(persisted=true)]" || cfg.(map[string]string)["key"] != "key-1" {
		t.Fatalf("events %v cfg %v", sink.events, cfg)
	}
	if b, _ := h.ev.LoadEvidence(h.runID); strings.Contains(string(b), "key-1\"") {
		t.Fatal("the sink key itself was stored")
	}
	h2, sink2 := sinkHarness(t)
	h2.ev.failSave = func([]byte) bool { return true }
	if _, err := h2.d.openSink(h2.runID); err == nil || len(sink2.events) != 0 {
		t.Fatalf("a failed save still registered: %v %v", err, sink2.events)
	}
}

// TestRunnerSinkDrainBeforeDelete: the proxy's sink is drained before runnerd deletes its pod, on
// StopProxy and on teardown.
func TestRunnerSinkDrainBeforeDelete(t *testing.T) {
	for _, path := range []string{"stop", "teardown"} {
		t.Run(path, func(t *testing.T) {
			h, sink := sinkHarness(t)
			h.boundProxy(t)
			h.onDelete = func(p *corev1.Pod) { terminate(p, "containerd://p1") }
			var err error
			if path == "stop" {
				err = h.d.StopProxy(context.Background(), agentPodName(h.runID))
			} else {
				err = h.d.teardownByRunID(context.Background(), h.runID, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			del := "delete " + proxyPodName(h.runID)
			for i, e := range sink.events {
				if e == del {
					if i == 0 || sink.events[i-1] != "drain" {
						t.Fatalf("events %q: the proxy delete was not preceded by a drain", sink.events)
					}
					return
				}
			}
			t.Fatalf("events %q: no proxy delete", sink.events)
		})
	}
}

// TestRunnerSinkProducerGoneOnlyOnEvidence: no settlement on termination unknown, a lapsed frame or
// a NotFound; exactly one on completion evidence.
func TestRunnerSinkProducerGoneOnlyOnEvidence(t *testing.T) {
	ctx := context.Background()
	h, sink := sinkHarness(t)
	h.boundProxy(t)
	if _, err := h.d.openSink(h.runID); err != nil {
		t.Fatal(err)
	}
	var pending *runner.ContainmentPendingError
	if err := h.d.StopProxy(ctx, agentPodName(h.runID)); !errors.As(err, &pending) {
		t.Fatalf("StopProxy with no evidence: %v", err)
	}
	if err := h.d.NoteValidatedLapse(h.runID); err != nil {
		t.Fatal(err)
	}
	if err := h.d.StopProxy(ctx, agentPodName(h.runID)); err != nil {
		t.Fatalf("lapsed StopProxy: %v", err)
	}
	if n := sink.count("producer_gone"); n != 0 {
		t.Fatalf("producer settled %d times without completion evidence", n)
	}
	// The kubelet now reports the proxy terminal: evidence, then exactly one settlement.
	if err := terminateProxy(h); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.containPod(ctx, h.runID, componentProxy, proxyPodName(h.runID), nil, proxyStopBound, true); err != nil {
		t.Fatalf("containPod with evidence: %v", err)
	}
	if _, err := h.d.containPod(ctx, h.runID, componentProxy, proxyPodName(h.runID), nil, proxyStopBound, true); err != nil {
		t.Fatal(err)
	}
	if n := sink.count("producer_gone"); n != 1 || !h.ev.get(t, h.runID).ProducerGone {
		t.Fatalf("producer settled %d times on evidence", n)
	}
}

func terminateProxy(h *runnerHarness) error {
	obj, err := h.cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(h.runID))
	if err != nil {
		return err
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	terminate(pod, "containerd://p1")
	return h.cs.Tracker().Update(podsGVR, pod, testNamespace)
}

// TestRunnerReregisterSinks: every persisted key hash is registered again (startup, before Online).
func TestRunnerReregisterSinks(t *testing.T) {
	h, sink := sinkHarness(t)
	if _, err := h.d.openSink(h.runID); err != nil {
		t.Fatal(err)
	}
	want := sink.registered[h.runID]
	restarted := &fakeSink{}
	h.d.cfg.Sink = restarted
	other := uuid.New()
	if err := h.d.ReregisterSinks([]uuid.UUID{h.runID, other}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.registered) != 1 || restarted.registered[h.runID] != want {
		t.Fatalf("re-registered %v", restarted.registered)
	}
}
