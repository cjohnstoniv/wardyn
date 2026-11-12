// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The image digest rule (site config images.require_digest): while it is on, a member's image must be
// pinned image@sha256:<digest>, whatever the image grants say. Operators are not bound and are marked
// image_digest_exempt on the run.create row. Every door that can start a member-chosen image reaches
// imageDigestVerdict through runRequestGovernance (create, preflight, a held run's start, a service
// identity's run) or seededImageRefusal (the workspace's base image, at create, preflight and a held start),
// and a revive re-runs that decision on the input the run's create recorded (reviveImageDigestRefusal).

const (
	digestTagRef    = "registry.example.com/team/app:1"
	digestPinnedRef = "registry.example.com/team/app@" + imgDigest
	digestBothRef   = "registry.example.com/team/app:1@" + imgDigest
)

func digestRuleSite(on bool) types.SiteConfig {
	return types.SiteConfig{Images: &types.SiteImages{RequireDigest: on}}
}

// digestRuleStore is a deployment with the rule set and an image grant covering the whole team prefix, so
// the grant alone would admit every ref below.
func digestRuleStore(on bool) *capStore {
	return &capStore{
		site:   digestRuleSite(on),
		enf:    map[string]bool{capImage: true},
		grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)},
	}
}

func digestRuleServer(st *capStore, governAdmin bool) *Server {
	return &Server{cfg: Config{Store: st, GovernAdminRuns: governAdmin}}
}

func isDigestRefusal(r *runRefusal) bool {
	return r != nil && r.body.Reason == reasonImageDigestRequired
}

func TestImagePinnedByDigest(t *testing.T) {
	short := "registry.example.com/team/app@sha256:abc"
	upper := "registry.example.com/team/app@sha256:" + strings.Repeat("A", 64)
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{digestPinnedRef, true},
		{digestBothRef, true},
		{"app@" + imgDigest, true},
		{digestTagRef, false},
		{"registry.example.com/team/app", false},
		{"", false},
		{short, false},
		{upper, false},
		{"registry.example.com/team/app@sha512:" + strings.Repeat("a", 64), false},
		{"registry.example.com/team/app@" + imgDigest + "0", false},
		{"registry.example.com/team/app@" + imgDigest + "/x", false},
		{"registry.example.com/team/app:sha256:" + strings.Repeat("a", 64), false},
	} {
		if got := imagePinnedByDigest(tc.ref); got != tc.want {
			t.Errorf("imagePinnedByDigest(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

// digestErrStore fails the site config read.
type digestErrStore struct{ *capStore }

func (digestErrStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("db down")
}

func TestImageDigestVerdict(t *testing.T) {
	member := memberCtx(nil)
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	for _, tc := range []struct {
		name        string
		ruleOn      bool
		governAdmin bool
		ctx         context.Context
		image       string
		want        digestVerdict
	}{
		{"off: member tag", false, false, member, digestTagRef, digestAllowed},
		{"off: operator tag", false, false, admin, digestTagRef, digestAllowed},
		{"on: member tag", true, false, member, digestTagRef, digestRefused},
		{"on: member no tag", true, false, member, "registry.example.com/team/app", digestRefused},
		{"on: member pinned", true, false, member, digestPinnedRef, digestAllowed},
		{"on: member tag and digest", true, false, member, digestBothRef, digestAllowed},
		{"on: member names no image", true, false, member, "", digestAllowed},
		{"on: operator tag", true, false, admin, digestTagRef, digestExempt},
		{"on: operator pinned", true, false, admin, digestPinnedRef, digestAllowed},
		{"on: operator names no image", true, false, admin, "", digestAllowed},
		// WARDYN_GOVERN_ADMIN_RUNS: an SSO admin's run is governed like a member's.
		{"on: governed admin tag (GovernAdminRuns)", true, true, admin, digestTagRef, digestRefused},
		{"on: service identity tag", true, false, withServiceIdentity(admin, "ci-bot", capEmail), digestTagRef, digestRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := digestRuleServer(digestRuleStore(tc.ruleOn), tc.governAdmin).imageDigestVerdict(tc.ctx, tc.image)
			if err != nil || got != tc.want {
				t.Fatalf("imageDigestVerdict(%q) = %v, %v; want %v", tc.image, got, err, tc.want)
			}
		})
	}
	t.Run("unreadable site config fails closed for an unpinned image", func(t *testing.T) {
		s := &Server{cfg: Config{Store: digestErrStore{digestRuleStore(true)}}}
		if _, err := s.imageDigestVerdict(member, digestTagRef); err == nil {
			t.Fatal("a site config read error was swallowed")
		}
		if refusal := s.imageDigestRefusal(member, digestTagRef, false); refusal == nil || refusal.operation == "" {
			t.Fatalf("refusal = %+v, want a server error", refusal)
		}
		if v, err := s.imageDigestVerdict(member, digestPinnedRef); err != nil || v != digestAllowed {
			t.Fatalf("a pinned image read the site config: %v, %v", v, err)
		}
	})
	t.Run("no store", func(t *testing.T) {
		if v, err := (&Server{}).imageDigestVerdict(member, digestTagRef); err != nil || v != digestAllowed {
			t.Fatalf("verdict = %v, %v; want allowed", v, err)
		}
	})
}

// TestImageDigestRule_CreatePreflightQueueAndServiceReachRunRequestGovernance: create and preflight reach
// runRequestGovernance through denyUserRequest, a held run's start through queueGovernance, and a service
// identity's run is the same create (never an operator). The calls are asserted in source, and the function
// they share is driven for a member and a service identity.
func TestImageDigestRule_CreatePreflightQueueAndServiceReachRunRequestGovernance(t *testing.T) {
	for file, call := range map[string]string{
		"runs_create_validate.go": "s.runRequestGovernance(r, req)",
		"run_queue_start.go":      "s.runRequestGovernance(st.r, req)",
	} {
		src, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(src), call) {
			t.Errorf("%s no longer calls %s: the digest rule's door moved", file, call)
		}
	}
	for file, call := range map[string]string{
		"runs_create_validate.go": "s.denyUserRequest(w, r, req)",
		"preflight.go":            "s.denyUserRequest(w, r, req)",
	} {
		src, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(src), call) {
			t.Errorf("%s no longer calls %s", file, call)
		}
	}
	member := memberCtx(nil)
	service := withServiceIdentity(member, "ci-bot", capEmail)
	for name, ctx := range map[string]context.Context{"member": member, "service identity": service} {
		t.Run(name, func(t *testing.T) {
			s := digestRuleServer(digestRuleStore(true), false)
			// The prefix grant admits the tag ref; the rule still refuses it.
			if _, err := s.capGranted(ctx, capImage, digestTagRef); err != nil {
				t.Fatalf("setup: resolve the image grant: %v", err)
			}
			_, refusal := s.runRequestGovernance(governReq(ctx), createRunRequest{Image: digestTagRef})
			if !isDigestRefusal(refusal) || refusal.status != 403 {
				t.Fatalf("tag ref under a prefix grant: refusal = %+v, want 403 %s", refusal, reasonImageDigestRequired)
			}
			msg := refusal.body.Error
			for _, want := range []string{digestTagRef, "images.require_digest", "image@sha256:<digest>"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not say %q", msg, want)
				}
			}
			// Pinned: the rule lets it through to the grant check, which admits it.
			if _, refusal := s.runRequestGovernance(governReq(ctx), createRunRequest{Image: digestPinnedRef}); isDigestRefusal(refusal) {
				t.Errorf("a pinned ref was refused by the digest rule: %+v", refusal)
			}
			// Off: the rule refuses nothing.
			off := digestRuleServer(digestRuleStore(false), false)
			if _, refusal := off.runRequestGovernance(governReq(ctx), createRunRequest{Image: digestTagRef}); isDigestRefusal(refusal) {
				t.Errorf("the rule is off and still refused: %+v", refusal)
			}
		})
	}
	t.Run("operator", func(t *testing.T) {
		s := digestRuleServer(digestRuleStore(true), false)
		admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
		if _, refusal := s.runRequestGovernance(governReq(admin), createRunRequest{Image: digestTagRef}); refusal != nil {
			t.Errorf("an operator was bound by the digest rule: %+v", refusal)
		}
	})
}

// TestImageDigestRule_WorkspaceSeedImage: the image an attached workspace's base image sets is checked at
// seededImageRefusal, whoever owns the workspace, which create and preflight reach through
// seedAuthorizedWorkspace and a held run's start reaches through foldRunRequest (foldCreate).
func TestImageDigestRule_WorkspaceSeedImage(t *testing.T) {
	src, err := os.ReadFile("run_fold_steps.go")
	if err != nil || !strings.Contains(string(src), "s.seedAuthorizedWorkspace(ctx, f.r, &f.spec, f.req)") ||
		!strings.Contains(string(src), "s.seedAndAdmitWorkspace(ctx, f.w, f.r, &f.spec, f.req, f.mode == foldCreate)") {
		t.Error("run_fold_steps.go no longer seeds the workspace for preview and create through seedAuthorizedWorkspace")
	}
	src, err = os.ReadFile("run_workspace_authorization.go")
	if err != nil || !strings.Contains(string(src), "s.seededImageRefusal(r, seed, req.Image)") {
		t.Error("run_workspace_authorization.go no longer calls seededImageRefusal")
	}
	s := digestRuleServer(digestRuleStore(true), false)
	member := memberCtx(nil)
	memberSeed := workspaceSeed{imageOpen: true, imageOwner: "member-sub"}
	operatorSeed := workspaceSeed{imageOpen: true} // an operator-owned workspace set the image: no member owner
	if ref := s.seededImageRefusal(governReq(member), memberSeed, digestTagRef); !isDigestRefusal(ref) || ref.status != 403 ||
		!strings.Contains(ref.body.Error, "the workspace's base image") {
		t.Errorf("a member-owned workspace's tag image: refusal = %+v", ref)
	}
	if ref := s.seededImageRefusal(governReq(member), memberSeed, digestPinnedRef); isDigestRefusal(ref) {
		t.Errorf("a pinned workspace image was refused by the digest rule: %+v", ref)
	}
	// An operator-owned workspace's tag base image, launched by a member: the digest rule binds it too.
	if ref := s.seededImageRefusal(governReq(member), operatorSeed, digestTagRef); !isDigestRefusal(ref) || ref.status != 403 ||
		!strings.Contains(ref.body.Error, digestTagRef) {
		t.Errorf("a member's run seeded by an operator-owned workspace's tag image: refusal = %+v", ref)
	}
	// ... and only the digest rule: with it off, the operator's image is not held to the member's grants.
	off := digestRuleServer(digestRuleStore(false), false)
	if ref := off.seededImageRefusal(governReq(member), operatorSeed, digestTagRef); ref != nil {
		t.Errorf("an operator-owned workspace image met the grant re-check: %+v", ref)
	}
	// A request that named its own image was checked at runRequestGovernance, with its own sentence.
	if ref := s.seededImageRefusal(governReq(member), workspaceSeed{}, digestTagRef); ref != nil {
		t.Errorf("a request-named image was re-checked as a workspace's: %+v", ref)
	}
	// An operator launching a member-owned workspace is not bound.
	if ref := s.seededImageRefusal(governReq(operatorCtx(capSub, capEmail, oidc.RoleAdmin)), memberSeed, digestTagRef); ref != nil {
		t.Errorf("an operator was bound by the digest rule at the seed: %+v", ref)
	}
}

// TestImageDigestRule_PolicyCarriedWorkspaceImage: a workspace the run's policy carries sets the image dispatch
// uses when neither the request nor an attached workspace chose one (resolveCreateRunImage), whether or not any
// workspace is attached, so stepWorkspaces holds its base image to the rule at every door. A member launching
// from their own workspaces is refused a tag ref with the workspace base image's sentence and passes a digest ref;
// the admin token is not bound, and its run.create row carries image_digest_exempt. (An SSO admin's browser
// session in the admin view is refused every launch first, as admin_view: runs start in the user view.)
func TestImageDigestRule_PolicyCarriedWorkspaceImage(t *testing.T) {
	for _, attach := range []bool{true, false} {
		for _, tc := range []struct {
			name, image     string
			ruleOn, refused bool
		}{
			{"tag ref", digestTagRef, true, true},
			{"digest ref", digestPinnedRef, true, false},
			{"tag ref, rule off", digestTagRef, false, false},
		} {
			name := tc.name + ", no workspace attached"
			if attach {
				name = tc.name + ", another workspace attached"
			}
			t.Run(name, func(t *testing.T) {
				const member = "sub-digest-member"
				plain, carried := repoWS("plain", helloRepo), repoWS("carried", spoonRepo)
				plain.OwnedBy, carried.OwnedBy = member, member
				carried.BaseImage = &types.WorkspaceBaseImage{Kind: "custom", Image: tc.image}
				srv := attachFixture(t, digestRuleSite(tc.ruleOn), plain, carried)
				policy := `"inline_policy":{"min_confinement_class":"CC2","workspace_repos":[{"repo":"` + spoonRepo + `"}]}}`
				body := `{"agent":"claude-code","task":"t",` + policy
				if attach {
					body = `{"agent":"claude-code","task":"t","workspace_id":"` + plain.ID.String() + `",` + policy
				}
				session := ssoSession(t, member, "member@corp.example", oidc.RoleUser)
				for _, door := range runDoors {
					w := doSSO(t, srv, http.MethodPost, door.path, session, body)
					got := strings.Contains(w.Body.String(), reasonImageDigestRequired)
					var eb errorBody
					if tc.refused && (w.Code != http.StatusForbidden || json.Unmarshal(w.Body.Bytes(), &eb) != nil ||
						eb.Reason != reasonImageDigestRequired || eb.Error != fmt.Sprintf(imageDigestSeededMsg, tc.image)) {
						t.Fatalf("%s: %d %s; want 403 %s with the workspace base image's sentence naming %s",
							door.name, w.Code, w.Body.String(), reasonImageDigestRequired, tc.image)
					}
					if !tc.refused && got {
						t.Fatalf("%s: refused by the digest rule: %d %s", door.name, w.Code, w.Body.String())
					}
					if w := do(t, srv, http.MethodPost, door.path, adminToken, body); strings.Contains(w.Body.String(), reasonImageDigestRequired) {
						t.Fatalf("%s: the admin token was bound by the digest rule: %d %s", door.name, w.Code, w.Body.String())
					}
				}
				if !tc.refused || !tc.ruleOn {
					return
				}
				marked := false
				for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
					if ev.Action == "run.create" && ev.Outcome == "success" && leaseAuditData(t, ev)["image_digest_exempt"] == true {
						marked = true
					}
				}
				if !marked {
					t.Error("the admin token's run.create row does not carry image_digest_exempt for the carried tag image")
				}
			})
		}
	}
	src, err := os.ReadFile("run_fold_steps.go")
	if err != nil || !strings.Contains(string(src), "s.imageDigestRefusal(f.r.Context(), f.carriedImage, true)") {
		t.Error("stepWorkspaces no longer holds a policy-carried workspace's base image to the digest rule")
	}
}

// TestImageDigestRule_Revive: a revive re-runs create's decision on the input the run's create checked
// (AgentRun.ImageInput), under the rule as it is at the revive, by the run owner's authority.
func TestImageDigestRule_Revive(t *testing.T) {
	tag, pinned, convention := digestTagRef, digestPinnedRef, ""
	for _, tc := range []struct {
		name     string
		ruleOn   bool
		input    *string
		operator bool // an operator-owned run (the admin token, local mode)
		owner    bool // the owner revives (a member session); else the admin token
		refused  bool
		exempt   bool
	}{
		{"tag ref", true, &tag, false, true, true, false},
		{"digest ref", true, &pinned, false, true, false, false},
		{"convention image", true, &convention, false, true, false, false},
		{"no recorded input (a run from before the rule reached revive), rule on", true, nil, false, true, true, false},
		{"no recorded input, rule off", false, nil, false, true, false, false},
		{"tag ref, rule off", false, &tag, false, true, false, false},
		{"operator-owned run", true, &tag, true, false, false, true},
		{"a member's run revived by an admin", true, &tag, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			f.st.site.Images = &types.SiteImages{RequireDigest: tc.ruleOn}
			f.st.run.ImageInput, f.st.run.OperatorOwned = tc.input, tc.operator
			code, body := f.reviveAs(t, tc.owner)
			if tc.refused {
				if code != http.StatusForbidden || !strings.Contains(body, reasonImageDigestRequired) ||
					!strings.Contains(body, "images.require_digest") || !strings.Contains(body, "Start a new run to use an allowed image") {
					t.Fatalf("revive = %d %s; want 403 %s with the revive sentence", code, body, reasonImageDigestRequired)
				}
				if tc.input != nil && !strings.Contains(body, *tc.input) {
					t.Errorf("refusal %s does not name the image %q", body, *tc.input)
				}
				f.assertReviveRefused(t, reasonImageDigestRequired)
				return
			}
			if code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200", code, body)
			}
			var marked bool
			for _, ev := range f.audit.eventsFor(f.run.ID, "run.revive") {
				if ev.Outcome == "success" {
					marked = leaseAuditData(t, ev)["image_digest_exempt"] == true
				}
			}
			if marked != tc.exempt {
				t.Errorf("run.revive success row image_digest_exempt = %v, want %v", marked, tc.exempt)
			}
		})
	}
	t.Run("a rule turned on after create refuses the revive", func(t *testing.T) {
		f, _ := newOwnerFixture(t)
		// Create's own decision on the tag ref, with the rule off: allowed.
		if v, err := f.srv.imageDigestVerdict(memberCtx(nil), tag); err != nil || v != digestAllowed {
			t.Fatalf("create with the rule off = %v, %v; want allowed", v, err)
		}
		f.st.run.ImageInput = &tag
		f.st.site.Images = &types.SiteImages{RequireDigest: true}
		if code, body := f.reviveAs(t, true); code != http.StatusForbidden {
			t.Fatalf("revive after the rule was turned on = %d %s, want 403", code, body)
		}
		f.assertReviveRefused(t, reasonImageDigestRequired)
	})
	t.Run("unreadable site config fails closed", func(t *testing.T) {
		f, _ := newOwnerFixture(t)
		f.st.run.ImageInput = &tag
		f.st.siteErr = errors.New("db down")
		if code, body := f.reviveAs(t, true); code != http.StatusServiceUnavailable {
			t.Fatalf("revive with the site config unreadable = %d %s, want 503", code, body)
		}
		if len(f.rr.replaced) != 0 {
			t.Error("a revive that could not read the digest rule replaced the proxy")
		}
	})
}

// TestImageDigestRule_TemplateAndPresetStartNothing: a template import only validates a document (it starts
// no image, and its image reaches POST /runs when it is launched), and a preset launch refuses an image
// beside the preset (reasonPresetField), so neither is a door of its own.
func TestImageDigestRule_TemplateAndPresetStartNothing(t *testing.T) {
	src, err := os.ReadFile("template_document.go")
	if err != nil || strings.Contains(string(src), "imageDigest") || strings.Contains(string(src), "runRequestGovernance") {
		t.Error("template_document.go reads the digest rule or governance: a template import is a validation, not a launch")
	}
	for _, name := range []string{"presets.go"} {
		src, err := os.ReadFile(name)
		if err != nil || !strings.Contains(string(src), "reasonPresetField") {
			t.Errorf("%s no longer refuses fields beside a preset", name)
		}
	}
}

func TestMarkImageDigestExempt(t *testing.T) {
	member := memberCtx(nil)
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	for _, tc := range []struct {
		name   string
		ruleOn bool
		ctx    context.Context
		image  string
		want   bool
	}{
		{"operator, rule on, tag", true, admin, digestTagRef, true},
		{"operator, rule on, pinned", true, admin, digestPinnedRef, false},
		{"operator, rule on, no image", true, admin, "", false},
		{"operator, rule off, tag", false, admin, digestTagRef, false},
		{"member, rule on, tag", true, member, digestTagRef, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			digestRuleServer(digestRuleStore(tc.ruleOn), false).markImageDigestExempt(tc.ctx, tc.image, data)
			if _, has := data["image_digest_exempt"]; has != tc.want {
				t.Errorf("data = %v, want image_digest_exempt present = %v", data, tc.want)
			}
		})
	}
	// Both run.create writers stamp it.
	for _, file := range []string{"runs.go", "run_queue.go"} {
		src, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(src), "s.markRunCreateExempt(ctx, ") {
			t.Errorf("%s does not call markRunCreateExempt (governance_exempt and image_digest_exempt) on run.create", file)
		}
	}
}

func TestSiteImagesCarryForwardAndAudit(t *testing.T) {
	on := &types.SiteImages{RequireDigest: true}
	for _, tc := range []struct {
		name    string
		body    *types.SiteImages
		named   bool
		want    *types.SiteImages
		wantKey bool
	}{
		{"body silent keeps the stored rule", nil, false, on, true},
		{"body names it on", on, true, on, true},
		{"body names an all-default block: stored as none", &types.SiteImages{}, true, nil, true},
		{"body names a null: stored as none", nil, true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &types.SiteConfig{Images: tc.body}
			carryForwardSiteImages(cfg, types.SiteConfig{Images: on}, map[string]bool{"images": tc.named})
			if (cfg.Images == nil) != (tc.want == nil) || (cfg.Images != nil && *cfg.Images != *tc.want) {
				t.Errorf("Images = %+v, want %+v", cfg.Images, tc.want)
			}
			datum := map[string]any{}
			auditSiteImages(datum, nil, cfg.Images, tc.named)
			if _, has := datum["images_require_digest"]; has != tc.wantKey {
				t.Errorf("datum = %v, want images_require_digest present = %v", datum, tc.wantKey)
			}
		})
	}
	datum := map[string]any{}
	auditSiteImages(datum, nil, nil, false)
	if len(datum) != 0 {
		t.Errorf("a deployment with no block wrote %v: its site_config.write row must be unchanged", datum)
	}
	auditSiteImages(datum, nil, on, true)
	if datum["images_require_digest"] != true {
		t.Errorf("datum = %v, want images_require_digest true", datum)
	}
}
