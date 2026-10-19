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

// memKeystore is an in-memory keystore that records the order of its calls
// (and, through traceSaves, of the index saves) and every call that started
// on an account while another call on it was still running. setGate, when
// set, holds each Set before it writes; deleteAllGate holds DeleteAll.
type memKeystore struct {
	mu            sync.Mutex
	items         map[string]string
	ops           []string
	inflight      map[string]bool
	overlaps      []string
	failSet       bool
	failDelete    bool
	precheckErr   error
	setGate       chan struct{}
	deleteGate    chan struct{}
	deleteAllGate chan struct{}
	setStarted    chan string
	onSet         func(account string)
	// lockFree, when set, is checked at every call: no keystore call is made
	// while Resident.mu is held.
	lockFree *Resident
	lockHeld []string
}

func (m *memKeystore) log(op string) {
	m.mu.Lock()
	m.ops = append(m.ops, op)
	m.mu.Unlock()
}

func (m *memKeystore) begin(op, account string) {
	if r := m.lockFree; r != nil {
		if r.mu.TryLock() {
			r.mu.Unlock()
		} else {
			m.mu.Lock()
			m.lockHeld = append(m.lockHeld, op)
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops = append(m.ops, op+" "+account)
	if m.inflight[account] {
		m.overlaps = append(m.overlaps, op+" "+account)
	}
	m.inflight[account] = true
}

func (m *memKeystore) end(account string) {
	m.mu.Lock()
	delete(m.inflight, account)
	m.mu.Unlock()
}

func (m *memKeystore) keystore() Keystore {
	return Keystore{
		Precheck: func(string, string) error { return m.precheckErr },
		Set: func(account, value string) error {
			m.begin("set", account)
			defer m.end(account)
			if m.onSet != nil {
				m.onSet(account)
			}
			if m.setStarted != nil {
				m.setStarted <- account
			}
			if m.setGate != nil {
				<-m.setGate
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.failSet {
				return errors.New("no secret service on this session bus")
			}
			m.items[account] = value
			return nil
		},
		Get: func(account string) (string, error) {
			m.begin("get", account)
			defer m.end(account)
			m.mu.Lock()
			defer m.mu.Unlock()
			v, ok := m.items[account]
			if !ok {
				return "", errors.New("not found")
			}
			return v, nil
		},
		Delete: func(account string) error {
			m.begin("delete", account)
			defer m.end(account)
			if m.deleteGate != nil {
				<-m.deleteGate
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.failDelete {
				return errors.New("keychain locked")
			}
			delete(m.items, account)
			return nil
		},
		DeleteAll: func() error {
			m.begin("deleteall", "*")
			defer m.end("*")
			if m.deleteAllGate != nil {
				<-m.deleteAllGate
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			clear(m.items)
			return nil
		},
	}
}

func (m *memKeystore) opsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ops)
}

func (m *memKeystore) count(prefix string) int {
	n := 0
	for _, op := range m.opsSnapshot() {
		if strings.HasPrefix(op, prefix) {
			n++
		}
	}
	return n
}

func (m *memKeystore) item(account string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[account]
	return v, ok
}

func (m *memKeystore) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// traceSaves logs each durable-save step into ks's op order, and fails the
// step named fault ("fsync tmp", "rename" or "fsync dir").
func traceSaves(t *testing.T, ks *memKeystore, fault *string) {
	t.Helper()
	prevSync, prevRename := syncFile, renameFile
	t.Cleanup(func() { syncFile, renameFile = prevSync, prevRename })
	step := func(name string) error {
		ks.log(name)
		if fault != nil && *fault == name {
			return errors.New("injected " + name + " fault")
		}
		return nil
	}
	syncFile = func(f *os.File) error {
		name := "fsync dir"
		if strings.HasSuffix(f.Name(), ".tmp") {
			name = "fsync tmp"
		}
		if err := step(name); err != nil {
			return err
		}
		return prevSync(f)
	}
	renameFile = func(from, to string) error {
		if err := step("rename"); err != nil {
			return err
		}
		return prevRename(from, to)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

type residentFixture struct {
	ks    *memKeystore
	clock *fakeClock
	dir   string
	fake  *runnertest.Fake
	res   *Resident
	d     *Dispatcher
	peer  *runnerwire.Peer
	run   uuid.UUID
	id    string
	// admit is what Resident's admit hook runs; nil admits.
	admit func(uuid.UUID) error

	evMu   sync.Mutex
	events []runnerwire.ResidentErase
}

// newResidentFixture builds a dispatcher with a Resident over memKeystore.
// onEvent, when set, answers each resident_erase event after it is recorded.
func newResidentFixture(t *testing.T, onEvent func(runnerwire.Event) error) *residentFixture {
	t.Helper()
	f := &residentFixture{ks: &memKeystore{items: map[string]string{}, inflight: map[string]bool{}}, clock: &fakeClock{t: time.Now()}, dir: t.TempDir(), fake: runnertest.NewFake(), run: uuid.New(), id: uuid.NewString()}
	var err error
	f.res, err = NewResident(f.ks.keystore(), f.dir, func(run uuid.UUID) error {
		if f.admit != nil {
			return f.admit(run)
		}
		return nil
	}, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	f.d, err = newTestDispatcher(Config{RunnerID: f.id, Substrate: f.fake, Resident: f.res,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.connect(t, onEvent)
	return f
}

// connect gives the runner a fresh connection to an org that records every
// resident_erase event; an event the org refuses ends the connection.
func (f *residentFixture) connect(t *testing.T, onEvent func(runnerwire.Event) error) {
	t.Helper()
	a, b := runnerwire.Loopback()
	org := runnerwire.NewPeer(a, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		var e runnerwire.ResidentErase
		if ev.Kind == runnerwire.EventResidentErase && runnerwire.Unmarshal(ev.Data, &e) == nil {
			f.evMu.Lock()
			f.events = append(f.events, e)
			f.evMu.Unlock()
		}
		if onEvent != nil {
			return onEvent(ev)
		}
		return nil
	}})
	f.peer = runnerwire.NewPeer(b, runnerwire.PeerConfig{})
	t.Cleanup(func() { org.Close(); f.peer.Close() })
	go org.Run(t.Context())    //nolint:errcheck // the test owns shutdown
	go f.peer.Run(t.Context()) //nolint:errcheck // the test owns shutdown
}

func (f *residentFixture) deliverTo(run, grant uuid.UUID, value string, sandbox bool, notAfter time.Time) error {
	data, _ := runnerwire.Marshal(runnerwire.DeliverResidentArgs{RunID: run, GrantID: grant, Kind: "env_secret", Value: value, Sandbox: sandbox, NotAfter: notAfter})
	_, err := f.d.Call(context.Background(), f.peer, 2, runnerwire.MethodDeliverResident, data)
	return err
}

func (f *residentFixture) deliver(grant uuid.UUID, value string, sandbox bool) error {
	return f.deliverTo(f.run, grant, value, sandbox, f.clock.now().Add(time.Hour))
}

func (f *residentFixture) erase() error {
	data, _ := runnerwire.Marshal(runnerwire.EraseResidentArgs{RunID: f.run})
	_, err := f.d.Call(context.Background(), f.peer, 5, runnerwire.MethodEraseResident, data)
	return err
}

func (f *residentFixture) create(spec runner.SandboxSpec) error {
	spec.RunID, spec.RunnerID = f.run, f.id
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	_, err := f.d.Call(context.Background(), f.peer, 3, runnerwire.MethodCreateSandbox, data)
	return err
}

func (f *residentFixture) takeEvents() []runnerwire.ResidentErase {
	f.evMu.Lock()
	defer f.evMu.Unlock()
	out := f.events
	f.events = nil
	return out
}

func (f *residentFixture) entry(grant uuid.UUID) (residentGrant, bool) {
	f.res.mu.Lock()
	defer f.res.mu.Unlock()
	return f.res.entry(f.run, grant)
}

// settled waits until no keystore call of r is in flight: a call the caller
// stopped waiting for records its result only when it really returns.
func settled(t *testing.T, r *Resident) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		idle := len(r.busy) == 0 && !r.deleteAll
		r.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a keystore call never returned")
		}
		time.Sleep(time.Millisecond)
	}
}

func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := keystoreTimeout
	keystoreTimeout = d
	t.Cleanup(func() { keystoreTimeout = prev })
}

func refusedUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), string(placement.ReasonRunnerKeystoreUnavailable))
}

// A keystore that cannot hold the value refuses the delivery with
// runner_keystore_unavailable, nothing falls back to a file, and the entry is
// erased, never held.
func TestResident_KeystoreFailureRefuses(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.ks.failSet = true
	grant := uuid.New()
	if err := f.deliver(grant, "s3cret-value", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want %s", err, placement.ReasonRunnerKeystoreUnavailable)
	}
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(f.dir, e.Name())); strings.Contains(string(b), "s3cret-value") {
			t.Fatalf("%s holds the value after a refused delivery", e.Name())
		}
	}
	reloaded, err := NewResident(f.ks.keystore(), f.dir, nil, f.clock.now)
	if g, ok := reloaded.idx.Grants[f.run][grant]; err != nil || (ok && g.State != grantErased) {
		t.Fatalf("index after a refused delivery = %+v (%v), want no held entry", reloaded.idx.Grants, err)
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
		// A failed create erases its run's values, so each refusal gets its
		// own run.
		r := newResidentFixture(t, nil)
		if err := r.deliver(proxyOnly, "proxy-only-pat", false); err != nil {
			t.Fatal(err)
		}
		if err := r.create(spec); err == nil || len(r.fake.Created) != 0 {
			t.Fatalf("%s: a proxy-only grant reached the sandbox (%v)", name, err)
		}
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
// grant's erase is reported as a resident_erase event with its report id.
func TestResident_ErasedAtRunEnd(t *testing.T) {
	f := newResidentFixture(t, nil)
	grants := []uuid.UUID{uuid.New(), uuid.New()}
	for _, g := range grants {
		if err := f.deliver(g, "v-"+g.String(), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	if f.ks.size() != 0 || f.ks.count("delete ") != 2 {
		t.Fatalf("keystore after erase: items=%d ops=%v", f.ks.size(), f.ks.opsSnapshot())
	}
	events := f.takeEvents()
	if len(events) != 2 || events[0].Outcome != runnerwire.ResidentErased || events[1].Outcome != runnerwire.ResidentErased || events[0].ReportID == uuid.Nil || events[0].ReportID == events[1].ReportID {
		t.Fatalf("resident_erase events = %+v", events)
	}
	reloaded, err := NewResident(f.ks.keystore(), f.dir, nil, f.clock.now)
	if err != nil || len(reloaded.idx.Grants) != 0 {
		t.Fatalf("index after erase = %v (%v)", reloaded.idx.Grants, err)
	}
}

// A keystore delete that fails keeps the grant indexed and erase returns the
// retryable error while any value is held; the next erase retries it.
func TestResident_EraseReturnsErrorWhileAnyHeld(t *testing.T) {
	f := newResidentFixture(t, nil)
	grant := uuid.New()
	if err := f.deliver(grant, "v", true); err != nil {
		t.Fatal(err)
	}
	f.ks.failDelete = true
	if err := f.erase(); !errors.Is(err, errEraseIncomplete) {
		t.Fatalf("erase with a failed delete = %v, want %v", err, errEraseIncomplete)
	}
	if ev := f.takeEvents(); len(ev) != 1 || ev[0].Outcome != runnerwire.ResidentEraseFailed || ev[0].ReportID == uuid.Nil {
		t.Fatalf("events after a failed delete = %+v", ev)
	}
	if reloaded, err := NewResident(f.ks.keystore(), f.dir, nil, f.clock.now); err != nil || len(reloaded.idx.Grants[f.run]) != 1 {
		t.Fatalf("index after a failed erase = %v (%v), want the grant kept", reloaded.idx.Grants, err)
	}
	f.ks.failDelete = false
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	if f.ks.size() != 0 {
		t.Fatalf("keystore after the retried erase holds %d items", f.ks.size())
	}
}

// A keystore call that never returns (an unanswered OS prompt) is refused
// after keystoreTimeout; the grant stays indexed, unsettled, so erase still
// covers it.
func TestResident_KeystorePromptIsBounded(t *testing.T) {
	f := newResidentFixture(t, nil)
	shortTimeout(t, 20*time.Millisecond)
	f.ks.setGate = make(chan struct{})
	t.Cleanup(func() { close(f.ks.setGate); settled(t, f.res) })
	if err := f.deliver(uuid.New(), "v", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want %s after the timeout", err, placement.ReasonRunnerKeystoreUnavailable)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "resident.json"))
	if err != nil || !strings.Contains(string(b), `"state":"writing"`) || !strings.Contains(string(b), `"unsettled":true`) {
		t.Fatalf("index after a timed-out write = %s (%v), want the grant kept writing and unsettled", b, err)
	}
}

// The hand-off pin: a write that outlives its timeout cannot leave a value
// behind an erase. The erase meets the busy account, issues no delete and
// reports failed; the write lands; its completion deletes the value; the next
// erase reports erased. No two calls on the account ever overlap.
func TestResident_LateWriteAfterEraseLeavesNothing(t *testing.T) {
	f := newResidentFixture(t, nil)
	shortTimeout(t, 50*time.Millisecond)
	f.ks.setGate = make(chan struct{})
	grant := uuid.New()
	account := residentAccount(f.run, grant)
	if err := f.deliver(grant, "late", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want a timeout refusal", err)
	}
	if err := f.erase(); err == nil {
		t.Fatal("erase returned nil while the write was in flight")
	}
	if n := f.ks.count("delete "); n != 0 {
		t.Fatalf("erase issued %d deletes while the write was in flight: %v", n, f.ks.opsSnapshot())
	}
	failed := f.takeEvents()
	if len(failed) != 1 || failed[0].Outcome != runnerwire.ResidentEraseFailed {
		t.Fatalf("events while the write was in flight = %+v, want one failed", failed)
	}
	close(f.ks.setGate)
	settled(t, f.res)
	if _, ok := f.ks.item(account); ok {
		t.Fatal("the late write's value survived its completion")
	}
	if err := f.erase(); err != nil {
		t.Fatalf("erase after the late write settled = %v", err)
	}
	erased := f.takeEvents()
	if len(erased) != 1 || erased[0].Outcome != runnerwire.ResidentErased || erased[0].ReportID == failed[0].ReportID {
		t.Fatalf("events after the late write settled = %+v (failed id %s)", erased, failed[0].ReportID)
	}
	if f.ks.size() != 0 || len(f.ks.overlaps) != 0 {
		t.Fatalf("keystore items=%d overlapping calls=%v", f.ks.size(), f.ks.overlaps)
	}
}

// Deliver's tombstone check, admission, writing save and busy mark are one
// critical section, so an erase that straddles a delivery either comes first
// (the delivery is refused) or waits for it and lists its grant.
func TestResident_EraseDuringAdmitCannotLeaveValue(t *testing.T) {
	t.Run("deliver first", func(t *testing.T) {
		f := newResidentFixture(t, nil)
		entered, release := make(chan struct{}), make(chan struct{})
		f.admit = func(uuid.UUID) error { close(entered); <-release; return nil }
		f.ks.setGate = make(chan struct{})
		grant := uuid.New()
		delivered := make(chan error, 1)
		go func() { delivered <- f.deliver(grant, "v", true) }()
		<-entered
		erased := make(chan error, 1)
		go func() { erased <- f.erase() }()
		select {
		case err := <-erased:
			t.Fatalf("erase finished (%v) while a delivery was inside its critical section", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-erased; err == nil {
			t.Fatal("erase returned nil with the delivery's write in flight")
		}
		if ev := f.takeEvents(); len(ev) != 1 || ev[0].GrantID != grant || ev[0].Outcome != runnerwire.ResidentEraseFailed {
			t.Fatalf("erase's list = %+v, want the delivery's grant", ev)
		}
		if g, _ := f.entry(grant); g.State != grantWriting || !g.EraseRequested {
			t.Fatalf("entry erase saw = %+v, want writing with the erase requested", g)
		}
		close(f.ks.setGate)
		if err := <-delivered; err == nil {
			t.Fatal("a delivery its run's erase overtook answered success")
		}
		settled(t, f.res)
		if err := f.erase(); err != nil || f.ks.size() != 0 {
			t.Fatalf("second erase = %v, keystore holds %d", err, f.ks.size())
		}
	})
	t.Run("erase first", func(t *testing.T) {
		f := newResidentFixture(t, nil)
		if err := f.erase(); err != nil {
			t.Fatal(err)
		}
		if err := f.deliver(uuid.New(), "v", true); err == nil || f.ks.count("set ") != 0 {
			t.Fatalf("delivery after its run's erase = %v (ops %v), want refused with no write", err, f.ks.opsSnapshot())
		}
	})
}

// The delivery's answer is read from the final state: a write that succeeded
// after its run's erase asked for the delete is refused, not reported held.
func TestResident_AnswerReadsFinalState(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.ks.setGate = make(chan struct{})
	f.ks.setStarted = make(chan string, 1)
	grant := uuid.New()
	delivered := make(chan error, 1)
	go func() { delivered <- f.deliver(grant, "v", true) }()
	<-f.ks.setStarted
	if err := f.erase(); err == nil {
		t.Fatal("erase returned nil with a write in flight")
	}
	close(f.ks.setGate)
	err := <-delivered
	if err == nil || !strings.Contains(err.Error(), "not held") {
		t.Fatalf("deliver = %v, want a refusal read from the final state", err)
	}
	settled(t, f.res)
	if g, _ := f.entry(grant); g.State != grantErased {
		t.Fatalf("entry = %+v, want erased", g)
	}
	other := newResidentFixture(t, nil)
	if err := other.deliver(uuid.New(), "v", true); err != nil {
		t.Fatalf("an undisturbed delivery = %v", err)
	}
}

// Lock order: admit (the lifecycle manager's lock) is taken only inside
// Resident.mu, no keystore call is made while Resident.mu is held, and the
// lifecycle manager's package never imports this one, so it cannot call back.
func TestResident_LockOrder(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.ks.lockFree = f.res
	var outside []uuid.UUID
	f.admit = func(run uuid.UUID) error {
		if f.res.mu.TryLock() {
			f.res.mu.Unlock()
			outside = append(outside, run)
		}
		return nil
	}
	ctx := context.Background()
	grant := uuid.New()
	for _, g := range []uuid.UUID{grant, uuid.New()} {
		if err := f.deliver(g, "v", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.create(runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(grant)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	if err := f.deliverTo(uuid.New(), uuid.New(), "v", false, f.clock.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.res.EraseExpired(ctx, f.clock.add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.deliverTo(uuid.New(), uuid.New(), "v", false, f.clock.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.res.EraseAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(outside) != 0 || len(f.ks.lockHeld) != 0 {
		t.Fatalf("admit outside Resident.mu for %v; keystore calls under Resident.mu: %v", outside, f.ks.lockHeld)
	}
	if f.ks.count("deleteall") == 0 {
		t.Fatal("the idle backstop never ran, so its lock discipline went unchecked")
	}
	files, _ := filepath.Glob(filepath.Join("..", "offline", "*.go"))
	if len(files) == 0 {
		t.Fatal("no offline sources found")
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"`) {
			t.Fatalf("%s imports runneragent: the manager must never call into Resident", path)
		}
	}
}

// A revoke racing deliveries cannot deadlock: the manager releases its lock
// before the resident erase, as production's hook does.
func TestResident_RevokeDuringDeliverNoDeadlock(t *testing.T) {
	f := newResidentFixture(t, nil)
	var mgr sync.Mutex
	revoked := false
	f.admit = func(uuid.UUID) error {
		mgr.Lock()
		defer mgr.Unlock()
		if revoked {
			return runner.ErrRunnerOffline
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Go(func() {
			for range 100 {
				_ = f.deliverTo(uuid.New(), uuid.New(), "v", true, f.clock.now().Add(time.Hour))
			}
		})
		wg.Go(func() {
			for range 100 {
				mgr.Lock()
				revoked = true
				mgr.Unlock()
				_ = f.res.EraseAll(context.Background())
				mgr.Lock()
				revoked = false
				mgr.Unlock()
			}
		})
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deliveries racing a revoke deadlocked")
	}
	settled(t, f.res)
	if err := f.res.EraseAll(context.Background()); err != nil || f.ks.size() != 0 || len(f.ks.overlaps) != 0 {
		t.Fatalf("after the race: EraseAll=%v items=%d overlaps=%v", err, f.ks.size(), f.ks.overlaps)
	}
}

// The writing entry is durable before the write: the index save runs temp
// sync, rename and directory sync, in that order, before Set.
func TestResident_SaveIsDurableBeforeSet(t *testing.T) {
	f := newResidentFixture(t, nil)
	traceSaves(t, f.ks, nil)
	if err := f.deliver(uuid.New(), "v", true); err != nil {
		t.Fatal(err)
	}
	ops := f.ks.opsSnapshot()
	i := slices.IndexFunc(ops, func(op string) bool { return strings.HasPrefix(op, "set ") })
	if i < 3 || !slices.Equal(ops[i-3:i], []string{"fsync tmp", "rename", "fsync dir"}) {
		t.Fatalf("op order = %v, want fsync tmp, rename, fsync dir right before the set", ops)
	}
}

// A save fault at the temp sync, the rename or the directory sync refuses the
// delivery and issues no Set; nothing is left busy, so a later delivery works.
func TestResident_SaveFaultIssuesNoSet(t *testing.T) {
	for _, step := range []string{"fsync tmp", "rename", "fsync dir"} {
		t.Run(step, func(t *testing.T) {
			f := newResidentFixture(t, nil)
			fault := step
			traceSaves(t, f.ks, &fault)
			grant := uuid.New()
			if err := f.deliver(grant, "v", true); !refusedUnavailable(err) {
				t.Fatalf("deliver with a %s fault = %v, want refused", step, err)
			}
			if n := f.ks.count("set "); n != 0 {
				t.Fatalf("a %s fault still issued %d sets", step, n)
			}
			if _, ok := f.entry(grant); ok {
				t.Fatalf("a %s fault left the entry indexed in memory", step)
			}
			fault = ""
			if err := f.deliver(grant, "v", true); err != nil {
				t.Fatalf("deliver after the fault cleared = %v", err)
			}
		})
	}
}

// A runner that stops between the writing save and the write's return finds
// the entry writing at start: it is marked for erasure and unsettled, the
// first sweep deletes it, a late write inside the settle window is deleted
// again, the next erase reports it, and only past the window is it unindexed.
func TestResident_RestartWithWriteInFlightIsReErased(t *testing.T) {
	f := newResidentFixture(t, nil)
	crashed := t.TempDir()
	f.ks.onSet = func(string) {
		b, err := os.ReadFile(filepath.Join(f.dir, "resident.json"))
		if err == nil {
			err = os.WriteFile(filepath.Join(crashed, "resident.json"), b, 0o600)
		}
		if err != nil {
			t.Error(err)
		}
	}
	grant := uuid.New()
	if err := f.deliver(grant, "v", true); err != nil {
		t.Fatal(err)
	}
	ks := &memKeystore{items: map[string]string{}, inflight: map[string]bool{}}
	r, err := NewResident(ks.keystore(), crashed, func(uuid.UUID) error { return nil }, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := r.idx.Grants[f.run][grant]
	if !ok || g.State != grantWriting || !g.EraseRequested || !g.Unsettled {
		t.Fatalf("entry at start = %+v, want writing, erase requested, unsettled", g)
	}
	if b, _ := os.ReadFile(filepath.Join(crashed, "resident.json")); !strings.Contains(string(b), `"erase_requested":true`) {
		t.Fatalf("the start-up re-erase mark is not durable: %s", b)
	}
	ctx := context.Background()
	if err := r.EraseExpired(ctx, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	account := residentAccount(f.run, grant)
	ks.items[account] = "landed late"
	if err := r.EraseExpired(ctx, f.clock.add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ks.size() != 0 {
		t.Fatal("a late write inside the settle window survived the sweep")
	}
	if err := r.erase(ctx, f.peer, f.run); err != nil {
		t.Fatalf("erase after the re-erase = %v", err)
	}
	if ev := f.takeEvents(); len(ev) != 1 || ev[0].Outcome != runnerwire.ResidentErased {
		t.Fatalf("events = %+v, want the re-erase reported", ev)
	}
	if g := r.idx.Grants[f.run][grant]; !g.Unsettled || g.State != grantErased {
		t.Fatalf("entry inside the settle window = %+v, want kept, erased and still unsettled", g)
	}
	if err := r.EraseExpired(ctx, f.clock.add(settleWindow())); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.idx.Grants[f.run][grant]; ok {
		t.Fatal("an erased, reported entry past its settle window is still indexed")
	}
}

// A value the keystore cannot take is refused before any entry is written.
func TestResident_PrecheckWritesNoEntry(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.ks.precheckErr = errors.New("value too long for the keychain")
	if err := f.deliver(uuid.New(), "v", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want %s", err, placement.ReasonRunnerKeystoreUnavailable)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "resident.json")); !errors.Is(err, os.ErrNotExist) || len(f.ks.opsSnapshot()) != 0 {
		t.Fatalf("a refused precheck wrote the index (%v) or reached the keystore (%v)", err, f.ks.opsSnapshot())
	}
}

// A delivery after its run was erased here is refused, with no write.
func TestResident_DeliverAfterEraseIsRefused(t *testing.T) {
	f := newResidentFixture(t, nil)
	if err := f.deliver(uuid.New(), "v", true); err != nil {
		t.Fatal(err)
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	sets := f.ks.count("set ")
	if err := f.deliver(uuid.New(), "v", true); err == nil || f.ks.count("set ") != sets {
		t.Fatalf("delivery after erase = %v, sets %d → %d", err, sets, f.ks.count("set "))
	}
}

// A delivery with no not_after, or one not ahead of now, is refused.
func TestResident_DeliverRefusesZeroOrPastNotAfter(t *testing.T) {
	f := newResidentFixture(t, nil)
	for _, notAfter := range []time.Time{{}, f.clock.now(), f.clock.now().Add(-time.Second)} {
		if err := f.deliverTo(f.run, uuid.New(), "v", true, notAfter); err == nil {
			t.Fatalf("delivery with not_after %v was taken", notAfter)
		}
	}
	if len(f.ks.opsSnapshot()) != 0 {
		t.Fatalf("refused deliveries reached the keystore: %v", f.ks.opsSnapshot())
	}
}

// The runner keeps a value at most 24 h, whatever not_after the org sent: a
// 48 h not_after is clamped, not refused, and the value is swept at +24h.
func TestResident_NotAfterCeiling24h(t *testing.T) {
	f := newResidentFixture(t, nil)
	start := f.clock.now()
	grant := uuid.New()
	if err := f.deliverTo(f.run, grant, "v", true, start.Add(48*time.Hour)); err != nil {
		t.Fatalf("a 48 h not_after = %v, want clamped, not refused", err)
	}
	if g, _ := f.entry(grant); !g.NotAfter.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("stored not_after = %v, want %v", g.NotAfter, start.Add(24*time.Hour))
	}
	ctx := context.Background()
	if err := f.res.EraseExpired(ctx, f.clock.add(24*time.Hour-time.Second)); err != nil || f.ks.size() != 1 {
		t.Fatalf("sweep before the ceiling = %v, items %d", err, f.ks.size())
	}
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Second)); err != nil || f.ks.size() != 0 {
		t.Fatalf("sweep at the ceiling = %v, items %d", err, f.ks.size())
	}
}

// A value past its NotAfter is erased by the runner's own sweep, with no org
// erase; a failed delete keeps it for the next sweep; an unexpired value is
// left alone; and the run's next erase reports the sweep's erasure.
func TestResident_ExpiryErasureIsReportedOnNextErase(t *testing.T) {
	f := newResidentFixture(t, nil)
	start := f.clock.now()
	expired, live := uuid.New(), uuid.New()
	if err := f.deliverTo(f.run, expired, "v", true, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.deliverTo(f.run, live, "v", true, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.ks.failDelete = true
	if err := f.res.EraseExpired(ctx, f.clock.add(2*time.Minute)); err == nil {
		t.Fatal("a failed delete was not reported")
	}
	f.ks.failDelete = false
	if err := f.res.EraseExpired(ctx, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.ks.item(residentAccount(f.run, expired)); ok || f.ks.size() != 1 {
		t.Fatalf("keystore after the sweep holds %d items, want only the unexpired one", f.ks.size())
	}
	if g, _ := f.entry(expired); g.State != grantErased || g.Reported || g.ReportOutcome != runnerwire.ResidentErased {
		t.Fatalf("swept entry = %+v, want erased and unreported", g)
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	ev := f.takeEvents()
	if len(ev) != 2 || ev[0].Outcome != runnerwire.ResidentErased || ev[1].Outcome != runnerwire.ResidentErased {
		t.Fatalf("events = %+v, want both grants reported erased", ev)
	}
}

// A report whose acknowledgement was lost is resent, on the next connection,
// with the same id.
func TestResident_UnackedReportIsResentWithSameID(t *testing.T) {
	f := newResidentFixture(t, func(runnerwire.Event) error { return errors.New("audit write failed") })
	if err := f.deliver(uuid.New(), "v", true); err != nil {
		t.Fatal(err)
	}
	if err := f.erase(); err == nil {
		t.Fatal("erase returned nil with its report unacknowledged")
	}
	f.connect(t, nil)
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	ev := f.takeEvents()
	if len(ev) != 2 || ev[0].ReportID != ev[1].ReportID || ev[0].Outcome != runnerwire.ResidentErased {
		t.Fatalf("events = %+v, want one report sent twice with one id", ev)
	}
}

// An acknowledgement of a failed report that lands after the write's
// completion erased the value does not mark the new erased outcome reported;
// the next erase sends it under its own id.
func TestResident_StaleAckDoesNotMarkNewOutcomeReported(t *testing.T) {
	got, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := newResidentFixture(t, func(runnerwire.Event) error {
		first := false
		once.Do(func() { first = true })
		if first {
			close(got)
			<-proceed
		}
		return nil
	})
	shortTimeout(t, 50*time.Millisecond)
	f.ks.setGate = make(chan struct{})
	grant := uuid.New()
	if err := f.deliver(grant, "v", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want a timeout refusal", err)
	}
	erased := make(chan error, 1)
	go func() { erased <- f.erase() }()
	<-got
	close(f.ks.setGate)
	settled(t, f.res)
	close(proceed)
	if err := <-erased; !errors.Is(err, errEraseIncomplete) {
		t.Fatalf("erase = %v, want %v", err, errEraseIncomplete)
	}
	g, _ := f.entry(grant)
	failed := f.takeEvents()
	if len(failed) != 1 || failed[0].Outcome != runnerwire.ResidentEraseFailed || g.State != grantErased || g.Reported || g.ReportID == failed[0].ReportID {
		t.Fatalf("entry %+v after a stale ack of %+v, want erased, unreported, a new id", g, failed)
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	if ev := f.takeEvents(); len(ev) != 1 || ev[0].Outcome != runnerwire.ResidentErased || ev[0].ReportID != g.ReportID {
		t.Fatalf("events = %+v, want the erased outcome under id %s", ev, g.ReportID)
	}
}

// On revoke every value is deleted with no report sent; a write in flight is
// deleted by its own completion.
func TestResident_RevokedErasesAll(t *testing.T) {
	f := newResidentFixture(t, nil)
	for range 2 {
		if err := f.deliver(uuid.New(), "v", true); err != nil {
			t.Fatal(err)
		}
	}
	shortTimeout(t, 50*time.Millisecond)
	f.ks.setGate = make(chan struct{})
	if err := f.deliver(uuid.New(), "in flight", true); !refusedUnavailable(err) {
		t.Fatalf("deliver = %v, want a timeout refusal", err)
	}
	// Only the refusal above needs the short bound; EraseAll's own deletes get the real one, or a
	// slow -race runner times them out.
	shortTimeout(t, 30*time.Second)
	if err := f.res.EraseAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ks.size() != 0 {
		t.Fatalf("keystore after EraseAll holds %d items", f.ks.size())
	}
	close(f.ks.setGate)
	settled(t, f.res)
	if f.ks.size() != 0 || len(f.takeEvents()) != 0 || len(f.ks.overlaps) != 0 {
		t.Fatalf("after the in-flight write: items=%d overlaps=%v (or events were sent)", f.ks.size(), f.ks.overlaps)
	}
}

// When the runner holds nothing, DeleteAll over its service catches a value a
// keystore helper wrote late: at the first idle check, then at most hourly.
// A DeleteAll that outlives its timeout does not hold the sweep; while it
// runs a delivery refuses at once and no second call starts; the flag clears
// only when the call really returns, and the next check retries it.
func TestResident_IdleDeleteAllCatchesLateOrphan(t *testing.T) {
	f := newResidentFixture(t, nil)
	if err := f.deliver(uuid.New(), "v", true); err != nil {
		t.Fatal(err)
	}
	if err := f.erase(); err != nil {
		t.Fatal(err)
	}
	if n := f.ks.count("deleteall"); n != 1 {
		t.Fatalf("DeleteAll calls after the erase that emptied the runner = %d, want 1", n)
	}
	ctx := context.Background()
	f.ks.mu.Lock()
	f.ks.items["orphan"] = "written late"
	f.ks.mu.Unlock()
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err != nil || f.ks.size() != 1 {
		t.Fatalf("sweep inside the hour = %v, items %d: want the backstop rate-limited", err, f.ks.size())
	}
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Hour)); err != nil || f.ks.size() != 0 {
		t.Fatalf("hourly sweep = %v, items %d: want the orphan deleted", err, f.ks.size())
	}

	shortTimeout(t, 50*time.Millisecond)
	f.ks.deleteAllGate = make(chan struct{})
	swept := make(chan error, 1)
	go func() { swept <- f.res.EraseExpired(ctx, f.clock.add(time.Hour)) }()
	select {
	case err := <-swept:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sweep with DeleteAll stuck = %v, want its timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck DeleteAll held the sweep")
	}
	start := time.Now()
	if err := f.deliverTo(uuid.New(), uuid.New(), "v", true, f.clock.now().Add(time.Hour)); !refusedUnavailable(err) || time.Since(start) > time.Second {
		t.Fatalf("delivery while DeleteAll runs = %v after %v, want refused at once", err, time.Since(start))
	}
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := f.ks.count("deleteall"); n != 3 {
		t.Fatalf("DeleteAll calls with one still running = %d, want 3 (single flight)", n)
	}
	close(f.ks.deleteAllGate)
	settled(t, f.res)
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := f.ks.count("deleteall"); n != 4 {
		t.Fatalf("DeleteAll calls after the timed-out one returned = %d, want 4 (retried)", n)
	}
	if err := f.deliverTo(uuid.New(), uuid.New(), "v", true, f.clock.now().Add(time.Hour)); err != nil {
		t.Fatalf("delivery once DeleteAll returned = %v", err)
	}
}

// valueOf reads a grant through the one read path, for the sandbox.
func (f *residentFixture) valueOf(grant uuid.UUID) error {
	_, err := f.res.value(context.Background(), f.run, grant, true)
	return err
}

// A read is refused from NotAfter on, also on revive (fillReplacedProxy);
// just before it the value is read.
func TestResident_ValueRefusedAtNotAfter(t *testing.T) {
	f := newResidentFixture(t, nil)
	start := f.clock.now()
	grant := uuid.New()
	if err := f.deliverTo(f.run, grant, "pat", true, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"run_id":"` + f.run.String() + `","pat_grants":{"dev.azure.com":{"grant_id":"` + grant.String() + `","username":"` + placement.ResidentPlaceholder(grant) + `"}}}`)
	f.clock.add(time.Minute - time.Nanosecond)
	if err := f.valueOf(grant); err != nil {
		t.Fatalf("read just before not_after = %v", err)
	}
	if out, err := f.res.fillReplacedProxy(context.Background(), cfg); err != nil || !strings.Contains(string(out), `"username":"pat"`) {
		t.Fatalf("revive just before not_after = %s (%v)", out, err)
	}
	f.clock.add(time.Nanosecond)
	if err := f.valueOf(grant); err == nil {
		t.Fatal("a read at not_after was served")
	}
	if _, err := f.res.fillReplacedProxy(context.Background(), cfg); err == nil {
		t.Fatal("a revive at not_after was served")
	}
}

// NotAfter falls between sweeps: the read is refused at once, the entry stays
// held (nothing erased early) until the next sweep deletes it.
func TestResident_ExpiryBetweenTicks(t *testing.T) {
	f := newResidentFixture(t, nil)
	grant := uuid.New()
	if err := f.deliverTo(f.run, grant, "v", true, f.clock.now().Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.clock.add(31 * time.Second)
	if err := f.valueOf(grant); err == nil {
		t.Fatal("a read past not_after, before the sweep, was served")
	}
	if g, _ := f.entry(grant); g.State != grantHeld || g.ReportOutcome != "" || f.ks.size() != 1 {
		t.Fatalf("entry between ticks = %+v (items %d), want still held with no outcome", g, f.ks.size())
	}
	if err := f.res.EraseExpired(ctx, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.entry(grant); g.State != grantErased || g.ReportOutcome != runnerwire.ResidentErased || f.ks.size() != 0 {
		t.Fatalf("entry after the next sweep = %+v (items %d), want erased", g, f.ks.size())
	}
}

// A keystore call held across NotAfter: the sweep skips the busy account and
// issues no overlapping call, the read is refused and nothing is marked
// erased. A write landing past NotAfter is not kept (its completion deletes
// it); after a held delete fails, the first sweep deletes the value.
func TestResident_BusyAcrossExpiry(t *testing.T) {
	ctx := context.Background()
	t.Run("set", func(t *testing.T) {
		f := newResidentFixture(t, nil)
		f.ks.setGate = make(chan struct{})
		f.ks.setStarted = make(chan string, 1)
		grant := uuid.New()
		delivered := make(chan error, 1)
		go func() { delivered <- f.deliverTo(f.run, grant, "v", true, f.clock.now().Add(time.Minute)) }()
		<-f.ks.setStarted
		if err := f.res.EraseExpired(ctx, f.clock.add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := f.valueOf(grant); err == nil {
			t.Fatal("a read with the write in flight past not_after was served")
		}
		if g, _ := f.entry(grant); g.State != grantWriting || g.ReportOutcome != "" || f.ks.count("delete ") != 0 {
			t.Fatalf("entry while busy = %+v, deletes %d: want skipped, not erased", g, f.ks.count("delete "))
		}
		close(f.ks.setGate)
		// A write that lands past not_after is not kept: the delivery fails and
		// the completion deletes the value.
		if err := <-delivered; err == nil || !strings.Contains(err.Error(), "not held") {
			t.Fatalf("a delivery whose write landed past not_after = %v, want refused", err)
		}
		settled(t, f.res)
		if err := f.valueOf(grant); err == nil {
			t.Fatal("a read past not_after was served once the write returned")
		}
		if err := f.res.EraseExpired(ctx, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		if g, _ := f.entry(grant); g.State != grantErased || f.ks.size() != 0 || len(f.ks.overlaps) != 0 {
			t.Fatalf("after the write returned: %+v items=%d overlaps=%v", g, f.ks.size(), f.ks.overlaps)
		}
	})
	t.Run("delete", func(t *testing.T) {
		f := newResidentFixture(t, nil)
		grant := uuid.New()
		if err := f.deliverTo(f.run, grant, "v", true, f.clock.now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		shortTimeout(t, 50*time.Millisecond)
		f.ks.deleteGate = make(chan struct{})
		if err := f.erase(); err == nil {
			t.Fatal("erase returned nil with its delete in flight")
		}
		if err := f.res.EraseExpired(ctx, f.clock.add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := f.valueOf(grant); err == nil {
			t.Fatal("a read with a delete in flight was served")
		}
		if g, _ := f.entry(grant); g.State != grantHeld || g.ReportOutcome == runnerwire.ResidentErased || f.ks.count("delete ") != 1 {
			t.Fatalf("entry while the delete is held = %+v, deletes %d: want skipped, not erased", g, f.ks.count("delete "))
		}
		f.ks.mu.Lock()
		f.ks.failDelete = true
		f.ks.mu.Unlock()
		close(f.ks.deleteGate)
		settled(t, f.res)
		f.ks.mu.Lock()
		f.ks.failDelete = false
		f.ks.mu.Unlock()
		if g, _ := f.entry(grant); g.State != grantHeld || g.ReportOutcome == runnerwire.ResidentErased {
			t.Fatalf("entry after the held delete failed = %+v, want unchanged", g)
		}
		if err := f.res.EraseExpired(ctx, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		if g, _ := f.entry(grant); g.State != grantErased || g.ReportOutcome != runnerwire.ResidentErased || f.ks.size() != 0 || len(f.ks.overlaps) != 0 {
			t.Fatalf("after the first sweep past the delete: %+v items=%d overlaps=%v", g, f.ks.size(), f.ks.overlaps)
		}
	})
}

// The sweep's delete fails twice, then succeeds: the entry is unchanged after
// each failure and the read is refused throughout; erased is recorded only
// after the successful delete.
func TestResident_ExpiryDeleteFailureRetries(t *testing.T) {
	f := newResidentFixture(t, nil)
	grant := uuid.New()
	if err := f.deliverTo(f.run, grant, "v", true, f.clock.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, _ := f.entry(grant)
	ctx := context.Background()
	f.clock.add(2 * time.Minute)
	f.ks.failDelete = true
	for i := range 2 {
		if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err == nil {
			t.Fatalf("sweep %d with a failing delete returned nil", i+1)
		}
		if g, _ := f.entry(grant); g != before {
			t.Fatalf("entry after failed sweep %d = %+v, want unchanged %+v", i+1, g, before)
		}
		if err := f.valueOf(grant); err == nil {
			t.Fatalf("a read past not_after was served after failed sweep %d", i+1)
		}
	}
	f.ks.failDelete = false
	if err := f.res.EraseExpired(ctx, f.clock.add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.entry(grant); g.State != grantErased || g.ReportOutcome != runnerwire.ResidentErased || g.ReportID == uuid.Nil || f.ks.size() != 0 {
		t.Fatalf("entry after the successful delete = %+v (items %d)", g, f.ks.size())
	}
}

// A keystore without Precheck or DeleteAll does not panic: a delivery skips the
// precheck, and the idle backstop reports errNoDeleteAll as a failed erase.
func TestResident_KeystoreWithoutPrecheckOrDeleteAll(t *testing.T) {
	f := newResidentFixture(t, nil)
	f.res.keystore.Precheck, f.res.keystore.DeleteAll = nil, nil
	if err := f.res.EraseExpired(context.Background(), f.clock.add(2*time.Hour)); !errors.Is(err, errNoDeleteAll) {
		t.Fatalf("idle backstop without DeleteAll = %v, want errNoDeleteAll", err)
	}
	if err := f.deliver(uuid.New(), "v", true); err != nil {
		t.Fatalf("delivery without Precheck: %v", err)
	}
}
