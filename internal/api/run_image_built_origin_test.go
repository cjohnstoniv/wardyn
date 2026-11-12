// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// A launch records where a run's image came from when the member named none (image_admission.origin):
// convention, devcontainer (built from the request's devcontainer repository) or workspace (built from a
// workspace with no base image: its repo's devcontainer or a generated image). Image admission does not cover
// an image Wardyn built for the run, so a revive never refuses one for having no admitted digest; a run with
// no origin recorded is revived as before, and its refusal says only what is true of it.

// recordedJSON is the verdict the launch wrote to the run, as the run row holds it ("null" for none).
func recordedJSON(t *testing.T, st *pinStore) string {
	t.Helper()
	raw, err := json.Marshal(st.recorded())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// generatedWorkspace is a scanned workspace with no base image whose generated image is already built.
func generatedWorkspace() types.Workspace {
	ws := types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		Profile: json.RawMessage(`{}`),
	}
	ws.ImageRef, ws.BuiltProfileHash = "wardyn-workspace/generated:1", workspacescan.WorkspaceProfile{}.CacheKey()
	return ws
}

// TestImageOriginRecordedAtLaunch: every launch on an image the member did not name records its origin,
// whatever the admission mode, and reads no registry for it. An image Wardyn built (a devcontainer, a generated
// workspace image) and a convention image admission did not check record the origin alone, never a verdict; a
// convention image checked under scope all records its verdict with origin convention. A record that cannot
// be written fails the run under enforce and is logged otherwise.
func TestImageOriginRecordedAtLaunch(t *testing.T) {
	site := func(mode, scope string) types.SiteConfig {
		sc := admissionSite(mode)
		sc.Images.Admission.Scope = scope
		return sc
	}
	built := []struct {
		name, origin string
		req          createRunRequest
		wsRefs       []types.Workspace
	}{
		{"devcontainer", "devcontainer", createRunRequest{Agent: "claude-code", DevcontainerRepo: "https://example.com/org/repo.git"}, nil},
		{"generated workspace image", "workspace", createRunRequest{Agent: "claude-code"}, []types.Workspace{generatedWorkspace()}},
	}
	for _, b := range built {
		for _, mode := range []string{types.ImageAdmissionAudit, types.ImageAdmissionEnforce, types.ImageAdmissionOff} {
			for _, scope := range []string{types.ImageAdmissionScopeAll, types.ImageAdmissionScopeMember} {
				t.Run(fmt.Sprintf("%s, %s, scope %s", b.name, mode, scope), func(t *testing.T) {
					srv, st, _, run := pinFixture(t, nil)
					a := &fakeAdmitter{}
					st.site, srv.cfg.ImageAdmitter = site(mode, scope), a
					if _, _, failed := srv.resolveCreateRunImage(t.Context(), b.req, run, b.wsRefs); failed {
						t.Fatal("the launch failed")
					}
					if r, n := a.calls(); r != 0 || n != 0 {
						t.Errorf("launch asked %d Require / %d Admit, want none: admission does not cover a built image", r, n)
					}
					if got, want := recordedJSON(t, st), `{"origin":"`+b.origin+`"}`; got != want {
						t.Errorf("recorded %s, want %s: the origin alone, whatever the mode", got, want)
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		name, mode, scope string
		req               createRunRequest
	}{
		{"devcontainer, enforce", types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll, built[0].req},
		{"devcontainer, audit", types.ImageAdmissionAudit, types.ImageAdmissionScopeAll, built[0].req},
		{"devcontainer, off", types.ImageAdmissionOff, types.ImageAdmissionScopeAll, built[0].req},
		{"convention image, enforce, scope member", types.ImageAdmissionEnforce, types.ImageAdmissionScopeMember, createRunRequest{Agent: "claude-code"}},
		{"convention image, off", types.ImageAdmissionOff, types.ImageAdmissionScopeAll, createRunRequest{Agent: "claude-code"}},
	} {
		t.Run("origin that cannot be recorded, "+tc.name, func(t *testing.T) {
			srv, st, _, run := pinFixture(t, nil)
			audit := &recRecorder{}
			st.site, st.admissionErr = site(tc.mode, tc.scope), errors.New("db down")
			srv.cfg.Audit, srv.cfg.ImageAdmitter = audit, &fakeAdmitter{}
			_, _, failed := srv.resolveCreateRunImage(t.Context(), tc.req, run, nil)
			if want := tc.mode == types.ImageAdmissionEnforce; failed != want {
				t.Fatalf("launch failed = %v, want %v: enforce fails a run whose origin cannot be recorded, audit logs it", failed, want)
			}
			if !failed {
				return
			}
			if got := st.state(run.ID); got != types.RunFailed {
				t.Errorf("state = %s, want FAILED", got)
			}
			row := findAudit(audit.snapshot(), run.ID, "run.dispatch", "failure")
			if row == nil || strings.Contains(string(row.Data), imageAdmissionRefused) || !strings.Contains(string(row.Data), "could not") {
				t.Errorf("run.dispatch failure row = %v, want one saying the record could not be written, not that the image was refused", row)
			}
		})
	}
	for _, tc := range []struct{ mode, scope string }{
		{types.ImageAdmissionOff, types.ImageAdmissionScopeAll},
		{types.ImageAdmissionAudit, types.ImageAdmissionScopeMember},
		{types.ImageAdmissionEnforce, types.ImageAdmissionScopeMember},
	} {
		t.Run(fmt.Sprintf("convention image, %s, scope %s: the origin alone", tc.mode, tc.scope), func(t *testing.T) {
			srv, st, _, run := pinFixture(t, nil)
			a := &fakeAdmitter{}
			st.site, srv.cfg.ImageAdmitter = site(tc.mode, tc.scope), a
			if _, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code"}, run, nil); failed {
				t.Fatal("the launch failed")
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("launch asked %d Require / %d Admit, want none", r, n)
			}
			if got, want := recordedJSON(t, st), `{"origin":"convention"}`; got != want {
				t.Errorf("recorded %s, want %s", got, want)
			}
		})
	}
	for _, mode := range []string{types.ImageAdmissionAudit, types.ImageAdmissionEnforce} {
		t.Run("convention image, "+mode+", scope all", func(t *testing.T) {
			srv, st, _, run := pinFixture(t, nil)
			st.site, srv.cfg.ImageAdmitter = site(mode, types.ImageAdmissionScopeAll), &fakeAdmitter{}
			if _, _, failed := srv.resolveCreateRunImage(memberContext(), createRunRequest{Agent: "claude-code"}, run, nil); failed {
				t.Fatal("the launch failed")
			}
			if got := recordedJSON(t, st); !strings.Contains(got, `"origin":"convention"`) || !strings.Contains(got, `"verdict":"admitted"`) {
				t.Errorf("recorded %s, want an admitted verdict with origin convention", got)
			}
		})
	}
}

// builtVerdict is what a launch records for an image Wardyn built (origin devcontainer or workspace), read from
// the JSON the run row holds.
func builtVerdict(t *testing.T, mode, origin string) *types.ImageAdmissionVerdict {
	t.Helper()
	var v types.ImageAdmissionVerdict
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"mode":%q,"verdict":"not_applicable","origin":%q}`, mode, origin)), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// TestReviveImageAdmission_BuiltImageOrigin: under enforce with scope all, a kept sandbox on an image Wardyn
// built for the run (its launch recorded origin devcontainer or workspace) revives, by its owner and through
// the admin bulk restart, without a check: admission never covered it. A member's image is held to the member's
// checks whatever origin its row records, and a run with no origin recorded is refused as before, by a sentence
// that is true of it.
func TestReviveImageAdmission_BuiltImageOrigin(t *testing.T) {
	builtRef := "wardyn-devcontainer/" + uuid.NewString() + ":latest"
	for _, origin := range []string{"devcontainer", "workspace"} {
		t.Run(origin+": revived unchecked", func(t *testing.T) {
			a := &fakeAdmitter{}
			f := scopeAllConventionFixture(t, builtVerdict(t, types.ImageAdmissionEnforce, origin), a)
			f.st.run.Image = builtRef
			if code, body := f.reviveAs(t, true); code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200: admission does not cover a built image", code, body)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("revive asked %d Require / %d Admit, want none", r, n)
			}
		})
		t.Run(origin+": admin bulk restart", func(t *testing.T) {
			f := scopeAllConventionFixture(t, builtVerdict(t, types.ImageAdmissionEnforce, origin), &fakeAdmitter{})
			f.st.run.Image = builtRef
			if res := f.restart(t); !res.OK {
				t.Fatalf("restart result = %+v, want ok", res)
			}
		})
	}
	t.Run("a member's image with a built origin keeps the member's checks", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, builtVerdict(t, types.ImageAdmissionEnforce, "workspace"), a)
		f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeAll
		code, body := f.reviveAs(t, true)
		wantReviveNotAdmitted(t, f, a, code, body)
	})
	t.Run("a member's image never takes its pin from a convention record", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce,
			conventionVerdict(t, types.ImageAdmissionEnforce, types.ImageAdmissionAdmitted, convPinned), a)
		f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeAll
		code, body := f.reviveAs(t, true)
		wantReviveNotAdmitted(t, f, a, code, body)
	})
	t.Run("no origin recorded: refused, saying only what is true", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, nil, a)
		f.st.run.Image = builtRef
		code, body := f.reviveAs(t, true)
		if code != http.StatusForbidden || !strings.Contains(body, reviveNotAdmitted) || !strings.Contains(body, builtRef) ||
			!strings.Contains(body, "has no digest that image admission checked and pinned when this run started") ||
			!strings.Contains(body, "admission was off, in audit mode, or did not check this image") {
			t.Fatalf("revive = %d %s; want 403 %s naming %s, with the sentence that holds for any unpinned run", code, body, reviveNotAdmitted, builtRef)
		}
		if strings.Contains(body, "before images.admission enforced it") {
			t.Errorf("revive sentence %s claims the run started before admission was enforced", body)
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("revive asked %d Require / %d Admit, want none", r, n)
		}
		f.assertReviveRefused(t, reviveNotAdmitted)
	})
	t.Run("no origin recorded, scope member: unchecked as before", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, nil, a)
		f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeMember
		f.st.run.Image = builtRef
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("revive asked %d Require / %d Admit, want none", r, n)
		}
	})
}

// TestReviveImageAdmission_StepRunOrigin: a run Wardyn launches itself (newStepRun: a source scan, a workspace
// record, a harness sign-in, a site-config probe) names no image and is never checked at launch, even under
// scope all, so newStepRun records origin wardyn and a revive under enforce with scope all does not refuse it.
// A run that named no image and recorded no origin is still refused.
func TestReviveImageAdmission_StepRunOrigin(t *testing.T) {
	t.Run("newStepRun records origin wardyn", func(t *testing.T) {
		srv := New(baseTestConfig(newHarness(t), &dispatchTestStore{}))
		srv.cfg.Runner = &fakeRunner{}
		gov := stepRunGoverned(governanceCeiling{}, false, false)
		for _, task := range []string{harnessLoginTask, "source scan", "workspace record"} {
			run, _, err := srv.newStepRun(t.Context(), uuid.New(), "owner", task, types.CC2, gov, nil)
			if err != nil {
				t.Fatal(err)
			}
			if run.ImageInput == nil || *run.ImageInput != "" || run.ImageAdmission == nil || run.ImageAdmission.Origin != "wardyn" {
				t.Errorf("%s run: image input %v, image admission %+v; want \"\" and origin wardyn", task, run.ImageInput, run.ImageAdmission)
			}
		}
	})
	t.Run("a step run revives unchecked", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, &types.ImageAdmissionVerdict{Origin: "wardyn"}, a)
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200: image admission does not check a run Wardyn launches itself", code, body)
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("revive asked %d Require / %d Admit, want none", r, n)
		}
	})
	t.Run("a step run: admin bulk restart", func(t *testing.T) {
		f := scopeAllConventionFixture(t, &types.ImageAdmissionVerdict{Origin: "wardyn"}, &fakeAdmitter{})
		if res := f.restart(t); !res.OK {
			t.Fatalf("restart result = %+v, want ok", res)
		}
	})
	t.Run("no origin recorded: still refused", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, nil, a)
		code, body := f.reviveAs(t, true)
		wantConventionNotAdmitted(t, f, a, code, body)
	})
	t.Run("a member's image with origin wardyn keeps the member's checks", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, &types.ImageAdmissionVerdict{Origin: "wardyn"}, a)
		f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeAll
		code, body := f.reviveAs(t, true)
		wantReviveNotAdmitted(t, f, a, code, body)
	})
	// Only newStepRun writes origin wardyn: no POST /runs body carries a verdict or an origin, since the create
	// decodes strictly and its request has no such field.
	t.Run("POST /runs cannot carry an origin", func(t *testing.T) {
		srv := agentRosterFixture(t, nil)
		for _, extra := range []string{
			`"image_admission":{"origin":"wardyn"}`,
			`"image":"registry.example.com/team/agent:1","image_admission":{"origin":"wardyn"}`,
			`"origin":"wardyn"`,
		} {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code",`+extra+`}`)
			if w.Code != http.StatusBadRequest || errorReason(w) != reasonInvalidRequestBody {
				t.Errorf("create with %s = %d %s, want 400 %s", extra, w.Code, w.Body.String(), reasonInvalidRequestBody)
			}
		}
	})
}
