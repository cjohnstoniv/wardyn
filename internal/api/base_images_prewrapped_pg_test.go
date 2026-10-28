// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Design final §3 against a real Postgres: who registers a prewrapped image, its async check and
// "Check again", attach-only workspace writes, and both run doors. Guarded by WARDYN_TEST_PG.

const prewrapRef = "ghcr.io/acme/sdk-agent:2.3@sha256:4f1c2a7e0d9b8c6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b9be0"

// prewrapRunner is a fake runner whose substrate checks prewrapped images: each check waits for one
// token on gate, then answers verdicts.
type prewrapRunner struct {
	*fakeRunner
	unavailable string
	verdicts    []runner.ImageArchVerdict
	gate        chan struct{}
	mu          sync.Mutex
	attempts    []uuid.UUID
}

func (p *prewrapRunner) PrewrappedCheckUnavailable() string { return p.unavailable }

func (p *prewrapRunner) CheckPrewrappedImage(_ context.Context, attempt uuid.UUID, _ string) []runner.ImageArchVerdict {
	p.mu.Lock()
	p.attempts = append(p.attempts, attempt)
	p.mu.Unlock()
	<-p.gate
	return p.verdicts
}

func (p *prewrapRunner) checks() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.attempts)
}

type prewrapEnv struct {
	srv    *Server
	st     store.PG
	rn     *prewrapRunner
	secSes *http.Cookie
}

func newPrewrapEnv(t *testing.T) prewrapEnv {
	t.Helper()
	st := store.PG{Pool: throwawayPGPool(t)}
	rn := &prewrapRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{}),
		verdicts: []runner.ImageArchVerdict{{Arch: "amd64", Code: runner.ImageCheckPassed}, {Arch: "arm64", Code: runner.ImageCheckPlatformMissing}}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
	cfg.Runner = rn
	srv := New(cfg)
	t.Cleanup(func() {
		close(rn.gate) // release any check a test left waiting
		srv.WaitBackground()
	})
	return prewrapEnv{srv: srv, st: st, rn: rn,
		secSes: accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})}
}

func (e prewrapEnv) asSec(t *testing.T, method, path, body string) (int, string, types.BaseImageEntry) {
	t.Helper()
	w := doSSO(t, e.srv, method, path, e.secSes, body)
	var b types.BaseImageEntry
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return w.Code, errorReason(w), b
}

// settled waits for id's check to leave checking.
func (e prewrapEnv) settled(t *testing.T, id uuid.UUID) types.BaseImageEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := e.st.GetBaseImage(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Check != nil && b.Check.Status != types.BaseImageCheckChecking {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("check of %s never settled: %+v", id, b.Check)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func prewrapBody(ref string) string {
	return `{"kind":"prewrapped","name":"sdk","image":"` + ref + `"}`
}

func TestPG_PrewrappedRegistrationAndRoles(t *testing.T) {
	e := newPrewrapEnv(t)
	ctx := context.Background()
	reg, err := e.st.UpsertBaseImage(ctx, types.BaseImageEntry{ID: uuid.New(), Kind: "registry", Name: "corp", Image: "registry.corp.example/base:1",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	// The security tier writes prewrapped only.
	if code, _, _ := e.asSec(t, http.MethodPost, "/api/v1/base-images", `{"kind":"registry","image":"ghcr.io/acme/x:1"}`); code != http.StatusForbidden {
		t.Fatalf("security admin POST registry = %d, want 403", code)
	}
	if code, reason, _ := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody("ghcr.io/acme/sdk:2.3")); code != http.StatusBadRequest || reason != reasonPrewrappedImageNotPinned {
		t.Fatalf("unpinned prewrapped = %d %s, want 400 %s", code, reason, reasonPrewrappedImageNotPinned)
	}
	// A status, architectures or attempt in the body is never taken: the strict decoder refuses it.
	if code, _, _ := e.asSec(t, http.MethodPost, "/api/v1/base-images",
		`{"kind":"prewrapped","image":"`+prewrapRef+`","check":{"status":"ready","accepted_arches":["amd64"]}}`); code != http.StatusBadRequest {
		t.Fatalf("spoofed check = %d, want 400", code)
	}
	e.rn.unavailable = "not proven"
	if code, reason, _ := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef)); code != http.StatusUnprocessableEntity || reason != reasonPrewrappedCheckUnavailable {
		t.Fatalf("no checker = %d %s, want 422 %s", code, reason, reasonPrewrappedCheckUnavailable)
	}
	e.rn.unavailable = ""

	code, _, b := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef))
	if code != http.StatusCreated || b.Check == nil || b.Check.Status != types.BaseImageCheckChecking {
		t.Fatalf("register = %d %+v, want 201 checking", code, b.Check)
	}
	// Re-registering the same image is an identity hit, and starts no second check.
	if code, _, again := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef)); code != http.StatusOK || again.ID != b.ID {
		t.Fatalf("re-register = %d %s, want 200 on %s", code, again.ID, b.ID)
	}
	e.rn.gate <- struct{}{}
	done := e.settled(t, b.ID)
	if done.Check.Status != types.BaseImageCheckReady || !slices.Equal(done.Check.AcceptedArches, []string{"amd64"}) || done.Check.Reason != "platform_missing:arm64" {
		t.Fatalf("verdict = %+v, want ready on amd64 with arm64 left out", done.Check)
	}
	if n := e.rn.checks(); n != 1 {
		t.Fatalf("%d checks ran, want 1", n)
	}
	if code, reason, _ := e.asSec(t, http.MethodPost, "/api/v1/base-images/"+b.ID.String()+"/check", ""); code != http.StatusConflict || reason != reasonPrewrappedAlreadyReady {
		t.Fatalf("check again on ready = %d %s, want 409 %s", code, reason, reasonPrewrappedAlreadyReady)
	}

	// The security tier lists prewrapped rows only; an admin lists both.
	w := doSSO(t, e.srv, http.MethodGet, "/api/v1/base-images", e.secSes, "")
	if strings.Contains(w.Body.String(), reg.Image) || !strings.Contains(w.Body.String(), prewrapRef) {
		t.Fatalf("security admin list = %s, want the prewrapped row only", w.Body.String())
	}
	if w := do(t, e.srv, http.MethodGet, "/api/v1/base-images", adminToken, ""); !strings.Contains(w.Body.String(), reg.Image) {
		t.Fatalf("admin list = %s, want the registry row too", w.Body.String())
	}

	// DELETE authorizes the stored kind, and force stays an admin's.
	if code, _, _ := e.asSec(t, http.MethodDelete, "/api/v1/base-images/"+reg.ID.String(), ""); code != http.StatusForbidden {
		t.Fatalf("security admin DELETE registry = %d, want 403", code)
	}
	if code, _, _ := e.asSec(t, http.MethodDelete, "/api/v1/base-images/"+b.ID.String()+"?force=1", ""); code != http.StatusForbidden {
		t.Fatalf("security admin forced DELETE = %d, want 403", code)
	}
	if code, _, _ := e.asSec(t, http.MethodDelete, "/api/v1/base-images/"+b.ID.String(), ""); code != http.StatusNoContent {
		t.Fatalf("security admin DELETE prewrapped = %d, want 204", code)
	}
}

func TestPG_PrewrappedCheckAgainConcurrent(t *testing.T) {
	e := newPrewrapEnv(t)
	e.rn.verdicts = []runner.ImageArchVerdict{{Arch: "amd64", Code: runner.ImageCheckContractMissing, Detail: "wardyn-rec"}, {Arch: "arm64", Code: runner.ImageCheckNoNode}}
	_, _, b := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef))
	check := "/api/v1/base-images/" + b.ID.String() + "/check"
	if code, reason, _ := e.asSec(t, http.MethodPost, check, ""); code != http.StatusConflict || reason != reasonPrewrappedCheckInProgress {
		t.Fatalf("check again over a live attempt = %d %s, want 409 %s", code, reason, reasonPrewrappedCheckInProgress)
	}
	e.rn.gate <- struct{}{}
	if got := e.settled(t, b.ID); got.Check.Status != types.BaseImageCheckRefused || got.Check.Reason != "contract_missing:amd64:wardyn-rec" {
		t.Fatalf("verdict = %+v, want refused for the missing tool", got.Check)
	}

	// Two concurrent retries start one attempt.
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = e.asSec(t, http.MethodPost, check, "")
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusAccepted, http.StatusConflict}) {
		t.Fatalf("concurrent retries = %v, want one 202 and one 409", codes)
	}

	// An interrupted attempt reads as refused, and Check again claims it; the old attempt's late verdict
	// publishes nothing.
	if _, err := e.st.Pool.Exec(context.Background(), `UPDATE base_images SET check_deadline = clock_timestamp() - interval '1 second' WHERE id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	w := do(t, e.srv, http.MethodGet, "/api/v1/base-images", adminToken, "")
	if !strings.Contains(w.Body.String(), `"reason":"check_interrupted"`) {
		t.Fatalf("expired attempt reads %s, want check_interrupted", w.Body.String())
	}
	if code, _, _ := e.asSec(t, http.MethodPost, check, ""); code != http.StatusAccepted {
		t.Fatalf("check again on an interrupted row = %d, want 202", code)
	}
	for deadline := time.Now().Add(10 * time.Second); e.rn.checks() < 3 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.rn.checks(); n != 3 {
		t.Fatalf("%d checks started, want 3 (register, one retry, the retry after the interruption)", n)
	}
	cur, _ := e.st.GetBaseImage(context.Background(), b.ID)
	e.rn.verdicts = []runner.ImageArchVerdict{{Arch: "amd64", Code: runner.ImageCheckPassed}, {Arch: "arm64", Code: runner.ImageCheckPassed}}
	e.rn.gate <- struct{}{}
	e.rn.gate <- struct{}{}
	got := e.settled(t, b.ID)
	if got.Check.Attempt != cur.Check.Attempt || got.Check.Status != types.BaseImageCheckReady {
		t.Fatalf("published %+v (attempt %s), want ready from the current attempt %s", got.Check, got.Check.Attempt, cur.Check.Attempt)
	}
}

func TestPG_PrewrappedWorkspaceAttachOnly(t *testing.T) {
	e := newPrewrapEnv(t)
	ctx := context.Background()
	rows := func() int {
		list, err := e.st.ListBaseImages(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	ws := func(name, ref string) *httptest.ResponseRecorder {
		return do(t, e.srv, http.MethodPost, "/api/v1/workspaces", adminToken,
			`{"name":"`+name+`","base_image":{"kind":"prewrapped","image":"`+ref+`"}}`)
	}
	if w := ws("unknown", prewrapRef); w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonPrewrappedImageNotRegistered || rows() != 0 {
		t.Fatalf("unregistered = %d %s with %d rows, want 422 %s and no row", w.Code, errorReason(w), rows(), reasonPrewrappedImageNotRegistered)
	}
	_, _, b := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef))
	if w := ws("checking", prewrapRef); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not yet ready = %d, want 422", w.Code)
	}
	e.rn.gate <- struct{}{}
	e.settled(t, b.ID)
	w := ws("ready", prewrapRef)
	if w.Code != http.StatusCreated {
		t.Fatalf("ready = %d %s, want 201", w.Code, w.Body.String())
	}
	var created types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetWorkspace(ctx, created.ID)
	if err != nil || got.BaseImageID == nil || *got.BaseImageID != b.ID || rows() != 1 {
		t.Fatalf("workspace base = %v (%v) with %d rows, want the registered row %s", got.BaseImageID, err, rows(), b.ID)
	}
	// The console's pinned-image field sends byo: the exact ref of a ready prewrapped image attaches it.
	w = do(t, e.srv, http.MethodPost, "/api/v1/workspaces", adminToken, `{"name":"typed","base_image":{"kind":"byo","image":"`+prewrapRef+`"}}`)
	if err := json.Unmarshal(w.Body.Bytes(), &created); w.Code != http.StatusCreated || err != nil {
		t.Fatalf("typed ref = %d %s", w.Code, w.Body.String())
	}
	if got, err = e.st.GetWorkspace(ctx, created.ID); err != nil || got.BaseImageID == nil || *got.BaseImageID != b.ID || rows() != 1 {
		t.Fatalf("typed ref base = %v (%v) with %d rows, want the prewrapped row %s and no byo row", got.BaseImageID, err, rows(), b.ID)
	}
}

func TestPG_PrewrappedRunDoors(t *testing.T) {
	e := newPrewrapEnv(t)
	ctx := context.Background()
	e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(prewrapRef))
	const checkingRef = "ghcr.io/acme/other@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, _, other := e.asSec(t, http.MethodPost, "/api/v1/base-images", prewrapBody(checkingRef)); other.Check == nil {
		t.Fatal("second registration failed")
	}
	e.rn.gate <- struct{}{}
	// Which check takes the token is not ordered: wait for either to settle, and know which is ready.
	deadline := time.Now().Add(10 * time.Second)
	readyRef := ""
	for readyRef == "" && time.Now().Before(deadline) {
		for _, ref := range []string{prewrapRef, checkingRef} {
			row, _ := e.st.GetBaseImageByRef(ctx, types.BaseImageKindPrewrapped, ref)
			if row.Check != nil && row.Check.Status == types.BaseImageCheckReady {
				readyRef = ref
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if readyRef == "" {
		t.Fatal("no check settled")
	}
	waitingRef := checkingRef
	if readyRef == checkingRef {
		waitingRef = prewrapRef
	}

	// The no-builder door: a ready prewrapped image needs no builder, a waiting one is refused for that,
	// and the image/devcontainer exclusivity still holds.
	if msg, reason := e.srv.validateImageBuildRequest(ctx, createRunRequest{Image: readyRef}); msg != "" {
		t.Fatalf("ready prewrapped refused: %s %s", reason, msg)
	}
	if _, reason := e.srv.validateImageBuildRequest(ctx, createRunRequest{Image: waitingRef}); reason != reasonPrewrappedImageNotRegistered {
		t.Fatalf("waiting prewrapped = %q, want %s", reason, reasonPrewrappedImageNotRegistered)
	}
	if _, reason := e.srv.validateImageBuildRequest(ctx, createRunRequest{Image: "ghcr.io/acme/plain:1"}); reason != reasonImageBuilderUnavailable {
		t.Fatalf("plain image = %q, want %s", reason, reasonImageBuilderUnavailable)
	}
	if _, reason := e.srv.validateImageBuildRequest(ctx, createRunRequest{Image: readyRef, DevcontainerRepo: "https://github.com/acme/x"}); reason != reasonImageDevcontainerExclusive {
		t.Fatalf("image + devcontainer = %q, want %s", reason, reasonImageDevcontainerExclusive)
	}

	// The workspace door: the pinned ref as is, nothing to build, and a waiting one fails the run.
	ws := types.Workspace{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: types.BaseImageKindPrewrapped, Image: readyRef}}
	if v := e.srv.resolveBuildView(ws, workspaceReadFull); v.State != "nothing_to_build" {
		t.Fatalf("build view = %+v, want nothing_to_build", v)
	}
	if res := e.srv.resolveWorkspaceImageWith(ctx, uuid.New(), ws, nil, nil, imageResolveOpts{}); !res.OK || res.Image != readyRef ||
		!slices.Equal(res.ImageArches, []string{"amd64"}) {
		t.Fatalf("workspace image = %+v, want %s as is with [amd64]", res, readyRef)
	}
	ws.BaseImage.Image = waitingRef
	if res := e.srv.resolveWorkspaceImageWith(ctx, uuid.New(), ws, nil, nil, imageResolveOpts{}); res.OK || res.BaseErr == nil {
		t.Fatalf("waiting workspace image = %+v, want a failed run", res)
	}

	// A direct run records the accepted architectures when its image resolves, and dispatch places it by
	// them even once the catalog row is gone; a macOS VM never runs one.
	run, err := e.st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sec", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "k8s", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	image, arches, failed := e.srv.resolveCreateRunImage(ctx, createRunRequest{Agent: "claude-code", Image: readyRef}, run, nil)
	if failed || image != readyRef || !slices.Equal(arches, []string{"amd64"}) {
		t.Fatalf("resolved %q %v failed=%v, want %s [amd64]", image, arches, failed, readyRef)
	}
	ready, _ := e.st.GetBaseImageByRef(ctx, types.BaseImageKindPrewrapped, readyRef)
	if err := e.st.DeleteBaseImage(ctx, ready.ID, false); err != nil {
		t.Fatal(err)
	}
	if run, err = e.st.GetRun(ctx, run.ID); err != nil || !slices.Equal(run.ImageArches, []string{"amd64"}) {
		t.Fatalf("stored run arches = %v (%v), want [amd64]", run.ImageArches, err)
	}
	if _, err := e.srv.createSandboxAtTarget(ctx, run, runner.SandboxSpec{Image: readyRef}); err != nil {
		t.Fatal(err)
	}
	if got := e.rn.lastSpec.ImageArches; !slices.Equal(got, []string{"amd64"}) {
		t.Fatalf("spec.ImageArches = %v, want [amd64] after the catalog row was deleted", got)
	}
	run.MachineKind = types.MachineMacOSVM
	_, err = e.srv.createSandboxAtTarget(ctx, run, runner.SandboxSpec{Image: readyRef})
	if pe := (*prewrappedPlatformError)(nil); !errors.As(err, &pe) || pe.reason != reasonPrewrappedPlatformMismatch {
		t.Fatalf("macOS VM = %v, want %s", err, reasonPrewrappedPlatformMismatch)
	}
	if _, reason := sandboxCreateFailure(err); reason != reasonPrewrappedPlatformMismatch {
		t.Fatalf("audit reason = %s, want %s", reason, reasonPrewrappedPlatformMismatch)
	}
	// Any other run is placed as before.
	if _, err := e.srv.createSandboxAtTarget(ctx, types.AgentRun{ID: uuid.New()}, runner.SandboxSpec{Image: readyRef}); err != nil {
		t.Fatal(err)
	}
	if got := e.rn.lastSpec.ImageArches; got != nil {
		t.Fatalf("a run with no recorded arches got %v", got)
	}
}

func TestPrewrappedRunnerArch(t *testing.T) {
	for _, c := range []struct {
		accepted []string
		arch     string
		reason   string
	}{
		{nil, "arm64", ""},
		{[]string{"amd64"}, "amd64", ""},
		{[]string{"amd64"}, "arm64", reasonPrewrappedPlatformMismatch},
		{[]string{"amd64", "arm64"}, "", reasonPrewrappedPlatformUnknown},
	} {
		err := prewrappedRunnerArch(c.accepted, c.arch)
		var pe *prewrappedPlatformError
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%v on %q = %v, want none", c.accepted, c.arch, err)
		case c.reason != "" && (!errors.As(err, &pe) || pe.reason != c.reason):
			t.Errorf("%v on %q = %v, want %s", c.accepted, c.arch, err, c.reason)
		}
	}
}
