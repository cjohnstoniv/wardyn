// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The composed runtime's caps event names every run proxy setting this build carries: the org
// refuses a run that sets one a runner does not name, so a runner that dropped them would refuse
// those runs on every version.
func TestRunnerRuntimeAdvertisesEveryProxySetting(t *testing.T) {
	if got := composedCaps(t, runtimeExtras{noPosture: true, arch: "amd64"}).ProxySettings; !slices.Equal(got, runnerwire.ProxySettings) {
		t.Fatalf("caps advertise proxy settings %v, want %v", got, runnerwire.ProxySettings)
	}
	// The Docker runtime relays a laptop local model (#2957), and says so; the org sends one to no
	// runner that does not.
	want := append(slices.Clone(runnerwire.ProxySettings), runnerwire.ProxySettingLocalModelRelay)
	if got := composedCaps(t, runtimeExtras{noPosture: true, arch: "amd64", localModelRelay: true}).ProxySettings; !slices.Equal(got, want) {
		t.Fatalf("with the local model relay, caps advertise proxy settings %v, want %v", got, want)
	}
}

// The Docker runtime binds a member's folder bound to it (the driver's G1-G5 checks) and says so; a
// runtime that does not, Kubernetes, never does. The org places no bound folder on a runner without it.
func TestRunnerRuntimeAdvertisesBoundFolders(t *testing.T) {
	if !composedCaps(t, runtimeExtras{noPosture: true, arch: "amd64", boundFolders: true}).BoundFolders {
		t.Fatal("the Docker runtime's caps do not say bound_folders")
	}
	if composedCaps(t, runtimeExtras{noPosture: true, arch: "amd64"}).BoundFolders {
		t.Fatal("a runtime that binds no folder says bound_folders")
	}
}

// A k8s runner sends its caps again when a canary re-check changes the NetworkPolicy verdict, so the
// organisation's health and audit see it mid-session, not only at the next session.
func TestRunnerRuntimeResendsCapsOnNetPolChange(t *testing.T) {
	sub := &netPolFake{Fake: runnertest.NewFake()}
	sub.check.Store(&runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: time.Now().UTC()})
	var hook func()
	org := composedRuntime(t, sub, runtimeExtras{noPosture: true, arch: "amd64", netPolWatch: func(f func()) { hook = f }}, nil)
	if c, _ := org.Caps(); c.Support.NetworkPolicyCheck.Verdict != runner.NetPolEnforced {
		t.Fatalf("session caps carry verdict %q, want %q", c.Support.NetworkPolicyCheck.Verdict, runner.NetPolEnforced)
	}
	if hook == nil {
		t.Fatal("the runtime never set the NetworkPolicy hook")
	}
	sub.check.Store(&runner.NetPolCheck{Verdict: runner.NetPolRefused, At: time.Now().UTC()})
	hook()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if c, _ := org.Caps(); c.Support.NetworkPolicyCheck.Verdict == runner.NetPolRefused {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no caps carried the re-check's refused verdict")
		}
	}
}

// A verdict change with no session only marks the caps unsent; the next session's caps carry it
// (afterOnline and Replay send on the mark).
func TestNetPolChangeWithNoSessionMarksCapsUnsent(t *testing.T) {
	c := &capsReporter{netPol: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.resendNetPol(ctx) }()
	c.netPolChanged()
	c.netPolChanged() // coalesced with the first; never blocks
	for deadline := time.Now().Add(5 * time.Second); !c.dirty.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a change with no session did not mark the caps unsent")
		}
	}
	cancel()
	<-done
}

// A send holds every other send from its capture until its ack, so its older capture never lands after a
// newer one: here a relay change's send captures "enforced" and the organisation holds its caps event
// before the ack; a re-check changes the verdict to "refused" meanwhile, no other send may capture until
// the ack, and the organisation ends on "refused".
func TestCapsSendsNeverAckAnOlderCapture(t *testing.T) {
	sub := &netPolFake{Fake: runnertest.NewFake()}
	sub.check.Store(&runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: time.Now().UTC()})
	var unreachable atomic.Bool
	tick := make(chan time.Time)
	relay := newRelayWatch(func(context.Context) error {
		if unreachable.Load() {
			return errors.New("relay down")
		}
		return nil
	}, nil)
	relay.ticker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
	// held, once set, holds the next caps event at the organisation, so its send waits for the ack.
	var held atomic.Pointer[capsGate]
	onEvent := func(ev runnerwire.Event) {
		if g := held.Load(); ev.Kind == runnerwire.EventCaps && g != nil && held.CompareAndSwap(g, nil) {
			close(g.paused)
			<-g.release
		}
	}
	var hook func()
	org := composedRuntime(t, sub, runtimeExtras{noPosture: true, arch: "amd64", report: connectReport{relay: relay},
		netPolWatch: func(f func()) { hook = f }}, onEvent)

	gate := &capsGate{paused: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(release) // before the runtime's own cleanup, so a failed test never leaves the event held
	held.Store(gate)
	unreachable.Store(true)
	// Tick until the relay's caps event is held. A tick that beats the session's start sends nothing, and
	// the change stays unsent, so the next tick sends it.
	deadline := time.After(10 * time.Second)
	for paused := false; !paused; {
		select {
		case tick <- time.Now():
		case <-gate.paused:
			paused = true
		case <-deadline:
			t.Fatal("the relay change's caps never reached the organisation")
		}
	}
	calls := sub.calls.Load()
	sub.check.Store(&runner.NetPolCheck{Verdict: runner.NetPolRefused, At: time.Now().UTC()})
	hook()
	time.Sleep(200 * time.Millisecond)
	if got := sub.calls.Load(); got != calls {
		t.Fatalf("a second send captured caps (%d captures, want %d) while the first waited for its ack", got, calls)
	}
	release()
	for until := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		c, _ := org.Caps()
		if c.Support.NetworkPolicyCheck.Verdict == runner.NetPolRefused && c.Relay != nil && c.Relay.State == types.RelayUnreachable {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("the organisation's caps carry %q (relay %+v), want refused with the relay unreachable", c.Support.NetworkPolicyCheck.Verdict, c.Relay)
		}
	}
	time.Sleep(100 * time.Millisecond) // nothing later may put the older capture back
	if c, _ := org.Caps(); c.Support.NetworkPolicyCheck.Verdict != runner.NetPolRefused {
		t.Fatalf("the organisation's caps went back to %q", c.Support.NetworkPolicyCheck.Verdict)
	}
}

// The macOS VM fallback (Docker down) reads the last caps and the macOS VM state and acks them under the
// lock every send holds from capture through ack: a send that comes while the fallback is paused in its
// capture waits, so the fallback's older caps never land after the send's newer ones.
func TestMacOSCapsFallbackCapturesUnderTheSendLock(t *testing.T) {
	var dirty atomic.Bool
	var last atomic.Pointer[runnerwire.Caps]
	var mu sync.Mutex
	last.Store(&runnerwire.Caps{Arch: "old"})
	var orderMu sync.Mutex
	var order []string
	acked := func(arch string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, arch)
	}
	send := func(context.Context, *runnerwire.Peer, bool, bool) error { return fmt.Errorf("%w: down", errHostCaps) }
	ack := func(_ context.Context, _ *runnerwire.Peer, c runnerwire.Caps) error { acked(c.Arch); return nil }
	paused, release := make(chan struct{}), make(chan struct{})
	cur := func() *runnerwire.MacVMCaps {
		close(paused)
		<-release
		return &runnerwire.MacVMCaps{Max: 1}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resendMacOSCaps(t.Context(), new(runnerwire.Peer), &dirty, &last, &mu, send, ack, cur)
	}()
	<-paused
	// A send as capsReporter.send makes one: capture and ack under the lock.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		mu.Lock()
		defer mu.Unlock()
		last.Store(&runnerwire.Caps{Arch: "new"})
		acked("new")
	}()
	select {
	case <-sent:
		t.Fatal("a send acked while the fallback was paused in its capture")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-done
	<-sent
	orderMu.Lock()
	defer orderMu.Unlock()
	if !slices.Equal(order, []string{"old", "new"}) {
		t.Fatalf("acked %v, want the fallback's older caps before the send's newer ones", order)
	}
}

// capsGate holds one caps event at the organisation: paused closes when it arrives, and it is let
// through when release closes.
type capsGate struct{ paused, release chan struct{} }

// netPolFake is the fake substrate with a NetworkPolicy verdict a test changes while the runtime reads it.
type netPolFake struct {
	*runnertest.Fake
	check atomic.Pointer[runner.NetPolCheck]
	// calls counts captures (Classes calls).
	calls atomic.Int32
}

func (f *netPolFake) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	cs, err := f.Fake.Classes(ctx)
	if c := f.check.Load(); c != nil {
		cs.NetworkPolicyCheck = *c
	}
	f.calls.Add(1)
	return cs, err
}

// composedCaps is the caps event the composed runtime with extra sends the organisation.
func composedCaps(t *testing.T, extra runtimeExtras) runnerwire.Caps {
	t.Helper()
	caps, ok := composedRuntime(t, runnertest.NewFake(), extra, nil).Caps()
	if !ok {
		t.Fatal("no caps")
	}
	return caps
}

// composedRuntime serves the composed runtime on sub with extra to an organisation until the test ends,
// and returns the organisation's view of the runner once it is READY. onEvent, when set, sees each event
// the organisation takes before it is acked.
func composedRuntime(t *testing.T, sub substrate.Substrate, extra runtimeExtras, onEvent func(runnerwire.Event)) *remote.Substrate {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	runtime, err := runnerRuntimeWith(ctx, identity, t.TempDir(), sub, func(id uuid.UUID) string { return "fake-" + id.String() },
		newLocalRelay(ctx), unsized, extra)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.close() })
	const orgURL = "https://org.example"
	row := types.Runner{ID: identity.RunnerID, PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
	link := &remote.Link{}
	remoteSub := remote.New(row.ID.String(), link, remote.Options{})
	hub := remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: remoteSub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event: func(_ context.Context, ev runnerwire.Event) error {
					if onEvent != nil {
						onEvent(ev)
					}
					return nil
				},
			}, nil
		},
	})
	t.Cleanup(hub.Close)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: runtime.hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, local := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return local, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = runRunnerServices(ctx, client.Run, runtime.run) }()
	t.Cleanup(func() { cancel(); <-done })
	for !link.Online() {
		select {
		case <-done:
			t.Fatal("composed runtime stopped before READY")
		case <-ctx.Done():
			t.Fatal("composed runtime did not reach READY", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	return remoteSub
}
