// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestQueueWaitSec(t *testing.T) {
	for _, tc := range []struct {
		name        string
		asked       int
		interactive bool
		want        int
		warns       bool
	}{
		{"background default", 0, false, 24 * 60 * 60, false},
		{"interactive default", 0, true, 60 * 60, false},
		{"asked", 90, false, 90, false},
		{"asked longest", 604800, true, 604800, false},
		{"cut at seven days with a warning", 604801, false, 604800, true},
	} {
		req := createRunRequest{}
		if tc.asked != 0 {
			req.MaxWaitSec = &tc.asked
		}
		got, warn := queueWaitSec(req, tc.interactive)
		if got != tc.want || (warn != "") != tc.warns {
			t.Errorf("%s: wait %d, warning %q; want %d, warning %v", tc.name, got, warn, tc.want, tc.warns)
		}
	}
}

// A run that waits for a slot is refused for its size only when a quota's hard limit could never hold
// it; one that merely exceeds what is left waits, and the same run is refused when it is not waiting.
func TestJudgeFit_AWaitingRunIsRefusedOnlyWhenItCanNeverFit(t *testing.T) {
	fit := func(need, left, hard int64) runner.Fit {
		return runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadOK, Quota: []runner.QuotaFit{
			{Name: "runs", Axes: []runner.QuotaAxis{{Key: "requests.cpu", Need: need, Left: left, Hard: hard}}}}}
	}
	if v := judgeFit(fit(4000, 2000, 10000), false); v.refusal == "" {
		t.Error("a run needing more than is left was admitted when it does not wait")
	}
	if v := judgeFit(fit(4000, 2000, 10000), true); v.refusal != "" {
		t.Errorf("a waiting run needing more than is left was refused: %s", v.refusal)
	}
	if v := judgeFit(fit(12000, 2000, 10000), true); v.refusal == "" {
		t.Error("a waiting run needing more than the quota's hard limit was admitted")
	}
	if v := judgeFit(fit(4000, 2000, 0), true); v.refusal != "" {
		t.Error("an axis with no hard limit refused a waiting run")
	}
}

// A waiting run is refused only for a quota that can never hold it, and the sentence says so
// rather than telling the person to stop a run, which would not make room.
func TestJudgeFit_AWaitingRunThatNeverFitsIsNotToldToStopARun(t *testing.T) {
	fit := runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadOK, Quota: []runner.QuotaFit{
		{Name: "q", Axes: []runner.QuotaAxis{{Key: "requests.cpu", Need: 2000, Left: 500, Hard: 1000}}}}}
	want := "this run needs 2 CPU, more than quota q can ever hold (1 CPU). Make the run smaller, or ask your admin to raise the quota."
	if v := judgeFit(fit, true); v.refusal != want {
		t.Errorf("refusal = %q, want %q", v.refusal, want)
	}
}

// Nothing is renewed while a run waits: the provider check reads the sign-in's status and the start
// does the renewing, so the credential is spent only for a run that is about to use it.
func TestQueuedRunRenewsNothing(t *testing.T) {
	req := createRunRequest{Agent: "claude-code", Task: "t", ModelProvider: "bedrock-sso"}
	for _, tc := range []struct {
		name  string
		step  bool
		queue bool
		want  int32
	}{
		{"provider check, waiting", false, true, 0},
		{"provider check, starting", false, false, 1},
		{"fold step, waiting", true, true, 0},
		{"fold step, starting", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createRenewalFixture(t)
			calls := renewedOIDC(t)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
			r = r.WithContext(withHumanIdentity(r.Context(), createRenewalOwner, "", "admin", "", nil, false))
			w := httptest.NewRecorder()
			var ok bool
			if tc.step {
				f := &runFold{mode: foldCreate, queued: tc.queue, w: w, r: r, req: &req}
				ok = srv.stepModelProvider(f, &foldRecorder{})
			} else {
				_, ok = srv.enforceRunModelProvider(w, r, req, types.RunPolicySpec{}, nil, true, tc.queue)
			}
			if !ok {
				t.Fatalf("the provider check refused: %d %s", w.Code, w.Body)
			}
			if calls.Load() != tc.want {
				t.Errorf("CreateToken calls = %d, want %d", calls.Load(), tc.want)
			}
		})
	}
}

// A waiting run's other harnesses renew nothing either: a Bedrock sign-in beside a Codex primary is
// refused before any renewal, whether the run waits or starts.
func TestQueuedRunsOtherHarnessesRenewNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue bool
	}{{"waiting", true}, {"starting", false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createRenewalFixture(t)
			calls := renewedOIDC(t)
			req := createRunRequest{Agent: "codex-cli", Task: "t"}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
			r = r.WithContext(withHumanIdentity(r.Context(), createRenewalOwner, "", "admin", "", nil, false))
			w := httptest.NewRecorder()
			f := &runFold{mode: foldCreate, queued: tc.queue, w: w, r: r, req: &req,
				harnesses: []types.RunHarness{{Harness: "codex-cli", Starts: true}, {Harness: "claude-code", ModelProviderID: "bedrock-sso"}}}
			if srv.stepModelProvider(f, &foldRecorder{}) || w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("a sign-in on a harness the run does not start was admitted: %d %s", w.Code, w.Body)
			}
			if calls.Load() != 0 {
				t.Errorf("CreateToken calls = %d, want 0", calls.Load())
			}
		})
	}
}

// The start only narrows: a different model provider or a weaker barrier than the launch was admitted
// with ends the run as a policy change; the same or a stronger one starts.
func TestQueueNarrowOnly(t *testing.T) {
	s := &Server{}
	fold := func(provider string, class types.ConfinementClass) runFold {
		return runFold{mpChoice: runProviderChoice{provider: types.ModelProvider{ID: provider}}, enforced: class}
	}
	for _, tc := range []struct {
		name string
		f    runFold
		ends bool
	}{
		{"same", fold("corp", types.CC2), false},
		{"stronger barrier", fold("corp", types.CC3), false},
		{"weaker barrier", fold("corp", types.CC1), true},
		{"another provider", fold("other", types.CC2), true},
		{"a provider appeared", fold("", types.CC2), true},
	} {
		end := s.queueNarrowOnly(&queuedStart{run: types.AgentRun{ModelProviderID: "corp", ConfinementClass: types.CC2}, f: tc.f})
		if (end != nil) != tc.ends || (end != nil && (end.token != string(types.RunEndQueuePolicyChanged) || !strings.Contains(end.hint, "policy"))) {
			t.Errorf("%s: end = %+v, ends = %v", tc.name, end, tc.ends)
		}
	}
}

// max_wait_sec below 1, a 0 included, is refused 422 rather than read as absent; absent is the default.
func TestMaxWaitShape(t *testing.T) {
	for _, bad := range []int{-1, 0} {
		if ref := runEndShapeRefusal(createRunRequest{MaxWaitSec: &bad}); ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != reasonRunMaxWaitInvalid {
			t.Errorf("max_wait_sec %d = %+v; want 422 %s", bad, ref, reasonRunMaxWaitInvalid)
		}
	}
	if ref := runEndShapeRefusal(createRunRequest{}); ref != nil {
		t.Errorf("an absent max_wait_sec refused: %+v", ref)
	}
	for _, ok := range []int{1, 604800, 999999} {
		if ref := runEndShapeRefusal(createRunRequest{MaxWaitSec: &ok}); ref != nil {
			t.Errorf("max_wait_sec %d refused: %+v", ok, ref)
		}
	}
	var decoded createRunRequest
	if err := json.Unmarshal([]byte(`{"max_wait_sec":0}`), &decoded); err != nil || runEndShapeRefusal(decoded) == nil {
		t.Errorf("a request carrying max_wait_sec 0 was not refused: %v", err)
	}
}

// The identity a launch carries survives the queue row: a person's sub, email, role, user type, group
// snapshot (nil kept apart from empty), truncation bit and epoch, a local principal, a service
// identity and a portal's via all come back on the start's context as they were.
func TestQueueAuthorityRoundTrip(t *testing.T) {
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	deviceID := uuid.New()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		assert func(t *testing.T, ctx context.Context)
	}{
		{"person with groups", withHumanIdentity(context.Background(), "sub-p", "p@example.com", "user", "dev", []string{"a", "b"}, true),
			func(t *testing.T, ctx context.Context) {
				if oidcHumanFromContext(ctx) != "sub-p" || oidcEmailFromContext(ctx) != "p@example.com" || oidcRoleFromContext(ctx) != "user" ||
					oidcUserTypeFromContext(ctx) != "dev" || !slices.Equal(oidcGroupsFromContext(ctx), []string{"a", "b"}) || !oidcGroupsTruncatedFromContext(ctx) {
					t.Errorf("identity not restored")
				}
			}},
		{"no group snapshot stays none", withHumanIdentity(context.Background(), "sub-p", "", "user", "", nil, false),
			func(t *testing.T, ctx context.Context) {
				if oidcGroupsFromContext(ctx) != nil {
					t.Errorf("groups = %#v; a missing snapshot must stay missing", oidcGroupsFromContext(ctx))
				}
			}},
		{"empty group snapshot stays empty", withHumanIdentity(context.Background(), "sub-p", "", "user", "", []string{}, false),
			func(t *testing.T, ctx context.Context) {
				if g := oidcGroupsFromContext(ctx); g == nil || len(g) != 0 {
					t.Errorf("groups = %#v; a complete empty snapshot must stay empty", g)
				}
			}},
		{"local operator", withLocalPrincipal(context.Background(), "local:alice"),
			func(t *testing.T, ctx context.Context) {
				if localPrincipalFromContext(ctx) != "local:alice" {
					t.Error("local principal not restored")
				}
			}},
		{"service identity", withServiceIdentity(withHumanIdentity(context.Background(), "svc:ci", "", "user", "", []string{}, false), "ci", "sponsor-1"),
			func(t *testing.T, ctx context.Context) {
				if name, sponsor, ok := serviceIdentityFromContext(ctx); !ok || name != "ci" || sponsor != "sponsor-1" {
					t.Error("service identity not restored")
				}
			}},
		{"portal launch", audit.WithDelegation(withHumanIdentity(context.Background(), "sub-p", "", "user", "", nil, false), via),
			func(t *testing.T, ctx context.Context) {
				if got, ok := audit.DelegationFrom(ctx); !ok || got != via {
					t.Errorf("via = %+v, %v; want %+v", got, ok, via)
				}
			}},
		{"device", context.WithValue(context.Background(), deviceCtxKey{}, types.Device{ID: deviceID}),
			func(t *testing.T, ctx context.Context) {
				if d, ok := deviceFromContext(ctx); !ok || d.ID != deviceID {
					t.Fatalf("device = %+v, %v; want %s", d, ok, deviceID)
				}
				if _, actor := actorFromContext(ctx, ""); actor != deviceActor(deviceID) || operatorOwnedRequest(ctx) {
					t.Errorf("a promoted device run acts as %q, operator-owned %v; want the device, not the admin token", actor, operatorOwnedRequest(ctx))
				}
			}},
		{"the admin token", context.Background(),
			func(t *testing.T, ctx context.Context) {
				if oidcHumanFromContext(ctx) != "" || localPrincipalFromContext(ctx) != "" {
					t.Error("the admin token gained an identity")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(captureQueueAuthority(tc.ctx))
			if err != nil {
				t.Fatal(err)
			}
			var back queueAuthority
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			tc.assert(t, back.context(context.Background()))
		})
	}
}

func TestJSONEqual(t *testing.T) {
	if !jsonEqual(json.RawMessage(`{"a":1,"b":"x"}`), json.RawMessage(`{ "b":"x", "a":1 }`)) {
		t.Error("the same document in another order or spacing is not equal")
	}
	if jsonEqual(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)) || jsonEqual(json.RawMessage(`{`), json.RawMessage(`{}`)) {
		t.Error("different or unreadable documents compare equal")
	}
}
