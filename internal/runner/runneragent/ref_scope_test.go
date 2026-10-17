// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// labelledEngine is the engine fake with container labels; it proves a ref
// from them as the docker driver does.
type labelledEngine struct {
	*runnertest.Fake
	labels map[string]map[string]string
}

func (e labelledEngine) ProveRunnerSandbox(_ context.Context, ref, runnerID string) error {
	l := e.labels[ref]
	if l["wardyn.managed"] != "true" || l[runner.LabelRunnerID] != runnerID {
		return errors.New("container was not created by this runner")
	}
	return nil
}

// scopeVerbs are the ref verbs that reach the substrate whatever the lease,
// with the name the engine fake records each under.
var scopeVerbs = []struct{ method, call string }{
	{runnerwire.MethodKill, "kill"},
	{runnerwire.MethodStop, "stop"},
	{runnerwire.MethodEnd, "end"},
	{runnerwire.MethodStopProxy, "stop_proxy"},
	{runnerwire.MethodStatus, "status"},
	{runnerwire.MethodWait, "wait"},
	{runnerwire.MethodAgentStatus, "agent_status "},
}

func TestDispatcherRefVerbsReachOnlyThisRunnersSandboxes(t *testing.T) {
	runnerID := uuid.NewString()
	cases := []struct {
		name   string
		held   bool
		labels map[string]string
		reach  bool
	}{
		{name: "unrelated container", reach: false},
		{name: "another runner's sandbox", labels: map[string]string{"wardyn.managed": "true", runner.LabelRunnerID: uuid.NewString()}, reach: false},
		{name: "unmanaged container with this runner's label", labels: map[string]string{runner.LabelRunnerID: runnerID}, reach: false},
		{name: "bound run", held: true, reach: true},
		{name: "lost identity", labels: map[string]string{"wardyn.managed": "true", runner.LabelRunnerID: runnerID}, reach: true},
	}
	for _, tc := range cases {
		for _, v := range scopeVerbs {
			t.Run(tc.name+"/"+v.method, func(t *testing.T) {
				fake := runnertest.NewFake()
				sb, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
				if err != nil {
					t.Fatal(err)
				}
				d, err := newTestDispatcher(Config{RunnerID: runnerID,
					Substrate:    labelledEngine{fake, map[string]map[string]string{sb.Ref: tc.labels}},
					HoldsSandbox: func(ref string) bool { return tc.held && ref == sb.Ref },
					BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
					Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				a, b := runnerwire.Loopback()
				defer b.Close()
				peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
				defer peer.Close()
				data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: placement.RunnerRefPrefix(runnerID) + sb.Ref})
				_, err = d.Call(t.Context(), peer, 1, v.method, data)
				var refusal *runnerwire.Error
				refused := errors.As(err, &refusal) && refusal.Code == runnerwire.CodeRefused
				if reached := fake.Called(v.call, sb.Ref); reached != tc.reach || refused == tc.reach {
					t.Fatalf("reached the engine = %v, refused = %v (%v); want reached = %v", reached, refused, err, tc.reach)
				}
				if !tc.reach && len(fake.Calls) != 0 {
					t.Fatalf("a refused ref touched the engine: %v", fake.Calls)
				}
			})
		}
	}
}

// goneEngine proves a ref as the docker driver does once nothing on the engine
// answers to it: a name the engine gives a sandbox proves, any other does not.
type goneEngine struct{ *runnertest.Fake }

func (goneEngine) ProveRunnerSandbox(_ context.Context, ref, _ string) error {
	id, ok := strings.CutPrefix(ref, "fake-")
	if _, err := uuid.Parse(id); !ok || err != nil {
		return fmt.Errorf("%q is not a sandbox name: %w", ref, runner.ErrNotRunnerSandbox)
	}
	return nil
}

// TestRemoteKillOfAMissingSandboxAnswersNil is the conformance suite's
// KillMissingRef on the registered-runner path, without an engine. The org's
// kill of a sandbox this runner could have created and no longer has reaches
// the substrate and answers nil; a name the runner cannot prove comes back to
// the org as a refusal, never as a success that stopped nothing.
func TestRemoteKillOfAMissingSandboxAnswersNil(t *testing.T) {
	runnerID := uuid.NewString()
	fake := runnertest.NewFake()
	d, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: goneEngine{fake},
		HoldsSandbox: func(string) bool { return false },
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	link := &remote.Link{}
	org := remote.New(runnerID, link, remote.Options{})
	orgEnd, runEnd := runnerwire.Loopback()
	op := link.Attach(t.Context(), orgEnd)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(runEnd, runnerwire.PeerConfig{OnCall: func(ctx context.Context, call uint32, method string, raw json.RawMessage) (any, error) {
		return d.Call(ctx, rp, call, method, raw)
	}})
	go rp.Run(t.Context())
	t.Cleanup(func() { rp.Close(); op.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	gone := "fake-" + uuid.NewString()
	if err := org.KillSandbox(ctx, placement.RunnerRefPrefix(runnerID)+gone); err != nil || !fake.Called("kill", gone) {
		t.Fatalf("kill of a missing sandbox = %v, reached the engine = %v; want nil (idempotent)", err, fake.Called("kill", gone))
	}
	err = org.KillSandbox(ctx, placement.RunnerRefPrefix(runnerID)+"my-postgres")
	var refusal *runnerwire.Error
	if !errors.As(err, &refusal) || refusal.Code != runnerwire.CodeRefused || len(fake.Calls) != 1 {
		t.Fatalf("kill of a name this runner cannot prove = %v, engine calls %v; want a refusal that touches nothing", err, fake.Calls)
	}
}

func TestDispatcherRequiresTheHeldSandboxRecord(t *testing.T) {
	_, err := New(Config{RunnerID: uuid.NewString(), Substrate: runnertest.NewFake(),
		BeforeCreate:     func(context.Context, runner.SandboxSpec) error { return nil },
		Created:          func(context.Context, runner.SandboxSpec, string) error { return nil },
		CreateFailed:     func(context.Context, runner.SandboxSpec) error { return nil },
		BeforeRefCall:    func(context.Context, string, string, uuid.UUID) error { return nil },
		AfterRefCall:     func(context.Context, string, string, string, uuid.UUID) error { return nil },
		MutationFinished: func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error { return nil },
		ExecutionResult: func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, nil
		},
	})
	if err == nil {
		t.Fatal("a dispatcher without the held-sandbox record would reach any container")
	}
}

func TestDispatcherStampsItsOwnRunnerLabel(t *testing.T) {
	runnerID := uuid.NewString()
	fake := runnertest.NewFake()
	d, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: fake,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer peer.Close()
	spec := runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID, Labels: map[string]string{runner.LabelRunnerID: uuid.NewString(), "wardyn.run": "x"}}
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	if _, err := d.Call(t.Context(), peer, 1, runnerwire.MethodCreateSandbox, data); err != nil {
		t.Fatal(err)
	}
	if got := fake.Created[0].Labels; got[runner.LabelRunnerID] != runnerID || got["wardyn.run"] != "x" {
		t.Fatalf("sandbox labels %v do not name this runner", got)
	}
}
