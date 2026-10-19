// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A folder on the person's computer for a run on another machine (#2982), held by the person's
// own folder-only runner (`wardyn-runnerd serve --substrate folders`). The person browses it
// through that runner (GET /me/runners/{id}/folders), and Wardyn seeds it into the sandbox itself
// before the agent starts, with no `wardyn sync`. That pass is the `wardyn sync` engine, run by
// the runner as the sftp client over its own outbound session. wardynd joins that stream to the
// sandbox's sftp-server, which it already execs for the gateway's wardyn-sync. With
// direction "both", one more pass brings the sandbox's changes back when the run ends, under the
// `wardyn sync --pull` rules: one side edits at a time.
//
// Off unless WARDYN_COMPUTER_FOLDERS is set. The runner checks every path itself (internal/runner/folders).

const (
	// computerFolderListTimeout bounds one browse call, the runner's counting included.
	computerFolderListTimeout = 10 * time.Second
	// computerFolderBackTimeout bounds the pass that brings changes back, which holds the teardown.
	computerFolderBackTimeout = 2 * time.Minute
)

// computerFolderOffMsg is the "blocked by your org" refusal.
const computerFolderOffMsg = "your organisation doesn't allow folders from personal computers (WARDYN_COMPUTER_FOLDERS is off)"

// folderRunner is the caller's own claimed runner id that shares folders and is online on this
// replica, with its endpoint. It writes the refusal and returns false otherwise.
func (s *Server) folderRunner(ctx context.Context, owner string, id uuid.UUID) (*remote.Substrate, *runRefusal) {
	if !s.cfg.ComputerFolders {
		return nil, runError(http.StatusForbidden, reasonComputerFoldersOff, computerFolderOffMsg)
	}
	rs, ok := s.cfg.Store.(runnerGetter)
	if !ok {
		return nil, runError(http.StatusNotImplemented, reasonComputerRunnerNoFolders, "this server's store holds no runners")
	}
	row, err := rs.GetRunner(ctx, id)
	if err != nil || owner == "" || row.Owner != owner || row.OwnerKind == types.RunnerOwnerOrg || row.State != types.RunnerClaimed {
		return nil, runError(http.StatusNotFound, reasonRunnerNotFound, "no runner of yours has that id")
	}
	s.runnerStreams.subsMu.Lock()
	ep, live := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !live || !s.runnerOnline(id) {
		return nil, runError(http.StatusConflict, reasonComputerRunnerOffline,
			fmt.Sprintf("%s is offline: start the runner on that computer, then try again", runnerLabel(row)))
	}
	if caps, ok := ep.sub.Caps(); !ok || !caps.Folders {
		return nil, runError(http.StatusConflict, reasonComputerRunnerNoFolders,
			fmt.Sprintf("%s does not share folders: run 'wardyn-runnerd serve --substrate folders' (0.10 or later) on that computer", runnerLabel(row)))
	}
	return ep.sub, nil
}

// runnerGetter is the one runner read a computer folder needs.
type runnerGetter interface {
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
}

// runnerLabel is how a person knows the runner: its computer's name, else the runner's.
func runnerLabel(row types.Runner) string {
	if row.MachineName != "" {
		return row.MachineName
	}
	return row.Name
}

// computerFolderView is GET /me/runners/{id}/folders: the folders in path and, with survey=1, what
// a run would get from it.
type computerFolderView struct {
	runnerwire.FolderList
	Runner string `json:"runner"`
}

// handleListMyRunnerFolders is GET /api/v1/me/runners/{id}/folders?path=&survey=1. Only folder
// names, file counts and sizes reach Wardyn; no file is sent until a run starts.
func (s *Server) handleListMyRunnerFolders(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerOwnerForRead(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	sub, refusal := s.folderRunner(r.Context(), owner, id)
	if refusal.write(s, w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computerFolderListTimeout)
	defer cancel()
	list, err := sub.ListFolders(ctx, runnerwire.ListFoldersArgs{
		Path: r.URL.Query().Get("path"), Survey: r.URL.Query().Get("survey") == "1", Deny: s.cfg.SyncDeny,
	})
	var werr *runnerwire.Error
	switch {
	case errors.As(err, &werr) && werr.Code == runnerwire.CodeRefused:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonComputerFolderRefused, werr.Message)
		return
	case err != nil:
		writeErrorReason(w, http.StatusBadGateway, reasonComputerRunnerOffline, "the runner did not answer: "+err.Error())
		return
	}
	row, _ := s.cfg.Store.(runnerGetter).GetRunner(r.Context(), id)
	writeJSON(w, http.StatusOK, computerFolderView{FolderList: list, Runner: runnerLabel(row)})
}

// computerFolderRefusal checks a local_folder that names a runner, for the caller in ctx, and
// normalises it. It is localFolderRefusal's branch for a folder Wardyn seeds itself, so it needs no
// SSH gateway, and a background-only run may have one.
func (s *Server) computerFolderRefusal(ctx context.Context, req *createRunRequest, name, dir string) *runRefusal {
	f := req.LocalFolder
	bad := func(msg string) *runRefusal {
		return runError(http.StatusBadRequest, reasonLocalFolderInvalid, "local_folder."+msg)
	}
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || strings.ContainsFunc(f.Path, isControlRune) {
		return bad("path must be a clean absolute path on that computer")
	}
	direction := f.Direction
	switch direction {
	case "":
		direction = types.ComputerFolderIn
	case types.ComputerFolderIn, types.ComputerFolderBoth:
	default:
		return bad(`direction must be "both" or "in"`)
	}
	if name == "" {
		name = filepath.Base(f.Path)
	}
	switch {
	case req.Interactive || req.Experience == types.ExperienceInteractive:
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported,
			"local_folder: an interactive run has no agent start to hold for the folder's first sync")
	case req.Task == "":
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported, "local_folder: a run with no task has no agent start to hold")
	}
	if _, ok := s.cfg.Store.(store.RunLocalFolderStatuses); !ok {
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported, "local_folder: this server's store cannot record a folder's sync")
	}
	if _, refusal := s.folderRunner(ctx, poolOwner(ctx), *f.RunnerID); refusal != nil {
		return refusal
	}
	id := *f.RunnerID
	req.LocalFolder = &types.RunLocalFolder{Name: name, RemoteDir: dir, RunnerID: &id, Path: f.Path, Direction: direction}
	return nil
}

func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// seedComputerFolder runs the first pass of run's computer folder, before the agent starts, and
// records its outcome as the folder's first sync (localFolderPassed), so the hold that follows
// reads it as it reads a `wardyn sync` pass.
func (s *Server) seedComputerFolder(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, wait time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	res, err := s.computerFolderPass(ctx, run, folder, false)
	errText := res.Error
	if err != nil {
		errText = err.Error()
	}
	s.recordComputerFolderPass(ctx, run.ID, func(st *types.RunLocalFolderStatus) { st.Seed = folderPassRecord(s.cfg.Now(), res, errText, false) })
	s.localFolderPassed(ctx, run.ID, run.CreatedBy, folder.RemoteDir, errText == "", errText)
}

// bringComputerFolderBack is the pass that brings the sandbox's changes back to the person's
// computer when a "both" run ends: after the revoke cascade, before the sandbox is stopped. A run
// whose folder never seeded, or that has no such folder, is left alone.
func (s *Server) bringComputerFolderBack(ctx context.Context, runID uuid.UUID, ref string) {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok || ref == "" {
		return
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, runID)
	if err != nil || folder == nil || folder.RunnerID == nil || folder.Direction != types.ComputerFolderBoth || seed.SeededAt == nil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	run.SandboxRef = ref
	ctx, cancel := context.WithTimeout(ctx, computerFolderBackTimeout)
	defer cancel()
	res, err := s.computerFolderPass(ctx, run, *folder, true)
	errText := res.Error
	if err != nil {
		errText = err.Error()
	}
	s.recordComputerFolderPass(ctx, runID, func(st *types.RunLocalFolderStatus) { st.Back = folderPassRecord(s.cfg.Now(), res, errText, true) })
	outcome, data := "success", map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir, "files": res.Pulled, "bytes": res.BytesIn}
	if errText != "" {
		outcome, data["error"] = "failure", localFolderSeedError(errText)
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.folder.transfer", runID.String(), outcome, mustJSON(data)))
}

func folderPassRecord(now time.Time, res runnerwire.FolderPass, errText string, back bool) *types.FolderPassRecord {
	rec := &types.FolderPassRecord{At: now, Files: res.Pushed, Bytes: res.BytesOut, Refused: []types.FolderRefusal{}}
	if back {
		rec.Files, rec.Bytes = res.Pulled, res.BytesIn
	}
	if errText != "" {
		rec.Error = localFolderSeedError(errText)
	}
	for _, r := range res.Refused[:min(len(res.Refused), runnerwire.MaxFolderRefusals)] {
		rec.Refused = append(rec.Refused, types.FolderRefusal{Path: localFolderSeedError(r.Path), Reason: localFolderSeedError(r.Reason)})
	}
	return rec
}

// recordComputerFolderPass applies set to run id's pass record.
func (s *Server) recordComputerFolderPass(ctx context.Context, id uuid.UUID, set func(*types.RunLocalFolderStatus)) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return
	}
	cur, err := st.RunLocalFolderStatus(ctx, id)
	if err == nil {
		set(&cur)
		_, err = st.SetRunLocalFolderStatus(ctx, id, cur)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: record a computer folder's pass", slog.String("run_id", id.String()), slog.Any("err", err))
	}
}

// computerFolderPass is one pass between run's computer folder and its sandbox: the runner's sftp
// client on one side, the sandbox's own sftp-server started in folder.RemoteDir on the other. The
// runner is checked again here: it must still be the run creator's own, online, sharing folders.
func (s *Server) computerFolderPass(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, pull bool) (runnerwire.FolderPass, error) {
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	if refusal != nil {
		return runnerwire.FolderPass{}, errors.New(refusal.body.Error)
	}
	sess, reason, err := s.execSFTPServer(ctx, run, "-d", folder.RemoteDir)
	if err != nil {
		return runnerwire.FolderPass{}, errors.New(reason)
	}
	defer func() { _ = sess.Close() }()
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	pass, err := sub.SyncFolder(ctx, runnerwire.SyncFolderArgs{
		RunID: run.ID, Path: folder.Path, RemoteDir: folder.RemoteDir, Pull: pull, Deny: s.cfg.SyncDeny,
	})
	if err != nil {
		return runnerwire.FolderPass{}, fmt.Errorf("the runner could not start the sync: %w", err)
	}
	defer func() { _ = pass.Stream.Close() }()
	var joined sync.WaitGroup
	joined.Go(func() {
		_, _ = io.Copy(sess.Stdin, pass.Stream)
		_ = sess.Stdin.Close()
	})
	joined.Go(func() {
		_, _ = io.Copy(pass.Stream, sess.Stdout)
		_ = pass.Stream.CloseWrite()
	})
	res, err := pass.Wait(ctx)
	_ = pass.Stream.Close()
	_ = sess.Close()
	joined.Wait()
	if err != nil {
		return runnerwire.FolderPass{}, fmt.Errorf("the runner's sync did not finish: %w", err)
	}
	return res, nil
}

// computerFolderRunView is GET /runs/{id}'s computer_folder: the run's folder from the person's
// computer and its passes. The path on that computer is never shown, only its name.
type computerFolderRunView struct {
	Name      string                  `json:"name"`
	Runner    string                  `json:"runner"`
	RemoteDir string                  `json:"remote_dir"`
	Direction string                  `json:"direction"`
	State     string                  `json:"state"` // waiting, synced or failed: the first sync's
	Error     string                  `json:"error,omitempty"`
	Seed      *types.FolderPassRecord `json:"seed,omitempty"`
	Back      *types.FolderPassRecord `json:"back,omitempty"`
}

// computerFolderRunState values.
const (
	computerFolderWaiting = "waiting"
	computerFolderSynced  = "synced"
	computerFolderFailed  = "failed"
)

// computerFolderRunView reads run id's computer folder, nil when it has none.
func (s *Server) computerFolderRunView(ctx context.Context, id uuid.UUID) *computerFolderRunView {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return nil
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, id)
	if err != nil || folder == nil || folder.RunnerID == nil {
		return nil
	}
	v := &computerFolderRunView{Name: folder.Name, RemoteDir: folder.RemoteDir, Direction: folder.Direction, State: computerFolderWaiting}
	if rs, ok := s.cfg.Store.(runnerGetter); ok {
		if row, err := rs.GetRunner(ctx, *folder.RunnerID); err == nil {
			v.Runner = runnerLabel(row)
		}
	}
	switch {
	case seed.SeededAt != nil:
		v.State = computerFolderSynced
	case seed.Error != "":
		v.State, v.Error = computerFolderFailed, seed.Error
	}
	if ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := ss.RunLocalFolderStatus(ctx, id); err == nil {
			v.Seed, v.Back = rec.Seed, rec.Back
		}
	}
	return v
}
