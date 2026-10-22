// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's local folder (types.RunLocalFolder, migration 0195): a folder on the
// person's computer that `wardyn sync` seeds into the sandbox. Dispatch holds
// the agent's start until the first sync of it has completed, so the agent
// never works on an empty or partial tree. The sync client says when a pass
// completed (the wardyn-sync-pass request, sshgateway_sync.go); the gateway
// records it on the run, and dispatch reads it there, so the two may be on
// different replicas.

const (
	// DefaultLocalFolderSeedWait is how long dispatch waits for the first sync
	// when WARDYN_LOCAL_FOLDER_SEED_WAIT is unset.
	DefaultLocalFolderSeedWait = 10 * time.Minute
	maxLocalFolderName         = 255
	maxLocalFolderSeedError    = 300
)

// localFolderSeedPoll is how often dispatch reads the run while it waits.
var localFolderSeedPoll = 2 * time.Second

// localFolderRefusal checks and normalises req.LocalFolder. A run holds its
// agent for the folder only where the hold has a start to hold and the sync a
// door to come in by: a task run (the agent exec), not interactive (whose
// start is the person's own attach), not background-only (which refuses SSH),
// on a deployment with an SSH gateway and a store that keeps the seed.
func (s *Server) localFolderRefusal(ctx context.Context, req *createRunRequest) *runRefusal {
	f := req.LocalFolder
	if f == nil {
		return nil
	}
	name := strings.TrimSpace(f.Name)
	if f.RunnerID != nil && name == "" {
		name = filepath.Base(f.Path) // a computer folder may leave its name to its path
	}
	if name == "" || len(name) > maxLocalFolderName || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return runError(http.StatusBadRequest, reasonLocalFolderInvalid,
			fmt.Sprintf("local_folder.name must be 1 to %d bytes of UTF-8 with no control characters", maxLocalFolderName))
	}
	dir, err := sshSyncDir(f.RemoteDir)
	if err != nil {
		return runError(http.StatusBadRequest, reasonLocalFolderInvalid, "local_folder.remote_dir: "+strings.TrimPrefix(err.Error(), sshSyncDirEnv+" "))
	}
	if f.RunnerID != nil {
		return s.computerFolderRefusal(ctx, req, name, dir)
	}
	unsupported := func(why string) *runRefusal {
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported, "local_folder: "+why)
	}
	_, canSeed := s.cfg.Store.(store.RunLocalFolders)
	switch {
	case req.Interactive || req.Experience == types.ExperienceInteractive:
		return unsupported("an interactive run has no agent start to hold yet; sync the folder with 'wardyn sync' once the run is up")
	case req.Experience == types.ExperienceBackground:
		return unsupported("a background-only run refuses SSH, so 'wardyn sync' could never seed the folder")
	case req.Task == "":
		return unsupported("a run with no task has no agent start to hold")
	case s.cfg.SSHListenAddr == "":
		return unsupported("this deployment has no SSH gateway, so 'wardyn sync' could never seed the folder")
	case !canSeed:
		return unsupported("this server's store cannot record a folder's first sync")
	}
	req.LocalFolder = &types.RunLocalFolder{Name: name, RemoteDir: dir}
	return nil
}

// localFolderPassed records a sync client's report of a complete (or failed)
// pass into dir as the first sync of the run's local folder, when the run has
// one landing there and no outcome yet. Later reports change nothing.
func (s *Server) localFolderPassed(ctx context.Context, runID uuid.UUID, principal, dir string, complete bool, errText string) {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, runID)
	if err != nil || folder == nil || folder.RemoteDir != dir || seed.SeededAt != nil || seed.Error != "" {
		return
	}
	msg := ""
	if !complete {
		msg = localFolderSeedError(errText)
	}
	changed, err := st.SetRunLocalFolderSeed(ctx, runID, msg)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: record a local folder's first sync", slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if !changed {
		return
	}
	data := map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir}
	outcome := "success"
	if msg != "" {
		outcome, data["error"] = "failure", msg
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "run.seed", runID.String(), outcome, mustJSON(data)))
}

// localFolderSeedError is the client's error text as the run keeps it: one
// line, no control characters, at most maxLocalFolderSeedError runes.
func localFolderSeedError(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if s = strings.TrimSpace(s); s == "" {
		s = "the sync reported a failure without a message"
	}
	if r := []rune(s); len(r) > maxLocalFolderSeedError {
		s = string(r[:maxLocalFolderSeedError]) + "..."
	}
	return s
}

// holdForLocalFolder is dispatch's wait for a task run's local folder, before
// the agent exec. True when the run has no local folder or its first sync has
// landed; false when the run was failed here (a failed or late seed, a runtime
// that cannot be synced into) or left RUNNING while it waited.
func (s *Server) holdForLocalFolder(ctx context.Context, run types.AgentRun, ref string) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return true // localFolderRefusal accepts none on such a store
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, run.ID)
	if err != nil {
		s.failLocalFolderRun(ctx, run, ref, "", "the run's local folder could not be read, so the agent was not started")
		return false
	}
	if folder == nil {
		return true
	}
	if caps, cerr := s.runCapabilities(ctx, run); cerr == nil && strings.HasPrefix(caps.Resolved[run.ConfinementClass], "oci/krun") {
		s.failLocalFolderRun(ctx, run, ref, folder.Name, fmt.Sprintf(
			"the local folder %q cannot be synced into this run's exec-less (krun) runtime, so the agent was not started", folder.Name))
		return false
	}
	wait := s.cfg.LocalFolderSeedWait
	if wait <= 0 {
		wait = DefaultLocalFolderSeedWait
	}
	if run.EndsAt != nil {
		wait = min(wait, run.EndsAt.Sub(s.cfg.Now()))
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.seed.hold", run.ID.String(), "success",
		mustJSON(map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir, "wait_sec": int(wait.Seconds())})))
	if folder.RunnerID != nil && seed.SeededAt == nil && seed.Error == "" {
		// A computer folder is seeded here, through its runner, rather than awaited from `wardyn sync`.
		run.SandboxRef = ref
		s.seedComputerFolder(ctx, run, *folder, wait)
		if _, next, err := st.RunLocalFolderSeed(ctx, run.ID); err == nil {
			seed = next
		}
	}
	deadline := s.cfg.Now().Add(wait)
	for {
		switch {
		case seed.SeededAt != nil:
			return true
		case seed.Error != "":
			s.failLocalFolderRun(ctx, run, ref, "", fmt.Sprintf(
				"the first sync of the local folder %q failed, so the agent was not started: %s", folder.Name, seed.Error))
			return false
		case !s.cfg.Now().Before(deadline):
			msg := fmt.Sprintf("the local folder %q did not finish its first sync into %s within %s, so the agent was not started",
				folder.Name, folder.RemoteDir, wait.Round(time.Second))
			if _, err := st.SetRunLocalFolderSeed(ctx, run.ID, "timed out"); err == nil {
				s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.seed", run.ID.String(), "failure",
					mustJSON(map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir, "error": "timed out"})))
			}
			s.failLocalFolderRun(ctx, run, ref, "", msg)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(localFolderSeedPoll):
		}
		if got, err := s.cfg.Store.GetRun(ctx, run.ID); err == nil && got.State != types.RunRunning {
			return false // killed or ended while it waited; that path did its own teardown
		}
		if _, next, err := st.RunLocalFolderSeed(ctx, run.ID); err == nil {
			seed = next
		}
	}
}

// failLocalFolderRun stops the sandbox and fails the run with hint. A folder
// name, when given, is audited as the refusal's own run.seed row.
func (s *Server) failLocalFolderRun(ctx context.Context, run types.AgentRun, ref, folder, hint string) {
	if folder != "" {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.seed", run.ID.String(), "failure",
			mustJSON(map[string]any{"folder": folder, "error": hint})))
	}
	s.stopSandboxOrAudit(ctx, run.ID, ref, "run.seed")
	s.failAndRevoke(ctx, run.ID, types.RunRunning, hint)
}
