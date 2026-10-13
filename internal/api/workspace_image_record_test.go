// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

const (
	recBaseDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recImageID    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// builtWrite is one SetWorkspaceBuiltImage call.
type builtWrite struct {
	observedRef, imageRef, hash string
	rec                         *types.ImageBuild
}

// recordingBuildStore captures every SetWorkspaceBuiltImage call and applies it.
type recordingBuildStore struct {
	store.Store
	mu     sync.Mutex
	writes []builtWrite
	err    error
}

func (s *recordingBuildStore) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, observed, ref, hash string, rec *types.ImageBuild) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, builtWrite{observed, ref, hash, rec})
	return s.err == nil, s.err
}

func (s *recordingBuildStore) snapshot() []builtWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]builtWrite(nil), s.writes...)
}

// recordingBuilder answers every lane with a deterministic BuiltImage and
// remembers what it was asked to build.
type recordingBuilder struct {
	mu               sync.Mutex
	calls            int
	tags             []string
	repoURL, repoRef string
	files            map[string]string
	baseDigest       string
}

func (b *recordingBuilder) built(tag string) BuiltImage {
	return BuiltImage{Ref: "built/" + tag, ID: recImageID, BaseDigest: b.baseDigest}
}

func (b *recordingBuilder) note(tag string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.tags = append(b.tags, tag)
}

func (b *recordingBuilder) BuildDevcontainer(_ context.Context, url, ref, tag string, _ io.Writer) (BuiltImage, error) {
	b.note(tag)
	b.mu.Lock()
	b.repoURL, b.repoRef = url, ref
	b.mu.Unlock()
	return b.built(tag), nil
}

func (b *recordingBuilder) BuildFromDevcontainerFiles(_ context.Context, files map[string]string, tag string, _ io.Writer) (BuiltImage, error) {
	b.note(tag)
	b.mu.Lock()
	b.files = files
	b.mu.Unlock()
	return b.built(tag), nil
}

func (b *recordingBuilder) FinalizeBase(_ context.Context, _, tag string, _ io.Writer, _ bool) (BuiltImage, error) {
	b.note(tag)
	return b.built(tag), nil
}

func recProfile(devcontainer bool) workspacescan.WorkspaceProfile {
	return workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: devcontainer,
	}
}

type recLane struct {
	name, lane string
	ws         types.Workspace
	key        string
}

// recLanes is one workspace per build lane, each with the cache key the lane writes under.
func recLanes() []recLane {
	repoSrc := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}}
	return []recLane{
		{"generated", types.ImageBuildLaneGenerated, types.Workspace{Profile: mustJSON(recProfile(false))}, recProfile(false).CacheKey()},
		{"byoi", types.ImageBuildLaneBYOI, types.Workspace{BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}},
			byoiCacheKey("custom", "ghcr.io/acme/base:1")},
		{"repo", types.ImageBuildLaneRepo, types.Workspace{Sources: repoSrc, Profile: mustJSON(recProfile(true))},
			repoDevcontainerCacheKey("https://github.com/acme/widgets", "main")},
	}
}

func recServer(t *testing.T, st store.Store, b ImageBuilder) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = b
	return New(cfg), h
}

func recAuditData(t *testing.T, ev *types.AuditEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestResolveWorkspaceImage_EachLaneWritesOneRecord(t *testing.T) {
	for _, tc := range recLanes() {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingBuildStore{}
			b := &recordingBuilder{baseDigest: recBaseDigest}
			srv, _ := recServer(t, st, b)
			ws := tc.ws
			ws.ID = uuid.New()
			runID := uuid.New()

			got, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)
			w := st.snapshot()
			if !ok || len(w) != 1 || w[0].rec == nil {
				t.Fatalf("resolveWorkspaceImage = (%q, %v) with %d writes, want one write carrying a record", got, ok, len(w))
			}
			rec := w[0].rec
			if rec.Lane != tc.lane || rec.CacheKey != tc.key || rec.BuildID != runID.String() || rec.Schema != types.ImageBuildSchema {
				t.Errorf("record = %+v, want lane %q key %q build_id %s", rec, tc.lane, tc.key, runID)
			}
			if rec.Image.Ref != got || rec.Image.ID != recImageID || w[0].imageRef != got || w[0].hash != tc.key {
				t.Errorf("record image %+v / write (%q, %q): want the builder's ref %q, ID %q and key %q", rec.Image, w[0].imageRef, w[0].hash, got, recImageID, tc.key)
			}
			if rec.Reason != types.ImageBuildReasonCacheMiss {
				t.Errorf("reason = %q, want cache_miss for a workspace with nothing built", rec.Reason)
			}
		})
	}
}

func TestResolveWorkspaceImage_ImageMissingReason(t *testing.T) {
	lane := recLanes()[0]
	st := &recordingBuildStore{}
	srv, _ := recServer(t, st, &recordingBuilder{})
	ws := lane.ws
	ws.ID, ws.ImageRef, ws.BuiltProfileHash = uuid.New(), "gone/image", lane.key
	// Make the cache hit miss: the runner says the image is not on the host.
	srv.cfg.Runner = absentImageRunner{}

	srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil)
	w := st.snapshot()
	if len(w) != 1 || w[0].rec.Reason != types.ImageBuildReasonImageMissing {
		t.Fatalf("writes = %+v, want one record with reason image_missing (key matched, image gone)", w)
	}
}

type absentImageRunner struct{ *fakeRunner }

func (absentImageRunner) ImagePresent(context.Context, string) (bool, error) { return false, nil }

func TestResolveWorkspaceImage_TagsCarryTheBuildID(t *testing.T) {
	for _, tc := range recLanes() {
		t.Run(tc.name, func(t *testing.T) {
			b := &recordingBuilder{}
			srv, _ := recServer(t, &recordingBuildStore{}, b)
			ws := tc.ws
			ws.ID = uuid.New()
			runID := uuid.New()
			srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)

			want := map[string]string{
				"generated": "wardyn-workspace/" + ws.ID.String() + ":" + tc.key[:12] + "-" + runID.String()[:8],
				"byoi":      "wardyn-byoi/" + runID.String() + ":latest",
				"repo":      "wardyn-workspace/" + ws.ID.String() + ":devcontainer-" + runID.String()[:8],
			}[tc.name]
			if len(b.tags) != 1 || b.tags[0] != want {
				t.Fatalf("tags = %v, want [%s]", b.tags, want)
			}
		})
	}
}

// idBuilder is a builder that can say what a tag names on the host.
type idBuilder struct {
	recordingBuilder
	id    string
	found bool
	err   error
}

func (b *idBuilder) LocalImage(context.Context, string) (string, bool, error) {
	return b.id, b.found, b.err
}

func (b *idBuilder) RemoteDigest(context.Context, string) (string, error) {
	return "", errors.New("idBuilder resolves nothing")
}

func recCurrentWorkspace() types.Workspace {
	lane := recLanes()[0]
	ws := lane.ws
	ws.ID, ws.ImageRef, ws.BuiltProfileHash = uuid.New(), "built/x", lane.key
	ws.ImageBuild = newImageBuild(types.ImageBuildLaneGenerated, lane.key, uuid.New(), types.ImageBuildReasonCacheMiss,
		types.ImageBuildInputs{}, BuiltImage{Ref: "built/x", ID: recImageID}, time.Now())
	return ws
}

func TestImageBuildView_TagMovedUnderTheRecordReadsUnrecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		b       ImageBuilder
		mutate  func(*types.Workspace)
		wantRec bool
	}{
		{"same ID is current", &idBuilder{id: recImageID, found: true}, nil, true},
		{"another ID is unrecorded", &idBuilder{id: "sha256:2222", found: true}, nil, false},
		{"an unanswerable builder is unrecorded", &idBuilder{err: errors.New("daemon down")}, nil, false},
		{"a builder that cannot identify is current", &recordingBuilder{}, nil, true},
		{"no record", &idBuilder{id: recImageID, found: true}, func(w *types.Workspace) { w.ImageBuild = nil }, false},
		{"a newer schema", &idBuilder{id: recImageID, found: true}, func(w *types.Workspace) { w.ImageBuild.Schema = 2 }, false},
		{"the row moved to another image", &idBuilder{id: recImageID, found: true}, func(w *types.Workspace) { w.ImageRef = "built/y" }, false},
		{"the row moved to another key", &idBuilder{id: recImageID, found: true}, func(w *types.Workspace) { w.BuiltProfileHash = "other" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := recServer(t, nil, tc.b)
			ws := recCurrentWorkspace()
			if tc.mutate != nil {
				tc.mutate(&ws)
			}
			rec, missing, _ := srv.imageBuildView(context.Background(), ws)
			if (rec != nil) != tc.wantRec || missing {
				t.Fatalf("imageBuildView = (%v, missing=%v), want a record: %v", rec, missing, tc.wantRec)
			}
		})
	}
}

func TestImageBuildView_MissingImageIsCurrentAndMissing(t *testing.T) {
	srv, _ := recServer(t, nil, &idBuilder{found: false})
	rec, missing, liveID := srv.imageBuildView(context.Background(), recCurrentWorkspace())
	if rec == nil || !missing || liveID != "" {
		t.Fatalf("imageBuildView = (%v, missing=%v, %q), want a current record, missing, no live ID", rec, missing, liveID)
	}
}

func TestImageBuild_ResolvedFollowsTheLaneTable(t *testing.T) {
	pinnedGenerated := types.ImageBuildInputs{
		Base:       types.ImageBuildRef{Ref: "base", Digest: recBaseDigest},
		Features:   []types.ImageBuildFeature{{Ref: "f", Digest: recBaseDigest}},
		Installers: []types.ImageBuildInstaller{{Tool: "claude-code", Version: "1", SHA256: "ab"}},
	}
	unpinned := func(mut func(*types.ImageBuildInputs)) types.ImageBuildInputs {
		in := pinnedGenerated
		in.Features = append([]types.ImageBuildFeature(nil), in.Features...)
		in.Installers = append([]types.ImageBuildInstaller(nil), in.Installers...)
		mut(&in)
		return in
	}
	for _, tc := range []struct {
		name, lane string
		in         types.ImageBuildInputs
		want       bool
	}{
		{"generated, all pinned", types.ImageBuildLaneGenerated, pinnedGenerated, true},
		{"generated, base unpinned", types.ImageBuildLaneGenerated, unpinned(func(in *types.ImageBuildInputs) { in.Base.Digest = "" }), false},
		{"generated, feature unpinned", types.ImageBuildLaneGenerated, unpinned(func(in *types.ImageBuildInputs) { in.Features[0].Digest = "" }), false},
		{"generated, installer without a version", types.ImageBuildLaneGenerated, unpinned(func(in *types.ImageBuildInputs) { in.Installers[0].Version = "" }), false},
		{"generated, installer without a checksum", types.ImageBuildLaneGenerated, unpinned(func(in *types.ImageBuildInputs) { in.Installers[0].SHA256 = "" }), false},
		{"byoi with a digest", types.ImageBuildLaneBYOI, types.ImageBuildInputs{Base: types.ImageBuildRef{Ref: "b", Digest: recBaseDigest}}, true},
		{"byoi without a digest", types.ImageBuildLaneBYOI, types.ImageBuildInputs{Base: types.ImageBuildRef{Ref: "b"}}, false},
		{"repo, even fully pinned", types.ImageBuildLaneRepo, pinnedGenerated, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newImageBuild(tc.lane, "k", uuid.New(), types.ImageBuildReasonCacheMiss, tc.in, BuiltImage{Ref: "r"}, time.Now())
			if rec.Resolved != tc.want {
				t.Fatalf("Resolved = %v, want %v", rec.Resolved, tc.want)
			}
		})
	}
}

func TestResolveWorkspaceImage_CacheHitWritesNoRecord(t *testing.T) {
	for _, tc := range recLanes() {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingBuildStore{}
			b := &recordingBuilder{}
			srv, _ := recServer(t, st, b)
			ws := tc.ws
			ws.ID, ws.ImageRef, ws.BuiltProfileHash = uuid.New(), "built/cached", tc.key // ImageBuild stays nil: a pre-0.9 image
			got, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil)
			if !ok || got != "built/cached" {
				t.Fatalf("resolveWorkspaceImage = (%q, %v), want the cached image", got, ok)
			}
			if b.calls != 0 || len(st.snapshot()) != 0 {
				t.Fatalf("builder calls %d, store writes %d; a cache hit builds nothing and records nothing", b.calls, len(st.snapshot()))
			}
		})
	}
}

func TestResolveWorkspaceImage_RepoLaneRecordsOnlyItsRepo(t *testing.T) {
	st := &recordingBuildStore{}
	b := &recordingBuilder{}
	srv, _ := recServer(t, st, b)
	lane := recLanes()[2]
	ws := lane.ws
	ws.ID = uuid.New()
	srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil)

	w := st.snapshot()
	if len(w) != 1 {
		t.Fatalf("writes = %d, want 1", len(w))
	}
	in := w[0].rec.Inputs
	if b.repoURL != "https://github.com/acme/widgets" || b.repoRef != "main" || b.files != nil {
		t.Errorf("builder got url %q ref %q files %v, want the source unchanged and no files", b.repoURL, b.repoRef, b.files)
	}
	if in.Repo == nil || *in.Repo != (types.ImageBuildRepo{URL: "https://github.com/acme/widgets", Ref: "main"}) {
		t.Errorf("Inputs.Repo = %+v, want the url and ref as built", in.Repo)
	}
	if in.Base != (types.ImageBuildRef{}) || len(in.Features) != 0 || len(in.Installers) != 0 || in.Generator != nil {
		t.Errorf("repo record carries generated-lane inputs: %+v", in)
	}
}

func TestResolveWorkspaceImage_RecordedRepoURLCarriesNoUserinfo(t *testing.T) {
	st := &recordingBuildStore{}
	b := &recordingBuilder{}
	srv, h := recServer(t, st, b)
	ws := recLanes()[2].ws
	ws.ID = uuid.New()
	ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://user:tok@git.example.com/x.git", Ref: "main"}}
	runID := uuid.New()
	srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)

	w := st.snapshot()
	if len(w) != 1 || w[0].rec.Inputs.Repo == nil || w[0].rec.Inputs.Repo.URL != "https://git.example.com/x.git" {
		t.Fatalf("writes = %+v, want the recorded URL without userinfo", w)
	}
	ev := waitForRecAudit(t, h.audit, runID, "run.build", "success")
	if strings.Contains(string(ev.Data), "tok") || strings.Contains(string(ev.Data), "user:") {
		t.Fatalf("the audit row carries the URL's userinfo: %s", ev.Data)
	}
}

func TestRunBuildAudit_SuccessCarriesTheRecordAndItsHash(t *testing.T) {
	for _, tc := range recLanes() {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingBuildStore{}
			srv, h := recServer(t, st, &recordingBuilder{baseDigest: recBaseDigest})
			ws := tc.ws
			ws.ID = uuid.New()
			runID := uuid.New()
			got, _ := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)

			data := recAuditData(t, waitForRecAudit(t, h.audit, runID, "run.build", "success"))
			written := st.snapshot()[0].rec
			wantSum, err := written.SHA256()
			if err != nil {
				t.Fatal(err)
			}
			if data["record_sha256"] != wantSum || data["image"] != got || data["image_id"] != recImageID ||
				data["reason"] != types.ImageBuildReasonCacheMiss || data["resolved"] != written.Resolved {
				t.Fatalf("run.build data = %v, want record_sha256 %s image %q image_id %s", data, wantSum, got, recImageID)
			}
			// The row's record is the record that was written: hashing it gives the row's hash.
			raw, _ := json.Marshal(data["record"])
			var inRow types.ImageBuild
			if err := json.Unmarshal(raw, &inRow); err != nil {
				t.Fatal(err)
			}
			if sum, _ := inRow.SHA256(); sum != wantSum {
				t.Fatalf("the row's record hashes to %s, its record_sha256 says %s", sum, wantSum)
			}
		})
	}
}

func TestRunBuildAudit_GeneratedCacheHitWritesARow(t *testing.T) {
	for _, tc := range recLanes() {
		t.Run(tc.name, func(t *testing.T) {
			srv, h := recServer(t, &recordingBuildStore{}, &idBuilder{id: recImageID, found: true})
			ws := tc.ws
			ws.ID, ws.ImageRef, ws.BuiltProfileHash = uuid.New(), "built/x", tc.key
			ws.ImageBuild = newImageBuild(tc.lane, tc.key, uuid.New(), types.ImageBuildReasonCacheMiss, types.ImageBuildInputs{},
				BuiltImage{Ref: "built/x", ID: recImageID}, time.Now())
			runID := uuid.New()
			srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)

			data := recAuditData(t, waitForRecAudit(t, h.audit, runID, "run.build", "success"))
			wantSum, _ := ws.ImageBuild.SHA256()
			if data["cache_hit"] != true || data["image"] != "built/x" || data["image_id"] != recImageID || data["record_sha256"] != wantSum {
				t.Fatalf("cache-hit row = %v, want cache_hit, image, image_id and the record's hash", data)
			}
			if _, has := data["record"]; has {
				t.Errorf("a cache hit's row repeats the whole record: %v", data)
			}
		})
	}
}

func TestRunBuildAudit_CacheHitWithoutACurrentRecordUsesTheLiveID(t *testing.T) {
	lane := recLanes()[0]
	srv, h := recServer(t, &recordingBuildStore{}, &idBuilder{id: "sha256:live", found: true})
	ws := lane.ws
	ws.ID, ws.ImageRef, ws.BuiltProfileHash = uuid.New(), "built/x", lane.key // no record: pre-0.9
	runID := uuid.New()
	srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil)
	data := recAuditData(t, waitForRecAudit(t, h.audit, runID, "run.build", "success"))
	if data["image_id"] != "sha256:live" || data["record_sha256"] != nil {
		t.Fatalf("row = %v, want the builder's live ID and no record hash", data)
	}
}

func TestGetWorkspaceBuild_RecordOnlyWhenCurrentAndRedactedBelowFull(t *testing.T) {
	const base = "registry.corp.internal/base:1"
	newServer := func(t *testing.T, mutate func(*types.Workspace)) (*Server, *r3TopologyStore, string) {
		srv, st := newTopologyWorkspaceServer(t, "")
		srv.cfg.ImageBuilder = &idBuilder{id: recImageID, found: true}
		key := byoiCacheKey("custom", base)
		st.ws.BaseImage = &types.WorkspaceBaseImage{Kind: "custom", Image: base}
		st.ws.ImageRef, st.ws.BuiltProfileHash = "built/byoi", key
		st.ws.ImageBuild = newImageBuild(types.ImageBuildLaneBYOI, key, uuid.New(), types.ImageBuildReasonCacheMiss,
			types.ImageBuildInputs{Base: types.ImageBuildRef{Ref: base, Digest: recBaseDigest}}, BuiltImage{Ref: "built/byoi", ID: recImageID}, time.Now())
		if mutate != nil {
			mutate(&st.ws)
		}
		return srv, st, st.ws.ID.String()
	}
	get := func(t *testing.T, srv *Server, id string, session *http.Cookie) (map[string]any, string) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/build", session, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET .../build = %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, w.Body.String()
	}
	ownerSession := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	memberSession := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleUser)

	srv, st, id := newServer(t, nil)
	full, fullBody := get(t, srv, id, ownerSession)
	member, memberBody := get(t, srv, id, memberSession)
	wantSum, _ := st.ws.ImageBuild.SHA256()
	if full["state"] != "done" || full["record_sha256"] != wantSum || full["reproducible"] != true {
		t.Fatalf("full tier = %v, want a done answer with the record's hash and reproducible", full)
	}
	if !strings.Contains(fullBody, base) || !strings.Contains(fullBody, recBaseDigest) {
		t.Errorf("the full tier lost the byoi base: %s", fullBody)
	}
	if member["record_sha256"] != wantSum {
		t.Errorf("member record_sha256 = %v, want the FULL record's hash %s", member["record_sha256"], wantSum)
	}
	if strings.Contains(memberBody, base) || strings.Contains(memberBody, recBaseDigest) {
		t.Errorf("a member saw the operator's authored base: %s", memberBody)
	}
	if rec, _ := member["record"].(map[string]any); rec == nil || rec["image"] == nil {
		t.Errorf("a member lost the record's image: %v", member)
	}

	// The POST early answer carries the record too.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+id+"/build", ownerSession, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), wantSum) {
		t.Errorf("POST .../build = %d without the record: %s", w.Code, w.Body.String())
	}

	// A record that is not current is not shown.
	srv, _, id = newServer(t, func(w *types.Workspace) { w.ImageBuild.CacheKey = "stale" })
	if out, _ := get(t, srv, id, ownerSession); out["record"] != nil || out["record_sha256"] != nil || out["reproducible"] != nil {
		t.Errorf("a non-current record was shown: %v", out)
	}
	srv, _, id = newServer(t, nil)
	srv.cfg.ImageBuilder = &idBuilder{id: "sha256:moved", found: true}
	if out, _ := get(t, srv, id, ownerSession); out["record"] != nil {
		t.Errorf("a record whose tag moved was shown: %v", out)
	}
	// A missing image leaves the record current.
	srv, st, id = newServer(t, nil)
	srv.cfg.ImageBuilder = &idBuilder{found: false}
	missingSum, _ := st.ws.ImageBuild.SHA256()
	if out, _ := get(t, srv, id, ownerSession); out["record_sha256"] != missingSum {
		t.Errorf("a record whose image is missing was hidden: %v", out)
	}
}

func TestRepoRecordURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok@host.example/x.git": "https://host.example/x.git",
		"https://tok@host.example/x.git":      "https://host.example/x.git",
		"https://host.example/x.git":          "https://host.example/x.git",
		"git@host.example:org/x.git":          "git@host.example:org/x.git",
	} {
		if got := repoRecordURL(in); got != want {
			t.Errorf("repoRecordURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoRecordURL_UnparseableURLFailsClosed(t *testing.T) {
	for _, tc := range []struct{ in, secret, want string }{
		{"https://user:p%zz@host/x.git", "p%zz", "https://host/x.git"},
		{"https://user:pa ss@host/x.git", "pa ss", "https://host/x.git"},
		{"https://x-token:abc^def@host/x.git", "abc^def", "https://host/x.git"},
		{"https://user:ab/cd@host/x.git", "user:ab", unparseableRepoURL},
		{"not a url with:secret@", "secret", unparseableRepoURL},
	} {
		got := repoRecordURL(tc.in)
		if got != tc.want || strings.Contains(got, tc.secret) || strings.Contains(got, "user:") {
			t.Errorf("repoRecordURL(%q) = %q, want %q with no part of the credential", tc.in, got, tc.want)
		}
	}
}

// TestGetWorkspaceBuild_ExplicitKindsShowTheirRecord: a registry or byo workspace whose row names a
// cached wrap of its base answers done with the record (read from the row alone, as after a restart),
// not "nothing_to_build"; with no wrap on the row it still boots as-is.
func TestGetWorkspaceBuild_ExplicitKindsShowTheirRecord(t *testing.T) {
	const base = "registry.corp.internal/base:1"
	for _, kind := range []string{"registry", "byo", "custom"} {
		t.Run(kind, func(t *testing.T) {
			srv, st := newTopologyWorkspaceServer(t, "")
			srv.cfg.ImageBuilder = &idBuilder{id: recImageID, found: true}
			key := byoiCacheKey(kind, base)
			st.ws.BaseImage = &types.WorkspaceBaseImage{Kind: kind, Image: base}
			st.ws.ImageRef, st.ws.BuiltProfileHash = "built/wrap", key
			st.ws.ImageBuild = newImageBuild(types.ImageBuildLaneBYOI, key, uuid.New(), types.ImageBuildReasonCacheMiss,
				types.ImageBuildInputs{Base: types.ImageBuildRef{Ref: base}}, BuiltImage{Ref: "built/wrap", ID: recImageID}, time.Now())
			wantSum, _ := st.ws.ImageBuild.SHA256()
			admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
			member := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleUser)
			id := st.ws.ID.String()

			for name, session := range map[string]*http.Cookie{"full": admin, "member": member} {
				w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/build", session, "")
				var out map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
					t.Fatalf("%s: GET .../build = %d: %s", name, w.Code, w.Body.String())
				}
				if out["state"] != "done" || out["record_sha256"] != wantSum || out["reproducible"] != false || out["image"] != "built/wrap" {
					t.Errorf("%s: view = %v, want done with the record, reproducible=false and the built image", name, out)
				}
				if name == "member" && strings.Contains(w.Body.String(), base) {
					t.Errorf("a member saw the authored base: %s", w.Body.String())
				}
			}

			st.ws.ImageRef, st.ws.BuiltProfileHash, st.ws.ImageBuild = "", "", nil
			w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/build", admin, "")
			wantState := "nothing_to_build"
			if kind == "custom" {
				wantState = "none"
			}
			if !strings.Contains(w.Body.String(), `"state":"`+wantState+`"`) {
				t.Errorf("with nothing built the view = %s, want state %s", w.Body.String(), wantState)
			}
		})
	}
}
