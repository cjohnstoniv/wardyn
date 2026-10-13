// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// #1512 across the hops a run placed on a self-hosted runner adds: every value the run receives is on
// its masking manifest before the runner is sent the create, a recording the laptop uploads through the
// relay meets the door a cluster run's does, and the output tail read back through the runner is masked
// or a gap. WARDYN_TEST_PG guarded (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localMaskSecret is the one value the run under test receives.
const localMaskSecret = "org-secret-the-laptop-must-never-keep"

// claimedLocalRunner is a runner of maskOwner on pool, as the placement doors read one.
func claimedLocalRunner(t *testing.T, st store.PG) types.Runner {
	t.Helper()
	id := uuid.New()
	rn, err := st.CreateRunner(context.Background(), types.Runner{ID: id, Owner: maskOwner, Name: "laptop", PublicKey: make([]byte, 32),
		KeyFingerprint: id.String(), Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL)})
	if err != nil {
		t.Fatal(err)
	}
	return rn
}

// localRunOn persists a RUNNING local run bound to runnerID, evidence runner_asserted, as dispatch leaves one.
func localRunOn(t *testing.T, st store.PG, runnerID uuid.UUID) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.OperatorOwned = maskOwner, false
	run.Placement, run.RunnerID, run.EvidenceSource = types.PlacementLocal, &runnerID, types.RunEvidenceRunnerAsserted
	run, err := st.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if run.EvidenceSource != types.RunEvidenceRunnerAsserted {
		t.Fatalf("stored evidence_source = %q, want runner_asserted", run.EvidenceSource)
	}
	run.SandboxRef = placement.RunnerRefPrefix(runnerID.String()) + "fake-" + run.ID.String()
	if err := st.SetSandboxRef(context.Background(), run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	return run
}

// relayedPut sends one internal request with the run's token as the relay listener does: the connection
// is tagged with runnerID and the closed route list filters it before the internal handler.
func relayedPut(t *testing.T, srv *Server, runnerID uuid.UUID, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: runnerID}))
	w := httptest.NewRecorder()
	srv.relayRoutes(srv.InternalHandler()).ServeHTTP(w, req)
	return w
}

// recordingRows are the recording.upload rows of run, decoded.
func recordingRows(l *maskLab, run types.AgentRun) []map[string]any {
	var out []map[string]any
	for _, ev := range l.rec.snapshot() {
		if ev.Action != "recording.upload" || ev.RunID == nil || *ev.RunID != run.ID {
			continue
		}
		d := map[string]any{}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &d)
		}
		d["_outcome"] = ev.Outcome
		out = append(out, d)
	}
	return out
}

// DONE WHEN 2: a recording a laptop uploads through the relay of its own runner is masked by the
// organisation (the secret is stored as <secret-hidden>) and audited runner_asserted; the same upload
// after the run is fenced is a 503 mask_state_unavailable and stores nothing; a relay from another
// runner never reaches the recording door (relay_runner_mismatch).
func TestPG_RelayedRecordingUploadIsMaskedRefusedAndLabelled(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	st := store.NewPG(l.pool)
	own, other := claimedLocalRunner(t, st), claimedLocalRunner(t, st)
	run := localRunOn(t, st, own.ID)
	rp.dispatch(t, run, localMaskSecret)
	path := "/api/v1/internal/recordings/" + run.ID.String()
	tok := mintRunTokenAs(t, l.h, run.ID, maskOwner)
	body := "$ echo " + localMaskSecret + "\n" + localMaskSecret + "\n"

	// A relay from a runner that does not own the run is refused by the binding, before the recording door.
	if w := relayedPut(t, rp.srv, other.ID, path, tok, body); w.Code != placement.ReasonRelayRunnerMismatch.Status() || !strings.Contains(w.Body.String(), string(placement.ReasonRelayRunnerMismatch)) {
		t.Fatalf("relay from another runner = %d %s, want %d %s", w.Code, w.Body, placement.ReasonRelayRunnerMismatch.Status(), placement.ReasonRelayRunnerMismatch)
	}
	if len(rp.rec.saved) != 0 || len(recordingRows(l, run)) != 0 {
		t.Fatalf("a mismatched relay reached the recording door: %d bytes stored, %d recording.upload rows", len(rp.rec.saved), len(recordingRows(l, run)))
	}

	// The run's own runner: stored masked, audited runner_asserted.
	if w := relayedPut(t, rp.srv, own.ID, path, tok, body); w.Code != http.StatusNoContent {
		t.Fatalf("relayed upload by the run's runner = %d %s, want 204", w.Code, w.Body)
	}
	saved := string(rp.rec.saved)
	if strings.Contains(saved, localMaskSecret) || strings.Count(saved, "<secret-hidden>") != 2 {
		t.Fatalf("stored recording = %q, want both occurrences of the secret replaced by <secret-hidden>", saved)
	}
	rows := recordingRows(l, run)
	if len(rows) != 1 || rows[0]["_outcome"] != "success" || rows[0]["evidence_source"] != string(types.RunEvidenceRunnerAsserted) {
		t.Fatalf("recording.upload rows = %v, want one success row with evidence_source runner_asserted", rows)
	}

	// Fenced: the same upload is refused and nothing more is stored.
	if _, err := rp.srv.cfg.MaskManifests.FenceSubject(context.Background(), maskOwner); err != nil {
		t.Fatal(err)
	}
	rp.rec.saved = nil
	w := relayedPut(t, rp.srv, own.ID, path, tok, body)
	wantMaskRefusal(t, w, "relayed upload of a fenced run")
	if len(rp.rec.saved) != 0 {
		t.Errorf("a fenced run's relayed upload stored %q", rp.rec.saved)
	}
	if got := len(recordingRows(l, run)); got != 1 {
		t.Errorf("a refused upload wrote a recording.upload row: %d rows, want the one success row", got)
	}
}

// outputSub is the runner's substrate with one output it gives back, and a count of the reads.
type outputSub struct {
	*runnertest.Fake
	out   string
	reads atomic.Int32
}

func (o *outputSub) RecoverOutput(_ context.Context, _ string, w io.Writer) error {
	o.reads.Add(1)
	_, err := io.WriteString(w, o.out)
	return err
}

// attachRunner connects id's endpoint on srv to a served runner backed by sub over loopback, as the hub does
// on admission, and waits for its capabilities.
func attachRunner(t *testing.T, srv *Server, id uuid.UUID, sub substrate.Substrate) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ep := srv.runnerEndpoint(id)
	org, run := runnerwire.Loopback()
	ep.link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, id.String(), sub)
	if err := served.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's caps never arrived")
		}
	}
}

// DONE WHEN 3: the output tail of a local run, read back through its runner, is stored masked when the
// run's manifest covers it, and is a capture gap (mask_uncovered, no read of the runner) when it does not.
func TestPG_LocalRunOutputCaptureMasksOrGaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		covered bool
	}{{"covered", true}, {"uncovered", false}} {
		t.Run(tc.name, func(t *testing.T) {
			l := newMaskLab(t)
			st := store.NewPG(l.pool)
			rn := claimedLocalRunner(t, st)
			dbRun := localRunOn(t, st, rn.ID)
			sub := &outputSub{Fake: runnertest.NewFake(), out: "agent printed " + localMaskSecret + " and stopped\n"}

			rp := l.replica()
			f := newOutputFixture(t, func(c *Config) {
				c.Runner = orchestrator.New()
				c.MaskRegistry, c.MaskManifests = rp.srv.cfg.MaskRegistry, rp.srv.cfg.MaskManifests
			})
			// The fixture's run is the Postgres run: same id, same runner ref.
			f.st.run = dbRun
			f.run = dbRun
			f.mem.rows[dbRun.ID] = store.RunOutput{RunID: dbRun.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
			attachRunner(t, f.srv, rn.ID, sub)

			ctx := context.Background()
			if !f.srv.beginMaskManifest(ctx, dbRun) {
				t.Fatal("the manifest could not begin")
			}
			if tc.covered {
				if err := f.srv.maskDispatchValue(ctx, dbRun.ID, []byte(localMaskSecret)); err != nil {
					t.Fatal(err)
				}
				if !f.srv.completeMaskManifest(ctx, dbRun) {
					t.Fatal("the manifest could not complete")
				}
			}
			if got := f.srv.maskCovered(ctx, dbRun.ID); got != tc.covered {
				t.Fatalf("maskCovered = %v, want %v: the premise of the case is gone", got, tc.covered)
			}

			// The run ended while no process held its tail: the sweeper resolves its stale pending row.
			f.st.mu.Lock()
			f.st.state = types.RunFailed
			f.st.mu.Unlock()
			if err := f.srv.SweepRunOutputs(ctx); err != nil {
				t.Fatal(err)
			}
			row := f.finalRow(t)
			if tc.covered {
				want := "agent printed <secret-hidden> and stopped\n"
				if string(row.Output) != want || row.CaptureGap || sub.reads.Load() != 1 {
					t.Fatalf("row = %q gap %v after %d read(s) of the runner, want %q, no gap, one read", row.Output, row.CaptureGap, sub.reads.Load(), want)
				}
				return
			}
			if !row.CaptureGap || len(row.Output) != 0 || sub.reads.Load() != 0 {
				t.Fatalf("row = %q gap %v after %d read(s) of the runner, want an empty capture gap and no read", row.Output, row.CaptureGap, sub.reads.Load())
			}
			if got := f.gapReason(t); got != "mask_uncovered" {
				t.Errorf("gap reason = %q, want mask_uncovered", got)
			}
		})
	}
}

var _ runner.OutputRecoverer = (*outputSub)(nil)

// wireCreateRunner is the deployment's runner with one change: a create for a run on a runner goes down the
// runner's own wire endpoint (the remote substrate), as the orchestrator sends it, instead of the pool double.
type wireCreateRunner struct {
	*poolRunner
	srv *Server
}

func (w *wireCreateRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	id, err := uuid.Parse(spec.RunnerID)
	if err != nil {
		return w.poolRunner.CreateSandbox(ctx, spec)
	}
	return w.srv.runnerEndpoint(id).sub.CreateSandbox(ctx, spec)
}

// createSeen is the runner's substrate: at the moment the runner is sent the create it reads the run's
// manifest from Postgres through a manifest cache that has never seen the run (another replica, or a
// restart), and records what the create carried.
type createSeen struct {
	*runnertest.Fake
	fresh func(context.Context, uuid.UUID) (covered bool, values [][]byte)

	mu      sync.Mutex
	creates int
	covered bool
	values  [][]byte
	spec    runner.SandboxSpec
}

func (c *createSeen) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	covered, values := c.fresh(ctx, spec.RunID)
	c.mu.Lock()
	c.creates++
	c.covered, c.values, c.spec = covered, values, spec
	c.mu.Unlock()
	return c.Fake.CreateSandbox(ctx, spec)
}

// DONE WHEN 1: when the runner is sent the create of a local run, the run's manifest is already complete
// in Postgres and holds every value the create carries.
func TestPG_LocalRunManifestIsCompleteBeforeTheRunnerSeesCreate(t *testing.T) {
	t.Setenv(envAllowMemberEnvSecret, "true")
	e := newPlacementEnv(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(e.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Secrets = sec
	e.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	e.srv.cfg.MaskManifests = maskmanifest.New(e.pool, sec.SubjectKeys(), e.srv.cfg.MaskRegistry)
	e.srv.cfg.Runner = &wireCreateRunner{poolRunner: e.runner, srv: e.srv}

	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	if err := sec.For(e.personP).Put(context.Background(), "my-token", []byte(localMaskSecret)); err != nil {
		t.Fatal(err)
	}

	e.srv.cfg.DefaultPolicy.EligibleGrants = append(e.srv.cfg.DefaultPolicy.EligibleGrants,
		types.GrantSpec{Kind: types.GrantEnvSecret, OwnerOnly: true, Scope: json.RawMessage(`{"name":"MY_TOKEN","secret_name":"my-token"}`)})

	seen := &createSeen{Fake: runnertest.NewFake(), fresh: func(ctx context.Context, run uuid.UUID) (bool, [][]byte) {
		reg := secretmask.NewRegistry()
		covered := maskmanifest.New(e.pool, sec.SubjectKeys(), reg).Covered(ctx, run)
		return covered, reg.Snapshot(run)
	}}
	seen.Fake.Support.Classes = []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	attachRunner(t, e.srv, awake, seen)

	body := runBody(`"runner_pool_id":"` + laptops.ID.String() + `","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"env_secret","owner_only":true,"scope":{"name":"MY_TOKEN","secret_name":"my-token"}}]}`)
	run := e.localCreate(body)
	if run.RunnerID == nil || *run.RunnerID != awake || run.Placement != types.PlacementLocal {
		t.Fatalf("run = placement %q runner %v, want local on %s", run.Placement, run.RunnerID, awake)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		seen.mu.Lock()
		n := seen.creates
		seen.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the runner was never sent the create; audit: %s", auditDump(e.h.audit.snapshot(), run.ID))
		}
	}

	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.creates != 1 || seen.spec.RunID != run.ID {
		t.Fatalf("runner saw %d create(s) for %s, want one for %s", seen.creates, seen.spec.RunID, run.ID)
	}
	if !seen.covered {
		t.Fatalf("when the runner was sent the create, Postgres did not hold a complete manifest for the run; audit: %s", auditDump(e.h.audit.snapshot(), run.ID))
	}
	if seen.spec.SecretEnv["MY_TOKEN"] != localMaskSecret {
		t.Fatalf("the create carried SecretEnv %v, want the injected secret in MY_TOKEN (the pin is vacuous otherwise); created by %q; audit: %s",
			seen.spec.SecretEnv, run.CreatedBy, auditDump(e.h.audit.snapshot(), run.ID))
	}
	for name, v := range seen.spec.SecretEnv {
		if !slices.ContainsFunc(seen.values, func(m []byte) bool { return string(m) == v }) {
			t.Errorf("SecretEnv %s is in the create but not on the run's manifest", name)
		}
	}
}
