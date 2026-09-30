// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// buildEventLog is one ordered record shared by the store and the builder, so a
// test can assert not just THAT the "image: Building" line was written but that
// it landed BEFORE the builder call it announces.
type buildEventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *buildEventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, e)
}

func (l *buildEventLog) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

// buildEventStore records status_detail writes into the shared log. Every other
// store method panics (embedded nil interface) except the three the image
// resolvers touch.
type buildEventStore struct {
	store.Store
	log *buildEventLog
}

func (s *buildEventStore) SetRunStatusDetail(_ context.Context, _ uuid.UUID, detail string) error {
	s.log.add("detail:" + detail)
	return nil
}
func (s *buildEventStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }
func (s *buildEventStore) SetWorkspaceBuiltImage(context.Context, uuid.UUID, string, string) (types.Workspace, error) {
	return types.Workspace{}, nil
}

// buildEventBuilder records which builder method ran into the same log.
type buildEventBuilder struct{ log *buildEventLog }

func (b buildEventBuilder) FinalizeBase(_ context.Context, _, tag string, _ io.Writer) (string, error) {
	b.log.add("build:FinalizeBase")
	return tag, nil
}
func (b buildEventBuilder) BuildDevcontainer(_ context.Context, _, _, tag string, _ io.Writer) (string, error) {
	b.log.add("build:BuildDevcontainer")
	return tag, nil
}
func (b buildEventBuilder) BuildFromDevcontainerFiles(_ context.Context, _ map[string]string, tag string, _ io.Writer) (string, error) {
	b.log.add("build:BuildFromDevcontainerFiles")
	return tag, nil
}

const buildingLine = "detail:" + statusDetailBuilding

func buildAnnounceServer(t *testing.T) (*Server, *buildEventLog) {
	t.Helper()
	log := &buildEventLog{}
	cfg := baseTestConfig(newHarness(t), &buildEventStore{log: log})
	cfg.ImageBuilder = buildEventBuilder{log: log}
	return New(cfg), log
}

// TestBuildAnnounce_EveryBuildSiteWritesOnMissAndNothingOnHit pins the rule for
// every place an image build can start: the Building line is written
// IMMEDIATELY BEFORE the builder call, and never on a cache hit (a hit builds
// nothing, so the line would be a lie). It runs through resolveCreateRunImage,
// the door the two request-level builds and the workspace path share.
func TestBuildAnnounce_EveryBuildSiteWritesOnMissAndNothingOnHit(t *testing.T) {
	devProfile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}
	genProfile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	repoSrc := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}}
	baseImg := &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}
	const cached = "wardyn-workspace/cached:abc"

	for _, tc := range []struct {
		name string
		req  createRunRequest
		ws   *types.Workspace
		want []string
	}{
		{name: "BYOI wrap", req: createRunRequest{Agent: "claude-code", Image: "ubuntu:24.04"},
			want: []string{buildingLine, "build:FinalizeBase"}},
		{name: "request devcontainer", req: createRunRequest{Agent: "claude-code", DevcontainerRepo: "acme/widgets"},
			want: []string{buildingLine, "build:BuildDevcontainer"}},
		{name: "workspace base image, miss",
			ws:   &types.Workspace{BaseImage: baseImg},
			want: []string{buildingLine, "build:FinalizeBase"}},
		{name: "workspace base image, hit",
			ws:   &types.Workspace{BaseImage: baseImg, ImageRef: cached, BuiltProfileHash: byoiCacheKey(baseImg.Kind, baseImg.Image)},
			want: nil},
		{name: "workspace repo devcontainer, miss",
			ws:   &types.Workspace{Sources: repoSrc, Profile: mustJSON(devProfile)},
			want: []string{buildingLine, "build:BuildDevcontainer"}},
		{name: "workspace repo devcontainer, hit",
			ws: &types.Workspace{Sources: repoSrc, Profile: mustJSON(devProfile), ImageRef: cached,
				BuiltProfileHash: repoDevcontainerCacheKey(repoCloneURL(repoSrc[0].Source), repoSrc[0].Ref)},
			want: nil},
		{name: "workspace generated devcontainer, miss",
			ws:   &types.Workspace{Profile: mustJSON(genProfile)},
			want: []string{buildingLine, "build:BuildFromDevcontainerFiles"}},
		{name: "workspace generated devcontainer, hit",
			ws:   &types.Workspace{Profile: mustJSON(genProfile), ImageRef: cached, BuiltProfileHash: genProfile.CacheKey()},
			want: nil},
		{name: "workspace with no profile: convention image, no build",
			ws:   &types.Workspace{},
			want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, log := buildAnnounceServer(t)
			var wsRefs []types.Workspace
			if tc.ws != nil {
				tc.ws.ID = uuid.New()
				wsRefs = []types.Workspace{*tc.ws}
			}
			if _, failed := srv.resolveCreateRunImage(context.Background(), tc.req, uuid.New(), wsRefs); failed {
				t.Fatal("resolveCreateRunImage reported a failed build")
			}
			if got := log.events(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildAnnounce_RecordVerifyPathWrites: the record/verify launch reaches the
// same resolver through workspaceRunImage with its own run id, and passes the
// announce too.
func TestBuildAnnounce_RecordVerifyPathWrites(t *testing.T) {
	srv, log := buildAnnounceServer(t)
	ws := types.Workspace{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}}
	srv.workspaceRunImage(context.Background(), uuid.New(), ws)
	want := []string{buildingLine, "build:FinalizeBase"}
	if got := log.events(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// TestBuildAnnounce_NilCallbackNeverWrites: the workspace Build step passes a
// BUILD id, not a run id, so its resolver call carries no announce. A build
// still happens; nothing is written.
func TestBuildAnnounce_NilCallbackNeverWrites(t *testing.T) {
	srv, log := buildAnnounceServer(t)
	ws := types.Workspace{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"}}
	if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil); !ok {
		t.Fatal("resolveWorkspaceImage failed")
	}
	want := []string{"build:FinalizeBase"}
	if got := log.events(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// TestBuildAnnounce_WriteIsBounded: a store whose write never returns costs the
// build at most statusDetailWriteTimeout, not the 30-minute build budget.
func TestBuildAnnounce_WriteIsBounded(t *testing.T) {
	rn := &fakeRunner{}
	srv, st, _ := statusDetailDispatchFixture(t, rn)
	st.block = make(chan struct{}) // never closed: the write parks until its ctx dies

	start := time.Now()
	srv.announceImageBuild(context.Background(), uuid.New())()
	if elapsed := time.Since(start); elapsed > 4*statusDetailWriteTimeout {
		t.Fatalf("announce blocked %v on a parked write, want ~%v", elapsed, statusDetailWriteTimeout)
	}
	if got := st.written(); len(got) != 1 || got[0] != statusDetailBuilding {
		t.Fatalf("writes = %v, want one %q", got, statusDetailBuilding)
	}
}

// TestBuildAnnounce_NotATimedStartWait pins that the Building line is NOT a
// substrate wait: a Building announce followed by CreateSandbox's own OnWaiting
// records the substrate's reason and nothing else in wardyn_run_start_wait_seconds
// — no "other" stretch and no Building stretch. It goes red if the build write is
// ever routed through runStatusDetailWriter, which would time a 30-minute build
// into the substrate series as "other".
func TestBuildAnnounce_NotATimedStartWait(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{"agent: ContainerCreating"}}
	srv, st, run := statusDetailDispatchFixture(t, rn)

	srv.announceImageBuild(context.Background(), run.ID)()
	dispatchOnce(srv, run)

	want := []string{statusDetailBuilding, "agent: ContainerCreating"}
	if got := st.written(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	srv.metrics.mu.Lock()
	defer srv.metrics.mu.Unlock()
	if n := srv.metrics.startWaitCount[startWaitReasonOther]; n != 0 {
		t.Errorf("start-wait %q count = %d, want 0: the build line was timed as a substrate wait", startWaitReasonOther, n)
	}
	if n := srv.metrics.startWaitCount[statusReasonBuilding]; n != 0 {
		t.Errorf("start-wait %q count = %d, want 0", statusReasonBuilding, n)
	}
	if n := srv.metrics.startWaitCount["ContainerCreating"]; n != 1 {
		t.Errorf("start-wait ContainerCreating count = %d, want 1", n)
	}
}

// TestProjectStatusDetail_BuildingIsPendingOnly: the build happens before
// dispatch, so the line is true only while the run is PENDING. STARTING blanks
// it (a warm Docker start never overwrites it, and a finished build must not
// narrate the sandbox start); FAILED already blanks a non-terminal reason.
func TestProjectStatusDetail_BuildingIsPendingOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      types.RunState
		detail     string
		wantDetail string
		wantReason string
	}{
		{"PENDING keeps Building", types.RunPending, statusDetailBuilding, statusDetailBuilding, statusReasonBuilding},
		{"STARTING blanks Building", types.RunStarting, statusDetailBuilding, "", ""},
		{"FAILED blanks Building", types.RunFailed, statusDetailBuilding, "", ""},
		{"RUNNING blanks Building", types.RunRunning, statusDetailBuilding, "", ""},
		{"PENDING still blanks any other reason", types.RunPending, "agent: ContainerCreating", "", ""},
		{"STARTING keeps a substrate reason", types.RunStarting, "agent: ContainerCreating", "agent: ContainerCreating", "ContainerCreating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := []types.AgentRun{{ID: uuid.New(), State: tc.state, StatusDetail: tc.detail}}
			projectStatusDetail(runs)
			if runs[0].StatusDetail != tc.wantDetail || runs[0].StatusReason != tc.wantReason {
				t.Errorf("(detail, reason) = (%q, %q), want (%q, %q)",
					runs[0].StatusDetail, runs[0].StatusReason, tc.wantDetail, tc.wantReason)
			}
		})
	}
}
