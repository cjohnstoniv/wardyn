// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// addRunner registers a claimed runner owned by owner, connected on this replica with caps.
func (f *moveFixture) addRunner(t *testing.T, owner string, kind types.RunnerOwnerKind, caps runnerwire.Caps) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.ms.mmu.Lock()
	f.ms.runners[id] = types.Runner{ID: id, Owner: owner, OwnerKind: kind, State: types.RunnerClaimed, Arch: "amd64"}
	f.ms.mmu.Unlock()
	registerMoveRunnerCaps(t, f.srv, id, caps)
	return id
}

// registerMoveRunnerCaps connects runner id on srv's stream with caps.
func registerMoveRunnerCaps(t *testing.T, srv *Server, id uuid.UUID, caps runnerwire.Caps) {
	t.Helper()
	caps.Version = "test"
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
}

// moveCapsAll is a runner that serves moves and carries every proxy setting.
var moveCapsAll = runnerwire.Caps{MoveState: true, MoveImportPresent: true, ProxySettings: runnerwire.ProxySettings,
	Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}, StructuralEgress: true, ManagedFiles: true}}

// wantNotStopped: a refused move stopped nothing, stamped nothing and moved no byte.
func (f *moveFixture) wantNotStopped(t *testing.T) {
	t.Helper()
	if f.fk.count("stop") != 0 || f.fk.count("stamp") != 0 || f.mr.exportCount() != 0 || f.mr.importCount() != 0 {
		t.Errorf("stop %d stamp %d exports %d imports %d, want nothing done", f.fk.count("stop"), f.fk.count("stamp"),
			f.mr.exportCount(), f.mr.importCount())
	}
	if cur := f.current(t); cur.PausedReason != f.run.PausedReason {
		t.Errorf("paused reason = %q, want %q unchanged", cur.PausedReason, f.run.PausedReason)
	}
}

// TestMoveDest_StripsOrgConfigForRunner: a run moved to a person's runner is created without the organisation's
// component config, keeps its own, and its spec carries no proxy config; a move between the organisation's own
// executors keeps the config.
func TestMoveDest_StripsOrgConfigForRunner(t *testing.T) {
	id := uuid.New()
	rec := runSandboxSpecRecord{Image: "img", Env: map[string]string{"ORG_COMPONENT_URL": "https://org.example", "MY_TOOL": "on"},
		OrgConfigKeys: []string{"ORG_COMPONENT_URL"}}
	run := types.AgentRun{ID: uuid.New()}
	local := moveSandboxSpec(run, rec, moveDest{Placement: types.PlacementLocal, RunnerID: &id}, moveDestPlan{})
	if _, ok := local.Env["ORG_COMPONENT_URL"]; ok || local.Env["MY_TOOL"] != "on" {
		t.Errorf("runner destination env = %v, want the org key gone and the person's kept", local.Env)
	}
	if err := runner.CheckImportSpec(local); err != nil {
		t.Errorf("the destination spec is refused by the importer: %v", err)
	}
	remoteSpec := moveSandboxSpec(run, rec, moveDest{Placement: types.PlacementRemote}, moveDestPlan{})
	if remoteSpec.Env["ORG_COMPONENT_URL"] == "" {
		t.Error("a remote destination lost the org's config")
	}
	if rec.Env["ORG_COMPONENT_URL"] == "" {
		t.Error("the strip changed the record")
	}
}

// TestMoveDest_KeepsRunLabels: the destination's spec carries the run's labels as dispatch sets them, for a runner
// and for the organisation's executors.
func TestMoveDest_KeepsRunLabels(t *testing.T) {
	id := uuid.New()
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code"}
	for name, d := range map[string]moveDest{"runner": {Placement: types.PlacementLocal, RunnerID: &id}, "org": {Placement: types.PlacementRemote}} {
		spec := moveSandboxSpec(run, runSandboxSpecRecord{Image: "img"}, d, moveDestPlan{})
		if spec.Labels["wardyn.run"] != run.ID.String() || spec.Labels["wardyn.agent"] != "claude-code" {
			t.Errorf("%s: labels = %v, want wardyn.run and wardyn.agent", name, spec.Labels)
		}
		if err := runner.CheckImportSpec(spec); err != nil {
			t.Errorf("%s: the importer refuses the labelled spec: %v", name, err)
		}
	}
}

// TestMoveCaps_ReadsProxySettings: the caps check asks moveProxySettingMissing, so a runner that cannot carry a
// setting the run's proxy uses is refused before anything stops.
func TestMoveCaps_ReadsProxySettings(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run_move_policy.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "moveCapsRefusal" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "moveProxySettingMissing" {
					calls++
				}
			}
			return true
		})
	}
	if calls == 0 {
		t.Error("moveCapsRefusal does not call moveProxySettingMissing")
	}
}

// TestMovePolicy_DestinationSubstrateRefused: a runner whose substrate cannot hold an imported, unstarted sandbox,
// and an executor that runs the class exec-less, each refuse 409 move_destination_substrate before anything stops.
func TestMovePolicy_DestinationSubstrateRefused(t *testing.T) {
	f := newMoveFixture(t)
	rid := f.addRunner(t, f.run.CreatedBy, types.RunnerOwnerOrg, runnerwire.Caps{MoveState: true, ProxySettings: runnerwire.ProxySettings})
	rec, _ := f.srv.loadRunSandboxSpec(context.Background(), f.run.ID)
	rn, _ := f.ms.GetRunner(context.Background(), rid)
	_, ref, err := f.srv.moveCapsRefusal(context.Background(), f.run, rec, moveDest{Placement: types.PlacementLocal, RunnerID: &rid, Runner: rn})
	if err != nil || ref == nil || ref.reason != reasonMoveDestinationSubstrate || ref.status != http.StatusConflict {
		t.Errorf("runner without an import present: %+v %v, want 409 move_destination_substrate", ref, err)
	}

	f.mr.capsResolved = map[types.ConfinementClass]string{types.CC1: "oci/krun"}
	code, body := f.move(t, "org")
	if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveDestinationSubstrate {
		t.Errorf("exec-less executor: %d %s, want 409 move_destination_substrate", code, body)
	}
	f.wantNotStopped(t)
}

// TestMovePolicyNeverWidensSpec: the narrowing only removes allowed hosts and adds denies; a destination that would
// allow more adds nothing, and one that differs in a field a move cannot narrow refuses by that field.
func TestMovePolicyNeverWidensSpec(t *testing.T) {
	src := types.RunPolicySpec{AllowedDomains: []string{"a.example", "b.example"}, DeniedDomains: []string{"x.example"}}
	for _, c := range []struct {
		name      string
		dst       types.RunPolicySpec
		ownerDeny []string
		want      moveNarrowed
		field     string
	}{
		{"wider allows nothing more", types.RunPolicySpec{AllowedDomains: []string{"a.example", "b.example", "c.example"}}, nil, moveNarrowed{}, ""},
		{"narrower removes", types.RunPolicySpec{AllowedDomains: []string{"a.example"}}, nil, moveNarrowed{Remove: []string{"b.example"}}, ""},
		{"owner deny added", src, []string{"y.example", "x.example"}, moveNarrowed{Deny: []string{"y.example"}}, ""},
		{"all egress", types.RunPolicySpec{AllowedDomains: src.AllowedDomains, AllowAllEgress: true}, nil, moveNarrowed{}, "AllowAllEgress"},
		{"tool rules", types.RunPolicySpec{AllowedDomains: src.AllowedDomains, MaxHolds: 9}, nil, moveNarrowed{}, "MaxHolds"},
	} {
		got, field := moveNarrowing(src, c.dst, c.ownerDeny)
		if field != c.field || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v %q, want %+v %q", c.name, got, field, c.want, c.field)
		}
		for _, d := range got.Remove {
			if !slices.Contains(src.AllowedDomains, d) {
				t.Errorf("%s: removes %s, which the run never allowed", c.name, d)
			}
		}
		for _, d := range got.Deny {
			if slices.Contains(src.DeniedDomains, d) {
				t.Errorf("%s: adds deny %s twice", c.name, d)
			}
		}
	}
}

// TestMovePolicyCoversEverySpecField: every field of the run's policy is classed, and every class names a field.
func TestMovePolicyCoversEverySpecField(t *testing.T) {
	typ := reflect.TypeOf(types.RunPolicySpec{})
	fields := map[string]bool{}
	for i := range typ.NumField() {
		fields[typ.Field(i).Name] = true
		if _, ok := moveSpecFields[typ.Field(i).Name]; !ok {
			t.Errorf("RunPolicySpec.%s has no move class", typ.Field(i).Name)
		}
	}
	var narrow []string
	for name, class := range moveSpecFields {
		if !fields[name] {
			t.Errorf("moveSpecFields names %s, which RunPolicySpec does not have", name)
		}
		if class == moveFieldNarrow {
			narrow = append(narrow, name)
		}
	}
	slices.Sort(narrow)
	if want := []string{"AllowedDomains", "DeniedDomains"}; !slices.Equal(narrow, want) {
		t.Errorf("fields a move narrows = %v, want exactly %v", narrow, want)
	}
}

// TestMovePolicy_SamePlanArms: commit's comparison names each part of the plan that changed, and finds two equal
// plans the same.
func TestMovePolicy_SamePlanArms(t *testing.T) {
	base := moveDestPlan{Trust: types.RunnerTrustPersonal, Deliveries: map[uuid.UUID]string{},
		Narrowed: moveNarrowed{Deny: []string{"a.example"}}}
	if layer, same := base.samePlan(base); !same || layer != "" {
		t.Errorf("a plan against itself = %q %v, want the same", layer, same)
	}
	for layer, change := range map[string]func(*moveDestPlan){
		"trust_class": func(p *moveDestPlan) { p.Trust = types.RunnerTrustManagedDevice },
		"grants":      func(p *moveDestPlan) { p.Mint.Keep = append(p.Mint.Keep, uuid.New()) },
		"deliveries": func(p *moveDestPlan) {
			p.Deliveries = map[uuid.UUID]string{uuid.New(): string(types.GrantDeliveryViaOrg)}
		},
		"narrowed":      func(p *moveDestPlan) { p.Narrowed.Deny = append(slices.Clone(p.Narrowed.Deny), "b.example") },
		"managed_files": func(p *moveDestPlan) { p.DropSettings = true },
	} {
		other := base
		change(&other)
		if got, same := base.samePlan(other); same || got != layer {
			t.Errorf("%s changed: samePlan = %q %v, want %q", layer, got, same, layer)
		}
	}
}

// TestMovePolicyNarrowsOrRefuses: one case per layer the move judges on the destination. Each refusal starts from a
// RUNNING run, stops nothing and ends its admitted row refused; a narrowing destination is admitted with the
// narrowing recorded; Claude Code's settings without tool rules follow dispatch's fallback.
func TestMovePolicyNarrowsOrRefuses(t *testing.T) {
	for _, c := range []struct {
		name, reason, layer string
		setup               func(t *testing.T, f *moveFixture)
	}{
		{"keep", reasonMoveDestinationRefused, "keep", func(_ *testing.T, f *moveFixture) {
			calls := 0
			movePortStopRefusal = func(context.Context, moveKeepArgs) (string, error) {
				calls++
				if calls > 1 {
					return "this executor ends a stopped sandbox", nil
				}
				return "", nil
			}
		}},
		{"bound folder", reasonMoveDestinationRefused, "bound_folder", func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, Mounts: []runner.Mount{{BoundRunner: uuid.NewString()}}})
		}},
		{"confinement", reasonMoveDestinationRefused, "confinement", func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, ConfinementClass: types.CC3})
			f.mr.capsClasses = []types.ConfinementClass{types.CC1}
		}},
		{"managed files", reasonMoveDestinationRefused, "managed_files", func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true,
				ManagedFiles: []runner.ManagedFile{{Path: agentpolicy.CodexRequirementsPath}}})
			f.mr.noManagedFiles = true
		}},
		{"withheld grant", "move_grant_refused", "grant:github", func(_ *testing.T, f *moveFixture) {
			f.fk.mintErr = &moveGrantRefusal{Layer: "grant:github", Reason: "move_grant_refused"}
		}},
		{"resident", reasonMoveResidentCredential, "resident", func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, Resident: true})
		}},
		{"mcp", reasonMoveMCPToken, "mcp", func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, MCPToken: true})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMoveFixture(t)
			c.setup(t, f)
			code, body := f.move(t, "org")
			reason, field := moveAnswer(t, body)
			if code != http.StatusForbidden || reason != c.reason || field != c.layer {
				t.Fatalf("move = %d %s, want 403 %s layer %s", code, body, c.reason, c.layer)
			}
			f.wantNotStopped(t)
			moves, _ := f.ms.ListRunMoves(context.Background(), f.run.ID)
			if c.layer == "resident" || c.layer == "mcp" {
				// The recorded spec's own refusal is step 1's, before the admitted row: a denied audit row and no move.
				if len(moves) != 0 || f.moveRows("denied") != 1 {
					t.Fatalf("run_moves rows = %+v, denied audit rows = %d, want none and 1", moves, f.moveRows("denied"))
				}
				return
			}
			if len(moves) == 0 {
				t.Fatal("the refusal left no run_moves row")
			}
			for _, mv := range moves {
				if mv.State != types.MoveRefused || mv.Failure != c.reason {
					t.Errorf("row = %s %q, want refused %q", mv.State, mv.Failure, c.reason)
				}
			}
		})
	}

	// The destination's architecture and a shared drive, on the recorded spec alone.
	rid := uuid.New()
	local := moveDest{Placement: types.PlacementLocal, RunnerID: &rid, Runner: types.Runner{ID: rid, Arch: "arm64"}}
	caps := runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}
	if ref := moveRecordRefusal(runSandboxSpecRecord{ImageArches: []string{"amd64"}}, types.AgentRun{}, local, caps, "docker"); ref == nil || ref.layer != "image_arch" {
		t.Errorf("image built for another architecture: %+v, want layer image_arch", ref)
	}
	team := runSandboxSpecRecord{Drives: []types.DriveMount{{DriveName: "team-notes", Team: true}}}
	if ref := moveRecordRefusal(team, types.AgentRun{}, local, caps, "docker"); ref == nil || ref.layer != "drive" {
		t.Errorf("team drive to a runner: %+v, want layer drive", ref)
	}

	// Claude Code's settings: a gap refuses with tool rules, and without them the file is left out and said so.
	const settings = "/etc/claude-code/managed-settings.json"
	noFiles := runner.Capabilities{Driver: "laptop"}
	withRules := runSandboxSpecRecord{ManagedFiles: []runner.ManagedFile{{Path: settings}},
		AgentPolicy: runSandboxSpecAgentPolicy{Path: settings, ToolRules: 2}}
	if _, _, refusal := moveSettingsFallback(withRules, noFiles); !strings.Contains(refusal, settings) {
		t.Errorf("settings with tool rules at a gap: %q, want a refusal naming %s", refusal, settings)
	}
	noRules := withRules
	noRules.AgentPolicy.ToolRules = 0
	drop, why, refusal := moveSettingsFallback(noRules, noFiles)
	if refusal != "" || !drop || why == "" {
		t.Errorf("settings without tool rules: drop %v why %q refusal %q, want the file left out with its reason", drop, why, refusal)
	}
	if spec := moveSandboxSpec(types.AgentRun{}, noRules, local, moveDestPlan{DropSettings: drop}); len(spec.ManagedFiles) != 0 {
		t.Errorf("destination spec managed files = %v, want none", spec.ManagedFiles)
	}

	// A narrowing destination is admitted: the owner's current denies are added and recorded.
	f := newMoveFixture(t)
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("narrowing move = %d %s, want 202", code, body)
	}
	var n moveNarrowed
	if err := json.Unmarshal(f.only(t).Narrowed, &n); err != nil {
		t.Fatal(err)
	}
	for _, host := range f.profile.Ceiling.DeniedDomains {
		if !slices.Contains(n.Deny, host) {
			t.Errorf("narrowed deny = %v, want the owner's deny %s", n.Deny, host)
		}
	}
}

// TestMovePolicy_RecheckedAtCommit: a grant the destination withholds that appears between the destination
// judgement and the stop fails the move move_policy_changed before commit; the source stays paused, resumable.
func TestMovePolicy_RecheckedAtCommit(t *testing.T) {
	f := newMoveFixture(t)
	f.fk.onStop = func() { f.fk.mintErr = &moveGrantRefusal{Layer: "grant:late", Reason: "move_grant_refused"} }
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("move = %d %s", code, body)
	}
	mv := f.only(t)
	if mv.State != types.MoveFailed || mv.Failure != reasonMovePolicyChanged {
		t.Errorf("row = %s %q, want failed move_policy_changed", mv.State, mv.Failure)
	}
	cur := f.current(t)
	if cur.PausedReason != movePauseStopped || cur.SandboxRef != f.run.SandboxRef {
		t.Errorf("source = %q on %s, want stopped on %s", cur.PausedReason, cur.SandboxRef, f.run.SandboxRef)
	}
	if held, _ := f.ms.RunMoveInFlight(context.Background(), f.run.ID); held {
		t.Error("the failed move still holds the run, so it cannot be resumed")
	}
}

// TestMovePolicyDestinationIsTheOwners: a super admin's "laptop" is the run owner's runner, never the admin's; the
// admin's own runner is unknown to the owner; and a person's runner needs to_personal_runners.
func TestMovePolicyDestinationIsTheOwners(t *testing.T) {
	f := newMoveFixture(t)
	mine := f.addRunner(t, f.run.CreatedBy, types.RunnerOwnerPerson, moveCapsAll)
	admins := f.addRunner(t, "admin@example.com", types.RunnerOwnerPerson, moveCapsAll)
	ctx := withOIDCHuman(context.Background(), "admin@example.com")
	sc := f.ms.site
	d, ref, err := f.srv.resolveMoveDestination(ctx, f.run, types.MoveDestination{Kind: types.MoveToLaptop}, sc)
	if err != nil || ref != nil || d.RunnerID == nil || *d.RunnerID != mine {
		t.Errorf("laptop = %+v %+v %v, want the owner's runner %s", d.RunnerID, ref, err, mine)
	}
	_, ref, err = f.srv.resolveMoveDestination(ctx, f.run, types.MoveDestination{Kind: types.MoveToRunner, Ref: admins.String()}, sc)
	if err != nil || ref == nil || ref.reason != reasonMoveDestinationUnknown {
		t.Errorf("the admin's own runner = %+v %v, want move_destination_unknown", ref, err)
	}
	for _, body := range []string{`{"features":{"run_move":{"enabled":true}}}`, `{"features":{"run_move":{"enabled":true,"to_personal_runners":false}}}`} {
		_, ref, err = f.srv.resolveMoveDestination(ctx, f.run, types.MoveDestination{Kind: types.MoveToLaptop}, siteFeaturesDoc(t, body))
		if err != nil || ref == nil || ref.reason != reasonMoveToPersonalOff || ref.status != http.StatusForbidden {
			t.Errorf("%s: %+v %v, want 403 move_to_personal_off", body, ref, err)
		}
	}
}

// TestMovePolicy_HarnessLoginRunRefused: a provider sign-in run is never moved.
func TestMovePolicy_HarnessLoginRunRefused(t *testing.T) {
	f := newMoveFixture(t)
	f.ms.mu.Lock()
	f.ms.run.Task = harnessLoginTask
	f.ms.mu.Unlock()
	code, body := f.move(t, "org")
	if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveHarnessLogin {
		t.Errorf("move = %d %s, want 409 move_harness_login", code, body)
	}
	f.wantNotStopped(t)
}

// TestMovePolicy_VMKindsRefused: a Linux, macOS or Windows VM run is never moved.
func TestMovePolicy_VMKindsRefused(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, types.MachineMacOSVM, "windows_vm"} {
		f := newMoveFixture(t)
		f.ms.mu.Lock()
		f.ms.run.MachineKind = kind
		f.ms.mu.Unlock()
		code, body := f.move(t, "org")
		if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveMachineKindUnsupported {
			t.Errorf("%s: %d %s, want 409 move_machine_kind_unsupported", kind, code, body)
		}
		f.wantNotStopped(t)
	}
}

// TestMovePolicy_RunnerTooOldRefused: a destination runner that does not serve moves, one missing a proxy setting
// the run's config sets, and a source runner that does not serve moves each refuse 409 move_runner_too_old.
func TestMovePolicy_RunnerTooOldRefused(t *testing.T) {
	f := newMoveFixture(t)
	rec, _ := f.srv.loadRunSandboxSpec(context.Background(), f.run.ID)
	old := f.addRunner(t, f.run.CreatedBy, types.RunnerOwnerOrg, runnerwire.Caps{ProxySettings: runnerwire.ProxySettings})
	_, ref, err := f.srv.moveCapsRefusal(context.Background(), f.run, rec, moveDest{Placement: types.PlacementLocal, RunnerID: &old})
	if err != nil || ref == nil || ref.reason != reasonMoveRunnerTooOld {
		t.Errorf("destination without move_state: %+v %v, want move_runner_too_old", ref, err)
	}
	pc := runner.ProxyConfig{ScrubResponses: true}
	if got := missingProxySetting(pc, runnerwire.Caps{MoveState: true}); got == "" {
		t.Error("a runner without the config's proxy setting was not refused")
	}
	if got := missingProxySetting(pc, moveCapsAll); got != "" {
		t.Errorf("a runner with every proxy setting is missing %q", got)
	}
	src := f.addRunner(t, f.run.CreatedBy, types.RunnerOwnerPerson, runnerwire.Caps{})
	run := f.run
	run.RunnerID = &src
	_, ref, err = f.srv.moveCapsRefusal(context.Background(), run, rec, moveDest{Placement: types.PlacementRemote})
	if err != nil || ref == nil || ref.reason != reasonMoveRunnerTooOld {
		t.Errorf("source without move_state: %+v %v, want move_runner_too_old", ref, err)
	}
}

// TestMovePolicy_LocallyContainedRefused: a contained run refuses 409 move_locally_contained and an unreadable latch
// 503 move_containment_unreadable, each with no runner call; a latch that lands before commit fails the move with the
// source paused.
func TestMovePolicy_LocallyContainedRefused(t *testing.T) {
	for _, c := range []struct {
		contained bool
		err       error
		code      int
		reason    string
	}{
		{true, nil, http.StatusConflict, reasonMoveLocallyContained},
		{false, errors.New("store down"), http.StatusServiceUnavailable, reasonMoveContainmentUnreadable},
	} {
		f := newMoveFixture(t)
		f.fk.contained, f.fk.containedErr = c.contained, c.err
		code, body := f.move(t, "org")
		if reason, _ := moveAnswer(t, body); code != c.code || reason != c.reason {
			t.Errorf("move = %d %s, want %d %s", code, body, c.code, c.reason)
		}
		f.wantNotStopped(t)
	}
	f := newMoveFixture(t)
	f.fk.onStop = func() { f.fk.contained = true }
	f.move(t, "org")
	if mv := f.only(t); mv.State != types.MoveFailed || mv.Failure != reasonMoveLocallyContained {
		t.Errorf("latch before commit: %s %q, want failed move_locally_contained", mv.State, mv.Failure)
	}
	if cur := f.current(t); cur.PausedReason != movePauseStopped {
		t.Errorf("source paused reason = %q, want stopped", cur.PausedReason)
	}
}
