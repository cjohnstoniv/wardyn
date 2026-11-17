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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	out, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`))
	if err != nil {
		t.Fatal(err)
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
	if _, err := h2.d.runnerProxyConfig(context.Background(), h2.runID, []byte(`{"run_id":"x"}`)); err == nil || len(keys2.events) != 0 {
		t.Fatalf("a failed save still handed the relay a key: %v %v", err, keys2.events)
	}
}

// TestRunnerUnkeyedProxyGetsNoRelayKey: an older proxy release's config has no relay_key and its run's
// record no hash, so it starts as before and the relay admits it by its address alone.
func TestRunnerUnkeyedProxyGetsNoRelayKey(t *testing.T) {
	h, keys := keyedHarness(t)
	h.d.cfg.ProxyImage = testProxyRepo + ":0.10.0"
	out, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.ev.LoadEvidence(h.runID)
	if bytes.Contains(out, []byte("relay_key")) || bytes.Contains(b, []byte("relay_key_hash")) || len(keys.events) != 0 {
		t.Fatalf("an older proxy was keyed: config %s record %s events %v", out, b, keys.events)
	}
}

// TestRunnerRelayIPRefusal: the create check refuses a proxy IP a fenced run still holds, exactly as
// before, unless the run's record holds a well-formed relay key hash.
func TestRunnerRelayIPRefusal(t *testing.T) {
	hash := sha256.Sum256([]byte("k"))
	for _, c := range []struct {
		name    string
		server  bool
		unwired bool
		fenced  bool
		record  string
		refused bool
	}{
		{name: "unfenced IP, unkeyed", record: `{}`},
		{name: "unfenced IP, keyed", record: `{"relay_key_hash":"` + hex.EncodeToString(hash[:]) + `"}`},
		{name: "fenced IP, unkeyed", fenced: true, record: `{}`, refused: true},
		{name: "fenced IP, no record", fenced: true, refused: true},
		{name: "fenced IP, keyed", fenced: true, record: `{"relay_key_hash":"` + hex.EncodeToString(hash[:]) + `"}`},
		{name: "fenced IP, short hash", fenced: true, record: `{"relay_key_hash":"abcd"}`, refused: true},
		{name: "fenced IP, unreadable record", fenced: true, record: `{`, refused: true},
		{name: "fenced IP, server mode", server: true, fenced: true, record: `{}`},
		{name: "IPFenced unwired", unwired: true, record: `{}`},
	} {
		h := newRunnerHarness(t)
		if c.record != "" {
			h.ev.m[h.runID] = []byte(c.record)
		}
		h.d.cfg.IPFenced = func(ip string) bool { return c.fenced && ip == testProxyIP }
		if c.unwired {
			h.d.cfg.IPFenced = nil
		}
		if c.server {
			h.d.cfg.RunnerID = ""
		}
		err := h.d.relayIPRefusal(h.runID, testProxyIP)
		if c.refused != errors.Is(err, runner.ErrRelayIPFenced) || !c.refused && err != nil {
			t.Errorf("%s: %v, refused want %v", c.name, err, c.refused)
		}
	}
}

// TestRunnerCreateOnFencedIP: a create whose new proxy pod reports an IP still fenced for another run
// is refused when its proxy is unkeyed, as before, and starts when it is keyed, with the Secret's
// relay_key matching the hash the relay holds.
func TestRunnerCreateOnFencedIP(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		px := httptest.NewServer(http.NotFoundHandler())
		h := newRunnerHarness(t)
		h.d.cfg.ProxyImage = testProxyRepo + ":" + relayPreambleRelease
		h.d.cfg.Sink = &fakeSink{h: h}
		h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
		h.d.cfg.IPFenced = func(string) bool { return true }
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
		if !keyed {
			if !errors.Is(err, runner.ErrRelayIPFenced) {
				t.Fatalf("unkeyed create on a fenced IP: %v, want relay_ip_fenced", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("keyed create on a fenced IP: %v", err)
		}
		secrets, err := h.cs.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			RelayKey string `json:"relay_key"`
		}
		for _, s := range secrets.Items {
			if s.Name == secretName(spec.RunID) {
				_ = json.Unmarshal(s.Data[proxyConfigSecretKey], &cfg)
			}
		}
		key, _ := base64.StdEncoding.DecodeString(cfg.RelayKey)
		if len(key) != relayKeyLen || keys.got[spec.RunID] != sha256.Sum256(key) {
			t.Fatalf("Secret relay_key %q does not match the hash the relay holds", cfg.RelayKey)
		}
	}
}

// TestRunnerReplayRelayKeys: startup hands the relay every persisted relay key hash again, except a
// fenced run's; a run with none is skipped and a malformed one is an error that stops no other.
func TestRunnerReplayRelayKeys(t *testing.T) {
	h, _ := keyedHarness(t)
	if _, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"run_id":"x"}`)); err != nil {
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
