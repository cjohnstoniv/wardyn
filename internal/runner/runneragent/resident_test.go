// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// memKeystore is an in-memory keystore; failSet makes every write fail.
type memKeystore struct {
	mu      sync.Mutex
	items   map[string]string
	failSet bool
	deleted []string
}

func (m *memKeystore) keystore() Keystore {
	return Keystore{
		Set: func(account, value string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.failSet {
				return errors.New("no secret service on this session bus")
			}
			m.items[account] = value
			return nil
		},
		Get: func(account string) (string, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			v, ok := m.items[account]
			if !ok {
				return "", errors.New("not found")
			}
			return v, nil
		},
		Delete: func(account string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.items, account)
			m.deleted = append(m.deleted, account)
			return nil
		},
	}
}

type residentFixture struct {
	ks   *memKeystore
	dir  string
	fake *runnertest.Fake
	d    *Dispatcher
	peer *runnerwire.Peer
	run  uuid.UUID
	id   string
}

func newResidentFixture(t *testing.T, onEvent func(runnerwire.Event) error) *residentFixture {
	t.Helper()
	f := &residentFixture{ks: &memKeystore{items: map[string]string{}}, dir: t.TempDir(), fake: runnertest.NewFake(), run: uuid.New(), id: uuid.NewString()}
	res, err := NewResident(f.ks.keystore(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.d, err = newTestDispatcher(Config{RunnerID: f.id, Substrate: f.fake, Resident: res,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	if onEvent == nil {
		onEvent = func(runnerwire.Event) error { return nil }
	}
	org := runnerwire.NewPeer(a, runnerwire.PeerConfig{Org: true, OnEventChecked: onEvent})
	f.peer = runnerwire.NewPeer(b, runnerwire.PeerConfig{})
	t.Cleanup(func() { org.Close(); f.peer.Close() })
	go org.Run(t.Context())    //nolint:errcheck // the test owns shutdown
	go f.peer.Run(t.Context()) //nolint:errcheck // the test owns shutdown
	return f
}

func (f *residentFixture) deliver(grant uuid.UUID, value string, sandbox bool) error {
	data, _ := runnerwire.Marshal(runnerwire.DeliverResidentArgs{RunID: f.run, GrantID: grant, Kind: "env_secret", Value: value, Sandbox: sandbox})
	_, err := f.d.Call(context.Background(), f.peer, 2, runnerwire.MethodDeliverResident, data)
	return err
}

func (f *residentFixture) create(spec runner.SandboxSpec) error {
	spec.RunID, spec.RunnerID = f.run, f.id
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	_, err := f.d.Call(context.Background(), f.peer, 3, runnerwire.MethodCreateSandbox, data)
	return err
}

// A keystore that cannot hold the value refuses the delivery with
// runner_keystore_unavailable, and nothing falls back to a file.
func TestResident_KeystoreFailureRefuses(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.ks.failSet = true
	grant := uuid.New()
	err := f.deliver(grant, "s3cret-value", true)
	if err == nil || !strings.Contains(err.Error(), string(placement.ReasonRunnerKeystoreUnavailable)) {
		t.Fatalf("deliver = %v, want %s", err, placement.ReasonRunnerKeystoreUnavailable)
	}
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(f.dir, e.Name())); strings.Contains(string(b), "s3cret-value") {
			t.Fatalf("%s holds the value after a refused delivery", e.Name())
		}
	}
	if reloaded, err := NewResident(f.ks.keystore(), f.dir); err != nil || len(reloaded.held) != 0 {
		t.Fatalf("index after a refused delivery = %v (%v), want empty", reloaded.held, err)
	}
	if err := f.create(runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(grant)}}); err == nil || len(f.fake.Created) != 0 {
		t.Fatalf("create after a refused delivery = %v (created %d)", err, len(f.fake.Created))
	}
}

// A grant delivered for the proxy only fills the proxy configuration and is
// refused in SecretEnv and ManagedFiles; a grant delivered for the sandbox
// fills both. The spec the lifecycle handlers see keeps the placeholder.
func TestResident_ProxyOnlyNeverReachesTheSandbox(t *testing.T) {
	f := newResidentFixture(t, nil)
	proxyOnly, sandbox := uuid.New(), uuid.New()
	if err := f.deliver(proxyOnly, "proxy-only-pat", false); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(sandbox, "sandbox-secret", true); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runner.SandboxSpec{
		"secret env":    {SecretEnv: map[string]string{"AZURE_DEVOPS_EXT_PAT": placement.ResidentPlaceholder(proxyOnly)}},
		"managed files": {ManagedFiles: []runner.ManagedFile{{Path: "/run/pat", AgentOwned: true, Content: []byte(placement.ResidentPlaceholder(proxyOnly))}}},
	} {
		if err := f.create(spec); err == nil {
			t.Fatalf("%s: a proxy-only grant reached the sandbox", name)
		}
	}
	if len(f.fake.Created) != 0 {
		t.Fatalf("created %d sandboxes, want none", len(f.fake.Created))
	}
	var persisted runner.SandboxSpec
	f.d.cfg.Created = func(_ context.Context, s runner.SandboxSpec, _ string) error { persisted = s; return nil }
	// A credential field of the proxy configuration is filled; a field that is
	// not one (a detail string the sandbox can read) keeps the placeholder.
	err := f.create(runner.SandboxSpec{
		SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(sandbox)},
		ProxyConfig: runner.ProxyConfig{
			PATGrants:            map[string]proxy.PATGrant{"dev.azure.com": {GrantID: proxyOnly, Username: placement.ResidentPlaceholder(proxyOnly)}},
			LLMUnavailableDetail: placement.ResidentPlaceholder(proxyOnly),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.fake.Created[0]
	if got.SecretEnv["TOKEN"] != "sandbox-secret" || got.ProxyConfig.PATGrants["dev.azure.com"].Username != "proxy-only-pat" {
		t.Fatalf("substrate spec: env=%v proxy=%+v", got.SecretEnv, got.ProxyConfig.PATGrants)
	}
	if got.ProxyConfig.LLMUnavailableDetail != placement.ResidentPlaceholder(proxyOnly) {
		t.Fatalf("a non-credential proxy field was filled: %q", got.ProxyConfig.LLMUnavailableDetail)
	}
	if persisted.SecretEnv["TOKEN"] != placement.ResidentPlaceholder(sandbox) || persisted.ProxyConfig.PATGrants["dev.azure.com"].Username != placement.ResidentPlaceholder(proxyOnly) {
		t.Fatalf("persisted spec carries a value: %+v", persisted.SecretEnv)
	}
	ref := placement.RunnerRefPrefix(f.id) + "fake-" + f.run.String()
	cfg := `{"run_id":"` + f.run.String() + `","pat_grants":{"dev.azure.com":{"grant_id":"` + proxyOnly.String() + `","username":"` + placement.ResidentPlaceholder(proxyOnly) + `"}},"llm_unavailable_detail":"` + placement.ResidentPlaceholder(proxyOnly) + `"}`
	data, _ := runnerwire.Marshal(runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: []byte(cfg)})
	if _, err := f.d.Call(context.Background(), f.peer, 4, runnerwire.MethodReplaceProxy, data); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(f.fake.Calls, func(c string) bool { return strings.HasPrefix(c, "replace_proxy ") })
	if i < 0 || !strings.Contains(f.fake.Calls[i], `"username":"proxy-only-pat"`) || !strings.Contains(f.fake.Calls[i], `"llm_unavailable_detail":"`+placement.ResidentPlaceholder(proxyOnly)) {
		t.Fatalf("revive re-applied the wrong fields: %v", f.fake.Calls)
	}
}

// At run end every value is deleted from the keystore and the index, and each
// grant's erase is reported as a resident_erase event.
func TestResident_ErasedAtRunEnd(t *testing.T) {
	var mu sync.Mutex
	var events []runnerwire.ResidentErase
	f := newResidentFixture(t, func(ev runnerwire.Event) error {
		var e runnerwire.ResidentErase
		if ev.Kind == runnerwire.EventResidentErase && runnerwire.Unmarshal(ev.Data, &e) == nil {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
		return nil
	})
	grants := []uuid.UUID{uuid.New(), uuid.New()}
	for _, g := range grants {
		if err := f.deliver(g, "v-"+g.String(), true); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := runnerwire.Marshal(runnerwire.EraseResidentArgs{RunID: f.run})
	if _, err := f.d.Call(context.Background(), f.peer, 5, runnerwire.MethodEraseResident, data); err != nil {
		t.Fatal(err)
	}
	if len(f.ks.items) != 0 || len(f.ks.deleted) != 2 {
		t.Fatalf("keystore after erase: items=%v deleted=%v", f.ks.items, f.ks.deleted)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Outcome != runnerwire.ResidentErased || events[1].Outcome != runnerwire.ResidentErased {
		t.Fatalf("resident_erase events = %+v", events)
	}
	reloaded, err := NewResident(f.ks.keystore(), f.dir)
	if err != nil || len(reloaded.held) != 0 {
		t.Fatalf("index after erase = %v (%v)", reloaded.held, err)
	}
}

// A keystore delete that fails keeps the grant indexed, so the next erase
// retries it instead of orphaning the value.
func TestResident_FailedEraseIsRetried(t *testing.T) {
	f := newResidentFixture(t, nil)
	grant := uuid.New()
	if err := f.deliver(grant, "v", true); err != nil {
		t.Fatal(err)
	}
	failing := f.ks.keystore()
	failing.Delete = func(string) error { return errors.New("keychain locked") }
	f.d.cfg.Resident.keystore = failing
	data, _ := runnerwire.Marshal(runnerwire.EraseResidentArgs{RunID: f.run})
	if _, err := f.d.Call(context.Background(), f.peer, 5, runnerwire.MethodEraseResident, data); err != nil {
		t.Fatal(err)
	}
	if reloaded, err := NewResident(f.ks.keystore(), f.dir); err != nil || len(reloaded.held[f.run]) != 1 {
		t.Fatalf("index after a failed erase = %v (%v), want the grant kept", reloaded.held, err)
	}
	f.d.cfg.Resident.keystore = f.ks.keystore()
	if _, err := f.d.Call(context.Background(), f.peer, 6, runnerwire.MethodEraseResident, data); err != nil {
		t.Fatal(err)
	}
	if len(f.ks.items) != 0 {
		t.Fatalf("keystore after the retried erase = %v", f.ks.items)
	}
}

// A keystore call that never returns (an unanswered OS prompt) is refused
// after keystoreTimeout; the grant stays indexed so erase still covers it.
func TestResident_KeystorePromptIsBounded(t *testing.T) {
	f := newResidentFixture(t, nil)
	prev := keystoreTimeout
	keystoreTimeout = 20 * time.Millisecond
	t.Cleanup(func() { keystoreTimeout = prev })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck := f.ks.keystore()
	stuck.Set = func(string, string) error { <-release; return nil }
	f.d.cfg.Resident.keystore = stuck
	err := f.deliver(uuid.New(), "v", true)
	if err == nil || !strings.Contains(err.Error(), string(placement.ReasonRunnerKeystoreUnavailable)) {
		t.Fatalf("deliver = %v, want %s after the timeout", err, placement.ReasonRunnerKeystoreUnavailable)
	}
	if reloaded, err := NewResident(f.ks.keystore(), f.dir); err != nil || len(reloaded.held[f.run]) != 1 {
		t.Fatalf("index after a timed-out write = %v (%v), want the grant kept", reloaded.held, err)
	}
}

// A value past its NotAfter is erased by the runner's own sweep, with no org erase: a run whose end never
// reached the runner does not leave its value in the keystore. A failed delete keeps the grant indexed for
// the next sweep; a value still within its NotAfter, or with none, is left alone.
func TestResident_ExpiredValuesAreErased(t *testing.T) {
	ks := &memKeystore{items: map[string]string{}}
	dir := t.TempDir()
	r, err := NewResident(ks.keystore(), dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	run, expired, live, unbounded := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for g, notAfter := range map[uuid.UUID]time.Time{expired: now.Add(-time.Minute), live: now.Add(time.Hour), unbounded: {}} {
		if err := r.track(run, g, &residentGrant{NotAfter: notAfter}); err != nil {
			t.Fatal(err)
		}
		ks.items[residentAccount(run, g)] = "v"
	}
	failing := ks.keystore()
	failing.Delete = func(string) error { return errors.New("keychain locked") }
	r.keystore = failing
	if err := r.EraseExpired(context.Background(), now); err == nil {
		t.Fatal("a failed delete was not reported")
	}
	if reloaded, err := NewResident(ks.keystore(), dir); err != nil || len(reloaded.held[run]) != 3 {
		t.Fatalf("index after a failed delete = %v (%v), want every grant kept", reloaded.held, err)
	}
	r.keystore = ks.keystore()
	if err := r.EraseExpired(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, ok := ks.items[residentAccount(run, expired)]; ok || len(ks.items) != 2 {
		t.Fatalf("keystore after the sweep = %v, want only the two unexpired values", ks.items)
	}
	reloaded, err := NewResident(ks.keystore(), dir)
	if _, ok := reloaded.held[run][expired]; err != nil || ok || len(reloaded.held[run]) != 2 {
		t.Fatalf("index after the sweep = %v (%v), want the expired grant gone", reloaded.held, err)
	}
}
