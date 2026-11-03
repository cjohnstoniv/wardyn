// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The image digest rule (site config images.require_digest): while it is on, a member's image must be
// pinned image@sha256:<digest>, whatever the image grants say. Operators are not bound and are marked
// image_digest_exempt on the run.create row. Every door that can start a member-chosen image reaches
// imageDigestVerdict through runRequestGovernance (create, preflight, a held run's start, a service
// identity's run) or seededImageRefusal (the workspace's base image, at create, preflight and a held start).

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

// TestImageDigestRule_ReviveStartsNoImage: a revive restarts the kept sandbox by its ref
// (runner.SandboxStarter.StartSandbox) and re-checks the owner's launch doors through persistedLaunchDoors,
// which has never carried the image (the row cannot tell a member-named image from the convention image).
// So the rule is a launch-time rule: a run created while it was off keeps reviving after it is turned on.
func TestImageDigestRule_ReviveStartsNoImage(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", Image: digestTagRef}
	for _, d := range persistedLaunchDoors(run, nil, nil) {
		if d.kind == capImage {
			t.Fatalf("persistedLaunchDoors carries an image door %+v: revive would now meet the digest rule, decide that on purpose", d)
		}
	}
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
			auditSiteImages(datum, cfg.Images, tc.named)
			if _, has := datum["images_require_digest"]; has != tc.wantKey {
				t.Errorf("datum = %v, want images_require_digest present = %v", datum, tc.wantKey)
			}
		})
	}
	datum := map[string]any{}
	auditSiteImages(datum, nil, false)
	if len(datum) != 0 {
		t.Errorf("a deployment with no block wrote %v: its site_config.write row must be unchanged", datum)
	}
	auditSiteImages(datum, on, true)
	if datum["images_require_digest"] != true {
		t.Errorf("datum = %v, want images_require_digest true", datum)
	}
}
