// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// moveAnswer is a refused move's body.
func moveAnswer(t *testing.T, body string) (reason, field string) {
	t.Helper()
	var b struct {
		Reason string `json:"reason"`
		Field  string `json:"field"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("answer %q is not an error body: %v", body, err)
	}
	return b.Reason, b.Field
}

// TestMoveExec_StopHoldsRunLock: the stop, the receipt, the cut and the token stamp each run under the run's lock,
// held on the context they are handed, and the move then commits at the destination.
func TestMoveExec_StopHoldsRunLock(t *testing.T) {
	f := newMoveFixture(t)
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("move = %d %s, want 202", code, body)
	}
	if got := f.fk.unlockedPorts(); len(got) != 0 {
		t.Errorf("ports called without the run's lock: %v", got)
	}
	for _, port := range []string{"stop", "cut", "stamp", "revoke_at_pause"} {
		if n := f.fk.count(port); n != 1 {
			t.Errorf("%s calls = %d, want 1", port, n)
		}
	}
	if mv := f.only(t); mv.State != types.MoveCommitted || mv.ReleasedAt == nil {
		t.Errorf("move = %s released %v, want committed and released", mv.State, mv.ReleasedAt)
	}
	if f.moveRows("success") != 1 {
		t.Errorf("run.move success rows = %d, want 1", f.moveRows("success"))
	}
}

// moveCase is one way a move ends before commit, or at it.
type moveCase struct {
	name   string
	stop   bool // the run is in a stopped pause before the request
	setup  func(t *testing.T, f *moveFixture)
	state  types.MoveState
	reason string
	// created: the destination was created, so the end tears it down.
	created bool
	code    int
	// msg, when set, is in the answer's body.
	msg string
}

// TestMoveExec_EveryErrorPathEndsNamed: every way a move fails or is refused after its row is admitted ends the row
// in a named state with its reason, writes exactly one run.move row (denied for a refusal, failure for a failure),
// leaves the run held by no move, and tears down a destination it created.
func TestMoveExec_EveryErrorPathEndsNamed(t *testing.T) {
	notDrained := func(_ *testing.T, f *moveFixture) { f.mr.drained = false }
	cases := []moveCase{
		{name: "stop errors", setup: func(_ *testing.T, f *moveFixture) { f.fk.stopErr = errors.New("daemon gone") },
			state: types.MoveFailed, reason: reasonMoveStopFailed},
		{name: "receipt incomplete", setup: func(_ *testing.T, f *moveFixture) { f.fk.receipt.Complete = false },
			state: types.MoveRefused, reason: reasonMovePauseRefused},
		{name: "receipt incomplete at a stopped pause", stop: true, setup: func(_ *testing.T, f *moveFixture) { f.fk.receipt.Complete = false },
			state: types.MoveRefused, reason: reasonMovePauseRefused, code: http.StatusConflict, msg: movePauseAgainMsg},
		{name: "step 2 read fails", setup: func(_ *testing.T, f *moveFixture) { f.fk.stopRefusalErrAt = 2 },
			state: types.MoveRefused, reason: reasonMovePolicyUnreadable, code: http.StatusServiceUnavailable},
		{name: "pause_gen write errors", setup: func(_ *testing.T, f *moveFixture) { f.ms.failWrites = map[string]bool{"pause_gen": true} },
			state: types.MoveFailed, reason: reasonMoveStopFailed},
		{name: "pause_gen write changes no row", setup: func(_ *testing.T, f *moveFixture) {
			f.ms.failWrites = map[string]bool{"pause_gen:none": true}
		}, state: types.MoveFailed, reason: reasonMoveStopFailed},
		{name: "advance to exporting errors", setup: func(_ *testing.T, f *moveFixture) {
			f.ms.failWrites = map[string]bool{"advance:" + string(types.MoveExporting): true}
		}, state: types.MoveFailed, reason: reasonMoveExportFailed, created: true},
		{name: "advance to importing errors", setup: func(_ *testing.T, f *moveFixture) {
			f.ms.failWrites = map[string]bool{"advance:" + string(types.MoveImporting): true}
		}, state: types.MoveFailed, reason: reasonMoveImportFailed, created: true},
		{name: "settle answers unsettled", setup: func(_ *testing.T, f *moveFixture) {
			f.srv.cfg.Store = &moveOwedSettle{moveStore: f.ms}
			f.srv.cfg.Runner = &moveFreezeFails{moveRunner: f.mr}
		}, state: types.MoveRefused, reason: reasonMovePauseRefused},
		{name: "identity refused", setup: func(_ *testing.T, f *moveFixture) {
			f.srv.cfg.Identity = &moveMintRefused{Provider: f.srv.cfg.Identity}
		}, state: types.MoveFailed, reason: reasonMoveIdentityRefused},
		{name: "source stream breaks", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.exportFn = func() (io.ReadCloser, error) {
				return io.NopCloser(iotest.ErrReader(errors.New("connection reset"))), nil
			}
		}, state: types.MoveFailed, reason: reasonMoveRelayFailed, created: true},
		{name: "git alternates outside the roots", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.files = append(f.mr.files, moveDir("home/agent/work/.git/objects"), moveDir("home/agent/work/.git/objects/info"),
				moveReg("home/agent/work/.git/objects/info/alternates", "/srv/elsewhere/objects\n"))
		}, state: types.MoveFailed, reason: reasonMoveGitOutsideRoots, created: true},
		{name: "cut not made", setup: func(_ *testing.T, f *moveFixture) { f.fk.cut = false },
			state: types.MoveRefused, reason: reasonMovePauseRefused},
		{name: "azure devops not revoked", setup: func(_ *testing.T, f *moveFixture) { f.fk.receipt.ADO = "unfinished" },
			state: types.MoveRefused, reason: reasonMoveRevokeRefused},
		{name: "stamp changes nothing", setup: func(_ *testing.T, f *moveFixture) { f.fk.stamp = false },
			state: types.MoveFailed, reason: reasonMovePauseRefused},
		{name: "stamp errors", setup: func(_ *testing.T, f *moveFixture) { f.fk.stampErr = errors.New("conn reset") },
			state: types.MoveFailed, reason: reasonMoveStopFailed},
		{name: "export fails", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.exportFn = func() (io.ReadCloser, error) { return nil, errors.New("runner went away") }
		}, state: types.MoveFailed, reason: reasonMoveExportFailed, created: true},
		{name: "broken stream", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.exportFn = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("not a stream")), nil }
		}, state: types.MoveFailed, reason: reasonMoveArchiveRefused, created: true},
		{name: "create fails", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.importFn = func(runner.SandboxSpec, io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
				return runner.Sandbox{}, runner.CarriedManifest{}, runner.ErrMoveCreate
			}
		}, state: types.MoveFailed, reason: reasonMoveCreateFailed, created: true},
		{name: "import fails", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.importFn = func(_ runner.SandboxSpec, r io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
				_, _ = io.Copy(io.Discard, r)
				return runner.Sandbox{Ref: "ref-dest-partial"}, runner.CarriedManifest{}, runner.ErrMoveFiles
			}
		}, state: types.MoveFailed, reason: reasonMoveImportFailed, created: true},
		{name: "digests differ", setup: func(_ *testing.T, f *moveFixture) {
			f.mr.importFn = func(_ runner.SandboxSpec, r io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
				_, _ = io.Copy(io.Discard, r)
				return runner.Sandbox{Ref: "ref-dest-odd"}, runner.CarriedManifest{Digest: "0000"}, nil
			}
		}, state: types.MoveFailed, reason: reasonMoveDigestMismatch, created: true},
		{name: "secret value in the files", setup: func(t *testing.T, f *moveFixture) {
			reg := secretmask.NewRegistry()
			if err := reg.Add(f.run.ID, []byte("sk-live-0123456789abcdef")); err != nil {
				t.Fatal(err)
			}
			f.srv.cfg.MaskRegistry = reg
			f.mr.files = append(f.mr.files, moveReg("home/agent/work/token.txt", "key=sk-live-0123456789abcdef\n"))
		}, state: types.MoveFailed, reason: reasonMoveScanHit, created: true},
		{name: "spool not delivered", setup: notDrained, state: types.MoveFailed, reason: reasonMoveSourceSpoolPending, created: true},
		{name: "contained before commit", setup: func(_ *testing.T, f *moveFixture) {
			f.fk.onStop = func() { f.fk.contained = true }
		}, state: types.MoveFailed, reason: reasonMoveLocallyContained, created: true},
		{name: "revoke reopened at commit", setup: func(_ *testing.T, f *moveFixture) {
			f.fk.receiptAt = func(call int) moveRevokeState {
				if call > 1 {
					return moveRevokeState{Complete: true, ADO: "unfinished"}
				}
				return moveRevokeState{Complete: true, ADO: moveADORevoked}
			}
		}, state: types.MoveFailed, reason: reasonMoveRevokeRefused, created: true},
		{name: "run lost before commit", setup: func(_ *testing.T, f *moveFixture) {
			f.ms.beforeCommit = func() { now := time.Now(); f.ms.mu.Lock(); f.ms.run.LostAt = &now; f.ms.mu.Unlock() }
		}, state: types.MoveFailed, reason: reasonMoveRunEnded, created: true},
		{name: "resume took the run before commit", setup: func(_ *testing.T, f *moveFixture) {
			f.ms.beforeCommit = func() {
				later := time.Now().Add(time.Second).UTC()
				f.ms.mu.Lock()
				f.ms.run.PausedAt = &later
				f.ms.mu.Unlock()
			}
		}, state: types.MoveFailed, reason: reasonMovePauseChanged, created: true},
		{name: "source unsettled", setup: func(_ *testing.T, f *moveFixture) { f.ms.unsettled = true },
			state: types.MoveFailed, reason: reasonMoveSourceUnsettled, created: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newMoveFixture(t)
			if c.stop {
				f.stop(t)
			}
			c.setup(t, f)
			want := c.code
			if want == 0 {
				want = http.StatusAccepted
			}
			code, body := f.move(t, "org")
			if code != want {
				t.Fatalf("move = %d %s, want %d", code, body, want)
			}
			if c.msg != "" && !strings.Contains(body, c.msg) {
				t.Errorf("answer %s, want it to say %q", body, c.msg)
			}
			mv := f.only(t)
			if mv.State != c.state || mv.Failure != c.reason {
				t.Errorf("row = %s %q, want %s %q", mv.State, mv.Failure, c.state, c.reason)
			}
			outcome := "failure"
			if c.state == types.MoveRefused {
				outcome = "denied"
			}
			if n := f.moveRows(outcome); n != 1 || f.moveRows("success") != 0 {
				t.Errorf("run.move %s rows = %d success %d, want 1 and 0", outcome, n, f.moveRows("success"))
			}
			if held, _ := f.ms.RunMoveInFlight(context.Background(), f.run.ID); held {
				t.Error("an ended move still holds the run")
			}
			if c.created && mv.ToTokenJTI == "" {
				t.Error("a move that created its destination recorded no destination token")
			}
			if mv.ToSandboxRef != "" && !slices.Contains(f.mr.killed(), mv.ToSandboxRef) {
				t.Errorf("destination %s was not torn down (kills %v)", mv.ToSandboxRef, f.mr.killed())
			}
			if cur := f.current(t); cur.SandboxRef != f.run.SandboxRef || cur.Placement != f.run.Placement {
				t.Errorf("the run row moved to %s %s on a failed move", cur.Placement, cur.SandboxRef)
			}
		})
	}
	// A lapsed hold: the executor stops at its next stamp and writes nothing more; the sweep fails the row.
	t.Run("hold lapsed", func(t *testing.T) {
		f := newMoveFixture(t)
		f.fk.onStop = func() { f.ms.skew = 10 * time.Minute }
		if code, body := f.move(t, "org"); code != http.StatusAccepted {
			t.Fatalf("move = %d %s", code, body)
		}
		if mv := f.only(t); !mv.State.InFlight() || f.mr.exportCount() != 0 {
			t.Fatalf("lapsed executor left %s with %d exports, want in flight and none", mv.State, f.mr.exportCount())
		}
		if err := f.srv.sweepRunMoves(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mv := f.only(t); mv.State != types.MoveFailed || mv.Failure != reasonMoveOrphaned {
			t.Errorf("swept row = %s %q, want failed move_orphaned", mv.State, mv.Failure)
		}
		if f.moveRows("failure") != 1 {
			t.Errorf("run.move failure rows = %d, want 1", f.moveRows("failure"))
		}
	})
	// Every stream rule's own reason, and a broken stream otherwise.
	for sentinel, want := range map[error]string{
		movepack.ErrArchiveRefused: reasonMoveArchiveRefused, movepack.ErrCarriedTooLarge: reasonMoveCarriedTooLarge,
		movepack.ErrDigestMismatch: reasonMoveDigestMismatch, movepack.ErrScanUnreadable: reasonMoveScanUnreadable,
		movepack.ErrGitOutsideRoots: reasonMoveGitOutsideRoots, movepack.ErrValueFound: reasonMoveScanHit,
		io.ErrUnexpectedEOF: reasonMoveRelayFailed,
	} {
		if got := moveRelayReason(errors.Join(errors.New("wrapped"), sentinel)); got != want {
			t.Errorf("moveRelayReason(%v) = %q, want %q", sentinel, got, want)
		}
	}
	// A stream that broke is the relay's failure even where the reader names a refused entry; an import that
	// ended first is the import's.
	refused := fmt.Errorf("%w: entry 3", movepack.ErrArchiveRefused)
	if got := moveRelayReason(moveStreamCause(refused, errors.New("connection reset"), nil)); got != reasonMoveRelayFailed {
		t.Errorf("source stream broke: %q, want %s", got, reasonMoveRelayFailed)
	}
	if got := moveStreamCause(refused, nil, fmt.Errorf("pipe: %w", errMoveImportEnded)); !errors.Is(got, errMoveImportEnded) {
		t.Errorf("import ended first: %v, want errMoveImportEnded", got)
	}
	if got := moveRelayReason(moveStreamCause(refused, nil, nil)); got != reasonMoveArchiveRefused {
		t.Errorf("refused entry: %q, want %s", got, reasonMoveArchiveRefused)
	}
	// The destination runner refuses the moved run's lease: the row ends failed, nothing exported.
	t.Run("lease refused", func(t *testing.T) {
		f := newMoveFixture(t)
		ctx := context.Background()
		rid := f.addRunner(t, f.run.CreatedBy, types.RunnerOwnerPerson, moveCapsAll)
		rn, _ := f.ms.GetRunner(ctx, rid)
		mv, err := f.ms.AdmitRunMove(ctx, types.RunMove{RunID: f.run.ID, ToPlacement: types.PlacementLocal, ToRunnerID: &rid}, store.RunMoveLimits{})
		if err != nil {
			t.Fatal(err)
		}
		x := &moveExec{s: f.srv, ms: f.ms, mv: mv, run: f.current(t), dest: moveDest{Placement: types.PlacementLocal, RunnerID: &rid, Runner: rn},
			maxBytes: 1 << 20, actorType: types.ActorHuman, principal: f.run.CreatedBy}
		end := x.carry(ctx)
		if end == nil || end.reason != reasonMoveLeaseRefused {
			t.Fatalf("carry = %+v, want move_lease_refused", end)
		}
		x.end(ctx, end)
		if mv := f.only(t); mv.State != types.MoveFailed || mv.Failure != reasonMoveLeaseRefused || f.moveRows("failure") != 1 {
			t.Errorf("row = %s %q with %d failure rows, want failed move_lease_refused and 1", mv.State, mv.Failure, f.moveRows("failure"))
		}
		if f.mr.exportCount() != 0 || f.mr.importCount() != 0 {
			t.Errorf("exports %d imports %d, want none", f.mr.exportCount(), f.mr.importCount())
		}
	})
	// The run's earlier moves cannot be read under the lock: the stop is refused, never skipped.
	t.Run("earlier moves unreadable", func(t *testing.T) {
		f := newMoveFixture(t)
		ctx := context.Background()
		mv, err := f.ms.AdmitRunMove(ctx, types.RunMove{RunID: f.run.ID}, store.RunMoveLimits{})
		if err != nil {
			t.Fatal(err)
		}
		x := &moveExec{s: f.srv, ms: &moveListFails{moveStore: f.ms}, mv: mv, run: f.current(t), maxBytes: 1 << 20}
		if end := x.stopLocked(ctx); end == nil || end.reason != reasonMoveStopFailed || f.fk.count("stop") != 0 {
			t.Errorf("stop = %+v with %d stops, want move_stop_failed and none", end, f.fk.count("stop"))
		}
	})
}

// moveOwedSettle holds a settle record for every run, so the locked sequence settles the stopped sandbox.
type moveOwedSettle struct{ *moveStore }

func (s *moveOwedSettle) PauseSettleFor(context.Context, uuid.UUID) (time.Time, error) {
	return time.Now().Add(-time.Minute), nil
}

// moveFreezeFails is a runner whose sandbox cannot be moved to its pause state.
type moveFreezeFails struct{ *moveRunner }

func (r *moveFreezeFails) FreezeSandbox(context.Context, string) error {
	return errors.New("the runtime refused the freeze")
}
func (r *moveFreezeFails) ThawSandbox(context.Context, string) error {
	return errors.New("the runtime refused the thaw")
}

// moveMintRefused is an identity provider that mints no run identity.
type moveMintRefused struct{ identity.Provider }

func (m *moveMintRefused) MintRunIdentity(context.Context, uuid.UUID, string, string, string, bool) (identity.RunIdentity, error) {
	return identity.RunIdentity{}, errors.New("the signer is unavailable")
}

// moveListFails is a move store whose list of a run's moves fails.
type moveListFails struct{ *moveStore }

func (s *moveListFails) ListRunMoves(context.Context, uuid.UUID) ([]types.RunMove, error) {
	return nil, errors.New("the move store is unreachable")
}

// TestMoveExec_SweepSparesLiveSandbox: a failed move whose destination the run now runs on (a retry on the same
// runner answered the same sandbox) and a committed move whose source is the run's sandbox again are each marked
// done with no kill; a new move is refused while an earlier one still has a sandbox to remove.
func TestMoveExec_SweepSparesLiveSandbox(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	failed, err := f.ms.AdmitRunMove(ctx, types.RunMove{RunID: f.run.ID}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	failed.ToSandboxRef = "ref-retried"
	if _, err := f.ms.SetRunMoveDestination(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ms.EndRunMove(ctx, failed.ID, types.MoveFailed, reasonMoveImportFailed, types.MoveCleanupPending); err != nil {
		t.Fatal(err)
	}
	code, body := f.move(t, "org")
	if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveBusy {
		t.Errorf("move with cleanup pending = %d %s, want 409 move_busy", code, body)
	}
	if f.fk.count("stop") != 0 || len(f.moveRowsAll(t)) != 1 {
		t.Errorf("stops %d rows %d, want none and the earlier row only", f.fk.count("stop"), len(f.moveRowsAll(t)))
	}
	f.ms.mu.Lock()
	f.ms.run.SandboxRef = "ref-retried"
	f.ms.mu.Unlock()
	if err := f.srv.sweepRunMoves(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.mr.killed(), "ref-retried") {
		t.Errorf("the sweep removed the run's live sandbox (kills %v)", f.mr.killed())
	}
	if mv, _ := f.ms.GetRunMove(ctx, failed.ID); mv.Cleanup != types.MoveCleanupDone {
		t.Errorf("failed move cleanup = %s, want done", mv.Cleanup)
	}
	// A committed move whose source sandbox the run runs on again.
	back, err := f.ms.AdmitRunMove(ctx, types.RunMove{RunID: f.run.ID, FromSandboxRef: "ref-retried"}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	back.ToSandboxRef = "ref-other"
	if _, err := f.ms.SetRunMoveDestination(ctx, back); err != nil {
		t.Fatal(err)
	}
	f.ms.mmu.Lock()
	if m := f.ms.find(back.ID); m != nil {
		m.State, m.Cleanup, m.ReleasedAt = types.MoveCommitted, types.MoveCleanupPending, new(time.Time)
	}
	f.ms.mmu.Unlock()
	if err := f.srv.sweepRunMoves(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.mr.killed(), "ref-retried") {
		t.Errorf("the sweep removed the run's live sandbox as a source (kills %v)", f.mr.killed())
	}
	if mv, _ := f.ms.GetRunMove(ctx, back.ID); mv.Cleanup != types.MoveCleanupDone {
		t.Errorf("committed move cleanup = %s, want done", mv.Cleanup)
	}
}

// TestMoveAdmit_SamePlaceRefused: a destination that is where the run runs now is refused at step 1, with no row
// and nothing stopped; the same runner, or the same executor of the organisation's, is the same place.
func TestMoveAdmit_SamePlaceRefused(t *testing.T) {
	f := newMoveFixture(t)
	f.ms.mu.Lock()
	f.ms.run.RunnerPoolExecutor = ""
	f.ms.mu.Unlock()
	code, body := f.move(t, "org")
	if reason, layer := moveAnswer(t, body); code != http.StatusForbidden || reason != reasonMoveDestinationRefused || layer != "same_place" {
		t.Errorf("move to the run's own executor = %d %s, want 403 move_destination_refused same_place", code, body)
	}
	if f.fk.count("stop") != 0 || len(f.moveRowsAll(t)) != 0 || f.moveRows("denied") != 1 {
		t.Errorf("stops %d rows %d denied %d, want 0, 0 and 1", f.fk.count("stop"), len(f.moveRowsAll(t)), f.moveRows("denied"))
	}
	a, b := uuid.New(), uuid.New()
	for name, c := range map[string]struct {
		run  types.AgentRun
		dest moveDest
		same bool
	}{
		"same runner":            {types.AgentRun{RunnerID: &a}, moveDest{Placement: types.PlacementLocal, RunnerID: &a}, true},
		"another runner":         {types.AgentRun{RunnerID: &a}, moveDest{Placement: types.PlacementLocal, RunnerID: &b}, false},
		"runner to org":          {types.AgentRun{RunnerID: &a}, moveDest{Placement: types.PlacementRemote}, false},
		"org to runner":          {types.AgentRun{}, moveDest{Placement: types.PlacementLocal, RunnerID: &a}, false},
		"default executor":       {types.AgentRun{}, moveDest{Placement: types.PlacementRemote}, true},
		"same named executor":    {types.AgentRun{RunnerPoolExecutor: "kind-a"}, moveDest{Placement: types.PlacementRemote, Executor: "kind-a"}, true},
		"another named executor": {types.AgentRun{RunnerPoolExecutor: "kind-a"}, moveDest{Placement: types.PlacementRemote}, false},
	} {
		if got := moveSamePlace(c.run, c.dest); got != c.same {
			t.Errorf("%s: same place %v, want %v", name, got, c.same)
		}
	}
}

// TestMoveAdmit_StepOneRefusals: each step 1 term refuses before any run_moves row is written and before anything
// is stopped, with one run.move denied row.
func TestMoveAdmit_StepOneRefusals(t *testing.T) {
	for _, c := range []struct {
		name, to, reason string
		code             int
		setup            func(t *testing.T, f *moveFixture)
	}{
		{"not running", "org", reasonMoveNotRunning, http.StatusConflict, func(_ *testing.T, f *moveFixture) { f.ms.state = types.RunStopped }},
		{"resuming", "org", reasonMoveNotRunning, http.StatusConflict, func(_ *testing.T, f *moveFixture) {
			now := time.Now()
			f.ms.run.PausedAt, f.ms.run.PausedReason = &now, movePauseResuming
		}},
		{"not interactive", "org", reasonMoveNotInteractive, http.StatusConflict, func(_ *testing.T, f *moveFixture) { f.ms.run.Interactive = false }},
		{"provider sign-in", "org", reasonMoveHarnessLogin, http.StatusConflict, func(_ *testing.T, f *moveFixture) { f.ms.run.Task = harnessLoginTask }},
		{"kept machine", "org", reasonMoveKeptMachine, http.StatusConflict, func(_ *testing.T, f *moveFixture) { f.ms.kept = true }},
		{"extra containers", "org", reasonMoveExtraContainers, http.StatusConflict, func(_ *testing.T, f *moveFixture) {
			f.ms.extras = []types.RunExtraContainer{{Name: "db", Image: "postgres:16"}}
		}},
		{"extra containers unreadable", "org", reasonMovePolicyUnreadable, http.StatusServiceUnavailable, func(_ *testing.T, f *moveFixture) {
			f.ms.extrasErr = errors.New("read failed")
		}},
		{"unrecorded", "org", reasonMoveSpecUnrecorded, http.StatusConflict, func(_ *testing.T, f *moveFixture) { delete(f.ms.specs, f.run.ID) }},
		{"resident value", "org", reasonMoveResidentCredential, http.StatusForbidden, func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, Resident: true})
		}},
		{"mcp token", "org", reasonMoveMCPToken, http.StatusForbidden, func(t *testing.T, f *moveFixture) {
			f.record(t, runSandboxSpecRecord{Image: "img", Interactive: true, MCPToken: true})
		}},
		{"pause refused", "org", reasonMovePauseRefused, http.StatusConflict, func(_ *testing.T, f *moveFixture) {
			f.fk.stopRefusal = "This server cannot record a person's pause."
		}},
		{"move in flight", "org", reasonMoveBusy, http.StatusConflict, func(t *testing.T, f *moveFixture) {
			if _, err := f.ms.AdmitRunMove(context.Background(), types.RunMove{RunID: f.run.ID}, store.RunMoveLimits{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"site", "site:berlin", reasonMoveSitesOff, http.StatusConflict, nil},
		{"unknown runner", "runner:" + uuid.NewString(), reasonMoveDestinationUnknown, http.StatusUnprocessableEntity, nil},
		{"unparsable", "somewhere", reasonMoveDestinationUnknown, http.StatusUnprocessableEntity, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMoveFixture(t)
			before := 0
			if c.setup != nil {
				f.ms.mu.Lock()
				c.setup(t, f)
				f.ms.mu.Unlock()
				moves, _ := f.ms.ListRunMoves(context.Background(), f.run.ID)
				before = len(moves)
			}
			code, body := f.move(t, c.to)
			if reason, _ := moveAnswer(t, body); code != c.code || reason != c.reason {
				t.Fatalf("move = %d %s, want %d %s", code, body, c.code, c.reason)
			}
			if moves, _ := f.ms.ListRunMoves(context.Background(), f.run.ID); len(moves) != before {
				t.Errorf("run_moves rows = %d, want %d: step 1 writes none", len(moves), before)
			}
			if f.fk.count("stop") != 0 || f.fk.count("stamp") != 0 || f.mr.exportCount() != 0 {
				t.Errorf("stop %d stamp %d exports %d, want nothing done", f.fk.count("stop"), f.fk.count("stamp"), f.mr.exportCount())
			}
			if f.moveRows("denied") != 1 {
				t.Errorf("run.move denied rows = %d, want 1", f.moveRows("denied"))
			}
		})
	}
}

// TestMoveUnbound_ResentOnReady: a runner's ready re-sends the notice of every committed move off it whose cleanup
// is pending, and nothing for another runner; the Ready hook starts the re-send.
func TestMoveUnbound_ResentOnReady(t *testing.T) {
	f := newMoveFixture(t)
	src, other := uuid.New(), uuid.New()
	mv, err := f.ms.AdmitRunMove(context.Background(), types.RunMove{RunID: f.run.ID, FromRunnerID: &src, FromSandboxRef: "ref-src"}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	f.ms.mmu.Lock()
	row := f.ms.find(mv.ID)
	row.State, row.Cleanup = types.MoveCommitted, types.MoveCleanupPending
	f.ms.mmu.Unlock()
	f.srv.resendMoveUnbound(context.Background(), other)
	if len(f.mr.unbound) != 0 {
		t.Fatalf("another runner's ready sent %v", f.mr.unbound)
	}
	f.srv.resendMoveUnbound(context.Background(), src)
	if want := (runner.MoveUnbound{RunID: f.run.ID, Ref: "ref-src"}); len(f.mr.unbound) != 1 || f.mr.unbound[0] != want {
		t.Errorf("notices = %v, want [%v]", f.mr.unbound, want)
	}
	src2, err := os.ReadFile("runner_actions.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src2, []byte("go s.resendMoveUnbound(s.cfg.BaseCtx, id)")) {
		t.Error("runner_actions.go's Ready hook does not re-send the move notices")
	}
}

// TestMoveExec_ExcludedPathsNeverArrive: a path the carried set leaves out never reaches the destination: an
// exporter that honours the set sends none, and a stream that carries one anyway is refused on the way.
func TestMoveExec_ExcludedPathsNeverArrive(t *testing.T) {
	const cred = "home/agent/work/.credentials.json"
	f := newMoveFixture(t)
	f.fk.carried = moveCarriedSet{Roots: []string{"/home/agent/work"}, Exclude: []string{"**/.credentials.json"}}
	f.mr.files = append(f.mr.files, moveReg(cred, `{"token":"x"}`))
	if code, body := f.move(t, "org"); code != http.StatusAccepted || f.only(t).State != types.MoveCommitted {
		t.Fatalf("move = %d %s, %s, want committed", code, body, f.only(t).State)
	}
	if slices.Contains(f.mr.importedPaths(), cred) {
		t.Error("an excluded path reached the destination")
	}

	g := newMoveFixture(t)
	g.fk.carried = f.fk.carried
	g.mr.exportFn = func() (io.ReadCloser, error) {
		var buf bytes.Buffer
		w, err := movepack.NewWriter(&buf, movepack.Set{Roots: []string{"/home/agent/work"}})
		if err != nil {
			return nil, err
		}
		for _, mf := range []moveFile{moveDir("home/agent/work"), moveReg(cred, `{"token":"x"}`)} {
			if err := w.Add(mf.h, strings.NewReader(mf.content)); err != nil {
				return nil, err
			}
		}
		_, err = w.Close()
		return io.NopCloser(&buf), err
	}
	g.move(t, "org")
	if mv := g.only(t); mv.State != types.MoveFailed || mv.Failure != reasonMoveArchiveRefused {
		t.Errorf("row = %s %q, want failed move_archive_refused", mv.State, mv.Failure)
	}
	if slices.Contains(g.mr.importedPaths(), cred) {
		t.Error("an excluded path the source sent reached the destination")
	}
}

// TestMoveExec_KeepsGitAndTranscripts: the work tree's git directory and the agent's transcripts arrive.
func TestMoveExec_KeepsGitAndTranscripts(t *testing.T) {
	f := newMoveFixture(t)
	f.fk.carried = moveCarriedSet{Roots: []string{"/home/agent/work", "/home/agent/.claude/projects"}}
	f.mr.files = append(f.mr.files, moveDir("home/agent/.claude/projects"),
		moveReg("home/agent/.claude/projects/run/session.jsonl", `{"role":"user"}`+"\n"))
	if code, body := f.move(t, "org"); code != http.StatusAccepted || f.only(t).State != types.MoveCommitted {
		t.Fatalf("move = %d %s, want committed", code, body)
	}
	got := f.mr.importedPaths()
	for _, p := range []string{"home/agent/work/.git/HEAD", "home/agent/.claude/projects/run/session.jsonl", "home/agent/work/README.md"} {
		if !slices.Contains(got, p) {
			t.Errorf("%s did not arrive (imported %v)", p, got)
		}
	}
	if head := f.mr.importedData("home/agent/work/.git/HEAD"); head != "ref: refs/heads/main\n" {
		t.Errorf("HEAD after import = %q, want the source's", head)
	}
	if got := f.mr.importedData("home/agent/.claude/projects/run/session.jsonl"); got != `{"role":"user"}`+"\n" {
		t.Errorf("transcript after import = %q, want the source's", got)
	}
	if mv := f.only(t); mv.ManifestSHA256 == "" || mv.Bytes == 0 {
		t.Errorf("committed row manifest %q bytes %d, want both recorded", mv.ManifestSHA256, mv.Bytes)
	}
}

// TestMoveExec_KillDuringMoveTearsDownDestination: a kill during the import wins: the move fails move_run_ended,
// the destination it created is removed and its token revoked, and the run row never names it.
func TestMoveExec_KillDuringMoveTearsDownDestination(t *testing.T) {
	f := newMoveFixture(t)
	f.mr.onImport = func() {
		f.ms.mu.Lock()
		f.ms.state = types.RunKilled
		f.ms.mu.Unlock()
	}
	jtis := &moveJTIs{Provider: f.srv.cfg.Identity}
	f.srv.cfg.Identity = jtis
	f.move(t, "org")
	mv := f.only(t)
	if mv.State != types.MoveFailed || mv.Failure != reasonMoveRunEnded {
		t.Fatalf("row = %s %q, want failed move_run_ended", mv.State, mv.Failure)
	}
	if mv.ToSandboxRef == "" || !slices.Contains(f.mr.killed(), mv.ToSandboxRef) {
		t.Errorf("destination %q not removed (kills %v)", mv.ToSandboxRef, f.mr.killed())
	}
	if mv.Cleanup != types.MoveCleanupDone {
		t.Errorf("cleanup = %s, want done", mv.Cleanup)
	}
	if mv.ToTokenJTI == "" || !slices.Contains(jtis.revoked(), mv.ToTokenJTI) {
		t.Errorf("the destination's create-time token %q was not revoked (revoked %v)", mv.ToTokenJTI, jtis.revoked())
	}
	if cur := f.current(t); cur.SandboxRef == mv.ToSandboxRef {
		t.Error("the killed run's row names the destination")
	}
}

// moveJTIs records the token ids revoked through the identity provider.
type moveJTIs struct {
	identity.Provider
	mu   sync.Mutex
	jtis []string
}

func (m *moveJTIs) RevokeJTI(_ context.Context, jti string, _ uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jtis = append(m.jtis, jti)
	return nil
}

func (m *moveJTIs) revoked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.jtis)
}

// TestMoveExec_RelayWritesNoFile: the relay is an in-memory pipe: a move writes no file in the temp directory, and
// the executor's source calls nothing that creates one.
func TestMoveExec_RelayWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	f := newMoveFixture(t)
	if code, body := f.move(t, "org"); code != http.StatusAccepted || f.only(t).State != types.MoveCommitted {
		t.Fatalf("move = %d %s, want committed", code, body)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the move left %d files in the temp directory", len(left))
	}
	fset := token.NewFileSet()
	for _, name := range []string{"run_move_exec.go", "run_move_dest.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" &&
				slices.Contains([]string{"Create", "CreateTemp", "WriteFile", "OpenFile", "MkdirTemp"}, sel.Sel.Name) {
				t.Errorf("%s: os.%s in the move's executor", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}

// TestMoveExec_RefusesPlainRunnerLink: a move that involves a runner is never carried over a link that is not TLS:
// it fails move_plain_link before the destination is made or anything exported.
func TestMoveExec_RefusesPlainRunnerLink(t *testing.T) {
	f := newMoveFixture(t)
	dest := uuid.New()
	x := &moveExec{s: f.srv, ms: f.ms, run: f.current(t), dest: moveDest{Placement: types.PlacementLocal, RunnerID: &dest},
		maxBytes: 1 << 20}
	for url, plain := range map[string]bool{
		"http://wardyn.corp.example": true, "": true, "https://wardyn.corp.example": false, "http://127.0.0.1:8080": false,
	} {
		f.srv.cfg.RunnerOrgURL = url
		mv, err := f.ms.AdmitRunMove(context.Background(), types.RunMove{RunID: f.run.ID}, store.RunMoveLimits{})
		if err != nil {
			t.Fatal(err)
		}
		x.mv = mv
		end := x.carry(context.Background())
		if got := end != nil && end.reason == reasonMovePlainLink; got != plain {
			t.Errorf("%q: plain-link refusal %v (%+v), want %v", url, got, end, plain)
		}
		if plain && (f.mr.exportCount() != 0 || f.mr.importCount() != 0) {
			t.Errorf("%q: exports %d imports %d, want none", url, f.mr.exportCount(), f.mr.importCount())
		}
		_, _ = f.ms.EndRunMove(context.Background(), mv.ID, types.MoveFailed, "test", types.MoveCleanupNone)
	}
	// The same link is refused at step 1, before anything is admitted or stopped, for a run on a runner.
	g := newMoveFixture(t)
	rid := uuid.New()
	g.ms.mu.Lock()
	g.ms.run.RunnerID, g.ms.run.Placement = &rid, types.PlacementLocal
	g.ms.mu.Unlock()
	g.srv.cfg.RunnerOrgURL = "http://wardyn.corp.example"
	code, body := g.move(t, "org")
	if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMovePlainLink {
		t.Errorf("admission over a plain link = %d %s, want 409 move_plain_link", code, body)
	}
	if g.fk.count("stop") != 0 || len(g.moveRowsAll(t)) != 0 {
		t.Errorf("stops %d rows %d, want none", g.fk.count("stop"), len(g.moveRowsAll(t)))
	}
}
