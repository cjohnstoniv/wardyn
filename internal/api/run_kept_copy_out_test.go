// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptCopyOutStore makes the entry fixture's run the shape the copy-out door is for: a RUNNING local run on a
// runner, lost to reason a minute ago, with the store reads the door makes (the database clock, the agent's stop
// and the containment report) answering as its fields say. It embeds store.Store as envelopeStore does.
type keptCopyOutStore struct {
	store.Store
	runnerID       uuid.UUID
	reason         types.LostReason
	endsAt         *time.Time
	clockErr       error
	stopped        bool
	stoppedErr     error
	pending        bool
	pendingErr     error
	containmentErr string
	experience     types.RunExperience
}

func (s *keptCopyOutStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.Store.GetRun(ctx, id)
	if err != nil {
		return run, err
	}
	lost := time.Now().Add(-time.Minute)
	run.Placement, run.RunnerID = types.PlacementLocal, &s.runnerID
	run.SandboxRef = "runner:" + s.runnerID.String() + "/sandbox"
	run.LostAt, run.LostReason, run.EndsAt, run.ContainmentError = &lost, s.reason, s.endsAt, s.containmentErr
	if s.experience != "" {
		run.Experience = s.experience
	}
	return run, nil
}

func (s *keptCopyOutStore) DatabaseNow(context.Context) (time.Time, error) {
	if s.clockErr != nil {
		return time.Time{}, s.clockErr
	}
	return time.Now(), nil
}

func (s *keptCopyOutStore) RunAgentStopped(context.Context, uuid.UUID) (bool, error) {
	return s.stopped, s.stoppedErr
}

func (s *keptCopyOutStore) GetRunContainmentPending(context.Context, uuid.UUID) (store.RunContainmentPendingRow, error) {
	switch {
	case s.pendingErr != nil:
		return store.RunContainmentPendingRow{}, s.pendingErr
	case s.pending:
		return store.RunContainmentPendingRow{}, nil
	}
	return store.RunContainmentPendingRow{}, store.ErrNotFound
}

func (s *keptCopyOutStore) ListRunNodeRecords(context.Context, uuid.UUID) ([]types.RunNodeRecord, error) {
	return nil, nil
}

func (s *keptCopyOutStore) QueueRunNodeRecord(context.Context, store.RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	return types.RunnerPendingAction{}, nil
}

// wrapKeptCopyOut puts st over srv's store and, unless noCaps, seeds the runner's caps (EndsUnkept: a Kubernetes runner).
func wrapKeptCopyOut(t *testing.T, srv *Server, st *keptCopyOutStore, noCaps bool) *keptCopyOutStore {
	t.Helper()
	st.Store = srv.cfg.Store
	st.runnerID = uuid.New()
	if st.reason == "" {
		st.reason = types.LostOutage
	}
	srv.cfg.Store = st
	if !noCaps {
		seedRunnerCaps(t, srv, st.runnerID, runnerwire.Caps{EndsUnkept: true})
	}
	return st
}

// keptCopyOutServer is the entry fixture (a person's run, owned by entryOwnerSub) made a kept run a Kubernetes
// runner lost to an outage.
func keptCopyOutServer(t *testing.T, operatorOwned bool) (*Server, types.AgentRun, map[string]*http.Cookie, *keptCopyOutStore) {
	t.Helper()
	srv, run, _, ses := entryServer(t, operatorOwned)
	st := wrapKeptCopyOut(t, srv, &keptCopyOutStore{}, false)
	return srv, run, ses, st
}

func keptRun(t *testing.T, srv *Server, id uuid.UUID) types.AgentRun {
	t.Helper()
	run, err := srv.cfg.Store.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func wantCopyOutRefusal(t *testing.T, what string, code int, body, reason, sentence string) {
	t.Helper()
	if code != http.StatusConflict || !strings.Contains(body, reasonAttachRunKept) || !strings.Contains(body, sentence) {
		t.Errorf("%s: %d %s, want 409 %s %q", what, code, body, reason, sentence)
	}
}

func TestKeptCopyOut_OnlyTheOwnerOfAnOutageRunOnAKubernetesRunnerOpens(t *testing.T) {
	const ended = "ENDED"
	past := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name      string
		principal string
		store     keptCopyOutStore
		mutate    func(*types.AgentRun)
		ctx       func(context.Context) context.Context
		noCaps    bool
		notK8s    bool
		bare      bool
		want      string
	}{
		{name: "the owner", principal: entryOwnerSub, want: ""},
		{name: "another principal", principal: "bob", want: ended},
		{name: "no principal", principal: "", want: ended},
		{name: "an operator-owned run", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.OperatorOwned = true }, want: ended},
		{name: "lost at its end", principal: entryOwnerSub, store: keptCopyOutStore{reason: types.LostEnded}, want: ended},
		{name: "lost to a reboot", principal: entryOwnerSub, store: keptCopyOutStore{reason: types.LostReboot}, want: ended},
		{name: "lost and gone", principal: entryOwnerSub, store: keptCopyOutStore{reason: types.LostGone}, want: ended},
		{name: "no lost reason", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.LostReason = "" }, want: ended},
		{name: "a delegated call", principal: entryOwnerSub, want: ended,
			ctx: func(c context.Context) context.Context {
				return audit.WithDelegation(c, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})
			}},
		{name: "no caps held for the runner", principal: entryOwnerSub, noCaps: true, want: ended},
		{name: "a runner that is not Kubernetes", principal: entryOwnerSub, notK8s: true, want: ended},
		{name: "no runner id", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.RunnerID = nil }, want: ended},
		{name: "not local", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.Placement = types.PlacementRemote }, want: ended},
		{name: "a store with none of the reads", principal: entryOwnerSub, bare: true, want: ended},
		{name: "not RUNNING", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.State = types.RunFailed }, want: ended},
		{name: "no sandbox", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.SandboxRef = "" }, want: ended},
		{name: "past its end", principal: entryOwnerSub, store: keptCopyOutStore{endsAt: &past}, want: keptCopyOutPastEnd},
		{name: "past its lifetime cap", principal: entryOwnerSub, mutate: func(r *types.AgentRun) { r.LifetimeCapAt = &past }, want: keptCopyOutPastEnd},
		{name: "the clock cannot be read", principal: entryOwnerSub, store: keptCopyOutStore{clockErr: context.DeadlineExceeded}, want: keptCopyOutUnreadable},
		{name: "the stop is not confirmed", principal: entryOwnerSub, store: keptCopyOutStore{containmentErr: "x"}, want: keptCopyOutNotCut},
		{name: "unconfirmed and past its end", principal: entryOwnerSub, store: keptCopyOutStore{containmentErr: "x", endsAt: &past}, want: keptCopyOutPastEnd},
		{name: "a widened server run left unresolved", principal: entryOwnerSub, store: keptCopyOutStore{containmentErr: "x"}, want: ended,
			mutate: func(r *types.AgentRun) { r.Placement, r.RunnerID = types.PlacementRemote, nil }},
		{name: "the agent stopped", principal: entryOwnerSub, store: keptCopyOutStore{stopped: true}, want: keptCopyOutAgentStopped},
		{name: "the stop cannot be read", principal: entryOwnerSub, store: keptCopyOutStore{stoppedErr: context.DeadlineExceeded}, want: keptCopyOutUnreadable},
		{name: "containment pending", principal: entryOwnerSub, store: keptCopyOutStore{pending: true}, want: keptCopyOutRemoved},
		{name: "the containment report cannot be read", principal: entryOwnerSub, store: keptCopyOutStore{pendingErr: context.DeadlineExceeded}, want: keptCopyOutUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, run, _, _ := entryServer(t, false)
			if tc.bare {
				// The bare entry store holds none of the optional reads: the run is only made kept and local here.
				rid, lost := uuid.New(), time.Now().Add(-time.Minute)
				run.Placement, run.RunnerID, run.SandboxRef = types.PlacementLocal, &rid, "runner:"+rid.String()+"/sandbox"
				run.LostAt, run.LostReason = &lost, types.LostOutage
				seedRunnerCaps(t, srv, rid, runnerwire.Caps{EndsUnkept: true})
			} else {
				st := tc.store
				wrapKeptCopyOut(t, srv, &st, tc.noCaps)
				if tc.notK8s {
					seedRunnerCaps(t, srv, st.runnerID, runnerwire.Caps{EndsUnkept: false})
				}
				run = keptRun(t, srv, run.ID)
			}
			if tc.mutate != nil {
				tc.mutate(&run)
			}
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			if got := srv.keptCopyOutRefusal(ctx, run, tc.principal, ended); got != tc.want {
				t.Errorf("keptCopyOutRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

func ownerTicketPath(run types.AgentRun) string {
	return "/api/v1/runs/" + run.ID.String() + "/attach-ticket"
}

func TestKeptCopyOut_TheOwnerMintsATerminalTicket(t *testing.T) {
	const ended = "run has ended; cannot attach"
	srv, run, ses, st := keptCopyOutServer(t, false)
	path := ownerTicketPath(run)
	if w := doSSO(t, srv, http.MethodPost, path, ses["owner"], ""); w.Code != http.StatusOK {
		t.Fatalf("the owner: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPost, path+"?purpose=ui", ses["owner"], ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ended) {
		t.Fatalf("the owner for a UI app: %d %s, want 409 %q", w.Code, w.Body.String(), ended)
	}
	if w := doSSO(t, srv, http.MethodPost, path, ses["admin"], ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin: %d %s, want the owner-only 403", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPost, path, ses["security"], ""); w.Code != http.StatusNotFound {
		t.Fatalf("the security tier: %d %s, want 404", w.Code, w.Body.String())
	}
	st.stopped = true
	w := doSSO(t, srv, http.MethodPost, path, ses["owner"], "")
	wantCopyOutRefusal(t, "the owner with the agent stopped", w.Code, w.Body.String(), reasonAttachRunKept, keptCopyOutAgentStopped)
	st.stopped, st.containmentErr = false, "x"
	w = doSSO(t, srv, http.MethodPost, path, ses["owner"], "")
	wantCopyOutRefusal(t, "the owner with the stop unconfirmed", w.Code, w.Body.String(), reasonAttachRunKept, keptCopyOutNotCut)
	st.containmentErr = ""
	seedRunnerCaps(t, srv, st.runnerID, runnerwire.Caps{EndsUnkept: false})
	w = doSSO(t, srv, http.MethodPost, path, ses["owner"], "")
	wantCopyOutRefusal(t, "the owner on a runner that is not Kubernetes", w.Code, w.Body.String(), reasonAttachRunKept, ended)
}

func TestKeptCopyOut_TheOwnersTicketOpensTheSocketOnly(t *testing.T) {
	srv, run, _, st := keptCopyOutServer(t, false)
	attachPath := func() string {
		tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, run.ID, types.ActorHuman, entryOwnerSub, oidc.RoleUser, oidc.Issue{At: time.Now()})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return "/api/v1/runs/" + run.ID.String() + "/attach?ticket=" + tok
	}
	if w := do(t, srv, http.MethodGet, attachPath(), "", ""); w.Code == http.StatusConflict || strings.Contains(w.Body.String(), reasonAttachRunKept) {
		t.Fatalf("the owner's ticket: %d %s, want it past the kept check", w.Code, w.Body.String())
	}
	st.reason = types.LostEnded
	w := do(t, srv, http.MethodGet, attachPath(), "", "")
	wantCopyOutRefusal(t, "a run lost at its end", w.Code, w.Body.String(), reasonAttachRunKept, "run has ended; cannot attach")
}

func TestKeptCopyOut_AttachWordsAreTheOwnersOnlyForTheirOwnHolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder *attachHolder
		gated  bool
		want   string
	}{
		{"the person themselves", &attachHolder{principal: "p"}, false, "p"},
		{"a support helper", &attachHolder{principal: "p", support: uuid.New()}, false, ""},
		{"a delegated holder", &attachHolder{principal: "p", via: &types.DelegationVia{}}, false, ""},
		{"behind an opening gate", &attachHolder{principal: "p"}, true, ""},
	} {
		if got := attachWordsFor(tc.holder, tc.gated).copyOutBy; got != tc.want {
			t.Errorf("%s: copyOutBy = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKeptCopyOut_SSHOpensForTheOwnerAtEveryChannelRead(t *testing.T) {
	srv, run, _, _ := keptCopyOutServer(t, false)
	fr, execs := countingExecs()
	srv.cfg.Runner = fr
	ctx := context.Background()
	if _, msg := srv.sshFreshRun(ctx, run.ID, entryOwnerSub); msg != "" {
		t.Errorf("sshFreshRun for the owner = %q, want it open", msg)
	}
	if _, msg := srv.sshFreshRun(ctx, run.ID, "bob"); msg != "run has ended; ssh unavailable" {
		t.Errorf("sshFreshRun for another person = %q, want the kept words", msg)
	}
	spec := runner.ExecSpec{Argv: []string{"true"}}
	if sess, err := srv.execFresh(ctx, run.ID, sshLiveRunWords.copyOutFor(entryOwnerSub), spec); err != nil || sess == nil || execs.Load() != 1 {
		t.Errorf("execFresh for the owner = %v, %v after %d execs, want one session", sess, err, execs.Load())
	}
	if _, err := srv.execFresh(ctx, run.ID, sshLiveRunWords, spec); err == nil || !strings.Contains(err.Error(), "run has ended; ssh unavailable") || execs.Load() != 1 {
		t.Errorf("execFresh with plain words = %v after %d execs, want the kept words and no further exec", err, execs.Load())
	}
	kept := keptRun(t, srv, run.ID)
	words := sshLiveRunWords.copyOutFor(entryOwnerSub)
	if sess, _, err := srv.execSFTPServer(ctx, kept, &words, "-d", "%d"); err != nil || sess == nil || execs.Load() != 2 {
		t.Errorf("execSFTPServer for the owner = %v after %d execs, want a session", err, execs.Load())
	}
	if _, _, err := srv.execSFTPServer(ctx, kept, &sshLiveRunWords, "-d", "%d"); err == nil || execs.Load() != 2 {
		t.Errorf("execSFTPServer with plain words = %v after %d execs, want it refused with no further exec", err, execs.Load())
	}
}

func TestKeptCopyOut_TheCommandRouteAndUIAppsStayClosed(t *testing.T) {
	srv, run, ses, _ := keptCopyOutServer(t, false)
	fr, execs := countingExecs()
	srv.cfg.Runner = fr
	ctx := context.Background()
	if _, msg := srv.liveRunForExec(ctx, run.ID, execLiveRunWords); msg != execLiveRunWords.ended {
		t.Errorf("the command door = %q, want %q", msg, execLiveRunWords.ended)
	}
	if _, msg := srv.liveRunForExec(ctx, run.ID, uiLiveRunWords); msg != uiLiveRunWords.ended {
		t.Errorf("the UI gateway = %q, want %q", msg, uiLiveRunWords.ended)
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/exec", ses["owner"], `{"argv":["true"]}`)
	if w.Code < 400 || !strings.Contains(w.Body.String(), execLiveRunWords.ended) || execs.Load() != 0 {
		t.Errorf("POST /exec as the owner: %d %s after %d execs, want it refused as %q", w.Code, w.Body.String(), execs.Load(), execLiveRunWords.ended)
	}
}

func keptFilesEnv(t *testing.T, desktop *types.DesktopControls) (*desktopFilesEnv, *keptCopyOutStore) {
	t.Helper()
	e := newDesktopFilesEnv(t, desktop)
	st := wrapKeptCopyOut(t, e.srv, &keptCopyOutStore{}, false)
	return e, st
}

func TestKeptCopyOut_TheFilesDoorDownloadsOnly(t *testing.T) {
	e, st := keptFilesEnv(t, &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed})
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	var view desktopFilesView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != http.StatusOK {
		t.Fatalf("the owner lists: %d %s", w.Code, w.Body.String())
	}
	if view.Rules.Upload != types.TransferBlocked || view.Rules.Download != types.TransferAllowed || len(view.Files) != 1 || view.Files[0].Name != "agent-report.csv" {
		t.Fatalf("the owner's list = %+v, want upload blocked, download allowed and the report listed", view)
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusOK || w.Body.String() != "agent,wrote,this\n" {
		t.Fatalf("the owner's download: %d %q", w.Code, w.Body.String())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("download rows = %+v, want one success", rows)
	}
	before := e.folder.execs
	w = doSSO(t, e.srv, http.MethodPut, e.path("notes.csv"), e.ses["owner"], "x")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) || e.folder.execs != before {
		t.Fatalf("the owner's upload: %d %s after %d execs, want blocked with nothing sent", w.Code, w.Body.String(), e.folder.execs-before)
	}
	if rows := e.transferRows("upload"); len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Fatalf("upload rows = %+v, want one denied", rows)
	}
	if _, ok := e.folder.files["notes.csv"]; ok {
		t.Fatal("an upload reached the folder")
	}
	if w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["admin"], ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin lists: %d %s, want the owner-only 403", w.Code, w.Body.String())
	}
	st.stopped = true
	w = doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	wantCopyOutRefusal(t, "the owner with the agent stopped", w.Code, w.Body.String(), reasonAttachRunKept, keptCopyOutAgentStopped)

	t.Run("download blocked by default", func(t *testing.T) {
		e, _ := keptFilesEnv(t, nil)
		w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) || e.folder.execs != 0 {
			t.Fatalf("download under the defaults: %d %s after %d execs", w.Code, w.Body.String(), e.folder.execs)
		}
		if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "denied" {
			t.Fatalf("download rows = %+v, want one denied", rows)
		}
	})
	t.Run("a reviewed download is blocked", func(t *testing.T) {
		e, _ := keptFilesEnv(t, &types.DesktopControls{Download: types.TransferReview})
		w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) || strings.Contains(w.Body.String(), reasonDesktopReviewRequired) || e.folder.execs != 0 {
			t.Fatalf("a reviewed download: %d %s after %d execs, want blocked and the review path not reached", w.Code, w.Body.String(), e.folder.execs)
		}
	})
}

func TestKeptCopyOut_RunDetailSaysWhoMayCopyOut(t *testing.T) {
	detail := func(srv *Server, run types.AgentRun, c *http.Cookie) bool {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), c, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET run: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			KeptCopyOut bool `json:"kept_copy_out"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(w.Body.String(), `"kept_copy_out"`) && out.KeptCopyOut
	}
	srv, run, ses, st := keptCopyOutServer(t, false)
	if !detail(srv, run, ses["owner"]) {
		t.Error("the owner reads kept_copy_out absent, want true")
	}
	if detail(srv, run, ses["admin"]) {
		t.Error("a super admin reads kept_copy_out")
	}
	st.containmentErr = "x"
	if detail(srv, run, ses["owner"]) {
		t.Error("the owner reads kept_copy_out with the stop unconfirmed")
	}
	st.containmentErr = ""
	st.reason = types.LostEnded
	if detail(srv, run, ses["owner"]) {
		t.Error("the owner reads kept_copy_out on a run lost at its end")
	}
	st.reason = types.LostOutage
	seedRunnerCaps(t, srv, st.runnerID, runnerwire.Caps{EndsUnkept: false})
	if detail(srv, run, ses["owner"]) {
		t.Error("the owner reads kept_copy_out on a runner that is not Kubernetes")
	}
	srv, run, ses, _ = keptCopyOutServer(t, false)
	srv.cfg.Store = srv.cfg.Store.(*keptCopyOutStore).Store // the live run: not kept
	if detail(srv, run, ses["owner"]) {
		t.Error("the owner reads kept_copy_out on a live run")
	}
}

func TestKeptCopyOut_ABackgroundRunStaysClosed(t *testing.T) {
	srv, run, ses, st := keptCopyOutServer(t, false)
	st.experience = types.ExperienceBackground
	srv.cfg.Runner = &sshFakeRunner{}
	if _, msg := srv.sshFreshRun(context.Background(), run.ID, entryOwnerSub); msg != backgroundOnlyRefusal(keptRun(t, srv, run.ID), "SSH access").body.Error {
		t.Errorf("sshFreshRun on a background run = %q, want the background refusal", msg)
	}
	w := doSSO(t, srv, http.MethodPost, ownerTicketPath(run), ses["owner"], "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonRunBackgroundOnly) {
		t.Errorf("the ticket mint on a background run: %d %s, want 409 %s", w.Code, w.Body.String(), reasonRunBackgroundOnly)
	}
}

// Only the owner's own doors set the exception, and only the doors that ask for it read it.
func TestKeptCopyOut_OnlyTheOwnersDoorsSetTheException(t *testing.T) {
	qualified := funcBodies(t, true)
	for name, want := range map[string][]string{
		"copyOutFor": {"Server.bridgeSSHExec", "Server.bridgeSSHSFTP", "Server.bridgeSSHSync", "Server.handleSSHDirectTCPIP", "Server.sshFreshRun", "attachWordsFor"},
		"keptCopyOutRefusal": {"Server.handleAttachTicket", "Server.handleAttachWS", "Server.keptCopyOutOpen",
			"Server.liveRunForExec", "Server.refuseDesktopNotLive"},
	} {
		var got []string
		for sym, body := range qualified {
			if bodyNames(body, name) {
				got = append(got, sym)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s is used by %v, want %v", name, got, want)
		}
	}
}

// bodyNames is whether body uses the identifier name.
func bodyNames(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
