// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// fakeSink is the decision sink as the driver drives it. Succeed refuses a run whose last proxy is not
// settled, as runnerd's sink does.
type fakeSink struct {
	mu         sync.Mutex
	live       map[uuid.UUID]bool
	succeeded  map[uuid.UUID][][32]byte
	registered map[uuid.UUID][32]byte
	calls      []string
}

func newFakeSink() *fakeSink {
	return &fakeSink{live: map[uuid.UUID]bool{}, succeeded: map[uuid.UUID][][32]byte{}, registered: map[uuid.UUID][32]byte{}}
}

func (s *fakeSink) NewKey() (string, [32]byte, error) {
	var k [32]byte
	_, _ = rand.Read(k[:])
	return hex.EncodeToString(k[:]), sha256.Sum256(k[:]), nil
}

func (s *fakeSink) Register(id uuid.UUID, h [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[id] = h
	return nil
}

func (s *fakeSink) Succeed(id uuid.UUID, h [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[id] {
		return errors.New("the run's last proxy is not settled")
	}
	s.live[id] = true
	s.succeeded[id] = append(s.succeeded[id], h)
	s.calls = append(s.calls, "succeed")
	return nil
}

func (s *fakeSink) ProxyConfig(key string) any {
	return proxy.DecisionSinkConfig{URL: "tls://host.docker.internal:7000", SPKISHA256: strings.Repeat("ab", 32), Key: key}
}

func (s *fakeSink) Drain(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "drain")
	return nil
}

func (s *fakeSink) ProducerGone(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[id] = false
	s.calls = append(s.calls, "gone")
	return nil
}

// memEvidence is the runner's record store in memory.
type memEvidence struct {
	mu sync.Mutex
	m  map[uuid.UUID][]byte
}

func (e *memEvidence) LoadEvidence(id uuid.UUID) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[id], nil
}

func (e *memEvidence) SaveEvidence(id uuid.UUID, b []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[id] = b
	return nil
}

// proxyStdinConfig is the last config the proxy name read on stdin.
func proxyStdinConfig(t *testing.T, f *fakeDocker, name string) map[string]json.RawMessage {
	t.Helper()
	var last map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(f.stdinOf(name)))
	for {
		var c map[string]json.RawMessage
		if err := dec.Decode(&c); errors.Is(err, io.EOF) {
			return last
		} else if err != nil {
			t.Fatal(err)
		}
		last = c
	}
}

// Every proxy the driver creates on a runner, at a run's create and at a revive's replace, mounts nothing
// from the host and sends its decisions to runnerd's sink with a key of its own: the key's hash is in the
// run's record and was handed to the sink, after the old proxy was drained, removed and settled. A
// decision_spool_dir that arrives in a received config never survives, on a runner or on the
// organisation's own substrate, which gets no sink either.
func TestEveryProxyMountsNothingAndRunnerProxiesSendToTheSink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner bool
	}{{"a runner", true}, {"the organisation's own substrate", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			sink, ev := newFakeSink(), &memEvidence{m: map[uuid.UUID][]byte{}}
			cfg := Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal"}
			if tc.runner {
				cfg.RunnerID, cfg.DecisionSink, cfg.Evidence = "runner-1", sink, ev
			}
			d := newWithClient(f, cfg)
			ctx := context.Background()
			spec := testSpec()
			name := proxyContainerName(spec.RunID)
			check := func(step string, n int) {
				t.Helper()
				f.mu.Lock()
				host := f.containers[name].host
				f.mu.Unlock()
				if len(host.Mounts) != 0 || len(host.Binds) != 0 {
					t.Fatalf("%s: the proxy mounts %v %v; a proxy shares no directory with the host", step, host.Mounts, host.Binds)
				}
				c := proxyStdinConfig(t, f, name)
				if _, ok := c["decision_spool_dir"]; ok {
					t.Fatalf("%s: decision_spool_dir survived: %s", step, c["decision_spool_dir"])
				}
				raw, ok := c["decision_sink"]
				if !tc.runner {
					if ok {
						t.Fatalf("%s: the organisation's substrate gave its proxy a decision_sink %s", step, raw)
					}
					return
				}
				var ds proxy.DecisionSinkConfig
				if err := json.Unmarshal(raw, &ds); err != nil || ds.Key == "" {
					t.Fatalf("%s: decision_sink %s (%v); want the run's sink with a key", step, raw, err)
				}
				key, _ := hex.DecodeString(ds.Key)
				h := sha256.Sum256(key)
				var kept sinkEvidence
				_ = json.Unmarshal(ev.m[spec.RunID], &kept)
				if got := sink.succeeded[spec.RunID]; len(got) != n || got[n-1] != h || kept.SinkKeyHash != hex.EncodeToString(h[:]) {
					t.Fatalf("%s: sink got %d keys, record keeps %q; want key %d of this proxy in both", step, len(got), kept.SinkKeyHash, n)
				}
			}
			sb, err := d.CreateSandbox(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			check("create", 1)
			received := withSpoolDir(t, renderedTestConfig(t, "fresh"))
			if err := d.ReplaceProxy(ctx, sb.Ref, received); err != nil {
				t.Fatal(err)
			}
			check("replace", 2)
			if err := d.StopProxy(ctx, sb.Ref); err != nil {
				t.Fatal(err)
			}
			if want := "succeed drain gone succeed drain gone"; tc.runner && strings.Join(sink.calls, " ") != want {
				t.Fatalf("sink calls %q, want %q: drain before each remove, settle once it is gone, a new key after", strings.Join(sink.calls, " "), want)
			}
			if !tc.runner {
				return
			}
			if err := newWithClient(f, cfg).ReregisterSinks([]uuid.UUID{spec.RunID}); err != nil || sink.registered[spec.RunID] != sink.succeeded[spec.RunID][1] {
				t.Fatalf("a restart registered %x (%v), want the current proxy's key hash", sink.registered[spec.RunID], err)
			}
		})
	}
}

// withSpoolDir adds the decision_spool_dir an organisation might send.
func withSpoolDir(t *testing.T, b []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["decision_spool_dir"] = "/org/chosen/spool"
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A runner's driver without the sink refuses to start a proxy, and leaves none behind.
func TestRunnerProxyRefusedWithoutTheSink(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", RunnerID: "runner-1"})
	if _, err := d.CreateSandbox(context.Background(), testSpec()); !errors.Is(err, errDecisionSinkUnwired) {
		t.Fatalf("CreateSandbox = %v, want %v", err, errDecisionSinkUnwired)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[proxyContainerName(testSpec().RunID)]; c != nil && !c.removed {
		t.Fatal("a refused create left a proxy behind")
	}
}
