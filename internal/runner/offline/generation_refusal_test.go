// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// A stop_proxy, end or replace_proxy refused because the run's proxy is a newer policy generation's touched
// nothing: it records no failed mutation, so no lapse check stops that newer proxy. The same refusal joined
// with another failure still does, for that failure may have had an effect.
func TestAGenerationRefusalIsNotAFailedMutation(t *testing.T) {
	refusal := fmt.Errorf("%w: it is for generation 3, this call is for 2", runner.ErrProxyGenerationNewer)
	for _, method := range []string{runnerwire.MethodStopProxy, runnerwire.MethodEnd, runnerwire.MethodReplaceProxy} {
		t.Run(method, func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
				t.Fatal(err)
			}
			finish := func(cause error) error {
				t.Helper()
				if err := m.BeforeRefCall(t.Context(), method, id.String(), uuid.Nil); err != nil {
					t.Fatal(err)
				}
				return m.MutationFinished(t.Context(), method, id.String(), uuid.Nil, uuid.Nil, cause)
			}
			if err := finish(refusal); err != nil {
				t.Fatal(err)
			}
			if err := m.CheckLapses(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r := m.state.Runs[id]; r.MutationFailure != nil || sub.stops != 0 || !m.mutationsDrained(id) {
				t.Fatalf("after the refusal: failure %+v, %d proxy stops, drained %v; want none, 0, true", r.MutationFailure, sub.stops, m.mutationsDrained(id))
			}
			if err := finish(errors.Join(refusal, errors.New("docker: stop agent: engine unavailable"))); err != nil {
				t.Fatal(err)
			}
			if r := m.state.Runs[id]; r.MutationFailure == nil || sub.stops == 0 {
				t.Fatalf("a refusal joined with a failure: failure %+v, %d proxy stops; want a failure contained", r.MutationFailure, sub.stops)
			}
		})
	}
}

// refusingSub is the manager's substrate whose generation-aware stop, end and replace refuse, as the Docker
// driver does for a proxy a newer policy generation started.
type refusingSub struct {
	*localSub
	refusal error
}

func (s refusingSub) StopProxyAt(context.Context, string, int) error  { return s.refusal }
func (s refusingSub) EndSandboxAt(context.Context, string, int) error { return s.refusal }
func (s refusingSub) CanReplaceProxy(context.Context, string) error   { return nil }
func (s refusingSub) EnsureProxyImage(context.Context) error          { return nil }
func (s refusingSub) ReplaceProxy(context.Context, string, []byte) error {
	return s.refusal
}
func (s refusingSub) ReplaceProxyHashedAt(context.Context, string, []byte, int) (string, error) {
	return "", s.refusal
}

// The dispatcher and the manager wired as the runner wires them: a refusal for a newer generation reaches
// MutationFinished as the refusal alone, so the lapse check stops no proxy.
func TestTheDispatcherHandsAGenerationRefusalOnUnjoined(t *testing.T) {
	refusal := fmt.Errorf("%w: it is for generation 3, this call is for 2", runner.ErrProxyGenerationNewer)
	for _, method := range []string{runnerwire.MethodStopProxy, runnerwire.MethodEnd, runnerwire.MethodReplaceProxy} {
		t.Run(method, func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
				t.Fatal(err)
			}
			runnerID := uuid.NewString()
			d, err := runneragent.New(runneragent.Config{
				RunnerID: runnerID, Substrate: refusingSub{sub, refusal}, HoldsSandbox: m.HoldsSandbox, Covered: m.CoversRef,
				BeforeCreate: m.BeforeCreate, Created: m.Created, CreateFailed: m.CreateFailed,
				BeforeRefCall: m.BeforeRefCall, AfterRefCall: m.AfterRefCall, CheckSelftest: m.CheckSelftest,
				MutationFinished: m.MutationFinished, ExecutionResult: m.ExecutionResult,
			})
			if err != nil {
				t.Fatal(err)
			}
			ref := placement.RunnerRefPrefix(runnerID) + id.String()
			var args any = runnerwire.RefArgs{Ref: ref, PolicyGeneration: 2}
			if method == runnerwire.MethodReplaceProxy {
				args = runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: []byte(`{"run_id":"` + id.String() + `"}`), PolicyGeneration: 2}
			}
			data, _ := runnerwire.Marshal(args)
			a, b := runnerwire.Loopback()
			defer b.Close()
			peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
			defer peer.Close()
			if _, err := d.Call(t.Context(), peer, 1, method, data); !errors.Is(err, runner.ErrProxyGenerationNewer) {
				t.Fatalf("%s = %v; want the refusal", method, err)
			}
			if err := m.CheckLapses(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r := m.state.Runs[id]; r.MutationFailure != nil || sub.stops != 0 || !m.mutationsDrained(id) {
				t.Fatalf("after the refusal: failure %+v, %d proxy stops, drained %v; want none, 0, true", r.MutationFailure, sub.stops, m.mutationsDrained(id))
			}
		})
	}
}
