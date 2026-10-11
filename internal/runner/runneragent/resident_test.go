// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

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
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Fatalf("state dir holds %v after a refused delivery, want nothing", entries)
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
	err := f.create(runner.SandboxSpec{
		SecretEnv:   map[string]string{"TOKEN": placement.ResidentPlaceholder(sandbox)},
		ProxyConfig: runner.ProxyConfig{LLMUnavailableDetail: placement.ResidentPlaceholder(proxyOnly)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.fake.Created[0]
	if got.SecretEnv["TOKEN"] != "sandbox-secret" || got.ProxyConfig.LLMUnavailableDetail != "proxy-only-pat" {
		t.Fatalf("substrate spec: env=%v proxy=%q", got.SecretEnv, got.ProxyConfig.LLMUnavailableDetail)
	}
	if persisted.SecretEnv["TOKEN"] != placement.ResidentPlaceholder(sandbox) || persisted.ProxyConfig.LLMUnavailableDetail != placement.ResidentPlaceholder(proxyOnly) {
		t.Fatalf("persisted spec carries a value: %+v", persisted.SecretEnv)
	}
	ref := placement.RunnerRefPrefix(f.id) + "fake-" + f.run.String()
	data, _ := runnerwire.Marshal(runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: []byte(`{"run_id":"` + f.run.String() + `","token":"` + placement.ResidentPlaceholder(proxyOnly) + `"}`)})
	if _, err := f.d.Call(context.Background(), f.peer, 4, runnerwire.MethodReplaceProxy, data); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.fake.Calls, func(c string) bool { return strings.Contains(c, `"token":"proxy-only-pat"`) }) {
		t.Fatalf("revive did not re-apply the value from the keystore: %v", f.fake.Calls)
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
