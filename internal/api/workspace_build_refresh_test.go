// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const refreshAdminSub = "sub-refresh-admin"

// casBuildStore is the /build double with the real store's compare-and-swap: the image is
// swapped in only when the row still names the ref the build observed.
type casBuildStore struct {
	diffBuildStore
	swapErr error // when set, the swap fails with it
}

func (s *casBuildStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, observed, ref, hash string, rec *types.ImageBuild) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.swapErr != nil {
		return false, s.swapErr
	}
	if s.ws.ImageRef != observed {
		return false, nil
	}
	s.ws.ImageRef, s.ws.BuiltProfileHash, s.ws.ImageBuild = ref, hash, rec
	return true, nil
}

// landRunLaunchBuild is a run-launch build winning the swap: another image is now the row's.
func (s *casBuildStore) landRunLaunchBuild(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws.ImageRef = ref
}

func (s *casBuildStore) row() types.Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws
}

// refreshBuilder builds every lane without a daemon. A build waits for release (when set),
// runs onBuild, then fails with failWith or answers a ref of its own.
type refreshBuilder struct {
	pinningBuilder
	mu        sync.Mutex
	release   chan struct{}
	onBuild   func()
	failWith  error
	pulls     []bool
	buildRefs []string
}

func (b *refreshBuilder) build(tag string) (BuiltImage, error) {
	if b.release != nil {
		<-b.release
	}
	if b.onBuild != nil {
		b.onBuild()
	}
	if b.failWith != nil {
		return BuiltImage{}, b.failWith
	}
	ref := "built/" + tag
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buildRefs = append(b.buildRefs, ref)
	return BuiltImage{Ref: ref, ID: "sha256:" + strings.Repeat("b", 64), PinFile: b.pinFile}, nil
}

func (b *refreshBuilder) BuildFromDevcontainerFiles(_ context.Context, _ map[string]string, tag string, _ io.Writer) (BuiltImage, error) {
	return b.build(tag)
}

func (b *refreshBuilder) FinalizeBase(_ context.Context, _, tag string, _ io.Writer, forcePull bool) (BuiltImage, error) {
	b.mu.Lock()
	b.pulls = append(b.pulls, forcePull)
	b.mu.Unlock()
	return b.build(tag)
}

func (b *refreshBuilder) builtRefs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buildRefs)
}

type buildRefreshFixture struct {
	srv     *Server
	st      *casBuildStore
	b       *refreshBuilder
	rr      *imageRemoverRunner
	h       *harness
	session *http.Cookie
	path    string
}

func newBuildRefreshFixture(t *testing.T, ws types.Workspace, b *refreshBuilder) *buildRefreshFixture {
	t.Helper()
	h := newHarness(t)
	ws.Name, ws.Status = "w", types.WorkspaceScanned
	if len(ws.Sources) == 0 {
		ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/payments"}}
	}
	st := &casBuildStore{diffBuildStore: diffBuildStore{b4BuildStore: b4BuildStore{ws: ws}, audit: h.audit}}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = b
	cfg.OIDC = &oidc.Authenticator{}
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rr
	return &buildRefreshFixture{
		srv: New(cfg), st: st, b: b, rr: rr, h: h,
		session: ssoSession(t, refreshAdminSub, "admin@corp.example", oidc.RoleAdmin),
		path:    "/api/v1/workspaces/" + ws.ID.String() + "/build",
	}
}

func (f *buildRefreshFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, f.srv, http.MethodPost, f.path, f.session, body)
}

func (f *buildRefreshFixture) get(t *testing.T) buildResponse {
	t.Helper()
	w := doSSO(t, f.srv, http.MethodGet, f.path, f.session, "")
	var got buildResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET /build = %d: %s", w.Code, w.Body.String())
	}
	return got
}

// refresh posts {"refresh": true}, expects it accepted, and returns once the build and its
// audit row are done.
func (f *buildRefreshFixture) refresh(t *testing.T) {
	t.Helper()
	if w := f.post(t, `{"refresh":true}`); w.Code != http.StatusAccepted {
		t.Fatalf("POST refresh = %d: %s", w.Code, w.Body.String())
	}
	f.waitDone(t)
}

func (f *buildRefreshFixture) waitDone(t *testing.T) {
	t.Helper()
	id := f.st.row().ID
	for deadline := time.Now().Add(5 * time.Second); f.srv.builds.get(id).Building; {
		if time.Now().After(deadline) {
			t.Fatal("the build never finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// refreshRow waits for the workspace.image.refresh row and returns it with its data.
func (f *buildRefreshFixture) refreshRow(t *testing.T) (types.AuditEvent, map[string]any) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		for _, ev := range f.h.audit.snapshot() {
			if ev.Action == "workspace.image.refresh" {
				return ev, recAuditData(t, &ev)
			}
		}
	}
	t.Fatal("no workspace.image.refresh row after 5s")
	return types.AuditEvent{}, nil
}

func (f *buildRefreshFixture) refreshRows() int {
	n := 0
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "workspace.image.refresh" {
			n++
		}
	}
	return n
}

func (f *buildRefreshFixture) removed() []string {
	f.rr.mu.Lock()
	defer f.rr.mu.Unlock()
	return slices.Clone(f.rr.removed)
}

// refreshDoneWorkspace is a built generated workspace: a pinned earlier build named "built/gone".
func refreshDoneWorkspace(t *testing.T) types.Workspace {
	t.Helper()
	return pinnedRecord(t, nil)
}

func TestBuildWorkspace_RefreshOnADoneWorkspaceSwapsAndRemovesTheOldTag(t *testing.T) {
	ws := refreshDoneWorkspace(t)
	f := newBuildRefreshFixture(t, ws, &refreshBuilder{pinningBuilder: *resolvingBuilder()})
	f.b.found = true // the kept image is on the host, so a plain build reads this workspace as done

	f.refresh(t)

	refs := f.b.builtRefs()
	row := f.st.row()
	if len(refs) != 1 || row.ImageRef != refs[0] || row.ImageRef == "built/gone" {
		t.Fatalf("image_ref = %q, builds = %v, want it moved to the one refresh build", row.ImageRef, refs)
	}
	if row.ImageBuild == nil || row.ImageBuild.Reason != types.ImageBuildReasonRefresh || row.ImageBuild.Image.Ref != refs[0] {
		t.Errorf("record = %+v, want reason refresh naming the new image", row.ImageBuild)
	}
	if got := f.removed(); !slices.Equal(got, []string{"built/gone"}) {
		t.Errorf("ImageRemove saw %v, want exactly the old tag", got)
	}
	if got := f.get(t); got.State != "done" || got.Image != refs[0] {
		t.Errorf("GET /build = %+v, want done on the new image", got)
	}
	if _, data := f.refreshRow(t); data["reason"] != types.ImageBuildReasonRefresh || data["image_id"] == nil {
		t.Errorf("refresh row = %v, want a success naming the new image", data)
	}
}

func TestBuildWorkspace_RefreshOfAByoiWorkspace(t *testing.T) {
	byoi := func() types.Workspace {
		return types.Workspace{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: "registry", Image: "ghcr.io/acme/base:1"}}
	}
	t.Run("succeeds", func(t *testing.T) {
		b := &refreshBuilder{release: make(chan struct{})}
		f := newBuildRefreshFixture(t, byoi(), b)
		if w := f.post(t, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "nothing_to_build") {
			t.Fatalf("plain POST = %d %s, want the nothing_to_build answer", w.Code, w.Body.String())
		}
		if w := f.post(t, `{"refresh":true}`); w.Code != http.StatusAccepted {
			t.Fatalf("POST refresh = %d: %s", w.Code, w.Body.String())
		}
		if got := f.get(t); got.State != "building" {
			t.Fatalf("GET while refreshing = %+v, want building", got)
		}
		close(b.release)
		f.waitDone(t)
		got := f.get(t)
		if got.State != "done" || got.Image == "" || got.Image != f.st.row().ImageRef {
			t.Errorf("GET after = %+v, want done on the new image", got)
		}
		if !slices.Equal(b.pulls, []bool{true}) {
			t.Errorf("FinalizeBase forcePull = %v, want [true]: the base is pulled again", b.pulls)
		}
	})
	t.Run("fails", func(t *testing.T) {
		b := &refreshBuilder{failWith: errors.New("pull denied")}
		f := newBuildRefreshFixture(t, byoi(), b)
		f.refresh(t)
		got := f.get(t)
		if got.State != "failed" || !strings.Contains(got.Detail, "pull denied") {
			t.Errorf("GET after a failed refresh = %+v, want failed naming the cause", got)
		}
		if f.st.row().ImageRef != "" {
			t.Errorf("image_ref = %q after a failed refresh", f.st.row().ImageRef)
		}
	})
}

func TestBuildWorkspace_RefreshLosingTheSwapIsSuperseded(t *testing.T) {
	ws := refreshDoneWorkspace(t)
	b := &refreshBuilder{pinningBuilder: *resolvingBuilder()}
	f := newBuildRefreshFixture(t, ws, b)
	f.b.found = true
	// A run-launch build lands between the refresh's build and its swap.
	b.onBuild = func() { f.st.landRunLaunchBuild("built/run-launch") }

	f.refresh(t)

	if got := f.st.row().ImageRef; got != "built/run-launch" {
		t.Errorf("image_ref = %q, want the run-launch build's, untouched by the loser", got)
	}
	refs := f.b.builtRefs()
	if got := f.removed(); len(refs) != 1 || !slices.Equal(got, refs) {
		t.Errorf("ImageRemove saw %v, want only the refresh's own tag %v (not the old or the winner's)", got, refs)
	}
	if got := f.get(t); got.State != "failed" || !strings.HasPrefix(got.Detail, "superseded") {
		t.Errorf("GET /build = %+v, want failed: superseded", got)
	}
	if ev, data := f.refreshRow(t); ev.Outcome != "failure" || data["reason"] != "superseded" {
		t.Errorf("refresh row = %s %v, want a failure with reason superseded", ev.Outcome, data)
	}
}

// A plain build that loses the swap reclaims its own tag and reports superseded too (a run
// launch keeps its image: its run is about to start it).
func TestBuildWorkspace_PlainBuildLosingTheSwapReclaimsItsTag(t *testing.T) {
	ws, _ := pinGeneratedWorkspace() // nothing built yet
	b := &refreshBuilder{pinningBuilder: *resolvingBuilder()}
	f := newBuildRefreshFixture(t, ws, b)
	b.onBuild = func() { f.st.landRunLaunchBuild("built/run-launch") }

	if w := f.post(t, ""); w.Code != http.StatusAccepted {
		t.Fatalf("POST build = %d: %s", w.Code, w.Body.String())
	}
	f.waitDone(t)

	refs := f.b.builtRefs()
	if got := f.removed(); len(refs) != 1 || !slices.Equal(got, refs) {
		t.Errorf("ImageRemove saw %v, want only the build's own tag %v", got, refs)
	}
	if got := f.get(t); got.State != "failed" || !strings.HasPrefix(got.Detail, "superseded") {
		t.Errorf("GET /build = %+v, want failed: superseded", got)
	}
	if n := f.refreshRows(); n != 0 {
		t.Errorf("%d workspace.image.refresh rows for a build that was not a refresh", n)
	}
}

func TestBuildWorkspace_FailedRefreshChangesNothing(t *testing.T) {
	ws := refreshDoneWorkspace(t)
	f := newBuildRefreshFixture(t, ws, &refreshBuilder{pinningBuilder: *resolvingBuilder(), failWith: errors.New("boom")})
	f.b.found = true

	f.refresh(t)

	row := f.st.row()
	if row.ImageRef != ws.ImageRef || row.BuiltProfileHash != ws.BuiltProfileHash || row.ImageBuild != ws.ImageBuild {
		t.Errorf("row = (%q, %q, %p), want it untouched (%q, %q, %p)", row.ImageRef, row.BuiltProfileHash, row.ImageBuild,
			ws.ImageRef, ws.BuiltProfileHash, ws.ImageBuild)
	}
	if got := f.removed(); len(got) != 0 {
		t.Errorf("ImageRemove saw %v after a failed refresh, want nothing", got)
	}
	if got := f.get(t); got.State != "failed" || !strings.Contains(got.Detail, "boom") {
		t.Errorf("GET /build = %+v, want failed naming the cause", got)
	}
	if ev, data := f.refreshRow(t); ev.Outcome != "failure" || data["reason"] != "build_failed" || data["image_id"] != nil {
		t.Errorf("refresh row = %s %v, want a build_failed failure naming no new image", ev.Outcome, data)
	}
}

// A refresh whose swap cannot be written changed nothing: it fails and keeps no tag.
func TestBuildWorkspace_RefreshThatCannotRecordItsImageFails(t *testing.T) {
	ws := refreshDoneWorkspace(t)
	f := newBuildRefreshFixture(t, ws, &refreshBuilder{pinningBuilder: *resolvingBuilder()})
	f.b.found = true
	f.st.swapErr = errors.New("store down")

	f.refresh(t)

	refs := f.b.builtRefs()
	if got := f.removed(); len(refs) != 1 || !slices.Equal(got, refs) {
		t.Errorf("ImageRemove saw %v, want only the refresh's own tag %v", got, refs)
	}
	if got := f.get(t); got.State != "failed" || !strings.Contains(got.Detail, "store down") {
		t.Errorf("GET /build = %+v, want failed naming the write error", got)
	}
	if ev, data := f.refreshRow(t); ev.Outcome != "failure" || data["reason"] != "build_failed" {
		t.Errorf("refresh row = %s %v, want a build_failed failure", ev.Outcome, data)
	}
}

func TestBuildWorkspace_RefreshBodyRules(t *testing.T) {
	ws := refreshDoneWorkspace(t)
	f := newBuildRefreshFixture(t, ws, &refreshBuilder{pinningBuilder: *resolvingBuilder()})
	f.b.found = true

	for name, body := range map[string]string{"no body": "", "{}": `{}`, "whitespace": " \n\t ", "refresh false": `{"refresh":false}`} {
		w := f.post(t, body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"done"`) {
			t.Errorf("%s: POST = %d %s, want today's 200 done and no build", name, w.Code, w.Body.String())
		}
	}
	for name, body := range map[string]string{"unknown field": `{"refresh":true,"x":1}`, "not json": `{not json`, "wrong type": `{"refresh":"yes"}`} {
		w := f.post(t, body)
		var eb errorBody
		if json.Unmarshal(w.Body.Bytes(), &eb) != nil || w.Code != http.StatusBadRequest || eb.Reason != reasonInvalidRequestBody {
			t.Errorf("%s: POST = %d %s, want 400 %s", name, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
	}
	if len(f.b.builtRefs()) != 0 || f.srv.builds.get(ws.ID).Building {
		t.Errorf("a build ran for a body that asked for none or was refused (builds %v)", f.b.builtRefs())
	}
}

// TestBuildWorkspace_RefreshIsRefusedLikeABuild: a member on another member's workspace gets the
// build door's own refusal, before the body is read, and the owner is not turned away.
func TestBuildWorkspace_RefreshIsRefusedLikeABuild(t *testing.T) {
	srv, st, _ := ownerHarness(t, runner.UserMountPolicy{})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
	foreign := st.put(types.Workspace{OwnedBy: ownerOtherSub})
	own := st.put(types.Workspace{OwnedBy: ownerMemberSub})
	for _, body := range []string{`{"refresh":true}`, `{"refresh":true,"x":1}`, `{not json`} {
		want := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+uuid.NewString()+"/build", member, body)
		got := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+foreign.String()+"/build", member, body)
		if got.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("foreign %q: %d %s, want the missing-workspace answer %d %s", body, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+own.String()+"/build", member, `{"refresh":true}`); w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
		t.Errorf("owner's refresh = %d %s, want it reachable", w.Code, w.Body.String())
	}
}

func TestWorkspaceImageRefreshAudit_NamesTheRequesterAndBothHashes(t *testing.T) {
	// The earlier build pinned the Go feature at pinGoDigest; the registry has moved it.
	ws := refreshDoneWorkspace(t)
	prev := ws.ImageBuild
	b := &refreshBuilder{pinningBuilder: pinningBuilder{
		digests: map[string]string{pinBaseRef: pinBaseDigest, pinGoRef: pinNewDigest},
		pinFile: []byte(pinFileContent),
	}}
	f := newBuildRefreshFixture(t, ws, b)
	f.b.found = true

	f.refresh(t)

	ev, data := f.refreshRow(t)
	if ev.Actor != refreshAdminSub || ev.ActorType != types.ActorHuman || ev.Target != ws.ID.String() || ev.Outcome != "success" {
		t.Errorf("row = actor %s/%s target %s outcome %s, want the requesting human on the workspace", ev.ActorType, ev.Actor, ev.Target, ev.Outcome)
	}
	prevSum, _ := prev.SHA256()
	newSum, _ := f.st.row().ImageBuild.SHA256()
	if data["previous_record_sha256"] != prevSum || data["record_sha256"] != newSum || prevSum == newSum {
		t.Errorf("hashes = %v / %v, want the previous record's %s and the new record's %s", data["previous_record_sha256"], data["record_sha256"], prevSum, newSum)
	}
	if data["previous_image_id"] != recImageID || data["image_id"] != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("image ids = %v -> %v, want the previous and the new image", data["previous_image_id"], data["image_id"])
	}
	if data["lane"] != types.ImageBuildLaneGenerated || data["workspace_id"] != ws.ID.String() || data["build_id"] == nil {
		t.Errorf("row data = %v, want lane generated, the workspace and a build id", data)
	}
	if got, _ := data["changed"].([]any); len(got) != 1 || got[0] != "feature:"+pinGoRef {
		t.Errorf("changed = %v, want [feature:%s]", data["changed"], pinGoRef)
	}
}

func TestChangedInputs(t *testing.T) {
	old := types.ImageBuildInputs{
		Base:       types.ImageBuildRef{Ref: "b", Digest: "d1"},
		Features:   []types.ImageBuildFeature{{Ref: "f/go", Digest: "g1"}, {Ref: "f/node", Digest: "n1"}},
		Installers: []types.ImageBuildInstaller{{Tool: "claude-code", Version: "1", SHA256: "s1"}},
	}
	cur := types.ImageBuildInputs{
		Base:       types.ImageBuildRef{Ref: "b", Digest: "d2"},
		Features:   []types.ImageBuildFeature{{Ref: "f/go", Digest: "g1"}, {Ref: "f/node", Digest: "n2"}},
		Installers: []types.ImageBuildInstaller{{Tool: "claude-code", Version: "2", SHA256: "s2"}},
	}
	if got, want := changedInputs(old, cur), []string{"base", "feature:f/node", "installer:claude-code"}; !slices.Equal(got, want) {
		t.Errorf("changed = %v, want %v", got, want)
	}
	if got := changedInputs(old, old); got == nil || len(got) != 0 {
		t.Errorf("equal inputs changed = %#v, want an empty list", got)
	}
}
