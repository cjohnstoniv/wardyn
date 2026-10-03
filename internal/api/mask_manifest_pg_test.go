// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The run masking manifest through the API's own doors, against a real
// Postgres: two Servers over one database are two wardynds, each with its own
// registry and manifest cache (internal/maskmanifest). Guarded by WARDYN_TEST_PG
// (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const maskOwner = "alice@example.com"

// maskLab is one database and one KEK shared by every replica it builds.
type maskLab struct {
	t    *testing.T
	pool *pgxpool.Pool
	id   *age.X25519Identity
	h    *harness
	rec  *sshTestRecorder
}

func newMaskLab(t *testing.T) *maskLab {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return &maskLab{t: t, pool: throwawayPGPool(t), id: id, h: newHarness(t), rec: &sshTestRecorder{}}
}

// replica is one wardynd: its own registry, manifest cache, recording store and
// runner, over the shared database. The beat is short so a fence is seen fast.
type replica struct {
	srv *Server
	reg *secretmask.Registry
	rec *fakeRecordingStore
	fr  *holderTestRunner
	sec *secretspg.Store
}

func (l *maskLab) replica() replica {
	l.t.Helper()
	sec, err := secretspg.New(l.pool, l.id)
	if err != nil {
		l.t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	rs := &fakeRecordingStore{}
	fr := &holderTestRunner{}
	cfg := baseTestConfig(l.h, store.NewPG(l.pool))
	cfg.Audit = l.rec
	cfg.Runner = fr
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = sec
	cfg.RecordingStore = rs
	cfg.MaskRegistry = reg
	cfg.MaskManifests = maskmanifest.New(l.pool, sec.SubjectKeys(), reg)
	srv := New(cfg)
	srv.maskBeat = 25 * time.Millisecond
	return replica{srv: srv, reg: reg, rec: rs, fr: fr, sec: sec}
}

// run persists a RUNNING, operator-owned run with a sandbox (so the attach
// doors admit it) whose owner is maskOwner.
func (l *maskLab) run() types.AgentRun {
	l.t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	r := types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: maskOwner, OperatorOwned: true,
		Agent: "claude-code", Task: "mask manifest", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker", SandboxRef: "sbx-1",
	}
	created, err := store.NewPG(l.pool).CreateRun(context.Background(), r)
	if err != nil {
		l.t.Fatalf("create run: %v", err)
	}
	if err := store.NewPG(l.pool).SetSandboxRef(context.Background(), id, "sbx-1"); err != nil {
		l.t.Fatal(err)
	}
	created.SandboxRef = "sbx-1"
	return created
}

// dispatch is what dispatchRun does for the run's secrets: begin, register and
// commit each value, complete.
func (rp replica) dispatch(t *testing.T, run types.AgentRun, values ...string) {
	t.Helper()
	ctx := context.Background()
	if !rp.srv.beginMaskManifest(ctx, run) {
		t.Fatal("the manifest could not begin")
	}
	for _, v := range values {
		if err := rp.srv.maskDispatchValue(ctx, run.ID, []byte(v)); err != nil {
			t.Fatalf("record %q: %v", v, err)
		}
	}
	if !rp.srv.completeMaskManifest(ctx, run) {
		t.Fatal("the manifest could not complete")
	}
}

// upload PUTs body as the run's own recording upload.
func (l *maskLab) upload(rp replica, run types.AgentRun, body string) *httptest.ResponseRecorder {
	l.t.Helper()
	return do(l.t, rp.srv, http.MethodPut, "/api/v1/internal/recordings/"+run.ID.String(), mintRunTokenAs(l.t, l.h, run.ID, maskOwner), body)
}

func wantMaskRefusal(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s: status %d, want 503: %s", what, w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Reason != string(authz.ReasonMaskStateUnavailable) {
		t.Fatalf("%s: body %s, want reason %q", what, w.Body.String(), authz.ReasonMaskStateUnavailable)
	}
}

// deniedEvent is the authz.denied event of an uncovered door, by target.
func (l *maskLab) deniedEvent(runID uuid.UUID, target string) types.AuditEvent {
	l.t.Helper()
	for _, ev := range l.rec.snapshot() {
		if ev.Action == authz.AuditAction && ev.Target == target && ev.RunID != nil && *ev.RunID == runID && ev.Outcome == "denied" {
			return ev
		}
	}
	l.t.Fatalf("no denied authz row for %s on run %s; events: %s", target, runID, auditDump(l.rec.snapshot(), runID))
	return types.AuditEvent{}
}

// deniedRow is deniedEvent's decoded datum.
func (l *maskLab) deniedRow(runID uuid.UUID, target string) map[string]any {
	l.t.Helper()
	var d map[string]any
	if err := json.Unmarshal(l.deniedEvent(runID, target).Data, &d); err != nil {
		l.t.Fatal(err)
	}
	return d
}

// The recording upload masks a value committed at dispatch on a replica that
// never saw it (healthy A, the upload landing on B), and on a restarted one,
// whatever the secret has been rotated to since dispatch.
func TestMaskManifest_UploadMasksOnAnotherReplicaAndAfterARestart(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "v1-secret-at-dispatch")

	// Rotate, delete and recreate the secret after dispatch: the name now says v2.
	ctx := context.Background()
	if err := a.sec.For(maskOwner).Put(ctx, "api-token", []byte("v1-secret-at-dispatch")); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.For(maskOwner).Delete(ctx, "api-token"); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.For(maskOwner).Put(ctx, "api-token", []byte("v2-secret-after-rotation")); err != nil {
		t.Fatal(err)
	}

	body := "out: v1-secret-at-dispatch and v2-secret-after-rotation\n"
	for name, rp := range map[string]replica{"another replica (B)": l.replica(), "a restarted replica": l.replica()} {
		if w := l.upload(rp, run, body); w.Code != http.StatusNoContent {
			t.Fatalf("%s: upload %d: %s", name, w.Code, w.Body.String())
		}
		saved := string(rp.rec.saved)
		if strings.Contains(saved, "v1-secret-at-dispatch") {
			t.Errorf("%s persisted the pre-rotation value unmasked: %q", name, saved)
		}
		if !strings.Contains(saved, "v2-secret-after-rotation") {
			t.Errorf("%s masked a value the run never received (it re-resolved the name): %q", name, saved)
		}
	}
}

// An incomplete manifest, and a run with none at all (dispatched before
// manifests existed, or a value injected after a restart), is refused at every
// door that relays or persists the run's output, with a denied audit row.
func TestMaskManifest_IncompleteManifestIsRefusedAtAllFiveDoors(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	partial, legacy := l.run(), l.run()
	ctx := context.Background()
	if err := rp.srv.cfg.MaskManifests.Start(ctx, partial.ID, maskOwner); err != nil {
		t.Fatal(err)
	}
	if err := rp.srv.maskDispatchValue(ctx, partial.ID, []byte("committed-never-completed")); err != nil {
		t.Fatal(err)
	}
	// An injection after a restart registers a value; it does not cover the run.
	rp.reg.Add(legacy.ID, []byte("injected-after-the-restart"))

	ts := httptest.NewServer(panicFails(t, rp.srv.Handler()))
	defer ts.Close()
	for name, run := range map[string]types.AgentRun{"incomplete": partial, "legacy": legacy} {
		t.Run(name, func(t *testing.T) {
			// 1. the recording upload
			w := l.upload(rp, run, "anything\n")
			wantMaskRefusal(t, w, "recording upload")
			if len(rp.rec.saved) != 0 {
				t.Errorf("an uncovered run's upload reached the store: %q", rp.rec.saved)
			}
			row := l.deniedRow(run.ID, "recordings.upload")
			if row["reason"] != string(authz.ReasonMaskStateUnavailable) || row["mask_scope"] != "globals_only" {
				t.Errorf("recording denial row = %v, want reason mask_state_unavailable and mask_scope globals_only", row)
			}

			// The upload is the run token's own: its row says so, never admin-token.
			id, err := l.h.idp.MintRunIdentity(ctx, run.ID, maskOwner, "", internalAudience, false)
			if err != nil {
				t.Fatal(err)
			}
			if ev := l.deniedEvent(run.ID, "recordings.upload"); ev.ActorType != types.ActorAgent || ev.Actor != id.SPIFFEID {
				t.Errorf("recording denial actor = %s %q, want %s %q", ev.ActorType, ev.Actor, types.ActorAgent, id.SPIFFEID)
			}

			// 2. the live attach (refused before the WebSocket upgrade)
			tok, err := mintAttachTicket(ctx, rp.srv.cfg.Store, run.ID, types.ActorHuman, maskOwner, oidc.RoleAdmin, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			_, resp, derr := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
			if derr == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("attach: err=%v resp=%v, want a 503 before the upgrade", derr, resp)
			}
			if r := l.deniedRow(run.ID, "runs.attach"); r["mask_scope"] != "globals_only" {
				t.Errorf("attach denial row = %v", r)
			}

			// 3. the exec relay keeps nothing for it
			if w := rp.srv.openExecOutput(types.AgentRun{ID: run.ID}, false); w != nil {
				t.Error("the exec relay opened a tail for an uncovered run")
			}

			// 4. the SSH shell
			ch := newFakeSSHChannel()
			rp.srv.bridgeSSHShell(ctx, run.ID, maskOwner, ch, 80, 24, nil)
			if !strings.Contains(ch.stderrString(), "cannot prove") {
				t.Errorf("the SSH shell was not refused: stderr %q", ch.stderrString())
			}
			if rp.fr.session(0) != nil {
				t.Error("the SSH shell opened a session on an uncovered run")
			}
			l.deniedRow(run.ID, "ssh.shell")

			// 5. the live output read
			w = do(t, rp.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/output", adminToken, "")
			wantMaskRefusal(t, w, "live output read")
			l.deniedRow(run.ID, "runs.output")
		})
	}
}

// A completed manifest admits every door. The doors that need no sandbox are
// checked here; the attach and the shell run in the fence tests below.
func TestMaskManifest_CompleteManifestAdmitsTheDoors(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	run := l.run()
	rp.dispatch(t, run, "complete-manifest-secret")

	if w := l.upload(rp, run, "ok\n"); w.Code != http.StatusNoContent {
		t.Errorf("upload on a covered run: %d %s", w.Code, w.Body.String())
	}
	if w := rp.srv.openExecOutput(types.AgentRun{ID: run.ID}, false); w == nil {
		t.Error("the exec relay refused a covered run")
	}
}

// A fence issued on server B ends an attach held on server A, and a new attach
// on A is refused: two servers, one database.
func TestMaskManifest_FenceOnBEndsAnAttachHeldOnA(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "attached-run-secret-value")

	ts := httptest.NewServer(panicFails(t, a.srv.Handler()))
	defer ts.Close()
	c := dialAttachWith(t, ts, a.srv, run.ID)
	readAttachMode(t, c)
	waitFor(t, "the attach's session to open", func() bool { return a.fr.session(0) != nil })

	// Output flows while covered.
	if _, err := a.fr.session(0).w.Write([]byte("hello\r\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, data, err := c.Read(ctx); err != nil || string(data) != "hello\r\n" {
		t.Fatalf("covered attach read %q, %v", data, err)
	}

	// B erases the person: the fence is a row, and A's beat reads it.
	if fenced, err := b.srv.cfg.MaskManifests.FenceSubject(context.Background(), maskOwner); err != nil || len(fenced) != 1 {
		t.Fatalf("FenceSubject on B = %v, %v", fenced, err)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	var end error
	for end == nil {
		if _, _, end = c.Read(rctx); rctx.Err() != nil {
			t.Fatal("the attach on A was not ended by the fence issued on B")
		}
	}
	if got := websocket.CloseStatus(end); got != websocket.StatusTryAgainLater {
		t.Errorf("the attach closed with status %v (%v), want 1013 try again later", got, end)
	}

	// And a new attach on A is refused.
	tok, err := mintAttachTicket(context.Background(), a.srv.cfg.Store, run.ID, types.ActorHuman, maskOwner, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, derr := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if derr == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a new attach after the fence: err=%v resp=%v, want 503", derr, resp)
	}
	waitFor(t, "session.detach to record why", func() bool {
		for _, ev := range l.rec.snapshot() {
			if ev.Action == "session.detach" && strings.Contains(string(ev.Data), maskFencedReason) {
				return true
			}
		}
		return false
	})
}

// The same fence ends an SSH shell held on A.
func TestMaskManifest_FenceOnBEndsAnSSHShellHeldOnA(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "ssh-shell-run-secret-value")

	ch := newFakeSSHChannel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.srv.bridgeSSHShell(context.Background(), run.ID, maskOwner, ch, 80, 24, nil)
	}()
	waitFor(t, "the shell's session to open", func() bool { return a.fr.session(0) != nil })

	if _, err := b.srv.cfg.MaskManifests.FenceSubject(context.Background(), maskOwner); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH shell on A was not ended by the fence issued on B")
	}
	if !strings.Contains(ch.stderrString(), "no longer prove") {
		t.Errorf("the shell was ended without saying why: stderr %q", ch.stderrString())
	}
}

// An upload in flight when the fence lands is cut off, not finished: bytes
// past the fence were not masked against a corpus proven whole.
func TestMaskManifest_FenceDuringAnUploadRefusesIt(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "upload-in-flight-secret")

	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/internal/recordings/"+run.ID.String(), pr)
	req.Header.Set("Authorization", "Bearer "+mintRunTokenAs(t, l.h, run.ID, maskOwner))
	req.Host, req.RemoteAddr = "127.0.0.1", "127.0.0.1:1234"
	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		panicFails(t, a.srv.Handler()).ServeHTTP(w, req)
	}()
	if _, err := pw.Write([]byte("first chunk of the cast\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.srv.cfg.MaskManifests.FenceSubject(context.Background(), maskOwner); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * a.srv.maskBeat)
	_, _ = pw.Write([]byte("second chunk, past the fence\n"))
	_ = pw.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never returned")
	}
	wantMaskRefusal(t, w, "an upload fenced mid-stream")
}

// ReconcileOnBoot loads the manifest of every live run, so a restarted replica
// holds coverage before any door asks, and an injection afterwards covers nothing.
func TestMaskManifest_ReconcileOnBootLoadsLiveRunsOnly(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	live, legacy := l.run(), l.run()
	a.dispatch(t, live, "live-run-secret-value")

	restarted := l.replica()
	if restarted.srv.cfg.MaskManifests.Held(live.ID) {
		t.Fatal("a fresh process holds a manifest it never loaded")
	}
	if err := restarted.srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}
	if !restarted.srv.cfg.MaskManifests.Held(live.ID) {
		t.Error("ReconcileOnBoot did not load the live run's manifest")
	}
	if !strings.Contains(string(restarted.reg.Masker(live.ID).Mask([]byte("x live-run-secret-value"))), "<secret-hidden>") {
		t.Error("the live run's value is not masked after boot")
	}
	restarted.reg.Add(legacy.ID, []byte("injected-after-restart"))
	if restarted.srv.cfg.MaskManifests.Held(legacy.ID) || restarted.srv.cfg.MaskManifests.Covered(context.Background(), legacy.ID) {
		t.Error("an injection after a restart covered a run with no manifest")
	}
}

// A value the run took from the operator's namespace by fallback is sealed in
// the run OWNER's key domain, and stays masked after the owner later shadows it
// with a secret of their own.
func TestMaskManifest_OperatorFallbackValueIsSealedForTheOwnerAndSurvivesShadowing(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	ctx := context.Background()
	if err := a.sec.Put(ctx, "shared-token", []byte("operator-fallback-value")); err != nil {
		t.Fatal(err)
	}
	if !a.srv.beginMaskManifest(ctx, run) {
		t.Fatal("begin")
	}
	scope, _ := json.Marshal(map[string]string{"name": "SHARED", "secret_name": "shared-token"})
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantEnvSecret, Scope: scope}}}
	env := map[string]string{}
	a.srv.resolveEnvSecretGrants(ctx, run, policy, env)
	if env["SHARED"] != "operator-fallback-value" {
		t.Fatalf("SHARED = %q, want the operator's fallback value", env["SHARED"])
	}
	if !a.srv.completeMaskManifest(ctx, run) {
		t.Fatal("complete")
	}

	var owner string
	if err := l.pool.QueryRow(ctx, `SELECT DISTINCT owner FROM run_mask_values WHERE run_id=$1`, run.ID).Scan(&owner); err != nil || owner != maskOwner {
		t.Fatalf("value rows are owned by %q (err %v), want the run owner %q even for an operator-fallback value", owner, err, maskOwner)
	}
	var live int
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FROM principal_keys WHERE owner=$1 AND purpose='cred' AND destroyed_at IS NULL`, maskOwner).Scan(&live); err != nil || live != 1 {
		t.Fatalf("the owner's cred key: %d live generations (err %v), want 1", live, err)
	}

	// The owner now shadows the operator's secret with their own.
	if err := a.sec.For(maskOwner).Put(ctx, "shared-token", []byte("owners-own-shadowing-value")); err != nil {
		t.Fatal(err)
	}
	restarted := l.replica()
	if w := l.upload(restarted, run, "leak: operator-fallback-value\n"); w.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(restarted.rec.saved), "operator-fallback-value") {
		t.Errorf("the value the sandbox holds is unmasked after the owner shadowed it: %q", restarted.rec.saved)
	}
}

// Erasing the person leaves a replica that had loaded the run unable to mask or
// to admit it: it drops what it held at its next check.
func TestMaskManifest_ForgetOnEvictionDropsTheCachedCopy(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "swept-run-secret-value")
	if !a.srv.cfg.MaskManifests.Held(run.ID) {
		t.Fatal("not held after dispatch")
	}
	a.srv.forgetMaskManifest(run.ID)
	if a.srv.cfg.MaskManifests.Held(run.ID) {
		t.Error("an evicted run's manifest is still cached")
	}
	if !a.srv.cfg.MaskManifests.Covered(context.Background(), run.ID) || !a.srv.cfg.MaskManifests.Held(run.ID) {
		t.Error("the next admission did not reload the manifest from Postgres")
	}
}

// dialAttachWith is dialAttach for a Server whose store is not holderTestServer's.
func dialAttachWith(t *testing.T, ts *httptest.Server, srv *Server, runID uuid.UUID) *websocket.Conn {
	t.Helper()
	return dialAttach(t, ts, srv, runID, maskOwner, "")
}
