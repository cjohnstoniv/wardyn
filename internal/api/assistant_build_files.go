// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A repository draft: the person picks a repository that a workspace available to them carries, and the server
// reads the root files on Wardyn's fixed list through the access that workspace's branch list already uses. Each
// file is scanned for well-known secret formats, and the person sees each file's name and size, never its
// contents, before anything is sent. Until they confirm, the files are held on the session row and reach no
// record and no provider; the confirm launches the run, sealed to the destination the person last confirmed.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantBuildFilesWindow is how long the person has to confirm: a session still fetching after it is ended as
// idle, by a confirm, a read of its events, or the person's next start.
const assistantBuildFilesWindow = 10 * time.Minute

// assistantBuildFilesReadTimeout bounds one repository read, scan included.
const assistantBuildFilesReadTimeout = time.Minute

// assistantRootFileReader is what the GitHub App lane reads a repository's root files through; the minter
// satisfies it beside the branch list (cfg.GitHubBranches).
type assistantRootFileReader interface {
	ReadRootFiles(ctx context.Context, repo string, names []string, maxBytes int) ([]broker.File, error)
}

// assistantRepoRead is what an arm read from a repository, before the scan and the caps.
type assistantRepoRead struct {
	branch     string
	credential string
	files      []broker.File
}

// assistantBuildFilesEvent is the build_files event the person's confirmation shows: names and sizes, never
// contents.
type assistantBuildFilesEvent struct {
	Files    []assistantBuildFileName `json:"files"`
	Skipped  []string                 `json:"skipped"`
	HeldBack []string                 `json:"held_back"`
}

type assistantBuildFileName struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// assistantHeldFiles is what a fetching session holds in its draft column until the person confirms: the files
// that passed the scan, with their text. Nothing else reads that column while the session fetches.
type assistantHeldFiles struct {
	Files []assistantHeldFile `json:"files"`
}

type assistantHeldFile struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

func (h assistantHeldFiles) files() []assistant.File {
	out := make([]assistant.File, len(h.Files))
	for i, f := range h.Files {
		out[i] = assistant.File{Name: f.Name, Text: f.Text}
	}
	return out
}

// The refusals of a repository read. None carries what the forge or the credential store said.
func assistantRepoHostUnsupported() *assistantRefusal {
	return assistantRefusalFor(http.StatusBadRequest, reasonAssistantRepoHostUnsupported, "This repository host isn't supported for drafts yet.") // PROPOSED
}

func assistantRepoNoCredential() *assistantRefusal {
	return assistantRefusalFor(http.StatusConflict, "", "No git access is set up for this repository, so its files cannot be read.") // PROPOSED
}

func assistantRepoSignInNeeded() *assistantRefusal {
	return assistantRefusalFor(http.StatusConflict, "", "Sign in to Azure DevOps again, then start over.") // PROPOSED
}

func assistantRepoReadRefused() *assistantRefusal {
	return assistantRefusalFor(http.StatusBadGateway, "", "The repository host refused the read.") // PROPOSED
}

func assistantRepoUnreachable() *assistantRefusal {
	return assistantRefusalFor(http.StatusBadGateway, "", "The repository host could not be reached.") // PROPOSED
}

func assistantBuildFilesNotPending() *assistantRefusal {
	return assistantRefusalFor(http.StatusConflict, reasonAssistantBuildFilesNotPending, "These files are not waiting to be sent. Start again.") // PROPOSED
}

// repoBuildFiles reads repo's root files through the arm the branch list would take for it, in the same order
// (listRepoBranches): the repository must be admitted; an Azure DevOps run on the person's own sign-in or token
// reads through it, and minted-token mode has no credential for it; any other Azure DevOps has none; the GitHub
// App reads github.com when the provider row allows its lane, and an unset provider passes, as it does for the
// branch list; any other host is unsupported. owner is the person the read acts for.
func (s *Server) repoBuildFiles(ctx context.Context, sc types.SiteConfig, repo, owner string) (assistantRepoRead, *assistantRefusal) {
	cloneURL := repoCloneURL(repo)
	v := admitRepoURL(sc, cloneURL)
	if !v.Admitted {
		return assistantRepoRead{}, assistantRepoHostUnsupported()
	}
	if a, ok := adoEntraRunForRepo(sc, repo, owner); ok {
		return s.adoRepoFiles(ctx, sc, a, repo)
	}
	if v.Provider.Kind == types.GitProviderAzureDevOps {
		return assistantRepoRead{}, assistantRepoNoCredential()
	}
	ghRepo, onGitHub := githubAppRepo(cloneURL)
	if !onGitHub {
		return assistantRepoRead{}, assistantRepoHostUnsupported()
	}
	if v.Provider.ID != "" && (v.Provider.Kind != types.GitProviderGitHub || !laneAllowed(v.Provider, types.GitLaneApp)) {
		return assistantRepoRead{}, assistantRepoNoCredential()
	}
	return s.githubRepoFiles(ctx, ghRepo)
}

// githubRepoFiles reads through the organisation's GitHub App. A deployment with no App, or a minter that cannot
// read files, has no credential.
func (s *Server) githubRepoFiles(ctx context.Context, ghRepo string) (assistantRepoRead, *assistantRefusal) {
	out := assistantRepoRead{credential: branchCredGitHubApp}
	rd, ok := s.cfg.GitHubBranches.(assistantRootFileReader)
	if !ok || rd == nil {
		return out, assistantRepoNoCredential()
	}
	files, err := rd.ReadRootFiles(ctx, ghRepo, assistant.BuildFileNames(), assistant.MaxBuildFileBytes)
	switch {
	case errors.Is(err, secretstore.ErrNotFound): // no App configured
		return out, assistantRepoNoCredential()
	case errors.Is(err, broker.ErrGitHubRefused):
		return out, assistantRepoReadRefused()
	case err != nil:
		return out, assistantRepoUnreachable()
	}
	out.files = files
	if len(files) > 0 {
		out.branch = files[0].Branch
	}
	return out, nil
}

// assistantScanRepoFiles is read's files after the secret scan and the size caps: the ones to send, with their
// text; and the names of those too large, and of those held back. The engine is built here, once, from
// assistant.BuildFilesScanSpec; one that comes back nil would scan nothing, so it is a server error and nothing
// is sent. A file with any finding, a scan that failed, or a scan that did not cover the whole file is held back.
func assistantScanRepoFiles(read []broker.File) (send []assistant.File, skipped, held []string, ref *assistantRefusal) {
	eng, err := contentscan.NewEngine(assistant.BuildFilesScanSpec(), nil)
	if err != nil || eng == nil {
		return nil, nil, nil, assistantRefusalFor(http.StatusInternalServerError, "", "Could not check the files for secrets.") // PROPOSED
	}
	var fit []assistant.File
	for _, f := range read {
		if f.TooLarge {
			skipped = append(skipped, f.Name)
			continue
		}
		if f.Text != "" {
			res, _, err := eng.ScanRequest(contentscan.ChannelGeneric, []byte(f.Text))
			if err != nil || len(res.Findings) > 0 || res.Skipped || eng.ShouldBlock(res) {
				held = append(held, f.Name)
				continue
			}
		}
		fit = append(fit, assistant.File{Name: f.Name, Text: f.Text})
	}
	send, over := assistant.KeepWithinCaps(fit)
	return send, append(skipped, over...), held, nil
}

// assistantBuildFilesAudit writes assistant.build_files.fetch: the person, the workspace, the repository, the
// branch, and the names sent, skipped and held back. Never a file's text.
func (s *Server) assistantBuildFilesAudit(r *http.Request, person string, ws uuid.UUID, repo string, read assistantRepoRead, ev assistantBuildFilesEvent, outcome string) {
	names := make([]string, len(ev.Files))
	for i, f := range ev.Files {
		names[i] = f.Name
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), person, assistantBuildFilesFetchAction, ws.String(), outcome,
		mustJSON(map[string]any{"workspace_id": ws, "repo": redactRepoURL(repo), "branch": read.branch, "credential": read.credential,
			"files": names, "skipped": ev.Skipped, "held_back": ev.HeldBack})))
}

// handleCreateAssistantRepoSession is POST /assistant/sessions with a workspace and a repository. In order: both
// fields, on a compose session; the rate bucket; the three workspace checks (callerWorkspaceRepo), before any
// read; the same checks and preview digest as any start; the read, the scan and the caps. It then ends the
// person's live session for the purpose as replaced, creates the new one in fetching with no run, holds the files
// that passed, and writes the fetching state and the build_files event. Nothing is sent and no run exists.
func (s *Server) handleCreateAssistantRepoSession(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, req assistantCreateRequest) {
	if req.WorkspaceID == nil || req.Repo == nil || *req.Repo == "" || req.Purpose != types.AssistantCompose {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "Files from a repository need a workspace and a repository, on a compose session.") // PROPOSED
		return
	}
	wsID, err := uuid.Parse(*req.WorkspaceID)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "workspace_id is not an id.") // PROPOSED
		return
	}
	if s.assistantRateRefused(w, oidcHumanFromContext(r.Context())) {
		return
	}
	r = r.WithContext(withCapBatch(r.Context()))
	ws, ok := s.callerWorkspaceRepo(w, r, wsID, *req.Repo)
	if !ok {
		return
	}
	prep, ref := s.assistantPrepare(r, req.Purpose, req.Target)
	if ref != nil {
		ref.write(w, r)
		return
	}
	if req.PreviewDigest != prep.digest {
		writeErrorReason(w, http.StatusConflict, reasonAssistantPreviewStale, "What will be sent changed since the preview. Preview again.") // PROPOSED
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), assistantBuildFilesReadTimeout)
	defer cancel()
	read, ref := s.repoBuildFiles(ctx, prep.sc, *req.Repo, runIdentitySubject(ctx, principalFromRequest(r)))
	var ev assistantBuildFilesEvent
	var send []assistant.File
	if ref == nil {
		var skipped, held []string
		if send, skipped, held, ref = assistantScanRepoFiles(read.files); ref == nil {
			ev = assistantBuildFilesEventOf(read.files, send, skipped, held)
		}
	}
	if ref != nil {
		s.assistantBuildFilesAudit(r, prep.snap.Principal, ws.ID, *req.Repo, read, assistantBuildFilesEvent{}, "failure")
		ref.write(w, r)
		return
	}
	sess, ref := s.assistantStartFetching(r.Context(), ss, prep, req.Purpose, ws.ID, *req.Repo, send, ev)
	if ref != nil {
		ref.write(w, r)
		return
	}
	s.assistantBuildFilesAudit(r, prep.snap.Principal, ws.ID, *req.Repo, read, ev, "success")
	writeJSON(w, http.StatusCreated, assistantSessionView{ID: sess.ID, State: sess.State, Provider: providerDisplayName(prep.provider),
		Model: prep.binding.Model, Letters: prep.payload.Letters})
}

// assistantBuildFilesEventOf is the event for what was read: the names and sizes that will be sent, then the names
// left out for size and for the scan. A file's size is its byte count as read.
func assistantBuildFilesEventOf(read []broker.File, send []assistant.File, skipped, held []string) assistantBuildFilesEvent {
	ev := assistantBuildFilesEvent{Files: []assistantBuildFileName{}, Skipped: skipped, HeldBack: held}
	if ev.Skipped == nil {
		ev.Skipped = []string{}
	}
	if ev.HeldBack == nil {
		ev.HeldBack = []string{}
	}
	for _, f := range send {
		ev.Files = append(ev.Files, assistantBuildFileName{Name: f.Name, Bytes: len(f.Text)})
	}
	return ev
}

// assistantStartFetching makes the fetching session. In one transaction it ends the person's live session for
// the purpose, if there is one, as replaced, and creates the new row with no run; the replaced session's run
// stops, since a repository draft starts cold. A fetching row it ends loses its held files first. Then it holds
// the files that passed and writes the two events.
func (s *Server) assistantStartFetching(ctx context.Context, ss store.AssistantSessionStore, prep *assistantPrep, purpose types.AssistantPurpose,
	ws uuid.UUID, repo string, send []assistant.File, ev assistantBuildFilesEvent) (types.AssistantSession, *assistantRefusal) {
	row := assistantSessionRow(prep, purpose, nil)
	row.State, row.WorkspaceID, row.Repo = types.AssistantSessionFetching, &ws, &repo
	held := assistantHeldFiles{Files: make([]assistantHeldFile, len(send))}
	for i, f := range send {
		held.Files[i] = assistantHeldFile{Name: f.Name, Text: f.Text}
	}
	old, err := ss.LiveAssistantSession(ctx, prep.snap.Principal, purpose)
	var sess types.AssistantSession
	switch {
	case errors.Is(err, store.ErrNotFound):
		sess, err = ss.CreateAssistantSession(ctx, row)
	case err != nil:
		return sess, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your sessions.") // PROPOSED
	default:
		if old.State == types.AssistantSessionFetching {
			s.assistantDropHeld(ctx, ss, old.ID)
		}
		sess, err = ss.ReplaceAssistantSession(ctx, old.ID, row, nil)
		if err == nil {
			s.assistantChanged(old.ID, old.RunID)
			s.assistantEndAudit(context.WithoutCancel(ctx), ss, old, types.AssistantEndReplaced)
			if serr := s.stopUnusedAssistantRun(context.WithoutCancel(ctx), ss, old, false); serr != nil {
				slog.WarnContext(ctx, "wardynd: could not stop a replaced assistant run", slog.String("session", old.ID.String()), slog.Any("err", serr))
			}
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrAssistantSessionLive):
		return sess, assistantRefusalFor(http.StatusConflict, reasonAssistantSessionNotFound, "Another session started first. Try again.") // PROPOSED
	case err != nil:
		return sess, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
	}
	fail := func() (types.AssistantSession, *assistantRefusal) {
		s.assistantDropHeld(ctx, ss, sess.ID)
		s.endAssistantSession(ctx, ss, sess, types.AssistantEndProviderError, false)
		return sess, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
	}
	heldJSON, _ := json.Marshal(held)
	if err := ss.SetAssistantDraft(ctx, sess.ID, heldJSON); err != nil {
		return fail()
	}
	state, _ := json.Marshal(map[string]any{"state": types.AssistantSessionFetching})
	files, _ := json.Marshal(ev)
	if _, err := ss.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventState, state); err != nil {
		return fail()
	}
	if _, err := ss.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventBuildFiles, files); err != nil {
		return fail()
	}
	s.assistantChanged(sess.ID, nil)
	return sess, nil
}

// assistantDropHeld clears the files a fetching session holds. It is best effort: an ended session's row is
// cleared by the retention sweep.
func (s *Server) assistantDropHeld(ctx context.Context, ss store.AssistantSessionStore, id uuid.UUID) {
	if err := ss.SetAssistantDraft(context.WithoutCancel(ctx), id, nil); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: could not clear the held build files", slog.String("session", id.String()), slog.Any("err", err))
	}
}

// assistantFetchingExpired reports whether a fetching session is past the time the person has to confirm.
func (s *Server) assistantFetchingExpired(sess types.AssistantSession) bool {
	return sess.State == types.AssistantSessionFetching && s.cfg.Now().Sub(sess.CreatedAt) >= assistantBuildFilesWindow
}

type assistantBuildFilesConfirmRequest struct {
	PreviewDigest string `json:"preview_digest"`
}

// assistantBuildFilesConfirmView is the confirm's answer: the session as a start answers it, and the digest of the
// records now sent, the files included.
type assistantBuildFilesConfirmView struct {
	assistantSessionView
	PreviewDigest string `json:"preview_digest"`
}

// assistantStatusWriter remembers the status a check wrote, so a caller can tell a refusal from a failed read.
type assistantStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *assistantStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *assistantStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// handleAssistantBuildFilesConfirm is POST .../{id}/build-files/confirm {preview_digest}: the person's last
// confirmation, "Send files". The session's owner only (another's id is 404). In order:
//  1. the session is fetching, else 409 assistant_build_files_not_pending (one that waited past ten minutes is
//     ended as idle first, so it answers the same);
//  2. the workspace checks again, with no read, then availability, the never-sent kind and the deferral, as a
//     start makes them: a refusal ends the session as turned_off, and nothing is sent;
//  3. the digest of the records and the serving binding now, the files left out, must be the one named, else 409
//     assistant_preview_stale and the session stays fetching;
//  4. the held files join the records as the untrusted build-files section;
//  5. the run is created, attached to the session before anything is dispatched, and then dispatched, sealed to
//     the binding the digest covers.
func (s *Server) handleAssistantBuildFilesConfirm(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok {
		return
	}
	var req assistantBuildFilesConfirmRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	var held assistantHeldFiles
	if sess.State != types.AssistantSessionFetching || sess.WorkspaceID == nil || sess.Repo == nil {
		assistantBuildFilesNotPending().write(w, r)
		return
	}
	if json.Unmarshal(sess.Draft, &held) != nil || sess.Draft == nil {
		s.assistantDropHeld(r.Context(), ss, sess.ID)
		s.endAssistantSession(r.Context(), ss, sess, types.AssistantEndIdle, false)
		assistantBuildFilesNotPending().write(w, r)
		return
	}
	if s.assistantRateRefused(w, sess.Principal) {
		return
	}
	r = r.WithContext(withCapBatch(r.Context()))
	sw := &assistantStatusWriter{ResponseWriter: w}
	if _, ok := s.callerWorkspaceRepo(sw, r, *sess.WorkspaceID, *sess.Repo); !ok {
		if sw.status < http.StatusInternalServerError {
			s.assistantEndFetching(r.Context(), ss, sess, types.AssistantEndTurnedOff)
		}
		return
	}
	prep, ref := s.assistantPrepare(r, sess.Purpose, types.AssistantTarget{Kind: sess.TargetKind, ID: sess.TargetKey})
	if ref != nil {
		if ref.status == http.StatusConflict && ref.reason != "" {
			s.assistantEndFetching(r.Context(), ss, sess, types.AssistantEndTurnedOff)
		}
		ref.write(w, r)
		return
	}
	if req.PreviewDigest != prep.digest {
		writeErrorReason(w, http.StatusConflict, reasonAssistantPreviewStale, "What will be sent changed since the preview. Preview again.") // PROPOSED
		return
	}
	final := assistant.AddBuildFiles(prep.payload, held.files())
	finalDigest := assistant.PreviewDigest(prep.binding, final.Sections)
	out, ref := s.assistantLaunchFetched(r, ss, sess, prep, final, finalDigest)
	if ref != nil {
		ref.write(w, r)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(out.RunID, actorTypeFromRequest(r), prep.snap.Principal, assistantSessionStartAction, out.ID.String(), "success",
		mustJSON(map[string]any{"purpose": out.Purpose, "provider_id": prep.binding.ProviderID, "model": prep.binding.Model,
			"run_id": out.RunID, "target_kind": out.TargetKind, "target_key": out.TargetKey, "preview_digest": finalDigest, "role": prep.role})))
	writeJSON(w, http.StatusOK, assistantBuildFilesConfirmView{
		assistantSessionView: assistantSessionView{ID: out.ID, State: out.State, RunID: out.RunID, Provider: providerDisplayName(prep.provider),
			Model: prep.binding.Model, Letters: final.Letters},
		PreviewDigest: finalDigest,
	})
}

// assistantEndFetching ends a fetching session and drops the files it holds.
func (s *Server) assistantEndFetching(ctx context.Context, ss store.AssistantSessionStore, sess types.AssistantSession, reason string) {
	s.assistantDropHeld(ctx, ss, sess.ID)
	s.endAssistantSession(ctx, ss, sess, reason, false)
}

// assistantRecordsOf is the payload of the state event that carries the records a session sends, the files included.
func assistantRecordsOf(final assistant.Payload, digest string) json.RawMessage {
	return mustJSON(map[string]any{"state": "records", "digest": digest, "records": final.Sections})
}

// assistantLaunchFetched launches the run of a fetching session whose files the person confirmed. The run is
// created, then its id is written to the session (moving it to starting) before anything is dispatched, since the
// seal-site deferral refusal finds an assistant run only through that row. When the serving binding or the
// letters differ from what the row stored, the row is replaced in one transaction by one that carries the new
// binding and the run, so the stored binding is the one the digest covers.
func (s *Server) assistantLaunchFetched(r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, prep *assistantPrep,
	final assistant.Payload, digest string) (types.AssistantSession, *assistantRefusal) {
	ctx := r.Context()
	l, err := s.assistantNewRun(ctx, prep.snap.Principal, prep.binding)
	if err != nil {
		return sess, s.assistantLaunchRefusal(err)
	}
	runID := l.run.ID
	records := assistantRecordsOf(final, digest)
	revoke := func() {
		s.failAndRevoke(context.WithoutCancel(ctx), runID, types.RunPending, "the assistant session could not start")
	}
	out := sess
	if sess.Binding == prep.binding && maps.Equal(sess.PersonLetters, prep.payload.Letters) {
		err = ss.AttachAssistantRun(ctx, sess.ID, runID)
		out.RunID, out.State = &runID, types.AssistantSessionStarting
	} else {
		row := assistantSessionRow(prep, sess.Purpose, &runID)
		row.WorkspaceID, row.Repo, row.PreviewDigest = sess.WorkspaceID, sess.Repo, digest
		s.assistantDropHeld(ctx, ss, sess.ID)
		out, err = ss.ReplaceAssistantSession(ctx, sess.ID, row, records)
		if err == nil {
			s.assistantChanged(sess.ID, nil)
			s.assistantEndAudit(context.WithoutCancel(ctx), ss, sess, types.AssistantEndReplaced)
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrAssistantSessionLive):
		revoke()
		return sess, assistantBuildFilesNotPending()
	case err != nil:
		revoke()
		return sess, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
	}
	if out.ID == sess.ID {
		s.assistantDropHeld(ctx, ss, sess.ID)
		if _, err := ss.AppendAssistantEvent(ctx, out.ID, types.AssistantEventState, records); err != nil {
			s.endAssistantSession(ctx, ss, out, types.AssistantEndProviderError, false)
			revoke()
			return out, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
		}
	}
	s.assistantChanged(out.ID, out.RunID)
	s.assistantDispatch(ctx, l)
	return out, nil
}
