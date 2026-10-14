// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capsTransport is a runner link that is up or down and carries no calls.
type capsTransport struct{ online bool }

func (t *capsTransport) Online() bool { return t.online }
func (t *capsTransport) Call(context.Context, string, any, any, ...runnerwire.CallOption) error {
	return errors.New("capsTransport: no calls")
}
func (t *capsTransport) TakeStream(uint32) (*runnerwire.Stream, error) {
	return nil, errors.New("capsTransport: no streams")
}
func (t *capsTransport) SetHandler(remote.Handler) {}

// capsOwner is the signed-in person capsCtx carries; installRunner's runners are theirs.
const capsOwner = "caps-owner"

var capsCtx = withOIDCHuman(context.Background(), capsOwner)

// ownedRunners is a store that knows only the runners' rows.
type ownedRunners struct {
	store.Store
	rows map[uuid.UUID]types.Runner
}

func (o ownedRunners) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	r, ok := o.rows[id]
	if !ok {
		return types.Runner{}, store.ErrNotFound
	}
	return r, nil
}

// installRunner registers a claimed runner of capsOwner on s as the boot and
// admission paths do. reported nil means the runner never sent a `caps` event.
func installRunner(t *testing.T, s *Server, online bool, reported *runnerwire.Caps) uuid.UUID {
	t.Helper()
	return installRunnerOf(t, s, capsOwner, online, reported)
}

func installRunnerOf(t *testing.T, s *Server, owner string, online bool, reported *runnerwire.Caps) uuid.UUID {
	t.Helper()
	id := uuid.New()
	s.cfg.Store.(ownedRunners).rows[id] = types.Runner{ID: id, Owner: owner, State: types.RunnerClaimed}
	sub := remote.New(id.String(), &capsTransport{online: online}, remote.Options{})
	if reported != nil {
		data, err := json.Marshal(reported)
		if err != nil {
			t.Fatal(err)
		}
		sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	}
	s.runnerStreams.subsMu.Lock()
	defer s.runnerStreams.subsMu.Unlock()
	if s.runnerStreams.subs == nil {
		s.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	s.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	return id
}

func laptopCaps(classes ...types.ConfinementClass) *runnerwire.Caps {
	return &runnerwire.Caps{
		Support:      substrate.ClassSupport{Classes: classes, EphemeralDiskEnforcement: types.StorageEnforcementFilesystem},
		Capacity:     placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192},
		AllowedRoots: []string{"/srv/dev/work"},
		Version:      "test",
	}
}

func capsServer(rn runner.Runner) *Server {
	return &Server{cfg: Config{Runner: rn, Store: ownedRunners{rows: map[uuid.UUID]types.Runner{}}}}
}

func asRefusal(t *testing.T, err error) *placement.Refusal {
	t.Helper()
	var ref *placement.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("error = %v, want a *placement.Refusal", err)
	}
	return ref
}

// An offline or never-reported runner is a refusal; the deployment's own
// answer (which advertises CC3 here) is never substituted for it.
func TestPlacementCapabilities_LocalOfflineRefuses(t *testing.T) {
	s := capsServer(&fakeRunner{})
	remoteCaps, err := s.placementCapabilities(context.Background(), types.PlacementRemote, uuid.Nil)
	if err != nil || len(remoteCaps.ConfinementClasses) != 3 || remoteCaps.Source != types.RunEvidenceSubstrate {
		t.Fatalf("remote = %+v, %v; want the deployment's three classes from the substrate", remoteCaps, err)
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"the link is down", installRunner(t, s, false, laptopCaps(types.CC1))},
		{"it never reported capabilities", installRunner(t, s, true, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.placementCapabilities(capsCtx, types.PlacementLocal, tc.id)
			if ref := asRefusal(t, err); ref.Reason != placement.ReasonRunnerOffline || ref.Reason.Status() != http.StatusUnprocessableEntity {
				t.Errorf("refusal = %+v, want runner_offline 422", ref)
			}
			if len(got.ConfinementClasses) != 0 {
				t.Errorf("classes = %v: an answer came back with the refusal", got.ConfinementClasses)
			}
		})
	}

	t.Run("create refuses runner_offline and never reads the deployment's classes", func(t *testing.T) {
		id := installRunner(t, s, false, laptopCaps(types.CC1))
		w := httptest.NewRecorder()
		_, _, ok := s.resolveEnforcedConfinement(capsCtx, w, types.PlacementLocal, id, types.RunPolicySpec{MinConfinementClass: types.CC1}, "", composer.Baseline{}, runPlacement{})
		if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"runner_offline"`) {
			t.Errorf("ok=%v %d %s, want a 422 runner_offline refusal", ok, w.Code, w.Body.String())
		}
	})
}

// The answer for a resolved local placement is the runner's own, marked
// runner-asserted, with its capacity and roots.
func TestPlacementCapabilities_LocalIsTheRunnersOwnAnswer(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := installRunner(t, s, true, laptopCaps(types.CC1))
	got, err := s.placementCapabilities(capsCtx, types.PlacementLocal, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ConfinementClasses) != 1 || got.ConfinementClasses[0] != types.CC1 || got.Source != types.RunEvidenceRunnerAsserted ||
		got.Capacity.CPUMillisMax != 4000 || len(got.AllowedRoots) != 1 {
		t.Errorf("caps = %+v, want CC1 only, runner_asserted, 4000 mCPU, one root", got)
	}
	w := httptest.NewRecorder()
	_, _, ok := s.resolveEnforcedConfinement(capsCtx, w, types.PlacementLocal, id, types.RunPolicySpec{MinConfinementClass: types.CC2}, "", composer.Baseline{}, runPlacement{})
	if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "confinement_class_unsupported") {
		t.Errorf("ok=%v %d %s: the runner offers CC1, so a CC2 demand must be refused though the deployment offers CC2", ok, w.Code, w.Body.String())
	}
}

// LE-4: another person's runner, online and reporting, is answered exactly like
// an id that does not exist, at every reader of a runner's capabilities: 404
// runner_not_found, with nothing of its stream (online, driver, classes) in it.
func TestPlacementCapabilities_AnotherPersonsRunnerIsNotFound(t *testing.T) {
	s := capsServer(&fakeRunner{})
	theirs := installRunnerOf(t, s, "someone-else", true, laptopCaps(types.CC1, types.CC2))
	unclaimed := installRunner(t, s, true, laptopCaps(types.CC1))
	rows := s.cfg.Store.(ownedRunners).rows
	r := rows[unclaimed]
	r.State = types.RunnerUnclaimed
	rows[unclaimed] = r
	for name, id := range map[string]uuid.UUID{"another person's": theirs, "my unclaimed": unclaimed, "unknown": uuid.New(), "none named": uuid.Nil} {
		t.Run(name, func(t *testing.T) {
			got, err := s.placementCapabilities(capsCtx, types.PlacementLocal, id)
			if ref := asRefusal(t, err); ref.Reason != placement.ReasonRunnerNotFound || ref.Reason.Status() != http.StatusNotFound {
				t.Errorf("refusal = %+v, want runner_not_found 404", ref)
			}
			if got.Driver != "" || len(got.ConfinementClasses) != 0 {
				t.Errorf("caps = %+v: the runner's stream was read", got)
			}
			pausing := governanceCeiling{Limits: types.GovernanceLimits{RunLimits: types.RunLimits{PauseIdleAfterSec: 300}}}
			req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}
			if ref := s.placementCapabilityRefusal(capsCtx, req, runPlacement{}, pausing); ref == nil || ref.status != http.StatusNotFound || ref.body.Reason != string(placement.ReasonRunnerNotFound) {
				t.Errorf("capability step = %+v, want 404 runner_not_found", ref)
			}
			w := httptest.NewRecorder()
			if _, _, ok := s.resolveEnforcedConfinement(capsCtx, w, types.PlacementLocal, id, types.RunPolicySpec{}, "", composer.Baseline{}, runPlacement{}); ok || w.Code != http.StatusNotFound {
				t.Fatalf("confinement = %d %s, want 404", w.Code, w.Body)
			}
			for _, leak := range []string{"offline", "CC2", "runner:"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("the 404 discloses %q: %s", leak, w.Body)
				}
			}
		})
	}
	// The same runner is its owner's to read.
	if _, err := s.placementCapabilities(withOIDCHuman(context.Background(), "someone-else"), types.PlacementLocal, theirs); err != nil {
		t.Errorf("the owner's own read = %v", err)
	}
}

// D-TM r2 finding 2: dispatch's managed-file decisions read the substrate the run lands on. A run bound to a
// runner that delivers no managed files withholds the harness credential file though the deployment's own
// executor delivers them; an unbound run still reads the deployment's.
func TestRunCapabilities_DispatchReadsTheLandingRunner(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := installRunner(t, s, true, laptopCaps(types.CC1, types.CC2))
	bound := types.AgentRun{ID: uuid.New(), Placement: types.PlacementLocal, RunnerID: &id, ConfinementClass: types.CC2}
	if reason := s.toolgateCredentialWithheld(context.Background(), bound, nil); reason == "" {
		t.Error("a runner without managed files was handed the harness credential file")
	}
	if caps, err := s.runCapabilities(context.Background(), bound); err != nil || caps.ManagedFiles || len(caps.ConfinementClasses) != 2 {
		t.Errorf("the bound run's capabilities = %+v (%v), want the runner's own: CC1 and CC2, no managed files", caps, err)
	}
	if reason := s.toolgateCredentialWithheld(context.Background(), types.AgentRun{ID: uuid.New(), ConfinementClass: types.CC2}, nil); reason != "" {
		t.Errorf("the deployment's executor delivers managed files, yet: %q", reason)
	}
}

func TestIntersectCapabilities_UnresolvedIsTheIntersection(t *testing.T) {
	org := placementCaps{Source: types.RunEvidenceSubstrate, Capabilities: runner.Capabilities{
		Driver: "org", ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
		Resolved:   map[types.ConfinementClass]string{types.CC1: "oci/runc", types.CC2: "oci/runsc", types.CC3: "oci/kata"},
		Freeze:     map[types.ConfinementClass]bool{types.CC1: true, types.CC2: true},
		UserDrives: true, ManagedFiles: true, SessionRecording: true, StructuralEgress: true, NetworkPolicy: true,
		EphemeralDiskEnforcement: types.StorageEnforcementFilesystem,
	}}
	laptop := placementCaps{Source: types.RunEvidenceRunnerAsserted, Capabilities: runner.Capabilities{
		Driver: "runner:x", ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2},
		Resolved:   map[types.ConfinementClass]string{types.CC1: "oci/runc", types.CC2: "oci/runsc"},
		Freeze:     map[types.ConfinementClass]bool{types.CC1: false, types.CC2: true},
		UserDrives: true, SessionRecording: true, StructuralEgress: true,
		EphemeralDiskEnforcement: types.StorageEnforcementEviction, ExecOutputUncaptured: true,
	}}

	got := intersectCapabilities(org, laptop)
	if want := []types.ConfinementClass{types.CC1, types.CC2}; !equalClasses(got.ConfinementClasses, want) {
		t.Errorf("classes = %v, want %v: CC3 is the organisation's alone", got.ConfinementClasses, want)
	}
	if got.EphemeralDiskEnforcement != types.StorageEnforcementEviction {
		t.Errorf("disk enforcement = %q, want eviction: the weakest word wins, never the strongest", got.EphemeralDiskEnforcement)
	}
	if got.ManagedFiles || got.NetworkPolicy || !got.UserDrives || !got.SessionRecording {
		t.Errorf("flags = %+v: a flag holds only when every placement has it", got.Capabilities)
	}
	if got.Freeze[types.CC1] || !got.Freeze[types.CC2] {
		t.Errorf("freeze = %v: per class, and only where every placement can", got.Freeze)
	}
	if !got.ExecOutputUncaptured || got.Source != types.RunEvidenceRunnerAsserted {
		t.Errorf("uncaptured=%v source=%q: either placement's gap and its asserted source carry over", got.ExecOutputUncaptured, got.Source)
	}
	if !equalClasses(intersectCapabilities(laptop, org).ConfinementClasses, got.ConfinementClasses) {
		t.Error("the intersection depends on argument order")
	}
	if one := intersectCapabilities(org); !equalClasses(one.ConfinementClasses, org.ConfinementClasses) || one.Source != types.RunEvidenceSubstrate {
		t.Errorf("one placement = %+v, want its own answer", one)
	}
}

func equalClasses(a, b []types.ConfinementClass) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// No executor wired is no answer: the primitive fails closed, so a caller that
// reads capabilities can never take an empty answer for "nothing is required".
func TestPlacementCapabilities_NoRunnerIsNoAnswer(t *testing.T) {
	_, err := capsServer(nil).placementCapabilities(context.Background(), types.PlacementRemote, uuid.Nil)
	if !errors.Is(err, errNoRemoteExecutor) {
		t.Errorf("error = %v, want errNoRemoteExecutor", err)
	}
	if _, err := capsServer(&fakeRunner{capsErr: errors.New("daemon down")}).placementCapabilities(context.Background(), types.PlacementRemote, uuid.Nil); err == nil {
		t.Error("an unreadable answer was taken for an empty one")
	}
}

// A demand for an optional interface the placement lacks is refused, never
// dropped: a runner implements neither Freezer nor ActivitySampler (design 3.4),
// so a profile that pauses idle runs cannot place one there.
func TestPlacementCapability_UnsupportedInterfaceRefused(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := installRunner(t, s, true, laptopCaps(types.CC1))
	local := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}
	pausing := governanceCeiling{Limits: types.GovernanceLimits{RunLimits: types.RunLimits{PauseIdleAfterSec: 300}}}

	ref := s.placementCapabilityRefusal(capsCtx, local, runPlacement{}, pausing)
	if ref == nil || ref.body.Reason != string(placement.ReasonPlacementCapability) || ref.status != http.StatusUnprocessableEntity ||
		!strings.Contains(ref.body.Error, "pause_idle_after_sec") {
		t.Fatalf("refusal = %+v, want 422 placement_capability naming pause_idle_after_sec", ref)
	}
	if ref := s.placementCapabilityRefusal(capsCtx, local, runPlacement{}, governanceCeiling{}); ref != nil {
		t.Errorf("no demand was made, yet: %+v", ref)
	}
	remoteReq := createRunRequest{Placement: types.PlacementRemote}
	if ref := s.placementCapabilityRefusal(capsCtx, remoteReq, runPlacement{}, pausing); ref != nil {
		t.Errorf("the deployment's own executor was asked: %+v", ref)
	}
	offline := createRunRequest{Placement: types.PlacementLocal, RunnerID: installRunner(t, s, false, laptopCaps(types.CC1)).String()}
	if ref := s.placementCapabilityRefusal(capsCtx, offline, runPlacement{}, pausing); ref == nil || ref.body.Reason != string(placement.ReasonRunnerOffline) {
		t.Errorf("an offline runner = %+v, want runner_offline", ref)
	}
}

// verifiedLocalPaths: a path counts only when its stored binding is the
// candidate runner and it lies inside that runner's allowed roots.
func TestVerifiedLocalPaths_BoundToTheCandidateInsideItsRoots(t *testing.T) {
	cand, other := uuid.New(), uuid.New()
	roots := []string{"/srv/dev/work"}
	got := verifiedLocalPaths(cand, roots, map[string]boundLocalPath{
		"SandboxSpec.Mounts[0]": {cand, "/srv/dev/work/app"},
		"SandboxSpec.Mounts[1]": {other, "/srv/dev/work/app"},
		"SandboxSpec.Mounts[2]": {cand, "/srv/dev/workshop"},
		"SandboxSpec.Mounts[3]": {cand, "/srv/dev/work/../.ssh"},
		"SandboxSpec.Mounts[4]": {cand, "work/app"},
		"SandboxSpec.Drives[0]": {cand, "/srv/dev/work"},
	})
	if len(got) != 2 || !got["SandboxSpec.Mounts[0]"] || !got["SandboxSpec.Drives[0]"] {
		t.Errorf("verified = %v, want only the candidate's paths inside its roots", got)
	}
	if v := verifiedLocalPaths(uuid.Nil, roots, map[string]boundLocalPath{"SandboxSpec.Drives[0]": {uuid.Nil, "/srv/dev/work"}}); len(v) != 0 {
		t.Errorf("no candidate runner verified %v", v)
	}
}

// deny_placements at every door a run request meets: create, Review (preflight)
// and the policy preview each answer 403 placement_denied naming the profile,
// with the authz.denied row a governance door writes, and create places no run.
func TestDenyPlacements_RefusedAtEveryDoor(t *testing.T) {
	doors := []struct{ name, path string }{
		{"create", "/api/v1/runs"}, {"preflight", "/api/v1/runs/preflight"}, {"preview", "/api/v1/runs/policy-preview"},
	}
	for _, tc := range []struct {
		name   string
		deny   types.Placement
		extra  string
		wantIn string
	}{
		{"local denied, local requested", types.PlacementLocal, `,"placement":"local"`, "placement local is not allowed"},
		{"remote denied, remote requested", types.PlacementRemote, `,"placement":"remote"`, "placement remote is not allowed"},
		{"remote denied, placement left empty", types.PlacementRemote, ``, "placement remote is not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit := govEscapeFixture(t, assignedStore(limitsProfile("walled", types.GovernanceLimits{DenyPlacements: []types.Placement{tc.deny}})))
			session := govSession(t, "sub-walled", []string{"eng"}, false)
			body := `{"agent":"claude-code","task":"t"` + tc.extra + `}`
			var messages []string
			for _, door := range doors {
				w := doSSO(t, srv, http.MethodPost, door.path, session, body)
				var got struct{ Error, Reason string }
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("%s: decode %q: %v", door.name, w.Body.String(), err)
				}
				if w.Code != http.StatusForbidden || got.Reason != "placement_denied" ||
					!strings.Contains(got.Error, tc.wantIn) || !strings.Contains(got.Error, `"walled"`) {
					t.Fatalf("%s = %d %s, want 403 placement_denied naming profile walled (%q)", door.name, w.Code, w.Body.String(), tc.wantIn)
				}
				messages = append(messages, got.Error)
			}
			if messages[0] != messages[1] || messages[0] != messages[2] {
				t.Errorf("the doors disagree: %q", messages)
			}
			rows := 0
			audit.mu.Lock()
			for _, ev := range audit.events {
				if ev.Action == authz.AuditAction && ev.Target == "runs.placement" {
					rows++
				}
			}
			audit.mu.Unlock()
			if rows != len(doors) || !recHasRefusal(audit, authz.ReasonGovernanceProfile, "runs.placement") {
				t.Errorf("authz.denied{runs.placement} rows = %d, want %d under governance_profile", rows, len(doors))
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if len(st.runs) != 0 {
				t.Errorf("a refused create left %d run row(s)", len(st.runs))
			}
		})
	}
}

// deny_placements is a deny list: absent means permitted, for a profile that
// names nothing, one that denies the other placement, a member with no
// profile, and a super admin (whose ceiling resolves no profile at all).
func TestDenyPlacements_AbsentMeansPermitted(t *testing.T) {
	create := func(t *testing.T, srv *Server, bearer *http.Cookie, body string) *httptest.ResponseRecorder {
		t.Helper()
		return doSSO(t, srv, http.MethodPost, "/api/v1/runs", bearer, body)
	}
	const remoteBody = `{"agent":"claude-code","task":"t","placement":"remote"}`

	t.Run("a profile with no list", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("open", types.GovernanceLimits{})))
		if w := create(t, srv, govSession(t, "sub-open", []string{"eng"}, false), remoteBody); w.Code != http.StatusCreated {
			t.Errorf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
	t.Run("a profile that denies the other placement", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-laptops", types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}})))
		if w := create(t, srv, govSession(t, "sub-open", []string{"eng"}, false), remoteBody); w.Code != http.StatusCreated {
			t.Errorf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
	t.Run("a member with no assigned profile is not denied local", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, &capStore{})
		w := create(t, srv, govSession(t, "sub-free", []string{"eng"}, false), `{"agent":"claude-code","task":"t","placement":"local"}`)
		if strings.Contains(w.Body.String(), "placement_denied") {
			t.Errorf("an unassigned member was denied a placement: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("a super admin is outside governance", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-remote", types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementRemote}})))
		if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, remoteBody); w.Code != http.StatusCreated {
			t.Errorf("admin create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
}

func TestDenyPlacements_ProfileDenyingBothRefusedAtWrite(t *testing.T) {
	decode := func(limits string) string {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", strings.NewReader(govLimitsBody(limits)))
		_, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r)
		return msg
	}
	const frozen = "deny_placements: a profile can't deny every placement — the people it binds could run nothing"
	if got := decode(`"deny_placements":["remote","local"]`); got != frozen {
		t.Errorf("both denied = %q, want the frozen REFUSED_ALL_PLACEMENTS sentence", got)
	}
	if got := decode(`"deny_placements":["local","remote","local"]`); got != frozen {
		t.Errorf("both denied, repeated = %q", got)
	}
	if got := decode(`"deny_placements":["cloud"]`); !strings.Contains(got, "limits.deny_placements") {
		t.Errorf("unknown placement = %q, want it named", got)
	}
	for _, ok := range []string{`"deny_placements":["local"]`, `"deny_placements":["remote"]`, `"deny_placements":[]`, ``} {
		if got := decode(ok); got != "" {
			t.Errorf("limits {%s} refused with %q", ok, got)
		}
	}
}

// resolveEnforcedConfinement's no-executor carve-out is exactly one case: the
// remote placement with nothing wired (headless -runner none). A local placement
// never reads s.cfg.Runner, and a wired runner that cannot answer refuses.
func TestResolveEnforcedConfinement_PlacementTable(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC1}
	for _, tc := range []struct {
		name       string
		srv        *Server
		placement  types.Placement
		wantOK     bool
		wantStatus int
		wantReason string
	}{
		{"local placement, no runner wired: runner_not_found", capsServer(nil), types.PlacementLocal, false, http.StatusNotFound, "runner_not_found"},
		{"remote placement, wired runner cannot answer: 503", capsServer(&fakeRunner{capsErr: errors.New("daemon down")}), types.PlacementRemote, false, http.StatusServiceUnavailable, reasonRunnerCapabilitiesUnavailable},
		{"remote placement, nothing wired: the headless carve-out", capsServer(nil), types.PlacementRemote, true, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, _, ok := tc.srv.resolveEnforcedConfinement(capsCtx, w, tc.placement, uuid.Nil, spec, "", composer.Baseline{}, runPlacement{})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (%d %s), want %v", ok, w.Code, w.Body.String(), tc.wantOK)
			}
			if !tc.wantOK && (w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), `"`+tc.wantReason+`"`)) {
				t.Errorf("= %d %s, want %d %s", w.Code, w.Body.String(), tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// The operator exemption: a caller whose runs stand outside governance is not
// denied a placement even when the ceiling it carries names a profile; with
// GovernAdminRuns on, an SSO admin is governed like a member and is denied, while
// the admin token (no person) stays break-glass.
func TestDenyPlacements_OperatorExemption(t *testing.T) {
	ceiling := governanceCeiling{
		Profile: &ResolvedProfile{ID: uuid.New(), Name: "walled"},
		Limits:  types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementRemote}},
	}
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		govern     bool
		wantDenied bool
	}{
		{"SSO admin, switch off: exempt even carrying a profile", governAdminSessionCtx(), false, false},
		{"admin token, switch off: exempt", governAdminTokenCtx(t), false, false},
		{"SSO admin, switch on: governed, denied", governAdminSessionCtx(), true, true},
		{"admin token, switch on: no person, still exempt", context.Background(), true, false},
		{"member: denied", govMemberCtx([]string{"eng"}, false), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Audit: &recRecorder{}, GovernAdminRuns: tc.govern})
			ref := s.placementDeniedRefusal(tc.ctx, types.PlacementRemote, ceiling)
			if denied := ref != nil; denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v", denied, tc.wantDenied)
			}
			if ref != nil && (ref.status != http.StatusForbidden || ref.body.Reason != "placement_denied") {
				t.Errorf("refusal = %+v, want 403 placement_denied", ref)
			}
		})
	}
}

// The source-scan door honours deny_placements through the same check the fold
// uses, releases its fence, and answers 403 placement_denied with the audit row.
func TestDenyPlacements_SourceScanDoor(t *testing.T) {
	denying := govProfile("walled")
	denying.Limits.DenyPlacements = []types.Placement{types.PlacementRemote}
	launch := func(t *testing.T, cs *capStore) (*Server, *recRecorder, *fakeRunner, error) {
		t.Helper()
		srv, st, audit := govEscapeFixture(t, cs)
		fr := &fakeRunner{}
		srv.cfg.Runner = fr
		srv.cfg.Store = &scanLaneStore{govEscapeStore: st}
		src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: govWorkspaceRepo}
		_, err := srv.launchSourceScanRun(govMemberCtx([]string{"eng"}, false), "bob@corp.example", src, nil)
		return srv, audit, fr, err
	}

	srv, audit, fr, err := launch(t, assignedStore(denying))
	var denied *placementDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("launch error = %v, want a placement denial", err)
	}
	if fr.createCalls != 0 {
		t.Errorf("a denied scan still created %d sandbox(es)", fr.createCalls)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/x/scan", nil).WithContext(govMemberCtx([]string{"eng"}, false))
	w := httptest.NewRecorder()
	if !srv.writeScanLaunchRefusal(w, req, err) || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"placement_denied"`) ||
		!strings.Contains(w.Body.String(), `walled`) {
		t.Errorf("handler answer = %d %s, want 403 placement_denied naming walled", w.Code, w.Body.String())
	}
	if !recHasRefusal(audit, authz.ReasonGovernanceProfile, "runs.placement") {
		t.Error("no authz.denied row for runs.placement")
	}

	// The control: the same member under a profile that denies the other
	// placement launches the scan, so the refusal above is the denial.
	allowing := govProfile("no-laptops")
	allowing.Limits.DenyPlacements = []types.Placement{types.PlacementLocal}
	if _, _, _, err := launch(t, assignedStore(allowing)); err != nil {
		t.Errorf("scan under a local-only denial: %v", err)
	}
}
