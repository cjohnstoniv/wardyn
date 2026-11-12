// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Image admission pins the run to the digest it verified (AgentRun.ImageAdmission.PinnedRef), and every
// consumer of the member's image reads the pin, never the tag: a BYOI wrap builds FROM it, a prewrapped image
// is dispatched as it, and a workspace's stored wrap is reused only when it was built FROM the same digest. A
// run with no pin (admission off, audit mode, an operator) is built and dispatched exactly as before.

// pinSwappedDigest is what the member's tag names now: the registry moved it after the check.
var pinSwappedDigest = "sha256:" + strings.Repeat("ee", 32)

// pinRecordingBuilder is an image builder over a registry whose tag moved after the check: a base named by
// digest builds FROM that digest, a bare tag FROM the tag's new digest (pinSwappedDigest). It records every
// base it was asked to wrap.
type pinRecordingBuilder struct {
	fakeImageBuilder
	mu    sync.Mutex
	bases []string
}

func (b *pinRecordingBuilder) FinalizeBase(_ context.Context, baseRef, outputTag string, _ io.Writer, _ bool) (BuiltImage, error) {
	b.mu.Lock()
	b.bases = append(b.bases, baseRef)
	b.mu.Unlock()
	digest := pinSwappedDigest
	if _, d, ok := strings.Cut(baseRef, "@"); ok {
		digest = d
	}
	return BuiltImage{Ref: outputTag, ID: "sha256:image-from-" + digest[len("sha256:"):][:12], BaseDigest: digest}, nil
}

func (b *pinRecordingBuilder) wrapped() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bases...)
}

// pinStore is a create store that also answers a prewrapped row and records a workspace's wrap.
type pinStore struct {
	*createDetachStore
	prewrapped types.BaseImageEntry
	site       types.SiteConfig
	wsMu       sync.Mutex
	wsRecord   *types.ImageBuild
	// admission is the verdict the launch recorded on the run (nil: none); admissionErr fails that write.
	admission    *types.ImageAdmissionVerdict
	admissionErr error
}

func (s *pinStore) SetRunImageAdmission(_ context.Context, _ uuid.UUID, v *types.ImageAdmissionVerdict) error {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.admissionErr != nil {
		return s.admissionErr
	}
	s.admission = v
	return nil
}

func (s *pinStore) recorded() *types.ImageAdmissionVerdict {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	return s.admission
}

func (s *pinStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (s *pinStore) GetBaseImageByRef(_ context.Context, kind, ref string) (types.BaseImageEntry, error) {
	if kind == types.BaseImageKindPrewrapped && s.prewrapped.Image != "" && ref == s.prewrapped.Image {
		return s.prewrapped, nil
	}
	return types.BaseImageEntry{}, store.ErrNotFound
}

func (s *pinStore) SetRunImageArches(context.Context, uuid.UUID, []string) error { return nil }

func (s *pinStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, _, _, _ string, rec *types.ImageBuild) (bool, error) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	s.wsRecord = rec
	return true, nil
}

// pinFixture is a server with no runner, an image builder over a moved tag, and one PENDING run.
func pinFixture(t *testing.T, verdict *types.ImageAdmissionVerdict) (*Server, *pinStore, *pinRecordingBuilder, types.AgentRun) {
	t.Helper()
	st := &pinStore{createDetachStore: newCreateDetachStore()}
	b := &pinRecordingBuilder{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.ImageBuilder = b
	run := newFinalizeRun()
	run.State, run.ImageAdmission = types.RunPending, verdict
	if _, err := st.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	return New(cfg), st, b, run
}

// pinnedVerdict is the verdict enforce writes for an admitted image: the pin is set only here.
func pinnedVerdict(pinned string) *types.ImageAdmissionVerdict {
	return &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionEnforce, Verdict: types.ImageAdmissionAdmitted,
		Digest: admitDigest, Signer: admitSigner, PinnedRef: pinned}
}

// The verdicts that carry no pin: none (admission off, an operator), audit's admitted and would-refuse ones.
var unpinnedVerdicts = []struct {
	name    string
	verdict *types.ImageAdmissionVerdict
}{
	{"no verdict", nil},
	{"audit admitted", &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionAdmitted, Digest: admitDigest}},
	{"audit would refuse", &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionWouldRefuse, Reason: reasonImageSignatureMissing}},
}

// TestImagePinBYOIWrapsTheVerifiedDigest: a member's own image is wrapped FROM the pin, so the tag moving after
// the check (the builder would build pinSwappedDigest from the tag) cannot change what runs. Without a pin the
// wrap is FROM the image as typed, as before.
func TestImagePinBYOIWrapsTheVerifiedDigest(t *testing.T) {
	srv, _, b, run := pinFixture(t, pinnedVerdict(admitPinned))
	image, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code", Image: admitTagRef}, run, nil)
	if failed {
		t.Fatal("the pinned BYOI wrap failed")
	}
	if got := b.wrapped(); len(got) != 1 || got[0] != admitPinned {
		t.Fatalf("FinalizeBase bases = %q, want exactly the pin %q (the tag now names %s)", got, admitPinned, pinSwappedDigest)
	}
	if want := "wardyn-byoi/" + run.ID.String() + ":latest"; image != want {
		t.Errorf("dispatched image = %q, want the wrap %q", image, want)
	}
	for _, tc := range unpinnedVerdicts {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, b, run := pinFixture(t, tc.verdict)
			if _, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code", Image: admitTagRef}, run, nil); failed {
				t.Fatal("the BYOI wrap failed")
			}
			if got := b.wrapped(); len(got) != 1 || got[0] != admitTagRef {
				t.Errorf("FinalizeBase bases = %q, want the image as typed %q", got, admitTagRef)
			}
		})
	}
}

// TestImagePinPrewrappedDispatchesThePin: a prewrapped image runs unwrapped, so the dispatched image is the pin
// itself (the canonical name at the verified digest); the row is still looked up by the image as typed.
func TestImagePinPrewrappedDispatchesThePin(t *testing.T) {
	typed := "app@" + admitDigest // Docker Hub short form: the pin's canonical name differs from it
	pinned, err := imageAdmissionPinnedRef(typed, admitDigest)
	if err != nil || pinned == typed {
		t.Fatalf("pin of %q = %q, %v; want a canonical ref that differs from the typed one", typed, pinned, err)
	}
	row := types.BaseImageEntry{Kind: types.BaseImageKindPrewrapped, Image: typed,
		Check: &types.BaseImageCheck{Status: types.BaseImageCheckReady, AcceptedArches: []string{"amd64"}}}
	for _, tc := range []struct {
		name    string
		verdict *types.ImageAdmissionVerdict
		want    string
	}{
		{"pinned", pinnedVerdict(pinned), pinned},
		{"unpinned", nil, typed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, b, run := pinFixture(t, tc.verdict)
			st.prewrapped = row
			image, arches, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code", Image: typed}, run, nil)
			if failed {
				t.Fatal("the prewrapped launch failed")
			}
			if image != tc.want || len(arches) != 1 || len(b.wrapped()) != 0 {
				t.Errorf("dispatched %q (arches %v, wraps %q), want %q unwrapped", image, arches, b.wrapped(), tc.want)
			}
		})
	}
}

// pinWorkspace is a workspace whose chosen base image is the member's tag, holding a stored wrap built FROM
// recorded (the record's inputs.base.digest; "" = a record that names none).
func pinWorkspace(recorded string) types.Workspace {
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: admitTagRef},
	}
	ws.ImageRef, ws.BuiltProfileHash = "wardyn-byoi/stored:latest", byoiCacheKey("custom", admitTagRef)
	ws.ImageBuild = &types.ImageBuild{Schema: types.ImageBuildSchema, Lane: types.ImageBuildLaneBYOI, CacheKey: ws.BuiltProfileHash,
		Inputs: types.ImageBuildInputs{Base: types.ImageBuildRef{Ref: admitTagRef, Digest: recorded}},
		Image:  types.ImageBuildImage{Ref: ws.ImageRef}}
	return ws
}

// TestImagePinWorkspaceWrapReuse: under a pin, a workspace's stored wrap is reused only when its record says it
// was built FROM the admitted digest; any other base digest, or a record that names none, is rebuilt FROM the
// pin, and the new record names the verified digest even though the tag has moved. Without a pin the stored wrap
// is reused as before.
func TestImagePinWorkspaceWrapReuse(t *testing.T) {
	other := "sha256:" + strings.Repeat("77", 32)
	for _, tc := range []struct {
		name     string
		verdict  *types.ImageAdmissionVerdict
		recorded string
		rebuilt  bool
	}{
		{"pinned, stored wrap from the admitted digest", pinnedVerdict(admitPinned), admitDigest, false},
		{"pinned, stored wrap from another digest", pinnedVerdict(admitPinned), other, true},
		{"pinned, stored wrap from the moved tag", pinnedVerdict(admitPinned), pinSwappedDigest, true},
		{"pinned, stored wrap that records no base digest", pinnedVerdict(admitPinned), "", true},
		{"unpinned, stored wrap from another digest", nil, other, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, b, run := pinFixture(t, tc.verdict)
			ws := pinWorkspace(tc.recorded)
			image, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code"}, run, []types.Workspace{ws})
			if failed {
				t.Fatal("the workspace image failed")
			}
			if !tc.rebuilt {
				if image != ws.ImageRef || len(b.wrapped()) != 0 {
					t.Fatalf("image %q after wraps %q, want the stored wrap %q reused", image, b.wrapped(), ws.ImageRef)
				}
				return
			}
			if got := b.wrapped(); len(got) != 1 || got[0] != admitPinned {
				t.Fatalf("FinalizeBase bases = %q, want one rebuild FROM the pin %q", got, admitPinned)
			}
			if image == ws.ImageRef {
				t.Errorf("image = the stale stored wrap %q, want the rebuilt one", image)
			}
			st.wsMu.Lock()
			rec := st.wsRecord
			st.wsMu.Unlock()
			if rec == nil || rec.Inputs.Base.Digest != admitDigest || rec.Inputs.Base.Ref != admitTagRef {
				t.Errorf("recorded wrap = %+v, want base %s at the verified digest %s", rec, admitTagRef, admitDigest)
			}
		})
	}
}

// TestImageAdmissionScopeAllChecksTheConventionImage: with scope all and mode enforce, the image a governed run
// gets by convention (no member image) is checked at launch and dispatched as its pin. A failure fails the run
// with the refusal sentence as its hint and a run.dispatch failure row; it is the operator's image, so there
// is no 403. Scope member, off, an operator and a member's own image leave the convention image unchecked
// (audit: TestImageAdmissionScopeAllAuditChecksAndRuns).
func TestImageAdmissionScopeAllChecksTheConventionImage(t *testing.T) {
	site := func(mode, scope string) types.SiteConfig {
		sc := admissionSite(mode)
		sc.Images.Admission.Scope = scope
		return sc
	}
	launch := func(t *testing.T, ctx context.Context, sc types.SiteConfig, a *fakeAdmitter, req createRunRequest) (string, bool, *pinStore, *recRecorder, types.AgentRun) {
		t.Helper()
		srv, st, _, run := pinFixture(t, nil)
		st.site = sc
		audit := &recRecorder{}
		srv.cfg.Audit, srv.cfg.ImageAdmitter = audit, a
		image, _, failed := srv.resolveCreateRunImage(ctx, req, run, nil)
		return image, failed, st, audit, run
	}
	convention := createRunRequest{Agent: "claude-code"}
	conventional, failed, _, _, _ := launch(t, t.Context(), site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeMember), &fakeAdmitter{}, convention)
	if failed || conventional == "" {
		t.Fatalf("convention launch under scope member = %q, failed %v", conventional, failed)
	}
	pinned, err := imageAdmissionPinnedRef(conventional, admitDigest)
	if err != nil {
		t.Fatalf("pin of the convention image %q: %v", conventional, err)
	}

	t.Run("enforce, scope all: checked and pinned", func(t *testing.T) {
		a := &fakeAdmitter{}
		image, failed, _, _, _ := launch(t, memberContext(), site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll), a, convention)
		if failed || image != pinned {
			t.Fatalf("dispatched %q (failed %v), want the pin %q", image, failed, pinned)
		}
		if !slices.Equal(a.admits, []string{conventional}) || len(a.requires) != 1 || !a.requires[0].site {
			t.Errorf("Require %+v / Admit %q, want one site check of %q", a.requires, a.admits, conventional)
		}
	})
	t.Run("enforce, scope all: a failure fails the run", func(t *testing.T) {
		a := &fakeAdmitter{admit: func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{admitSigner}}, nil
		}}
		_, failed, st, audit, run := launch(t, memberContext(), site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll), a, convention)
		if !failed {
			t.Fatal("an unsigned convention image under scope all was dispatched")
		}
		if got := st.state(run.ID); got != types.RunFailed {
			t.Errorf("state = %s, want FAILED", got)
		}
		row := findAudit(audit.snapshot(), run.ID, "run.dispatch", "failure")
		if row == nil || !strings.Contains(string(row.Data), reasonImageSignatureMissing) || !strings.Contains(string(row.Data), conventional) {
			t.Fatalf("run.dispatch failure row = %v, want one naming %s and the image", row, reasonImageSignatureMissing)
		}
	})
	admin := withOIDCRole(withOIDCHuman(t.Context(), "admin@example.com"), oidc.RoleAdmin)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		sc   types.SiteConfig
		req  createRunRequest
	}{
		{"enforce, scope member", memberContext(), site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeMember), convention},
		{"off", memberContext(), site(types.ImageAdmissionOff, types.ImageAdmissionScopeAll), convention},
		{"operator, enforce, scope all", admin, site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll), convention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			image, failed, _, _, _ := launch(t, tc.ctx, tc.sc, a, tc.req)
			if failed || image != conventional {
				t.Fatalf("dispatched %q (failed %v), want the convention image %q unchecked", image, failed, conventional)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("asked %d Require / %d Admit, want none", r, n)
			}
		})
	}
	t.Run("governed admin (GovernAdminRuns), enforce, scope all: checked", func(t *testing.T) {
		a := &fakeAdmitter{}
		srv, st, _, run := pinFixture(t, nil)
		st.site = site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll)
		srv.cfg.ImageAdmitter, srv.cfg.GovernAdminRuns = a, true
		if image, _, failed := srv.resolveCreateRunImage(admin, convention, run, nil); failed || image != pinned || len(a.admits) != 1 {
			t.Fatalf("dispatched %q (failed %v, admits %q), want the governed admin's convention image checked and pinned", image, failed, a.admits)
		}
	})
	t.Run("enforce, scope all: a member's own image is not the convention image", func(t *testing.T) {
		a := &fakeAdmitter{}
		srv, st, b, run := pinFixture(t, pinnedVerdict(admitPinned))
		st.site = site(types.ImageAdmissionEnforce, types.ImageAdmissionScopeAll)
		srv.cfg.ImageAdmitter = a
		if _, _, failed := srv.resolveCreateRunImage(t.Context(), createRunRequest{Agent: "claude-code", Image: admitTagRef}, run, nil); failed {
			t.Fatal("the member's pinned image failed")
		}
		if r, n := a.calls(); r != 0 || n != 0 || !slices.Equal(b.wrapped(), []string{admitPinned}) {
			t.Errorf("asked %d Require / %d Admit, wrapped %q; want no launch check and the member's pin wrapped", r, n, b.wrapped())
		}
	})
}

// TestImageAdmissionScopeAllRecordsTheConventionVerdict: a convention image admitted at launch under enforce
// and scope all has its verdict written to the run before dispatch: the image as configured, the verified
// digest and the pin, so a revive can re-check that digest without resolving the tag. A run whose verdict
// cannot be recorded is failed rather than dispatched with nothing on its row.
func TestImageAdmissionScopeAllRecordsTheConventionVerdict(t *testing.T) {
	sc := admissionSite(types.ImageAdmissionEnforce)
	sc.Images.Admission.Scope = types.ImageAdmissionScopeAll
	srv, st, _, run := pinFixture(t, nil)
	st.site, srv.cfg.ImageAdmitter = sc, &fakeAdmitter{}
	image, _, failed := srv.resolveCreateRunImage(memberContext(), createRunRequest{Agent: "claude-code"}, run, nil)
	if failed {
		t.Fatal("the admitted convention launch failed")
	}
	conventional, _, _ := strings.Cut(image, "@")
	v := st.recorded()
	raw, _ := json.Marshal(v)
	for _, want := range []string{`"mode":"enforce"`, `"verdict":"admitted"`, `"digest":"` + admitDigest + `"`, `"pinned_ref":"` + image + `"`, `"ref":"`} {
		if v == nil || !strings.Contains(string(raw), want) {
			t.Fatalf("recorded verdict %s, want it to carry %s", raw, want)
		}
	}
	if !strings.Contains(string(raw), `"ref":"`+conventional) {
		t.Errorf("recorded verdict %s does not name the convention image as configured", raw)
	}

	srv, st, _, run = pinFixture(t, nil)
	st.site, srv.cfg.ImageAdmitter, st.admissionErr = sc, &fakeAdmitter{}, errors.New("db down")
	if _, _, failed := srv.resolveCreateRunImage(memberContext(), createRunRequest{Agent: "claude-code"}, run, nil); !failed {
		t.Fatal("a convention image whose verdict could not be recorded was dispatched")
	}
	if got := st.state(run.ID); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
}

// TestImageAdmissionScopeAllAuditChecksAndRuns: audit with scope all checks a governed run's convention image at
// launch the same way enforce does and records the outcome on the run (admitted, or would_refuse with its
// reason, an unreachable registry included), but never pins it and never fails the launch: the dispatched image
// is the convention image as configured, even when the verdict cannot be written. Off asks nothing and records
// nothing.
func TestImageAdmissionScopeAllAuditChecksAndRuns(t *testing.T) {
	site := func(mode string) types.SiteConfig {
		sc := admissionSite(mode)
		sc.Images.Admission.Scope = types.ImageAdmissionScopeAll
		return sc
	}
	launch := func(t *testing.T, mode string, a *fakeAdmitter, recordErr error) (string, bool, *pinStore) {
		t.Helper()
		srv, st, _, run := pinFixture(t, nil)
		st.site, st.admissionErr, srv.cfg.ImageAdmitter = site(mode), recordErr, a
		image, _, failed := srv.resolveCreateRunImage(memberContext(), createRunRequest{Agent: "claude-code"}, run, nil)
		return image, failed, st
	}
	conventional, failed, _ := launch(t, types.ImageAdmissionOff, &fakeAdmitter{}, nil)
	if failed || conventional == "" {
		t.Fatalf("convention launch with admission off = %q, failed %v", conventional, failed)
	}
	unsigned := func(imageadmit.Requirements) (imageadmit.Verdict, error) {
		return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{admitSigner}}, nil
	}
	unavailable := func(imageadmit.Requirements) (imageadmit.Verdict, error) {
		return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: "registry.example.com could not be reached"}, errors.New("dial: refused")
	}
	for _, tc := range []struct {
		name         string
		admit        func(imageadmit.Requirements) (imageadmit.Verdict, error)
		verdict, why string
	}{
		{"unsigned: would refuse", unsigned, types.ImageAdmissionWouldRefuse, reasonImageSignatureMissing},
		{"registry unreachable: would refuse", unavailable, types.ImageAdmissionWouldRefuse, reasonImageAdmissionUnavailable},
		{"signed: admitted", nil, types.ImageAdmissionAdmitted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{admit: tc.admit}
			image, failed, st := launch(t, types.ImageAdmissionAudit, a, nil)
			if failed || image != conventional {
				t.Fatalf("dispatched %q (failed %v), want the convention image %q as configured", image, failed, conventional)
			}
			if !slices.Equal(a.admits, []string{conventional}) {
				t.Errorf("Admit refs = %q, want one check of %q", a.admits, conventional)
			}
			v := st.recorded()
			if v == nil || v.Mode != types.ImageAdmissionAudit || v.Verdict != tc.verdict || v.Reason != tc.why ||
				v.Ref != conventional || v.PinnedRef != "" {
				t.Errorf("recorded verdict %+v, want audit %s (%q) for %q, never pinned", v, tc.verdict, tc.why, conventional)
			}
		})
	}
	t.Run("a verdict that cannot be written does not fail the launch", func(t *testing.T) {
		image, failed, _ := launch(t, types.ImageAdmissionAudit, &fakeAdmitter{admit: unsigned}, errors.New("db down"))
		if failed || image != conventional {
			t.Fatalf("dispatched %q (failed %v), want the convention image %q", image, failed, conventional)
		}
	})
	t.Run("off: no probe, only the origin recorded", func(t *testing.T) {
		a := &fakeAdmitter{}
		image, failed, st := launch(t, types.ImageAdmissionOff, a, nil)
		got := recordedJSON(t, st)
		if r, n := a.calls(); failed || image != conventional || r != 0 || n != 0 || got != `{"origin":"convention"}` {
			t.Errorf("off: dispatched %q (failed %v), %d Require / %d Admit, recorded %s; want no check and the origin alone", image, failed, r, n, got)
		}
	})
}
