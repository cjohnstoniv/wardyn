// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Extra containers (CreateRunRequest.Containers): their shape, the image grant and admission for each, the
// run's size they share, the placements that can run them, and the profiles and organisations that refuse them.

const (
	kernelImage = "registry.example.com/team/kernel@" + imgDigest
	mcpImage    = "registry.example.com/team/mcp@" + imgDigest
)

func extra(name, image string, ports ...int) client.RunContainer {
	return client.RunContainer{Name: name, Image: image, Ports: ports,
		Resources: client.RunContainerResources{CPUMillis: 250, MemoryMiB: 256}}
}

// twoExtras is the two-container example: a notebook kernel and a tool server beside the agent.
func twoExtras() []client.RunContainer {
	return []client.RunContainer{extra("kernel", kernelImage, 8888), extra("mcp", mcpImage, 5050)}
}

func withExtras(edit func([]client.RunContainer) []client.RunContainer) createRunRequest {
	return createRunRequest{Containers: edit(twoExtras())}
}

// TestContainersShapeRefusal: each malformed entry is 400 run_containers_invalid naming its field, a VM is
// request_field_unavailable, and no value is large enough to wrap later arithmetic.
func TestContainersShapeRefusal(t *testing.T) {
	set := func(i int, edit func(*client.RunContainer)) createRunRequest {
		return withExtras(func(cs []client.RunContainer) []client.RunContainer { edit(&cs[i]); return cs })
	}
	vm := withExtras(func(cs []client.RunContainer) []client.RunContainer { return cs })
	vm.MachineKind = string(types.MachineLinuxVM)
	five := withExtras(func(cs []client.RunContainer) []client.RunContainer {
		return append(cs, extra("a", kernelImage), extra("b", kernelImage), extra("c", kernelImage))
	})
	args := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x"
		}
		return out
	}
	ninePorts := []int{2000, 2001, 2002, 2003, 2004, 2005, 2006, 2007, 2008}
	const invalid = reasonRunContainersInvalid
	for _, tc := range []struct {
		name   string
		req    createRunRequest
		status int
		reason string
		field  string
	}{
		{"none", createRunRequest{}, 0, "", ""},
		{"the two-container example", withExtras(func(cs []client.RunContainer) []client.RunContainer { return cs }), 0, "", ""},
		{"a VM", vm, http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, ""},
		{"five containers", five, http.StatusBadRequest, invalid, "containers"},
		{"named agent", set(0, func(c *client.RunContainer) { c.Name = "agent" }), http.StatusBadRequest, invalid, "containers[0].name"},
		{"named wardyn-x", set(0, func(c *client.RunContainer) { c.Name = "wardyn-x" }), http.StatusBadRequest, invalid, "containers[0].name"},
		{"named stage-x", set(0, func(c *client.RunContainer) { c.Name = "stage-x" }), http.StatusBadRequest, invalid, "containers[0].name"},
		{"an upper-case name", set(0, func(c *client.RunContainer) { c.Name = "Upper" }), http.StatusBadRequest, invalid, "containers[0].name"},
		{"a 41-character name", set(0, func(c *client.RunContainer) { c.Name = strings.Repeat("a", 41) }), http.StatusBadRequest, invalid, "containers[0].name"},
		{"a duplicate name", set(1, func(c *client.RunContainer) { c.Name = "kernel" }), http.StatusBadRequest, invalid, "containers[1].name"},
		{"a tag image", set(0, func(c *client.RunContainer) { c.Image = "registry.example.com/team/kernel:1" }), http.StatusBadRequest, invalid, "containers[0].image"},
		{"an image of 257 bytes", set(0, func(c *client.RunContainer) { c.Image = strings.Repeat("a", 257-len("@"+imgDigest)) + "@" + imgDigest }),
			http.StatusBadRequest, invalid, "containers[0].image"},
		{"an image with a control character", set(0, func(c *client.RunContainer) { c.Image = "registry.example.com/team/k\x01l@" + imgDigest }),
			http.StatusBadRequest, invalid, "containers[0].image"},
		{"zero cpu", set(0, func(c *client.RunContainer) { c.Resources.CPUMillis = 0 }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"zero memory", set(0, func(c *client.RunContainer) { c.Resources.MemoryMiB = 0 }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"cpu above the maximum", set(0, func(c *client.RunContainer) { c.Resources.CPUMillis = 1048577 }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"memory above the maximum", set(0, func(c *client.RunContainer) { c.Resources.MemoryMiB = 16777217 }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"memory of 2^62", set(0, func(c *client.RunContainer) { c.Resources.MemoryMiB = 1 << 62 }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"memory of the largest int", set(0, func(c *client.RunContainer) { c.Resources.MemoryMiB = math.MaxInt }), http.StatusBadRequest, invalid, "containers[0].resources"},
		{"33 command arguments", set(0, func(c *client.RunContainer) { c.Command = args(33) }), http.StatusBadRequest, invalid, "containers[0].command"},
		{"an empty argument", set(0, func(c *client.RunContainer) { c.Command = []string{"serve", ""} }), http.StatusBadRequest, invalid, "containers[0].command"},
		{"port 80", set(0, func(c *client.RunContainer) { c.Ports = []int{80} }), http.StatusBadRequest, invalid, "containers[0].ports"},
		{"port 70000", set(0, func(c *client.RunContainer) { c.Ports = []int{70000} }), http.StatusBadRequest, invalid, "containers[0].ports"},
		{"a port in two containers", set(1, func(c *client.RunContainer) { c.Ports = []int{8888} }), http.StatusBadRequest, invalid, "containers[1].ports"},
		{"nine ports", set(0, func(c *client.RunContainer) { c.Ports = ninePorts }), http.StatusBadRequest, invalid, "containers[0].ports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := containersShapeRefusal(tc.req)
			if tc.reason == "" {
				if refusal != nil {
					t.Fatalf("refused: %d %+v", refusal.status, refusal.body)
				}
				return
			}
			if refusal == nil || refusal.status != tc.status || refusal.body.Reason != tc.reason || refusal.body.Field != tc.field {
				t.Fatalf("refusal = %+v, want %d %s field %q", refusal, tc.status, tc.reason, tc.field)
			}
		})
	}
	// The contract's list holds it, so create, Review, the preview, the preset door and the template door answer alike.
	if refusal := runContractShapeRefusal(five); refusal == nil || refusal.body.Reason != invalid {
		t.Errorf("the contract's shape checks answered %+v for five containers, want %s", refusal, invalid)
	}
}

// TestContainersBudgetRefusal: each extra comes out of what is left of the applied size and the agent keeps its
// floor. Called directly, with no shape check in front, so values large enough to wrap a sum are refused too.
func TestContainersBudgetRefusal(t *testing.T) {
	size := &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 1024}
	sized := func(n, cpu, mem int) []client.RunContainer {
		out := make([]client.RunContainer, n)
		for i := range out {
			out[i] = client.RunContainer{Name: string(rune('a' + i)), Resources: client.RunContainerResources{CPUMillis: cpu, MemoryMiB: mem}}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		extras  []client.RunContainer
		applied *types.ResourceLimits
		refused bool
	}{
		{"none", nil, size, false},
		{"the agent keeps exactly its floor", sized(1, 750, 768), size, false},
		{"one millicore over", sized(1, 751, 768), size, true},
		{"one MiB over", sized(1, 750, 769), size, true},
		{"four of memory 2^62, whose sum wraps to zero", sized(4, 1, 1<<62), size, true},
		{"two of the largest int, whose sum wraps", sized(2, math.MaxInt, math.MaxInt), size, true},
		{"no applied size", sized(1, 1, 1), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := containersBudgetRefusal(tc.extras, tc.applied)
			if !tc.refused {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal.body)
				}
				return
			}
			if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonRunContainersOverBudget ||
				refusal.body.Field != "containers" {
				t.Fatalf("refusal = %+v, want 422 %s field containers", refusal, reasonRunContainersOverBudget)
			}
		})
	}
}

// TestRunRequestGovernanceGatesEachContainerImage: a governed member needs an image grant for every extra's
// image, as for `image`; an operator needs none; the door's own bound answers before any sentence echoes a ref.
func TestRunRequestGovernanceGatesEachContainerImage(t *testing.T) {
	const otherImage = "registry.example.com/other/side@" + imgDigest
	granting := func(values ...string) *capStore {
		st := &capStore{enf: map[string]bool{capImage: true}}
		for _, v := range values {
			st.grants = append(st.grants, grant(types.CapabilitySubjectAll, "", capImage, v, types.CapabilityAllow))
		}
		return st
	}
	req := createRunRequest{Containers: []client.RunContainer{extra("kernel", kernelImage), extra("side", otherImage)}}
	member := memberCtx(nil)
	imageRefusal := func(r *runRefusal) bool {
		return r != nil && (r.body.Reason == reasonRunContainersInvalid || (r.decision != nil && r.decision.Target == "runs.image"))
	}

	s := &Server{cfg: Config{Store: granting(imgPrefix)}}
	_, refusal := s.runRequestGovernance(governReq(member), req)
	if refusal == nil || refusal.decision == nil || refusal.decision.Target != "runs.image" ||
		refusal.decision.Reason != authz.ReasonBYOIUser || !strings.Contains(refusal.decision.Sentence, "containers[1]") {
		t.Fatalf("one image granted: refusal = %+v, want runs.image denied naming containers[1]", refusal)
	}

	both := &Server{cfg: Config{Store: granting(imgPrefix, "registry.example.com/other/*")}}
	if _, refusal := both.runRequestGovernance(governReq(member), req); imageRefusal(refusal) {
		t.Errorf("both images granted and still refused: %+v", refusal)
	}

	none := &Server{cfg: Config{Store: granting()}}
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	if _, refusal := none.runRequestGovernance(governReq(admin), req); refusal != nil {
		t.Errorf("an operator was held to the image grant: %+v", refusal)
	}

	long := strings.Repeat("a", 300-len("@"+imgDigest)) + "@" + imgDigest
	_, refusal = s.runRequestGovernance(governReq(member), createRunRequest{Containers: []client.RunContainer{extra("kernel", long)}})
	if refusal == nil || refusal.status != http.StatusBadRequest || refusal.body.Reason != reasonRunContainersInvalid ||
		refusal.body.Field != "containers[0].image" || strings.Contains(refusal.body.Error, long) {
		t.Errorf("a 300-byte ref: refusal = %+v, want 400 %s field containers[0].image without the ref", refusal, reasonRunContainersInvalid)
	}

	tag := createRunRequest{Containers: []client.RunContainer{extra("kernel", "registry.example.com/team/kernel:1")}}
	if _, refusal := none.runRequestGovernance(governReq(member), tag); refusal == nil || refusal.status != http.StatusBadRequest ||
		refusal.body.Reason != reasonRunContainersInvalid {
		t.Errorf("a tag image with no grant: refusal = %+v, want 400 %s before the grant check", refusal, reasonRunContainersInvalid)
	}
}

// TestImageAdmissionOfMatchesImageAdmission: the split moved imageAdmission's body without a change: for each
// admission case the two answer the same verdict, warning and refusal.
func TestImageAdmissionOfMatchesImageAdmission(t *testing.T) {
	noPolicy := func() *fakeAdmitter {
		return &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}}
	}
	admits := func() *fakeAdmitter { return &fakeAdmitter{} }
	missing := func() *fakeAdmitter {
		return &fakeAdmitter{admit: func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{admitSigner}}, nil
		}}
	}
	member, admin := memberCtx(nil), operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	macOS := createRunRequest{Image: admitTagRef, MachineKind: string(types.MachineMacOSVM)}
	for _, tc := range []struct {
		name     string
		site     types.SiteConfig
		admitter func() *fakeAdmitter
		ctx      context.Context
		req      createRunRequest
		profile  []string
	}{
		{"off", admissionSite(types.ImageAdmissionOff), admits, member, createRunRequest{Image: admitTagRef}, nil},
		{"enforce admits and pins", admissionSite(types.ImageAdmissionEnforce), admits, member, createRunRequest{Image: admitTagRef}, nil},
		{"enforce with no policy", admissionSite(types.ImageAdmissionEnforce), noPolicy, member, createRunRequest{Image: admitTagRef}, nil},
		{"enforce with a missing signature", admissionSite(types.ImageAdmissionEnforce), missing, member, createRunRequest{Image: admitTagRef}, nil},
		{"audit reports", admissionSite(types.ImageAdmissionAudit), missing, member, createRunRequest{Image: admitTagRef}, nil},
		{"a profile's signer under site off", admissionSite(types.ImageAdmissionOff), missing, member, createRunRequest{Image: admitTagRef}, []string{admitProfile}},
		{"a macOS VM", admissionSite(types.ImageAdmissionEnforce), noPolicy, member, macOS, nil},
		{"an operator", admissionSite(types.ImageAdmissionEnforce), noPolicy, admin, createRunRequest{Image: admitTagRef}, nil},
		{"no member image", admissionSite(types.ImageAdmissionEnforce), noPolicy, member, createRunRequest{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A server each, so neither answer is served from the other's cache.
			v1, w1, r1 := admissionServer(tc.site, tc.admitter()).imageAdmission(governReq(tc.ctx), tc.req, "", tc.profile, false)
			v2, w2, r2 := admissionServer(tc.site, tc.admitter()).imageAdmissionOf(governReq(tc.ctx), createDigestInput(tc.req, ""),
				types.MachineKind(tc.req.MachineKind), tc.profile, false)
			if !reflect.DeepEqual(v1, v2) || w1 != w2 {
				t.Errorf("verdict %+v warning %q, split %+v warning %q", v1, w1, v2, w2)
			}
			if (r1 == nil) != (r2 == nil) || (r1 != nil && (r1.status != r2.status || !reflect.DeepEqual(r1.body, r2.body) || r1.operation != r2.operation)) {
				t.Errorf("refusal %+v, split %+v", r1, r2)
			}
		})
	}
}

// TestStepContainerAdmissionFreezesVerdicts: each extra keeps the pin admission made and its verdict; a refusal
// names the extra's image field.
func TestStepContainerAdmissionFreezesVerdicts(t *testing.T) {
	stale := "registry.example.com/team/side@sha256:" + strings.Repeat("1", 64)
	run := func(a *fakeAdmitter, mode foldMode, cs ...client.RunContainer) admissionRun {
		resp := httptest.NewRecorder()
		f := runFold{mode: mode, w: resp, r: governReq(memberCtx(nil)), req: &createRunRequest{Containers: cs}}
		ok := admissionServer(admissionSite(types.ImageAdmissionEnforce), a).stepContainerAdmission(&f, &foldRecorder{})
		return admissionRun{ok: ok, f: f, resp: resp}
	}

	got := run(&fakeAdmitter{}, foldCreate, extra("side", stale, 9000))
	if !got.ok || len(got.f.containers) != 1 {
		t.Fatalf("admitted extra refused: %d %s, containers %+v", got.resp.Code, got.resp.Body.String(), got.f.containers)
	}
	c := got.f.containers[0]
	if c.ImageAdmission == nil || c.ImageAdmission.Verdict != types.ImageAdmissionAdmitted || c.ImageAdmission.PinnedRef == "" ||
		c.Image != c.ImageAdmission.PinnedRef || c.Image == stale || !strings.HasSuffix(c.Image, admitDigest) {
		t.Fatalf("frozen extra %+v (verdict %+v), want the image pinned to the verified digest with its verdict", c, c.ImageAdmission)
	}
	if c.Name != "side" || c.CPUMillis != 250 || c.MemoryMiB != 256 || !reflect.DeepEqual(c.Ports, []int{9000}) {
		t.Errorf("frozen extra %+v lost what the request said", c)
	}

	second := &fakeAdmitter{admit: func(req imageadmit.Requirements) (imageadmit.Verdict, error) {
		if strings.Contains(req.Ref, "/second@") {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{admitSigner}}, nil
		}
		return imageadmit.Verdict{Digest: admitDigest, Signer: admitSigner}, nil
	}}
	got = run(second, foldPreflight, extra("first", "registry.example.com/team/first@"+imgDigest),
		extra("second", "registry.example.com/team/second@"+imgDigest))
	if got.ok || got.f.containers != nil {
		t.Fatalf("a refused extra was admitted: containers %+v", got.f.containers)
	}
	if b := got.body(t); got.resp.Code != http.StatusForbidden || b.Reason != string(imageadmit.SignatureMissing) || b.Field != "containers[1].image" {
		t.Errorf("refusal %d %+v, want 403 %s field containers[1].image", got.resp.Code, b, imageadmit.SignatureMissing)
	}
}

// extrasRunner is a fakeRunner whose substrate runs extra containers.
type extrasRunner struct{ *fakeRunner }

func (e extrasRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	c, err := e.fakeRunner.Capabilities(ctx)
	c.ExtraContainers = true
	return c, err
}

// capsUnread fails the test when its capabilities are read.
type capsUnread struct {
	*fakeRunner
	t *testing.T
}

func (c capsUnread) Capabilities(context.Context) (runner.Capabilities, error) {
	c.t.Error("capabilities were read for a request with no extra containers")
	return runner.Capabilities{}, errors.New("unread")
}

// TestIntersectCapabilitiesExtraContainers: a pool runs extras only when every placement in it does, and a
// runner's own report carries the flag.
func TestIntersectCapabilitiesExtraContainers(t *testing.T) {
	with := placementCaps{Capabilities: runner.Capabilities{Driver: "k8s", ExtraContainers: true}}
	without := placementCaps{Capabilities: runner.Capabilities{Driver: "docker"}}
	if !intersectCapabilities(with).ExtraContainers || !intersectCapabilities(with, with).ExtraContainers {
		t.Error("placements that all run extras lost the flag")
	}
	if intersectCapabilities(with, without).ExtraContainers || intersectCapabilities(without, with).ExtraContainers {
		t.Error("a placement that cannot run extras kept the flag")
	}
	s := capsServer(&fakeRunner{})
	reported := laptopCaps(types.CC1)
	reported.Support.ExtraContainers = true
	id := installRunner(t, s, true, reported)
	pc, err := s.placementCapabilities(capsCtx, types.PlacementLocal, id)
	if err != nil || !pc.ExtraContainers {
		t.Errorf("a runner reporting extra containers: caps %+v, err %v", pc.Capabilities, err)
	}
}

// TestContainersPlacementRefusal: a placement that cannot run extras is 422 placement_capability naming
// containers, wherever the run could land; a request with none reads nothing.
func TestContainersPlacementRefusal(t *testing.T) {
	ask := createRunRequest{Placement: types.PlacementRemote, Containers: twoExtras()}
	wantRefused := func(t *testing.T, name string, ref *runRefusal) {
		t.Helper()
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) ||
			ref.body.Field != "containers" {
			t.Errorf("%s: refusal %+v, want 422 placement_capability field containers", name, ref)
		}
	}

	wantRefused(t, "wardynd's substrate without them", capsServer(&fakeRunner{}).containersPlacementRefusal(capsCtx, ask, runPlacement{}))
	if ref := capsServer(extrasRunner{&fakeRunner{}}).containersPlacementRefusal(capsCtx, ask, runPlacement{}); ref != nil {
		t.Errorf("wardynd's substrate with them: refused %+v", ref)
	}
	wantRefused(t, "no executor wired", capsServer(nil).containersPlacementRefusal(capsCtx, ask, runPlacement{}))

	s := capsServer(&fakeRunner{})
	runs := laptopCaps(types.CC1)
	runs.Support.ExtraContainers = true
	a, b := installRunner(t, s, true, runs), installRunner(t, s, true, runs)
	c := installRunner(t, s, true, laptopCaps(types.CC1))
	pool := func(ids ...uuid.UUID) runPlacement {
		p := runPlacement{pooled: true, placement: types.PlacementLocal}
		for _, id := range ids {
			p.eligible = append(p.eligible, poolTarget{runnerID: &id})
		}
		return p
	}
	wantRefused(t, "a pool with one runner without them", s.containersPlacementRefusal(capsCtx, ask, pool(a, c)))
	if ref := s.containersPlacementRefusal(capsCtx, ask, pool(a, b)); ref != nil {
		t.Errorf("a pool whose runners all run them: refused %+v", ref)
	}

	unread := capsServer(capsUnread{&fakeRunner{}, t})
	if ref := unread.containersPlacementRefusal(capsCtx, createRunRequest{Placement: types.PlacementRemote}, runPlacement{}); ref != nil {
		t.Errorf("a request with no extras: refused %+v", ref)
	}
}

// TestContainersRefusedUnderDenyTaskModeExec: a profile that closes exec closes extras too, attributed to the
// profile and the knob's lock; nothing else changes.
func TestContainersRefusedUnderDenyTaskModeExec(t *testing.T) {
	s := &Server{cfg: Config{Store: &capStore{}}}
	r := governReq(memberCtx(nil))
	unit := &ResolvedUnit{Name: "walled", Composed: true,
		Chain: []unitLink{{Name: "walled"}, {Name: "base", Locked: []string{"deny_task_mode_exec"}}}}
	closed := governanceCeiling{Unit: unit, Limits: types.GovernanceLimits{DenyTaskModeExec: true}}
	open := governanceCeiling{Unit: unit}
	withExtras := createRunRequest{Task: "t", Containers: twoExtras()}
	plain := createRunRequest{Task: "t"}

	refusal := s.runGovernancePosture(r, withExtras, closed, true)
	if refusal == nil || refusal.decision == nil {
		t.Fatalf("knob on with extras: refusal %+v, want a denial", refusal)
	}
	d := refusal.decision
	if d.Status != http.StatusForbidden || d.Reason != authz.ReasonGovernanceUnit || d.Target != "runs.containers" ||
		!strings.Contains(d.Sentence, `"walled"`) {
		t.Errorf("decision %d %s %s %q, want 403 governance_unit runs.containers naming the profile", d.Status, d.Reason, d.Target, d.Sentence)
	}
	if d.Knob != (policyref.Provenance{Layer: policyref.LayerUnit, Name: "base", Locked: true}) || d.Policy == nil {
		t.Errorf("knob %+v policy %+v, want the lock on deny_task_mode_exec and the profile", d.Knob, d.Policy)
	}
	if refusal := s.runGovernancePosture(r, plain, closed, true); refusal != nil {
		t.Errorf("knob on with no extras: refused %+v", refusal)
	}
	if refusal := s.runGovernancePosture(r, withExtras, open, true); refusal != nil {
		t.Errorf("knob off with extras: refused %+v", refusal)
	}
	if refusal := s.runGovernancePosture(r, withExtras, governanceCeiling{Operator: true}, true); refusal != nil {
		t.Errorf("an operator with extras: refused %+v", refusal)
	}
}

// TestStepContainersRecording: extras are refused where the organisation requires recording (its own or the
// pool's required setting, or a level locked above off), never for a profile's own level; a failed read is a 500.
func TestStepContainersRecording(t *testing.T) {
	profile := governanceCeiling{Unit: &ResolvedUnit{Name: "Platform"}}
	terminalProfile := profile
	terminalProfile.Spec.Recording = types.RecordingTerminal
	pooled := runPlacement{pooled: true, placement: types.PlacementRemote, row: types.RunnerPool{Name: "Cluster", Recording: types.RecordingRequired}}
	site := func(r *types.SiteRecording) types.SiteConfig { return types.SiteConfig{Recording: r} }
	for _, tc := range []struct {
		name    string
		extras  bool
		org     types.RunnerPoolDefaults
		site    types.SiteConfig
		siteErr error
		ceiling governanceCeiling
		place   runPlacement
		noStore bool
		status  int // 0: passes
	}{
		{name: "locked with no level", extras: true, site: site(&types.SiteRecording{Locked: true}), ceiling: profile, status: http.StatusUnprocessableEntity},
		{name: "locked at off", extras: true, site: site(&types.SiteRecording{Level: types.RecordingOff, Locked: true}), ceiling: profile},
		{name: "unlocked at screen and actions", extras: true, site: site(&types.SiteRecording{Level: types.RecordingScreenActions}), ceiling: profile},
		{name: "the organisation requires recording", extras: true, org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile,
			status: http.StatusUnprocessableEntity},
		{name: "the pool requires recording", extras: true, place: pooled, ceiling: profile, status: http.StatusUnprocessableEntity},
		{name: "a profile level of terminal", extras: true, ceiling: terminalProfile},
		{name: "no store", extras: true, org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile, noStore: true},
		{name: "a site read that fails", extras: true, siteErr: errors.New("db down"), ceiling: profile, status: http.StatusInternalServerError},
		{name: "no extras reads nothing", siteErr: errors.New("db down"), org: types.RunnerPoolDefaults{Recording: types.RecordingRequired}, ceiling: profile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			s.cfg.Store = &macRecordingStore{ownedRunners: s.cfg.Store.(ownedRunners), org: tc.org, site: tc.site, siteErr: tc.siteErr}
			if tc.noStore {
				s.cfg.Store = nil
			}
			req := createRunRequest{}
			if tc.extras {
				req.Containers = twoExtras()
			}
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req,
				ceiling: tc.ceiling, place: tc.place}
			ok := s.stepContainersRecording(f, nil)
			if tc.status == 0 {
				if !ok {
					t.Fatalf("refused: %d %s", w.Code, w.Body.String())
				}
				return
			}
			if ok || w.Code != tc.status {
				t.Fatalf("ok=%v %d %s, want %d", ok, w.Code, w.Body.String(), tc.status)
			}
			if tc.status == http.StatusUnprocessableEntity {
				var b errorBody
				if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Reason != reasonRequestFieldUnavailable || b.Field != "containers" {
					t.Errorf("body %s (%v), want %s field containers", w.Body.String(), err, reasonRequestFieldUnavailable)
				}
			}
		})
	}
}

// containersStore answers RunContainers for dispatchContainers.
type containersStore struct {
	store.Store
	cs  []types.RunExtraContainer
	err error
}

func (c containersStore) RunContainers(context.Context, uuid.UUID) ([]types.RunExtraContainer, error) {
	return c.cs, c.err
}

// TestDispatchContainers: the stored extras reach the runner's spec as they were admitted; a failed read is an
// error, which dispatch answers by failing the run; a store that keeps none answers none.
func TestDispatchContainers(t *testing.T) {
	stored := []types.RunExtraContainer{{Name: "kernel", Image: kernelImage, Command: []string{"serve"}, Ports: []int{8888},
		CPUMillis: 500, MemoryMiB: 512, ImageAdmission: &types.ImageAdmissionVerdict{Verdict: types.ImageAdmissionAdmitted}}}
	s := &Server{cfg: Config{Store: containersStore{cs: stored}}}
	got, err := s.dispatchContainers(context.Background(), uuid.New())
	want := []runner.ExtraContainer{{Name: "kernel", Image: kernelImage, Command: []string{"serve"}, Ports: []int{8888}, CPUMillis: 500, MemoryMiB: 512}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("extras %+v, %v; want %+v", got, err, want)
	}
	failing := &Server{cfg: Config{Store: containersStore{err: errors.New("db down")}}}
	if _, err := failing.dispatchContainers(context.Background(), uuid.New()); err == nil {
		t.Error("a failed read answered no error")
	}
	if got, err := (&Server{cfg: Config{Store: &capStore{}}}).dispatchContainers(context.Background(), uuid.New()); err != nil || len(got) != 0 {
		t.Errorf("a store with no extras answered %+v, %v", got, err)
	}
}

// TestRunCreateAuditContainersHaveNoCommand: the run.create row's view of the extras says what runs, how big and
// its verdict, and never the command, which can carry a pasted secret.
func TestRunCreateAuditContainersHaveNoCommand(t *testing.T) {
	cs := []types.RunExtraContainer{{Name: "kernel", Image: kernelImage, Command: []string{"serve", "--token=x"}, Ports: []int{8888},
		CPUMillis: 500, MemoryMiB: 512}}
	rows := auditContainers(cs)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	for _, k := range []string{"name", "image", "cpu_millis", "memory_mib", "ports", "image_admission"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("the row has no %q: %+v", k, rows[0])
		}
	}
	if _, ok := rows[0]["command"]; ok || strings.Contains(string(mustJSON(rows)), "--token=x") {
		t.Errorf("the row carries the command: %s", mustJSON(rows))
	}
}
