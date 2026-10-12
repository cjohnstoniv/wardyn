// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

const (
	pinBaseRef     = "mcr.microsoft.com/devcontainers/base:ubuntu"
	pinBaseRepo    = "mcr.microsoft.com/devcontainers/base"
	pinGoRef       = "ghcr.io/devcontainers/features/go:1"
	pinGoRepo      = "ghcr.io/devcontainers/features/go"
	pinBaseDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	pinGoDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	pinNewDigest   = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	pinInstallSum  = "3333333333333333333333333333333333333333333333333333333333333333"
	pinFileContent = "2.1.287 linux-x64 " + pinInstallSum + "\n"
)

const (
	genDockerfileKey   = ".devcontainer/Dockerfile"
	genDevcontainerKey = ".devcontainer/devcontainer.json"
)

// pinningBuilder is a builder that can resolve refs (ImageIdentifier) and whose generated build
// reports a pin file. digests answers by the ref exactly as asked; a ref with no entry is not found.
type pinningBuilder struct {
	recordingBuilder
	pmu     sync.Mutex
	digests map[string]string
	errs    map[string]error
	asked   []string
	pinFile []byte
	found   bool // LocalImage answer: the host still has the image
}

func (b *pinningBuilder) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, tag string, w io.Writer) (BuiltImage, error) {
	built, err := b.recordingBuilder.BuildFromDevcontainerFiles(ctx, files, tag, w)
	built.PinFile = b.pinFile
	return built, err
}

func (b *pinningBuilder) LocalImage(context.Context, string) (string, bool, error) {
	return recImageID, b.found, nil
}

func (b *pinningBuilder) RemoteDigest(_ context.Context, ref string) (string, error) {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	b.asked = append(b.asked, ref)
	if err, ok := b.errs[ref]; ok {
		return "", err
	}
	if d, ok := b.digests[ref]; ok {
		return d, nil
	}
	return "", fmt.Errorf("%w: %s", ErrRemoteDigestNotFound, ref)
}

func (b *pinningBuilder) askedRefs() []string {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	return slices.Clone(b.asked)
}

// resolvingBuilder resolves the two refs the Go golden profile uses, to pinBaseDigest and pinGoDigest.
func resolvingBuilder() *pinningBuilder {
	return &pinningBuilder{
		digests: map[string]string{pinBaseRef: pinBaseDigest, pinGoRef: pinGoDigest},
		pinFile: []byte(pinFileContent),
	}
}

func pinGeneratedWorkspace() (types.Workspace, string) {
	lane := recLanes()[0]
	ws := lane.ws
	ws.ID = uuid.New()
	return ws, lane.key
}

func TestResolveWorkspaceImage_ResolvingBuildPinsAndRecords(t *testing.T) {
	b := resolvingBuilder()
	st := &recordingBuildStore{}
	srv, _ := recServer(t, st, b)
	ws, _ := pinGeneratedWorkspace()

	if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil); !ok {
		t.Fatal("the build failed")
	}
	if got := b.askedRefs(); !slices.Equal(got, []string{pinBaseRef, pinGoRef}) {
		t.Errorf("resolved %v, want the base and the one feature, by their tag form", got)
	}
	if !strings.Contains(b.files[genDockerfileKey], "FROM "+pinBaseRef+"@"+pinBaseDigest+"\n") {
		t.Errorf("the Dockerfile does not build FROM the digest:\n%s", b.files[genDockerfileKey])
	}
	if !strings.Contains(b.files[genDevcontainerKey], `"`+pinGoRef+"@"+pinGoDigest+`"`) {
		t.Errorf("devcontainer.json does not key the feature by digest:\n%s", b.files[genDevcontainerKey])
	}
	rec := st.snapshot()[0].rec
	if rec.Inputs.Base.Digest != pinBaseDigest || len(rec.Inputs.Features) != 1 || rec.Inputs.Features[0].Digest != pinGoDigest {
		t.Errorf("recorded inputs = %+v, want the digests the build used", rec.Inputs)
	}
	if i := rec.Inputs.Installers[0]; i.Version != "2.1.287" || i.Platform != "linux-x64" || i.SHA256 != pinInstallSum {
		t.Errorf("recorded installer = %+v, want the pin file's", i)
	}
	if !rec.Resolved {
		t.Error("a build whose base, feature and installer are pinned must record resolved: true")
	}
}

func TestResolveWorkspaceImage_UnresolvableBuildsUnpinned(t *testing.T) {
	unreachable := errors.New("dial tcp: i/o timeout")
	for name, b := range map[string]ImageBuilder{
		"every lookup fails":   &pinningBuilder{errs: map[string]error{pinBaseRef: unreachable, pinGoRef: unreachable}, pinFile: []byte(pinFileContent)},
		"no digest capability": &recordingBuilder{},
		"an unknown manifest":  &pinningBuilder{pinFile: []byte(pinFileContent)},
		"a malformed digest":   &pinningBuilder{digests: map[string]string{pinBaseRef: "sha256:short", pinGoRef: "latest"}, pinFile: []byte(pinFileContent)},
	} {
		t.Run(name, func(t *testing.T) {
			st := &recordingBuildStore{}
			srv, h := recServer(t, st, b)
			ws, _ := pinGeneratedWorkspace()
			runID := uuid.New()

			if _, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil); !ok {
				t.Fatal("a build that could not be pinned must still succeed")
			}
			waitForRecAudit(t, h.audit, runID, "run.build", "success")
			var files map[string]string
			switch bb := b.(type) {
			case *pinningBuilder:
				files = bb.files
			case *recordingBuilder:
				files = bb.files
			}
			if strings.Contains(files[genDockerfileKey], "@sha256") || strings.Contains(files[genDevcontainerKey], "@") {
				t.Errorf("an input that did not resolve was pinned anyway:\n%s\n%s", files[genDockerfileKey], files[genDevcontainerKey])
			}
			rec := st.snapshot()[0].rec
			if rec.Inputs.Base.Digest != "" || rec.Inputs.Features[0].Digest != "" || rec.Resolved {
				t.Errorf("record = %+v, want empty digests and resolved: false", rec)
			}
		})
	}
}

// pinnedRecord is the record of an earlier pinned build of the Go golden workspace, and the
// workspace row that names its image.
func pinnedRecord(t *testing.T, mutate func(*types.ImageBuildInputs)) types.Workspace {
	t.Helper()
	ws, key := pinGeneratedWorkspace()
	p, _ := workspaceProfile(ws)
	in, err := workspacescan.GeneratedInputs(p, workspacescan.Pins{
		BaseDigest: pinBaseDigest,
		Features:   map[string]string{pinGoRef: pinGoDigest},
		Installers: map[string]workspacescan.InstallerPin{"claude-code": {Version: "2.1.287", Platform: "linux-x64", SHA256: pinInstallSum}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&in)
	}
	ws.ImageRef, ws.BuiltProfileHash = "built/gone", key
	ws.ImageBuild = newImageBuild(types.ImageBuildLaneGenerated, key, uuid.New(), types.ImageBuildReasonCacheMiss, in,
		BuiltImage{Ref: "built/gone", ID: recImageID}, time.Now())
	return ws
}

// missingImageServer is a server whose host has lost every image (the cache hit misses) and whose
// registry has moved on: the tag forms now answer pinNewDigest. The digest forms answer what they hold.
func missingImageServer(t *testing.T, st store.Store, b *pinningBuilder) (*Server, *harness) {
	t.Helper()
	srv, h := recServer(t, st, b)
	srv.cfg.Runner = absentImageRunner{}
	return srv, h
}

func TestResolveWorkspaceImage_MissingImageRebuildsFromTheRecord(t *testing.T) {
	b := &pinningBuilder{
		digests: map[string]string{
			pinBaseRef: pinNewDigest, pinGoRef: pinNewDigest, // what a fresh resolution would now pin
			pinBaseRepo + "@" + pinBaseDigest: pinBaseDigest, pinGoRepo + "@" + pinGoDigest: pinGoDigest,
		},
		pinFile: []byte("9.9.9 linux-x64 " + strings.Repeat("9", 64) + "\n"),
	}
	st := &recordingBuildStore{}
	srv, _ := missingImageServer(t, st, b)
	ws := pinnedRecord(t, nil)
	previous := ws.ImageBuild.Inputs

	if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil); !ok {
		t.Fatal("the rebuild failed")
	}
	for _, ref := range b.askedRefs() {
		if ref == pinBaseRef || ref == pinGoRef {
			t.Errorf("the resolver was asked for %q: a pinned input is never resolved again", ref)
		}
	}
	df := b.files[genDockerfileKey]
	if !strings.Contains(df, "FROM "+pinBaseRef+"@"+pinBaseDigest+"\n") || !strings.Contains(df, "ver=2.1.287; sum="+pinInstallSum) ||
		!strings.Contains(b.files[genDevcontainerKey], pinGoRef+"@"+pinGoDigest) {
		t.Errorf("the rebuild did not use the record's pins:\n%s\n%s", df, b.files[genDevcontainerKey])
	}
	rec := st.snapshot()[0].rec
	if rec.Reason != types.ImageBuildReasonImageMissing {
		t.Errorf("reason = %q, want image_missing", rec.Reason)
	}
	if rec.Inputs.Base != previous.Base || rec.Inputs.Features[0].Digest != previous.Features[0].Digest || rec.Inputs.Installers[0] != previous.Installers[0] {
		t.Errorf("rebuilt inputs = %+v, want the previous %+v", rec.Inputs, previous)
	}
	if !rec.Resolved {
		t.Error("a rebuild from a fully pinned record stays reproducible")
	}
}

func TestResolveWorkspaceImage_RebuildLeavesAnUnpinnedInputFloating(t *testing.T) {
	b := &pinningBuilder{
		digests: map[string]string{pinBaseRepo + "@" + pinBaseDigest: pinBaseDigest, pinGoRef: pinNewDigest},
		pinFile: []byte(pinFileContent),
	}
	st := &recordingBuildStore{}
	srv, _ := missingImageServer(t, st, b)
	ws := pinnedRecord(t, func(in *types.ImageBuildInputs) {
		in.Features[0].Digest = ""
		in.Installers[0] = types.ImageBuildInstaller{Tool: "claude-code", Channel: "stable"}
	})
	if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil, nil); !ok {
		t.Fatal("the rebuild failed")
	}
	rec := st.snapshot()[0].rec
	if rec.Inputs.Features[0].Digest != "" || rec.Inputs.Installers[0].Version != "" || rec.Resolved {
		t.Errorf("inputs = %+v resolved=%v: an input the record left unpinned stays unpinned and the build not reproducible", rec.Inputs, rec.Resolved)
	}
	if strings.Contains(b.files[genDevcontainerKey], "@") || !strings.Contains(b.files[genDockerfileKey], "/stable") {
		t.Errorf("the unpinned feature and installer must float:\n%s\n%s", b.files[genDockerfileKey], b.files[genDevcontainerKey])
	}
	if !slices.Equal(b.askedRefs(), []string{pinBaseRepo + "@" + pinBaseDigest}) {
		t.Errorf("asked %v, want only the pinned base's pre-check", b.askedRefs())
	}
}

func TestResolveWorkspaceImage_PinnedInputGoneWritesADifference(t *testing.T) {
	b := &pinningBuilder{
		// The base is still there; the feature's pinned digest is not. Fresh resolution WOULD succeed.
		digests: map[string]string{pinBaseRepo + "@" + pinBaseDigest: pinBaseDigest, pinBaseRef: pinNewDigest, pinGoRef: pinNewDigest},
		pinFile: []byte(pinFileContent),
	}
	st := &recordingBuildStore{}
	srv, h := missingImageServer(t, st, b)
	ws := pinnedRecord(t, nil)
	runID := uuid.New()

	if _, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil, nil); ok {
		t.Fatal("a rebuild whose pinned input is gone must not build")
	}
	if b.calls != 0 || len(st.snapshot()) != 0 {
		t.Errorf("builder called %d times, %d writes: nothing may be built or recorded", b.calls, len(st.snapshot()))
	}
	data := recAuditData(t, waitForRecAudit(t, h.audit, runID, "run.build", "failure"))
	raw, _ := json.Marshal(data["difference"])
	var diff []buildDifference
	_ = json.Unmarshal(raw, &diff)
	want := []buildDifference{{Input: "feature:" + pinGoRef, Want: pinGoDigest, Got: "not_found"}}
	if !slices.Equal(diff, want) || data["source"] != "generated-devcontainer" || data["error"] != pinnedInputGoneError {
		t.Fatalf("failure row = %v, want difference %+v", data, want)
	}
}

func TestResolveWorkspaceImage_UnreachableRegistryDoesNotBlockARebuild(t *testing.T) {
	down := errors.New("dial tcp: no route to host")
	b := &pinningBuilder{errs: map[string]error{pinBaseRepo + "@" + pinBaseDigest: down, pinGoRepo + "@" + pinGoDigest: down}, pinFile: []byte(pinFileContent)}
	st := &recordingBuildStore{}
	srv, _ := missingImageServer(t, st, b)
	if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), pinnedRecord(t, nil), nil, nil); !ok || b.calls != 1 {
		t.Fatalf("ok=%v, builder calls=%d: an inconclusive pre-check builds pinned", ok, b.calls)
	}
}

// diffBuildStore serves the /build handlers one workspace and the audit rows the harness recorded.
type diffBuildStore struct {
	b4BuildStore
	audit *recRecorder
}

func (s *diffBuildStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	for _, ev := range s.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func TestWorkspaceBuild_FailedRebuildDetailNamesTheInput(t *testing.T) {
	h := newHarness(t)
	ws := pinnedRecord(t, nil)
	ws.Name, ws.Status = "w", types.WorkspaceScanned
	ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/payments"}}
	st := &diffBuildStore{b4BuildStore: b4BuildStore{ws: ws}, audit: h.audit}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = &pinningBuilder{digests: map[string]string{pinBaseRepo + "@" + pinBaseDigest: pinBaseDigest}}
	cfg.Runner = absentImageRunner{}
	srv := New(cfg)
	path := "/api/v1/workspaces/" + ws.ID.String() + "/build"

	// An earlier failed attempt on this composition leaves the tracker failed, which is what lets Build run again on a row
	// whose image is recorded: the row alone reads done.
	key, _ := srv.workspaceBuiltImageKey(ws)
	srv.builds.begin(ws.ID, time.Now())
	srv.builds.bindKey(ws.ID, key)
	srv.builds.finish(ws.ID, "", "an earlier failure")

	if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusAccepted {
		t.Fatalf("POST /build = %d: %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(5 * time.Second); srv.builds.get(ws.ID).Building; {
		if time.Now().After(deadline) {
			t.Fatal("the build never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w := do(t, srv, http.MethodGet, path, adminToken, "")
	var got buildResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET /build = %d: %s", w.Code, w.Body.String())
	}
	want := pinnedInputGoneError + ": feature:" + pinGoRef + " (want " + pinGoDigest + ", got not_found)"
	if got.State != "failed" || got.Detail != want {
		t.Fatalf("view = %+v, want failed with detail %q", got, want)
	}
}
