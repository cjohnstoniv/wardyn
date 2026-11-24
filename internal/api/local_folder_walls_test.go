// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// lfwBoundaryStore answers the Wardyn boundary for the fold's layer reads.
type lfwBoundaryStore struct {
	store.Store
	b   types.Boundary
	err error
}

func (s lfwBoundaryStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, s.err }
func (s lfwBoundaryStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// lfwFold is a create fold over spec's walls and folder, with no boundary.
func lfwFold(spec types.RunPolicySpec, lf *types.RunLocalFolder, place client.Placement) (*runFold, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	return &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), spec: spec,
		req: &createRunRequest{LocalFolder: lf, Placement: place}}, w
}

func refusedWalled(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonLocalFolderWalled) || !strings.Contains(w.Body.String(), want) {
		t.Errorf("refusal = %d %s, want 422 %s with %q", w.Code, w.Body, reasonLocalFolderWalled, want)
	}
}

// stepRegistered reports whether runFoldSteps runs step at every door (foldAll).
func stepRegistered(step func(*Server, *runFold, *foldRecorder) bool) bool {
	for _, st := range runFoldSteps {
		if reflect.ValueOf(st.run).Pointer() == reflect.ValueOf(step).Pointer() {
			return st.modes == foldAll
		}
	}
	return false
}

// TestLocalFolderWalls_DenyRemoteEveryDoor: deny_remote refuses a computer folder and a `wardyn sync` folder on
// another machine at create, preflight and preview alike (the step runs at every door), and a `wardyn sync` folder
// placed local is admitted.
func TestLocalFolderWalls_DenyRemoteEveryDoor(t *testing.T) {
	if !stepRegistered((*Server).stepLocalFolderWalls) || !stepRegistered((*Server).stepLocalFolderWallsPlaced) {
		t.Fatal("the walls steps do not run at every door (foldAll)")
	}
	rn := uuid.New()
	spec := types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyRemote: true}}
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		f, w := lfwFold(spec, &types.RunLocalFolder{Name: "app", RunnerID: &rn, Direction: types.ComputerFolderIn}, client.PlacementLocal)
		f.mode = mode
		if (&Server{}).stepLocalFolderWalls(f, nil) {
			t.Errorf("mode %v: a computer folder was admitted under deny_remote", mode)
		}
		refusedWalled(t, w, "a folder from your computer for a run on another machine is walled off by")
		f, w = lfwFold(spec, &types.RunLocalFolder{Name: "app"}, client.PlacementRemote)
		f.mode = mode
		if (&Server{}).stepLocalFolderWalls(f, nil) {
			t.Errorf("mode %v: a wardyn sync folder on a remote run was admitted", mode)
		}
		refusedWalled(t, w, "--placement local")
		f, _ = lfwFold(spec, &types.RunLocalFolder{Name: "app"}, client.PlacementLocal)
		f.mode = mode
		if !(&Server{}).stepLocalFolderWalls(f, nil) {
			t.Errorf("mode %v: a wardyn sync folder placed local was refused", mode)
		}
	}
}

// TestLocalFolderWalls_DenyRemoteEmptyPlacementRefused: an empty placement is not local, so a `wardyn sync` folder
// under deny_remote is refused with the --placement local hint.
func TestLocalFolderWalls_DenyRemoteEmptyPlacementRefused(t *testing.T) {
	f, w := lfwFold(types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyRemote: true}}, &types.RunLocalFolder{Name: "app"}, "")
	if (&Server{}).stepLocalFolderWalls(f, nil) {
		t.Fatal("an empty placement was admitted under deny_remote")
	}
	refusedWalled(t, w, "; pass --placement local to keep this run on your own computer")
}

// TestLocalFolderWalls_DenyPullRefusesBoth: deny_pull refuses a computer folder that syncs back, and admits one that
// only goes in and a `wardyn sync` folder.
func TestLocalFolderWalls_DenyPullRefusesBoth(t *testing.T) {
	rn := uuid.New()
	spec := types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyPull: true}}
	f, w := lfwFold(spec, &types.RunLocalFolder{Name: "app", RunnerID: &rn, Direction: types.ComputerFolderBoth}, client.PlacementLocal)
	if (&Server{}).stepLocalFolderWalls(f, nil) {
		t.Fatal("a computer folder both ways was admitted under deny_pull")
	}
	refusedWalled(t, w, "bringing the sandbox's changes back to your computer is walled off by")
	for name, lf := range map[string]*types.RunLocalFolder{
		"in only":     {Name: "app", RunnerID: &rn, Direction: types.ComputerFolderIn},
		"wardyn sync": {Name: "app"},
	} {
		if f, w := lfwFold(spec, lf, client.PlacementLocal); !(&Server{}).stepLocalFolderWalls(f, nil) {
			t.Errorf("%s: refused under deny_pull: %d %s", name, w.Code, w.Body)
		}
	}
}

// TestLocalFolderWalls_RefusalNamesLayer: every refusal names the layer that set that wall, and "locked" when the
// knob is locked; with two layers each wall names its own.
func TestLocalFolderWalls_RefusalNamesLayer(t *testing.T) {
	rn := uuid.New()
	both := &types.RunLocalFolder{Name: "app", RunnerID: &rn, Direction: types.ComputerFolderBoth}
	unit := &ResolvedUnit{ID: uuid.New(), Name: "eng"}
	hard := func(lf *types.LocalFolderRules) types.Boundary {
		return types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{LocalFolders: lf}}
	}
	for _, tc := range []struct {
		name     string
		boundary types.Boundary
		unit     *types.LocalFolderRules
		locked   bool
		want     string
	}{
		{"boundary", hard(&types.LocalFolderRules{DenyPull: true}), nil, false, "walled off by your organisation's boundary"},
		{"a unit", types.Boundary{}, &types.LocalFolderRules{DenyPull: true}, false, "walled off by the eng unit"},
		{"a locked unit", types.Boundary{}, &types.LocalFolderRules{DenyPull: true}, true, "walled off by the eng unit (locked)"},
		{"an inline policy", types.Boundary{}, nil, false, "walled off by this run's policy"},
		{"a no_limits boundary is no layer", types.Boundary{Mode: types.BoundaryNoLimits, Spec: types.BoundarySpec{LocalFolders: &types.LocalFolderRules{DenyPull: true}}}, nil, false, "walled off by this run's policy"},
	} {
		f, w := lfwFold(types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyPull: true}}, both, client.PlacementLocal)
		u := *unit
		u.Ceiling.LocalFolders = tc.unit
		f.ceiling.Unit = &u
		if tc.locked {
			f.source.knobs = knobProvenance{"local_folders": {Layer: policyref.LayerUnit, Name: "eng", Locked: true}}
		}
		s := &Server{cfg: Config{Store: lfwBoundaryStore{b: tc.boundary}}}
		if s.stepLocalFolderWalls(f, nil) {
			t.Errorf("%s: admitted", tc.name)
			continue
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %s, want %q", tc.name, w.Body, tc.want)
		}
	}

	// Two layers: the boundary sets deny_pull, a unit adds max_push_mib.
	f, w := lfwFold(types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 10}}, both, client.PlacementLocal)
	u := *unit
	u.Ceiling.LocalFolders = &types.LocalFolderRules{MaxPushMiB: 10}
	f.ceiling.Unit = &u
	s := &Server{cfg: Config{Store: lfwBoundaryStore{b: hard(&types.LocalFolderRules{DenyPull: true})}}}
	if s.stepLocalFolderWalls(f, nil) || !strings.Contains(w.Body.String(), "walled off by your organisation's boundary") {
		t.Errorf("two layers: the deny_pull refusal = %s, want the boundary named", w.Body)
	}
	capText := localFolderCapText(10<<20, "push", f.source.localFolderLayers[wallMaxPush])
	if !strings.HasSuffix(capText, "set by the eng unit") {
		t.Errorf("two layers: the cap's sentence = %q, want the eng unit named", capText)
	}
}

// TestLocalFolderWallLayers_OutermostSetter: each wall names the outermost layer whose own local_folders sets it as
// the run holds it; a cap a layer sets larger than the run's did not set it.
func TestLocalFolderWallLayers_OutermostSetter(t *testing.T) {
	run := &types.LocalFolderRules{DenyRemote: true, DenyPull: true, MaxPushMiB: 10, MaxPullMiB: 20}
	f, _ := lfwFold(types.RunPolicySpec{LocalFolders: run}, nil, "")
	f.ceiling.Unit = &ResolvedUnit{ID: uuid.New(), Name: "eng", Ceiling: types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 10, MaxPullMiB: 40}}}
	s := &Server{cfg: Config{Store: lfwBoundaryStore{b: types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{
		LocalFolders: &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 50},
	}}}}}
	got, err := s.localFolderWallLayers(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]types.LocalFolderWallLayer{
		wallDenyRemote: {Layer: policyref.LayerMember},   // only the run's own policy sets it
		wallDenyPull:   {Layer: policyref.LayerBoundary}, // the boundary and the unit: the outermost
		wallMaxPush:    {Layer: policyref.LayerUnit, Name: "eng"},
		wallMaxPull:    {Layer: policyref.LayerMember}, // the unit's 40 is not the run's 20
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("layers = %+v, want %+v", got, want)
	}
	f.ceiling.Unit = nil
	s.cfg.Store = lfwBoundaryStore{err: errors.New("database down")}
	if _, err := s.localFolderWallLayers(context.Background(), f); err == nil {
		t.Error("a boundary that cannot be read gave layers")
	}
}

// TestLocalFolderWalls_DenyRemotePlacedOnOrgRunnerRefused: a `wardyn sync` folder under deny_remote is refused when
// placement's targets include an organisation's runner or a remote-hosted one, and admitted on the person's own.
func TestLocalFolderWalls_DenyRemotePlacedOnOrgRunnerRefused(t *testing.T) {
	mine, org := uuid.New(), uuid.New()
	rs := lfwRunnerStore{runners: map[uuid.UUID]types.Runner{
		mine: {ID: mine, Owner: adminTokenPrincipal, OwnerKind: types.RunnerOwnerPerson},
		org:  {ID: org, OwnerKind: types.RunnerOwnerOrg},
	}}
	spec := types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{DenyRemote: true}}
	for _, tc := range []struct {
		name    string
		place   runPlacement
		refused bool
	}{
		{"an organisation's runner among them", runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{
			{runnerID: &mine, ownerKind: types.RunnerOwnerPerson}, {runnerID: &org, ownerKind: types.RunnerOwnerOrg}}}, true},
		{"a remote-hosted target", runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{executor: "docker"}}}, true},
		{"a pool that decided remote", runPlacement{pooled: true, placement: types.PlacementRemote}, true},
		{"only the person's own", runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{
			{runnerID: &mine, ownerKind: types.RunnerOwnerPerson}}}, false},
	} {
		f, w := lfwFold(spec, &types.RunLocalFolder{Name: "app"}, client.PlacementLocal)
		f.place = tc.place
		if got := !(&Server{cfg: Config{Store: rs}}).stepLocalFolderWallsPlaced(f, nil); got != tc.refused {
			t.Errorf("%s: refused %v (%d %s), want %v", tc.name, got, w.Code, w.Body, tc.refused)
		} else if got {
			refusedWalled(t, w, "a folder from your computer for a run on another machine")
		}
	}
}

type lfwRunnerStore struct {
	store.Store
	runners map[uuid.UUID]types.Runner
}

func (s lfwRunnerStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if r, ok := s.runners[id]; ok {
		return r, nil
	}
	return types.Runner{}, store.ErrNotFound
}

// lfwStore keeps one walls record (store.RunLocalFolderWallsStore); err fails every read.
type lfwStore struct {
	store.Store
	rec     types.LocalFolderWallsRecord
	err     error
	keepErr error
}

func (s lfwStore) KeepRunLocalFolderWalls(context.Context, uuid.UUID, types.LocalFolderWallsRecord) error {
	return s.keepErr
}
func (s lfwStore) RunLocalFolderWalls(context.Context, uuid.UUID) (types.LocalFolderWallsRecord, error) {
	return s.rec, s.err
}

// TestLocalFolderWalls_NoRowIsNoWalls: a run with no walls record (NULL) has no walls: nothing refuses, the sync
// view carries only WARDYN_SYNC_DENY, and the pass back is not walled.
func TestLocalFolderWalls_NoRowIsNoWalls(t *testing.T) {
	s := &Server{cfg: Config{Store: lfwStore{}, SyncDeny: []string{"*.key"}}}
	id := uuid.New()
	v := s.runSyncViewFor(context.Background(), id)
	if !slices.Equal(v.Deny, []string{"*.key"}) || v.DenyPull || v.MaxPushBytes != 0 || v.MaxPullBytes != 0 || v.WallLayers != nil {
		t.Errorf("sync view = %+v, want WARDYN_SYNC_DENY alone", v)
	}
	if walled, err := s.folderPullWalled(context.Background(), id); walled || err != nil {
		t.Errorf("folderPullWalled = %v, %v; want false, nil", walled, err)
	}
	if v := s.folderWallsView(context.Background(), id); v != nil {
		t.Errorf("walls view = %+v, want none", v)
	}
	if rec, msg := s.syncWalls(context.Background(), types.AgentRun{ID: id}, "alice"); msg != "" || rec.Walls != nil {
		t.Errorf("syncWalls = %+v %q, want no walls and no refusal", rec, msg)
	}
	// A store that keeps no walls has none either.
	if rec, err := (&Server{}).runLocalFolderWalls(context.Background(), id); err != nil || rec.Walls != nil {
		t.Errorf("no store: %+v, %v", rec, err)
	}
}

// TestLocalFolderWalls_UnreadRefusesSync: walls that cannot be read refuse the sync channel with the unreadable
// sentence, launch no sftp-server, and record a failed transfer.
func TestLocalFolderWalls_UnreadRefusesSync(t *testing.T) {
	d := newLFWSSH(t, types.LocalFolderWallsRecord{}, errors.New("database down"), "", nil, types.Runner{})
	sess, stderr, in, _ := d.openSync(t, "push")
	_ = in.Close()
	if err := sess.Wait(); err == nil {
		t.Error("the sync ended 0, want a refusal")
	}
	if got, _ := io.ReadAll(stderr); !strings.Contains(string(got), localFolderWallsUnreadableMsg) {
		t.Errorf("stderr = %q, want %q", got, localFolderWallsUnreadableMsg)
	}
	if argv, _ := d.fr.lastCall(); argv != nil {
		t.Errorf("sftp-server launched with %v", argv)
	}
	if waitForAudit(t, d.h.audit, d.run.ID, "ssh.sync.transfer", "failure") == nil {
		t.Errorf("no failed transfer row; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
}

// TestLocalFolderWalls_DenyUnionOver64Refused: a policy over 64 patterns is refused at write, a union with the
// deployment's over 64 is refused by the check and at serve time (the sync view serves WARDYN_SYNC_DENY alone and the
// channel refuses); nothing is truncated.
func TestLocalFolderWalls_DenyUnionOver64Refused(t *testing.T) {
	pats := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = prefix + strings.Repeat("x", i+1)
		}
		return out
	}
	if err := validateLocalFolders(&types.LocalFolderRules{Deny: pats("a", 65)}); err == nil || !strings.Contains(err.Error(), "65 patterns") {
		t.Errorf("65 patterns at write = %v, want refused", err)
	}
	deploy := pats("d", 60)
	run := append(pats("r", 5), deploy[0]) // one shared pattern counts once
	if err := checkLocalFolderDeny(deploy, run); err == nil || err.Error() != "local_folders.deny: 65 patterns with the deployment's, at most 64" {
		t.Errorf("union check = %v", err)
	}
	if err := checkLocalFolderDeny(deploy, run[1:]); err != nil {
		t.Errorf("a union of 64 = %v, want admitted", err)
	}
	s := &Server{cfg: Config{SyncDeny: deploy, Store: lfwStore{rec: types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true, Deny: run}}}}}
	if v := s.runSyncViewFor(context.Background(), uuid.New()); !slices.Equal(v.Deny, deploy) || v.DenyPull {
		t.Errorf("sync view = %d patterns, deny_pull %v; want WARDYN_SYNC_DENY alone", len(v.Deny), v.DenyPull)
	}
	if _, msg := s.syncWalls(context.Background(), types.AgentRun{ID: uuid.New()}, "alice"); !strings.Contains(msg, "65 patterns") {
		t.Errorf("syncWalls = %q, want the union refused", msg)
	}
}

// TestLocalFolderWalls_DenyPatternWithComma: patterns are checked one by one, never joined, so a comma is part of a
// pattern; a control character or an over-long pattern is refused.
func TestLocalFolderWalls_DenyPatternWithComma(t *testing.T) {
	got, err := servedSyncDeny([]string{"*.key"}, []string{"a,b", "*.key", "c"})
	if err != nil || !slices.Equal(got, []string{"*.key", "a,b", "c"}) {
		t.Errorf("served = %v, %v; want the union with a,b whole", got, err)
	}
	for _, bad := range []string{"a\nb", strings.Repeat("x", 257), "\xff"} {
		if err := validateLocalFolders(&types.LocalFolderRules{Deny: []string{bad}}); err == nil {
			t.Errorf("pattern %q admitted", bad)
		}
	}
	for _, c := range []int{-1, maxLocalFolderCapMiB + 1} {
		if err := validateLocalFolders(&types.LocalFolderRules{MaxPullMiB: c}); err == nil {
			t.Errorf("cap %d admitted", c)
		}
	}
	if err := validatePolicySpec(types.RunPolicySpec{LocalFolders: &types.LocalFolderRules{MaxPushMiB: 1, Deny: []string{"a,b"}}}); err != nil {
		t.Errorf("a valid local_folders = %v", err)
	}
}

// TestLocalFolderByteCap_EndsAtCap: the reader passes exactly cap bytes; the byte after ends it once. The size-cap
// sentence names the cap in the console's form and has no local_folder: prefix.
func TestLocalFolderByteCap_EndsAtCap(t *testing.T) {
	const limit = 1000
	for _, n := range []int{limit - 1, limit, limit + 1} {
		overs := 0
		c := &localFolderByteCap{r: bytes.NewReader(make([]byte, n)), left: limit, over: func() { overs++ }}
		got, err := io.ReadAll(c)
		if n <= limit && (err != nil || len(got) != n || overs != 0) {
			t.Errorf("%d bytes: read %d, %v, overs %d; want all and no cap", n, len(got), err, overs)
		}
		if n > limit && (!errors.Is(err, errLocalFolderCap) || len(got) != limit || overs != 1) {
			t.Errorf("%d bytes: read %d, %v, overs %d; want %d and the cap once", n, len(got), err, overs, limit)
		}
		if n > limit {
			if _, err := c.Read(make([]byte, 8)); !errors.Is(err, errLocalFolderCap) || overs != 1 {
				t.Errorf("a read after the cap = %v, overs %d", err, overs)
			}
		}
	}
	msg := localFolderCapText(100<<20, "push", types.LocalFolderWallLayer{Layer: policyref.LayerBoundary})
	if msg != "This sync session reached its 100.0 MB limit for sending in, set by your organisation's boundary" || strings.Contains(msg, "local_folder:") {
		t.Errorf("cap sentence = %q", msg)
	}
	if got := consoleBytes(1536); got != "1.5 KB" {
		t.Errorf("consoleBytes(1536) = %q", got)
	}
}

// lfwArgvRunner records the argv of every sandbox exec.
type lfwArgvRunner struct {
	runner.Runner
	mu   sync.Mutex
	argv [][]string
}

func (r *lfwArgvRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	r.argv = append(r.argv, slices.Clone(spec.Argv))
	r.mu.Unlock()
	return r.Runner.ExecStream(ctx, ref, spec)
}

func (r *lfwArgvRunner) sftpArgv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.argv) - 1; i >= 0; i-- {
		if slices.Contains(r.argv[i], "-d") {
			return r.argv[i]
		}
	}
	return nil
}

// lfwFolderStore is a computer-folder store that keeps a walls record.
type lfwFolderStore struct {
	*computerFolderStore
	rec types.LocalFolderWallsRecord
	err error
}

func (s *lfwFolderStore) KeepRunLocalFolderWalls(context.Context, uuid.UUID, types.LocalFolderWallsRecord) error {
	return nil
}
func (s *lfwFolderStore) RunLocalFolderWalls(context.Context, uuid.UUID) (types.LocalFolderWallsRecord, error) {
	return s.rec, s.err
}

// TestComputerFolderPass_DenyIsUnion: a computer-folder pass is sent WARDYN_SYNC_DENY with the run's patterns.
func TestComputerFolderPass_DenyIsUnion(t *testing.T) {
	s, cs, run, r := scriptedFolderFixture(t, runnerwire.FolderPass{})
	s.cfg.SyncDeny = []string{"*.key"}
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, rec: types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{Deny: []string{"a,b", "*.key"}}}}
	if _, _, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, false, passGuard{}); err != nil || !settled {
		t.Fatalf("computerFolderPass: settled %v, err %v", settled, err)
	}
	if got := r.args().Deny; !slices.Equal(got, []string{"*.key", "a,b"}) {
		t.Errorf("the runner was sent deny %v, want the union", got)
	}
}

// TestComputerFolderPass_DenyPullStartsReadDenied: under deny_pull every pass starts its sftp-server read-closed,
// push included, and a pass back is refused, settled, with the console's sentence.
func TestComputerFolderPass_DenyPullStartsReadDenied(t *testing.T) {
	s, cs, run, _ := scriptedFolderFixture(t, runnerwire.FolderPass{})
	ar := &lfwArgvRunner{Runner: s.cfg.Runner}
	s.cfg.Runner = ar
	rec := types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true}, Layers: map[string]types.LocalFolderWallLayer{wallDenyPull: {Layer: policyref.LayerBoundary}}}
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, rec: rec}
	if _, _, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, false, passGuard{}); err != nil || !settled {
		t.Fatalf("push pass: settled %v, err %v", settled, err)
	}
	if got := ar.sftpArgv(); !slices.Equal(got[len(got)-2:], []string{"-P", sftpServerDeny}) {
		t.Errorf("push pass argv = %v, want -P %s", got, sftpServerDeny)
	}
	_, _, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, true, passGuard{})
	if err == nil || !settled || err.Error() != localFolderBackText(rec) {
		t.Errorf("pull pass: settled %v, err %v; want settled with %q", settled, err, localFolderBackText(rec))
	}
}

// TestComputerFolderPass_UnreadWallsNotSettled: walls that cannot be read make a pass not settled, so it is tried
// again and the sandbox kept; no sftp-server starts.
func TestComputerFolderPass_UnreadWallsNotSettled(t *testing.T) {
	s, cs, run, _ := scriptedFolderFixture(t, runnerwire.FolderPass{})
	ar := &lfwArgvRunner{Runner: s.cfg.Runner}
	s.cfg.Runner = ar
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, err: errors.New("database down")}
	_, _, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, true, passGuard{})
	if err == nil || settled {
		t.Errorf("computerFolderPass = settled %v, err %v; want an error, not settled", settled, err)
	}
	if got := ar.sftpArgv(); got != nil {
		t.Errorf("sftp-server started with %v", got)
	}
	if cs.status.Back != nil {
		t.Errorf("a Back record was written: %+v", cs.status.Back)
	}
}

// TestFolderBackEligible_DenyPull: a run otherwise owed a pass back is not eligible under deny_pull.
func TestFolderBackEligible_DenyPull(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	if got, err := s.folderBackEligible(context.Background(), run.ID); err != nil || !got {
		t.Fatalf("with no walls: eligible %v, %v; want true", got, err)
	}
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, rec: types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true}}}
	if got, err := s.folderBackEligible(context.Background(), run.ID); err != nil || got {
		t.Errorf("under deny_pull: eligible %v, %v; want false", got, err)
	}
}

// TestFolderBackEligible_UnreadIsError: walls that cannot be read are an error, never "not eligible", so the sandbox
// is kept for a later sweep.
func TestFolderBackEligible_UnreadIsError(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, err: errors.New("database down")}
	if got, err := s.folderBackEligible(context.Background(), run.ID); err == nil || got {
		t.Errorf("eligible %v, %v; want an error", got, err)
	}
}

// TestComputerFolderRunView_WallsLines: the folder view's walls say the pass back is walled (with the deny_pull layer
// and the server's sentence, no local_folder: prefix), or that the walls could not be read; with no walls, none.
func TestComputerFolderRunView_WallsLines(t *testing.T) {
	s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if v := s.computerFolderRunView(context.Background(), run.ID); v == nil || v.Walls != nil {
		t.Fatalf("no walls: view %+v, want no walls line", v)
	}
	l := types.LocalFolderWallLayer{Layer: policyref.LayerUnit, Name: "eng", Locked: true}
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, rec: types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true}, Layers: map[string]types.LocalFolderWallLayer{wallDenyPull: l}}}
	v := s.computerFolderRunView(context.Background(), run.ID)
	if v == nil || v.Walls == nil || v.Walls.BackWalled == nil || *v.Walls.BackWalled != l ||
		v.Walls.BackWalledText != "Bringing the sandbox's changes back to your computer is walled off by the eng unit (locked)." {
		t.Errorf("deny_pull: walls %+v", v.Walls)
	}
	s.cfg.Store = &lfwFolderStore{computerFolderStore: cs, err: errors.New("database down")}
	if v := s.computerFolderRunView(context.Background(), run.ID); v == nil || v.Walls == nil || !v.Walls.Unreadable || v.Walls.BackWalled != nil {
		t.Errorf("unreadable: walls %+v", v)
	}
}

// TestLockableKnobs_IncludesLocalFolders: local_folders can be locked.
func TestLockableKnobs_IncludesLocalFolders(t *testing.T) {
	if !lockableKnobs["local_folders"] {
		t.Error("local_folders is not a lockable knob")
	}
}

// lfwDispatchStore is a dispatch store whose walls write fails.
type lfwDispatchStore struct {
	*dispatchTestStore
	keepErr error
	kept    *types.LocalFolderWallsRecord
}

func (s *lfwDispatchStore) KeepRunLocalFolderWalls(_ context.Context, _ uuid.UUID, rec types.LocalFolderWallsRecord) error {
	s.kept = &rec
	return s.keepErr
}
func (s *lfwDispatchStore) RunLocalFolderWalls(context.Context, uuid.UUID) (types.LocalFolderWallsRecord, error) {
	return types.LocalFolderWallsRecord{}, nil
}

// TestKeepRunLocalFolderWalls_FailsTheRunWhenUnstored: walls that cannot be stored stop the lease and fail the run
// before any sandbox, audited by the failed step; no walls writes nothing; an envelope with no layers names the
// knob's layer for every wall.
func TestKeepRunLocalFolderWalls_FailsTheRunWhenUnstored(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &lfwDispatchStore{dispatchTestStore: &dispatchTestStore{run: types.AgentRun{ID: runID, CreatedBy: "t@example.com"}, state: types.RunStarting}}
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	srv := New(cfg)
	run := types.AgentRun{ID: runID}
	src := policyref.Provenance{Layer: policyref.LayerUnit, Name: "eng", Locked: true}

	if !srv.keepRunLocalFolderWalls(context.Background(), run, nil, src, nil, func() { t.Error("no walls stopped the lease") }) || st.kept != nil {
		t.Errorf("no walls wrote %+v", st.kept)
	}
	walls := &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 5}
	if !srv.keepRunLocalFolderWalls(context.Background(), run, walls, src, nil, func() {}) || st.kept == nil {
		t.Fatal("the walls were not stored")
	}
	want := map[string]types.LocalFolderWallLayer{wallDenyPull: {Layer: policyref.LayerUnit, Name: "eng", Locked: true}, wallMaxPush: {Layer: policyref.LayerUnit, Name: "eng", Locked: true}}
	if !reflect.DeepEqual(st.kept.Layers, want) {
		t.Errorf("layers with no envelope map = %+v, want the knob's for each wall", st.kept.Layers)
	}

	st.keepErr = errors.New("database down")
	stopped := false
	if srv.keepRunLocalFolderWalls(context.Background(), run, walls, src, nil, func() { stopped = true }) {
		t.Fatal("an unstored wall let the run start")
	}
	if !stopped {
		t.Error("the lease was not stopped")
	}
	if got := st.State(); got != types.RunFailed {
		t.Errorf("state = %q, want FAILED", got)
	}
	if !strings.Contains(st.FailureHint(), "folder walls could not be stored") {
		t.Errorf("hint = %q", st.FailureHint())
	}
	if !audit.has(runID, "run.create", "failure") {
		t.Error("no run.create failure row")
	}
}

// lfwSSH is an SSH connection to a run whose store keeps a walls record and the run's runner.
type lfwSSH struct {
	h      *sshTestHarness
	fr     *sshFakeRunner
	client *ssh.Client
	run    types.AgentRun
}

type lfwSSHStore struct {
	*sshMemStore
	rec    types.LocalFolderWallsRecord
	recErr error
	runner types.Runner
}

func (s lfwSSHStore) KeepRunLocalFolderWalls(context.Context, uuid.UUID, types.LocalFolderWallsRecord) error {
	return nil
}
func (s lfwSSHStore) RunLocalFolderWalls(context.Context, uuid.UUID) (types.LocalFolderWallsRecord, error) {
	return s.rec, s.recErr
}
func (s lfwSSHStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if s.runner.ID != id {
		return types.Runner{}, store.ErrNotFound
	}
	return s.runner, nil
}

// newLFWSSH dials a run whose walls are rec (recErr fails the read), whose data_out rule is rule, and which mutate
// may place; rn is the runner the store answers.
func newLFWSSH(t *testing.T, rec types.LocalFolderWallsRecord, recErr error, rule types.TransferRule, mutate func(*types.AgentRun), rn types.Runner) *lfwSSH {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	if mutate != nil {
		mutate(&run)
		st.putRun(run)
	}
	st.policy = types.RunPolicySpec{DataOut: rule}
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{
		execFn:   func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeEchoExecSession(), nil },
		attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil },
	}
	h := newSSHTestHarness(t, lfwSSHStore{sshMemStore: st, rec: rec, recErr: recErr, runner: rn}, fr)
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &lfwSSH{h: h, fr: fr, client: client, run: run}
}

// openSync opens a wardyn-sync channel with its stderr readable.
func (d *lfwSSH) openSync(t *testing.T, direction string) (*ssh.Session, io.Reader, io.WriteCloser, io.Reader) {
	t.Helper()
	sess, err := d.client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	_ = sess.Setenv("WARDYN_SYNC_DIR", "/home/agent/work/proj")
	_ = sess.Setenv("WARDYN_SYNC_DIRECTION", direction)
	stderr, _ := sess.StderrPipe()
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem(sshSyncSubsystem); err != nil {
		t.Fatalf("wardyn-sync refused at the request: %v", err)
	}
	return sess, stderr, in, out
}

func (d *lfwSSH) wallDenied(t *testing.T, wall string) *types.AuditEvent {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, ev := range d.h.audit.snapshot() {
			if ev.Action == "run.local_folder.deny" && ev.Outcome == "denied" && strings.Contains(string(ev.Data), `"wall":"`+wall+`"`) {
				return &ev
			}
		}
	}
	return nil
}

func denyRemote() types.LocalFolderWallsRecord {
	return types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyRemote: true}, Layers: map[string]types.LocalFolderWallLayer{wallDenyRemote: {Layer: policyref.LayerBoundary}}}
}

// TestBridgeSSHSync_DenyRemoteRefusesChannel: deny_remote refuses the sync channel of a run on another machine, with
// one run.local_folder.deny row naming the wall and its layer, and no sftp-server.
func TestBridgeSSHSync_DenyRemoteRefusesChannel(t *testing.T) {
	d := newLFWSSH(t, denyRemote(), nil, "", nil, types.Runner{})
	sess, stderr, in, _ := d.openSync(t, "push")
	_ = in.Close()
	if err := sess.Wait(); err == nil {
		t.Error("the sync ended 0")
	}
	if got, _ := io.ReadAll(stderr); !strings.Contains(string(got), "a folder from your computer for a run on another machine is walled off by your organisation's boundary") {
		t.Errorf("stderr = %q", got)
	}
	if argv, _ := d.fr.lastCall(); argv != nil {
		t.Errorf("sftp-server launched with %v", argv)
	}
	if ev := d.wallDenied(t, wallDenyRemote); ev == nil || !strings.Contains(string(ev.Data), `"layer":"boundary"`) {
		t.Errorf("no run.local_folder.deny row naming the boundary; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
}

// TestBridgeSSHSync_DenyRemoteOrgRunnerRefused: placement local on an organisation's runner is not the person's own
// computer, so deny_remote refuses; the creator's own runner is admitted.
func TestBridgeSSHSync_DenyRemoteOrgRunnerRefused(t *testing.T) {
	id := uuid.New()
	local := func(r *types.AgentRun) { r.Placement, r.RunnerID = types.PlacementLocal, &id }
	d := newLFWSSH(t, denyRemote(), nil, "", local, types.Runner{ID: id, OwnerKind: types.RunnerOwnerOrg})
	sess, _, in, _ := d.openSync(t, "push")
	_ = in.Close()
	if err := sess.Wait(); err == nil {
		t.Error("an organisation's runner was admitted")
	}
	if d.wallDenied(t, wallDenyRemote) == nil {
		t.Errorf("no run.local_folder.deny row; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
	own := newLFWSSH(t, denyRemote(), nil, "", local, types.Runner{ID: id, Owner: "alice@example.com", OwnerKind: types.RunnerOwnerPerson})
	_, _, in, out := own.openSync(t, "push")
	syncEcho(t, in, out, "twelve-bytes")
	_ = in.Close()
}

// TestBridgeSSHSync_DenyPullStartsReadDenied: under deny_pull the session's sftp-server starts with -P read,readlink
// whatever data_out is; with no walls it does not.
func TestBridgeSSHSync_DenyPullStartsReadDenied(t *testing.T) {
	pull := types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true}}
	for name, tc := range map[string]struct {
		rec  types.LocalFolderWallsRecord
		want []string
	}{
		"deny_pull": {pull, []string{sftpServerPath, "-e", "-d", "/home/agent/work/proj", "-P", "read,readlink"}},
		"no walls":  {types.LocalFolderWallsRecord{}, []string{sftpServerPath, "-e", "-d", "/home/agent/work/proj"}},
	} {
		d := newLFWSSH(t, tc.rec, nil, types.TransferAllowed, nil, types.Runner{})
		_, _, in, out := d.openSync(t, "push")
		syncEcho(t, in, out, "twelve-bytes")
		if argv, _ := d.fr.lastCall(); !slices.Equal(argv, tc.want) {
			t.Errorf("%s: argv = %v, want %v", name, argv, tc.want)
		}
		_ = in.Close()
	}
}

// TestBridgeSSHSync_DenyPullGuardNamesTheWall: under deny_pull with data_out allowed, the first content reply ends the
// sync with the deny_pull sentence naming its layer and one run.local_folder.deny row, never a data_out row; with
// data_out review the data_out guard answers as before.
func TestBridgeSSHSync_DenyPullGuardNamesTheWall(t *testing.T) {
	rec := types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyPull: true}, Layers: map[string]types.LocalFolderWallLayer{wallDenyPull: {Layer: policyref.LayerUnit, Name: "eng"}}}
	d := newLFWSSH(t, rec, nil, types.TransferAllowed, nil, types.Runner{})
	d.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { return sftpDataThenName(), nil }
	_, stderr, _, out := d.openSync(t, "pull")
	if got, _ := io.ReadAll(out); len(got) != 13 {
		t.Errorf("the client read %d bytes, want only the 13 of the NAME reply", len(got))
	}
	if got, _ := io.ReadAll(stderr); !strings.Contains(string(got), "bringing the sandbox's changes back to your computer is walled off by the eng unit") {
		t.Errorf("stderr = %q", got)
	}
	if d.wallDenied(t, wallDenyPull) == nil {
		t.Errorf("no run.local_folder.deny row; events=%s", auditDump(d.h.audit.snapshot(), d.run.ID))
	}
	if findAudit(d.h.audit.snapshot(), d.run.ID, "run.data_out.deny", "denied") != nil {
		t.Error("a data_out row was written for the walls' guard")
	}

	r := newLFWSSH(t, rec, nil, types.TransferReview, nil, types.Runner{})
	r.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { return sftpDataThenName(), nil }
	_, _, _, out = r.openSync(t, "pull")
	_, _ = io.ReadAll(out)
	if waitForAudit(t, r.h.audit, r.run.ID, "run.data_out.deny", "denied") == nil {
		t.Errorf("data_out review: no data_out row; events=%s", auditDump(r.h.audit.snapshot(), r.run.ID))
	}
	if findAudit(r.h.audit.snapshot(), r.run.ID, "run.local_folder.deny", "denied") != nil {
		t.Error("data_out review: the walls' guard answered instead of data_out's")
	}
}

// TestBridgeSSHSync_ByteCapEndsSession: a session past its push or pull cap is ended by the server with the size-cap
// sentence and exit status 3, and audited with the wall that ended it.
func TestBridgeSSHSync_ByteCapEndsSession(t *testing.T) {
	for _, tc := range []struct {
		wall  string
		walls *types.LocalFolderRules
		what  string
	}{
		{wallMaxPush, &types.LocalFolderRules{MaxPushMiB: 1}, "limit for sending in"},
		{wallMaxPull, &types.LocalFolderRules{MaxPullMiB: 1}, "limit for bringing back"},
	} {
		d := newLFWSSH(t, types.LocalFolderWallsRecord{Walls: tc.walls, Layers: map[string]types.LocalFolderWallLayer{tc.wall: {Layer: policyref.LayerBoundary}}}, nil, types.TransferAllowed, nil, types.Runner{})
		sess, stderr, in, out := d.openSync(t, "push")
		go func() {
			_, _ = in.Write(make([]byte, 1<<20+4096))
			_ = in.Close()
		}()
		_, _ = io.Copy(io.Discard, out)
		errOut, _ := io.ReadAll(stderr)
		var exit *ssh.ExitError
		if err := sess.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != syncCapExitStatus {
			t.Errorf("%s: Wait = %v, want exit status %d", tc.wall, err, syncCapExitStatus)
		}
		if !strings.Contains(string(errOut), "This sync session reached its 1.0 MB "+tc.what+", set by your organisation's boundary") {
			t.Errorf("%s: stderr = %q", tc.wall, errOut)
		}
		if d.wallDenied(t, tc.wall) == nil {
			t.Errorf("%s: no run.local_folder.deny row; events=%s", tc.wall, auditDump(d.h.audit.snapshot(), d.run.ID))
		}
	}
}
