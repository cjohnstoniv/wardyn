// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const testProxyRepo = "registry.example/wardyn-proxy"

// relayKeys stands in for the relay's key registry: each hash the driver hands it, and whether the
// run's record held that hash by then.
type relayKeys struct {
	got    map[uuid.UUID][32]byte
	events []string
}

func wireRelayKeys(h *runnerHarness) *relayKeys {
	k := &relayKeys{got: map[uuid.UUID][32]byte{}}
	h.d.cfg.RelayKey = func(id uuid.UUID, hash [32]byte) {
		b, _ := h.ev.LoadEvidence(id)
		ev, err := decodeEvidence(b)
		k.events = append(k.events, fmt.Sprintf("relay_key(persisted=%v)", err == nil && ev.RelayKeyHash == hex.EncodeToString(hash[:])))
		k.got[id] = hash
	}
	return k
}

// keyedHarness is a runner whose proxy image sends the relay preamble and whose relay checks keys.
func keyedHarness(t *testing.T) (*runnerHarness, *relayKeys) {
	h, _ := sinkHarness(t)
	h.d.cfg.ProxyImage = testProxyRepo + ":" + relayPreambleRelease
	return h, wireRelayKeys(h)
}

// TestRelayKeyedByProxyRelease: only a known proxy release at or after relayPreambleRelease gets a
// relay key, and only in runner mode with the relay's key check wired. An older or unreadable
// release keeps the address rule: a strict-decoding older proxy would refuse relay_key.
func TestRelayKeyedByProxyRelease(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		image string
		want  bool
	}{
		{testProxyRepo + ":0.10.0-rc.23", false},
		{testProxyRepo + ":0.10.0", false},
		{testProxyRepo + ":0.10.1", false},
		{testProxyRepo + ":" + relayPreambleRelease, true},
		{testProxyRepo + ":" + relayPreambleRelease + digest, true},
		{testProxyRepo + ":0.11.0", true},
		{testProxyRepo + ":1.0.0", true},
		{testProxyRepo + digest, false},
		{testProxyRepo + ":dev", false},
		{testProxyRepo + ":latest", false},
		{testProxyRepo, false},
		{"registry.example:5000/wardyn-proxy", false},
	} {
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = c.image
		wireRelayKeys(h)
		if got := h.d.relayKeyed(); got != c.want {
			t.Errorf("%s: keyed %v, want %v", c.image, got, c.want)
		}
	}
	h := newRunnerHarness(t)
	h.d.cfg.ProxyImage = testProxyRepo + ":" + relayPreambleRelease
	if h.d.relayKeyed() {
		t.Error("keyed with no relay key check wired")
	}
	wireRelayKeys(h)
	h.d.cfg.RunnerID = ""
	if h.d.relayKeyed() {
		t.Error("keyed in server mode")
	}
}

// TestRunnerRelayKeyPersistedBeforeHandOff: the keyed proxy's config carries a 32-byte relay_key
// whose sha256 is in the run's record before the relay gets it; the key itself is never stored, and
// a failed save hands the relay nothing.
func TestRunnerRelayKeyPersistedBeforeHandOff(t *testing.T) {
	h, keys := keyedHarness(t)
	out, keyed, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`))
	if err != nil || !keyed {
		t.Fatalf("keyed %v: %v", keyed, err)
	}
	var cfg struct {
		RelayKey string `json:"relay_key"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(cfg.RelayKey)
	if err != nil || len(key) != relayKeyLen {
		t.Fatalf("relay_key %q: %d bytes, %v", cfg.RelayKey, len(key), err)
	}
	if fmt.Sprint(keys.events) != "[relay_key(persisted=true)]" || keys.got[h.runID] != sha256.Sum256(key) {
		t.Fatalf("events %v, handed %x, want the sha256 of the config's key", keys.events, keys.got[h.runID])
	}
	if b, _ := h.ev.LoadEvidence(h.runID); bytes.Contains(b, []byte(cfg.RelayKey)) || bytes.Contains(b, []byte(hex.EncodeToString(key))) {
		t.Fatal("the relay key itself was stored")
	}

	h2, keys2 := keyedHarness(t)
	h2.ev.failSave = func(b []byte) bool { return bytes.Contains(b, []byte("relay_key_hash")) }
	if _, _, err := h2.d.runnerProxyConfig(context.Background(), h2.runID, []byte(`{"run_id":"x"}`)); err == nil || len(keys2.events) != 0 {
		t.Fatalf("a failed save still handed the relay a key: %v %v", err, keys2.events)
	}
}

// TestRunnerUnkeyedProxyGetsNoRelayKey: an older proxy release's config has no relay_key and its run's
// record no hash, so it starts as before and the relay admits it by its address alone.
func TestRunnerUnkeyedProxyGetsNoRelayKey(t *testing.T) {
	h, keys := keyedHarness(t)
	h.d.cfg.ProxyImage = testProxyRepo + ":0.10.0"
	out, keyed, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.ev.LoadEvidence(h.runID)
	if keyed || bytes.Contains(out, []byte("relay_key")) || bytes.Contains(b, []byte("relay_key_hash")) || len(keys.events) != 0 {
		t.Fatalf("an older proxy was keyed: config %s record %s events %v", out, b, keys.events)
	}
}

// relayRecord is a run record holding hash as its relay key hash and node as its proxy's node.
func relayRecord(hash, node string) []byte {
	ev := &runEvidence{RelayKeyHash: hash, Pods: map[string]*podEvidence{}}
	if node != "" {
		ev.Pods[componentProxy] = &podEvidence{Name: "wardyn-proxy-x", NodeName: node}
	}
	b, _ := json.Marshal(ev)
	return b
}

// TestRunnerRelayIPRefusal: the create check refuses a proxy IP a fenced run still holds, as before,
// unless this create keyed its proxy, the runner still keys proxies, the run's record holds a
// well-formed hash and its proxy's node, and every run fenced on the IP recorded its proxy on that
// node. "!" stands for a fenced run's record that cannot be read.
func TestRunnerRelayIPRefusal(t *testing.T) {
	hash := sha256.Sum256([]byte("k"))
	good := hex.EncodeToString(hash[:])
	for _, c := range []struct {
		name     string
		unfenced bool
		unkeyed  bool // the create did not key the proxy
		unhooked bool // RelayKey nil
		image    string
		record   []byte // the run's own record; nil is none
		fNodes   []string
		noFRuns  bool // FencedRuns nil
		server   bool
		unwired  bool // IPFenced nil
		admitted bool
	}{
		{name: "unfenced IP, unkeyed", unfenced: true, unkeyed: true, record: relayRecord("", "n1"), admitted: true},
		{name: "unfenced IP, keyed", unfenced: true, record: relayRecord(good, "n1"), fNodes: []string{"n2"}, admitted: true},
		{name: "fenced IP, unkeyed", unkeyed: true, record: relayRecord("", "n1"), fNodes: []string{"n1"}},
		{name: "fenced IP, no record", unkeyed: true, fNodes: []string{"n1"}},
		{name: "fenced IP, keyed, same node", record: relayRecord(good, "n1"), fNodes: []string{"n1"}, admitted: true},
		{name: "fenced IP, keyed, fenced run on another node", record: relayRecord(good, "n1"), fNodes: []string{"n2"}},
		{name: "fenced IP, keyed, fenced run's node unknown", record: relayRecord(good, "n1"), fNodes: []string{""}},
		{name: "fenced IP, keyed, own node unknown", record: relayRecord(good, ""), fNodes: []string{"n1"}},
		{name: "fenced IP, keyed, both nodes unknown", record: relayRecord(good, ""), fNodes: []string{""}},
		{name: "fenced IP, keyed, fenced run's record unreadable", record: relayRecord(good, "n1"), fNodes: []string{"!"}},
		{name: "fenced IP, keyed, one of two fenced runs on another node", record: relayRecord(good, "n1"), fNodes: []string{"n1", "n2"}},
		{name: "fenced IP, keyed, FencedRuns unwired", record: relayRecord(good, "n1"), fNodes: []string{"n1"}, noFRuns: true},
		{name: "fenced IP, keyed, no fenced run listed", record: relayRecord(good, "n1")},
		{name: "fenced IP, hash present, create did not key", unkeyed: true, record: relayRecord(good, "n1"), fNodes: []string{"n1"}},
		{name: "fenced IP, hash present, RelayKey nil", unhooked: true, record: relayRecord(good, "n1"), fNodes: []string{"n1"}},
		{name: "fenced IP, hash present, image now unkeyed", image: testProxyRepo + ":0.10.0", record: relayRecord(good, "n1"), fNodes: []string{"n1"}},
		{name: "fenced IP, short hash", record: relayRecord("abcd", "n1"), fNodes: []string{"n1"}},
		{name: "fenced IP, own record unreadable", record: []byte(`{`), fNodes: []string{"n1"}},
		{name: "fenced IP, server mode", server: true, unkeyed: true, record: relayRecord("", ""), admitted: true},
		{name: "IPFenced unwired", unwired: true, unkeyed: true, record: relayRecord("", ""), admitted: true},
	} {
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = testProxyRepo + ":" + relayPreambleRelease
		if c.image != "" {
			h.d.cfg.ProxyImage = c.image
		}
		if !c.unhooked {
			wireRelayKeys(h)
		}
		if c.record != nil {
			h.ev.m[h.runID] = c.record
		}
		var fenced []uuid.UUID
		for _, n := range c.fNodes {
			id := uuid.New()
			fenced = append(fenced, id)
			h.ev.m[id] = relayRecord("", n)
			if n == "!" {
				h.ev.m[id] = []byte(`{`)
			}
		}
		h.d.cfg.IPFenced = func(ip string) bool { return !c.unfenced && ip == testProxyIP }
		if c.unwired {
			h.d.cfg.IPFenced = nil
		}
		if !c.noFRuns {
			h.d.cfg.FencedRuns = func(string) []uuid.UUID { return fenced }
		}
		if c.server {
			h.d.cfg.RunnerID = ""
		}
		err := h.d.relayIPRefusal(h.runID, testProxyIP, !c.unkeyed)
		if c.admitted && err != nil || !c.admitted && !errors.Is(err, runner.ErrRelayIPFenced) {
			t.Errorf("%s: %v, admitted want %v", c.name, err, c.admitted)
		}
	}
}

// TestRunnerKeyedOnFencedIPReadsTheBoundNode: the node the check compares is the one the start wait's
// bound read recorded for the run's proxy (observePod), not one it is told.
func TestRunnerKeyedOnFencedIPReadsTheBoundNode(t *testing.T) {
	h, _ := keyedHarness(t)
	if _, _, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	h.boundProxy(t) // the proxy, bound to n1, read through its create-side bracket
	f := uuid.New()
	h.d.cfg.IPFenced = func(string) bool { return true }
	h.d.cfg.FencedRuns = func(string) []uuid.UUID { return []uuid.UUID{f} }
	h.ev.m[f] = relayRecord("", "n1")
	if err := h.d.relayIPRefusal(h.runID, testProxyIP, true); err != nil {
		t.Fatalf("fenced run on the bound node: %v", err)
	}
	h.ev.m[f] = relayRecord("", "n2")
	if err := h.d.relayIPRefusal(h.runID, testProxyIP, true); !errors.Is(err, runner.ErrRelayIPFenced) {
		t.Fatalf("fenced run on another node: %v, want relay_ip_fenced", err)
	}
}

// TestRunnerCreateOnFencedIP: a create whose new proxy pod reports an IP still fenced for another run
// is refused when its proxy is unkeyed, as before. A keyed create runs the keyed check (its key is
// made and handed over) and is refused too here: the fake's proxy pod is bound to no node, so the
// node rule cannot hold.
func TestRunnerCreateOnFencedIP(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		px := httptest.NewServer(http.NotFoundHandler())
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = testProxyRepo + ":" + relayPreambleRelease
		h.d.cfg.Sink = &fakeSink{h: h}
		h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
		h.d.cfg.IPFenced = func(string) bool { return true }
		f := uuid.New()
		h.ev.m[f] = relayRecord("", "n1")
		h.d.cfg.FencedRuns = func(string) []uuid.UUID { return []uuid.UUID{f} }
		var keys *relayKeys
		if keyed {
			keys = wireRelayKeys(h)
		}
		installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
		installAgentRunningReactor(t, h.cs)
		spec := testSandboxSpec()
		spec.RunID = h.runID
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := h.d.CreateSandbox(ctx, spec)
		cancel()
		px.Close()
		if !errors.Is(err, runner.ErrRelayIPFenced) {
			t.Fatalf("keyed %v create on a fenced IP: %v, want relay_ip_fenced", keyed, err)
		}
		if keyed && len(keys.got) != 1 {
			t.Fatalf("the keyed create handed the relay %d keys, want 1", len(keys.got))
		}
	}
}

// TestRunnerReplayRelayKeys: startup hands the relay every persisted relay key hash again, except a
// fenced run's; a run with none is skipped and a malformed one is an error that stops no other.
func TestRunnerReplayRelayKeys(t *testing.T) {
	h, _ := keyedHarness(t)
	if _, _, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	b, _ := h.ev.LoadEvidence(h.runID)
	ev, _ := decodeEvidence(b)
	want, ok := relayKeyHash(ev)
	if !ok {
		t.Fatal("no relay key hash persisted")
	}
	fenced, none, bad := uuid.New(), uuid.New(), uuid.New()
	h.ev.m[fenced] = []byte(`{"relay_key_hash":"` + hex.EncodeToString(want[:]) + `","fence":{"at":"2026-11-17T00:00:00Z"}}`)
	h.ev.m[none] = []byte(`{}`)
	h.ev.m[bad] = []byte(`{"relay_key_hash":"zz"}`)
	restarted := wireRelayKeys(h)
	err := h.d.ReplayRelayKeys([]uuid.UUID{bad, h.runID, fenced, none})
	if err == nil || !strings.Contains(err.Error(), bad.String()) {
		t.Fatalf("malformed hash: %v", err)
	}
	if len(restarted.got) != 1 || restarted.got[h.runID] != want {
		t.Fatalf("replayed %v, want only the live keyed run", restarted.got)
	}
	h.d.cfg.RelayKey = nil
	if err := h.d.ReplayRelayKeys([]uuid.UUID{h.runID}); err != nil {
		t.Fatalf("no relay key check wired: %v", err)
	}
}
