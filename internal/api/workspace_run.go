// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	neturl "net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requiredIntegrationIDs returns the integration ids every REQUIRED
// integration:<id> row in ws's effective contract names, in deterministic
// order. Shared by launchRecordRun (which folds each through
// applyIntegrationRequirement) and promoteSkipHosts' integration plumbing
// (record.go), so the two can never disagree about which integrations a
// session's egress already covers as contract plumbing.
func requiredIntegrationIDs(ws types.Workspace) []string {
	reqs := effectiveRequirements(ws)
	var ids []string
	for _, key := range sortedKeys(reqs) {
		if reqs[key].Level != "required" {
			continue
		}
		if typ, id, ok := splitRequirementKey(key); ok && typ == "integration" {
			ids = append(ids, id)
		}
	}
	return ids
}

// errWorkspaceSourceTarget marks a stored source whose authored target the
// record/verify composition refuses — the same class the create path answers 422
// for (seedRequestWorkspace), so handleRecordWorkspace can answer 422 too rather
// than the generic 500 every other launch failure gets. The status is the point:
// this is a stored row an operator must edit, not a fault in the daemon, and
// telling them "500 launch record run" for a workspace the OTHER door already
// rejects by name is how the inconsistency stayed hard to diagnose.
var errWorkspaceSourceTarget = errors.New("invalid workspace source target")

// wireWorkspaceSource points run+policy at EVERY one of the workspace's
// sources: each repo source clones (added to policy.WorkspaceRepos; the FIRST
// also sets run.Repo, the run-row label); each local_dir source is
// bind-mounted at its own target (falling back to the composer workspace
// target when unset). An ephemeral source gets NO policy entry (a mkdir
// inside the sandbox, not a mount/clone) — its target is collected into
// ephemeralDirs instead, for the caller to surface as WARDYN_EPHEMERAL_DIRS at
// dispatch, mirroring seedRequestWorkspace's identical handling for the
// ordinary create-run path (runs_create.go).
//
// It returns every repo source's clone URL, in Sources order, for
// workspaceSourceGrants — which today only auto-mints a clone credential for
// the FIRST one (see launchRecordRun's call site); an additional repo source
// needs its own pre-existing access until multi-repo grant minting is wired.
//
// ORDERING (load-bearing): this is a PURE run/policy mutation, so it must run
// BEFORE Store.CreateRun persists the row — whereas the clone grants it used to
// also create must run AFTER it, since credential_grants.run_id REFERENCES
// agent_runs(id) with an immediate FK. Doing both halves here forced one of the
// two orders to be wrong; the grant half is split out for that reason.
func wireWorkspaceSource(run *types.AgentRun, policy *types.RunPolicySpec, ws types.Workspace) (cloneURLs, ephemeralDirs []string, err error) {
	for _, src := range ws.Sources {
		// RE-VALIDATED ABOVE THE SWITCH, so it covers repo, local_dir and
		// ephemeral alike — the shape seedRequestWorkspace already has
		// (runs_create.go). It used to sit inside the EPHEMERAL arm only, which
		// is the branch that never needed it least: an ephemeral target is a
		// mkdir, while a repo or local_dir target becomes a real clone or bind
		// MOUNT, and nothing downstream re-imposes the rule for either. The
		// composed record policy never goes through validatePolicySpec, and the
		// driver's own gate is runner.ValidateMount -> ValidateTarget, which does
		// NOT carry the reserved-drive rule (targetReservedForDrive is reached
		// only from ValidateAuthoredTarget) — so /home/agent/drive passed it as an
		// ordinary /home/agent path and became an operator bind mount at the one
		// path the runner reserves for the user's own drive.
		//
		// A REFUSAL, not a silent skip. The repo half used to fail late and
		// invisibly — buildRepoRecords drops a repo whose dest fails this same
		// check with no error to the operator (runs_scm.go), so the session
		// started with a repo that never cloned and nothing said why. The same
		// stored workspace already 422s on the ordinary create-run path; a
		// workspace cannot be legal on one door and quietly broken on the other.
		if src.Target != "" {
			if verr := runner.ValidateAuthoredTarget(src.Target); verr != nil {
				return nil, nil, fmt.Errorf("%w: workspace %s source target: %v", errWorkspaceSourceTarget, ws.ID, verr)
			}
		}
		switch src.Type {
		case types.WorkspaceSourceTypeRepo:
			if run.Repo == "" {
				run.Repo = src.Source
			}
			policy.WorkspaceRepos = append(policy.WorkspaceRepos, types.WorkspaceRepo{Repo: src.Source, Target: src.Target, Ref: src.Ref})
			if url := repoCloneURL(src.Source); url != "" {
				cloneURLs = append(cloneURLs, url)
			}
		case types.WorkspaceSourceTypeLocalDir:
			target := src.Target
			if target == "" {
				target = composerWorkspaceTarget
			}
			// ReadOnly is a *bool whose SAFE DEFAULT is read-only when omitted.
			// Omitting it mounted every imported source read-only, which made the
			// Record step's own promise ("so the agent can make changes")
			// impossible to keep: `pnpm install` cannot write node_modules, a build
			// cannot emit artifacts, and no source file can be edited. Honor the
			// operator's explicit per-source opt-in instead; the default is still
			// read-only, so this widens nothing unless a human ticked the box.
			ro := !src.Writable
			if run.WorkspacePath == "" {
				run.WorkspacePath = src.Path
			}
			policy.WorkspaceMounts = append(policy.WorkspaceMounts, types.WorkspaceMount{
				Source: src.Path, Target: target, ReadOnly: &ro,
			})
		case types.WorkspaceSourceTypeEphemeral:
			// No policy entry — it's a mkdir inside the sandbox, not a mount/clone —
			// which also means it never passes through ValidateMount at CreateSandbox
			// time the way a repo/local_dir target does. The target was validated
			// above the switch, with every other kind.
			if src.Target != "" {
				ephemeralDirs = append(ephemeralDirs, src.Target)
			}
		}
	}
	return cloneURLs, ephemeralDirs, nil
}

// workspaceSourceGrants creates the repo clone's read credentials. It MUST be
// called AFTER Store.CreateRun: credential_grants.run_id REFERENCES
// agent_runs(id) (non-deferrable), so a grant written before the run row is
// rejected by the FK. A local dir (cloneURL "") needs no grant.
func (s *Server) workspaceSourceGrants(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (*uuid.UUID, map[string]string, error) {
	if cloneURL == "" {
		return nil, nil, nil
	}
	ghGrantID, err := s.maybeGitHubReadGrant(ctx, runID, now, cloneURL)
	if err != nil {
		return nil, nil, err
	}
	sshGrants, err := s.maybeSSHKeyGrant(ctx, runID, now, cloneURL)
	if err != nil {
		return nil, nil, err
	}
	return ghGrantID, sshGrants, nil
}

// maybeGitHubReadGrant creates a read-only github_token grant for a github.com
// clone URL (nil for any other host) — extracted from the scan launch's
// private-repo clone support so launchRecordRun can reuse it too. A CreateGrant
// failure is returned, never swallowed: the clone cannot authenticate without
// the grant, so the launch must fail loudly rather than dispatch a sandbox
// whose private-repo clone is guaranteed to 403.
//
// SCOPED TO THE CLONE'S OWN REPO — the SAME key gitBrokerGrant uses for the
// broker map, so the grant and the route it is reached through can never
// disagree. It used to write `"repos": []`, which the real minter refuses
// outright (githubMinter.MintInstallationToken: an installation token is
// per-installation and the owner comes from the first repo), so every
// scan/record clone of a GitHub HTTPS repo 502'd at handleGitBroker the
// moment a real GitHub App was configured. No test saw it because
// FakeGitHubMinter did not reproduce that precondition; it does now.
//
// A github.com URL with no derivable "<org>/<repo>" (a deeper path) yields NO
// grant: there is nothing a token could be scoped to, and an unmintable grant is
// worse than none — it also sets WARDYN_GITHUB_GRANT_ID, pointing the in-sandbox
// helper at a mint that can only fail.
func (s *Server) maybeGitHubReadGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (*uuid.UUID, error) {
	repo := gitBrokerKey(cloneURL) // "" for non-github, non-HTTPS, or a non-repo path
	if repo == "" {
		return nil, nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{
		"repos": []string{repo}, "permissions": map[string]string{"contents": "read"},
	})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil, fmt.Errorf("create github read grant: %w", gerr)
	}
	return &gid, nil
}

// maybeSSHKeyGrant synthesizes a run-scoped ssh_key grant for an SSH/scp clone
// URL — the SSH analog of maybeGitHubReadGrant — so onboarding an SSH workspace
// needs no manually-created grant. It returns the host→grant-id map dispatch
// marshals into WARDYN_SSH_GRANTS (nil when the source isn't an SSH URL to a
// supported provider, or the operator hasn't stored the ssh-key-<host> secret).
// The grant's host is the host git actually dials (so the sandbox ssh_config
// stanza matches), while key_secret_ref is the CANONICAL ssh-key-<host> secret
// (so either the github.com or ssh.github.com URL form resolves one stored key).
func (s *Server) maybeSSHKeyGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (map[string]string, error) {
	host, ok := sshCloneHost(cloneURL)
	if !ok {
		return nil, nil
	}
	if _, ok := sshOver443Endpoint(host); !ok {
		return nil, nil
	}
	secretName, ok := canonicalSSHKeySecret(host)
	if !ok {
		return nil, nil
	}
	// Only synthesize a grant when the key is actually present — otherwise the
	// clone would fail; the onboarding guard (below) rejects that case up front.
	names, err := s.listUserSecretNames(ctx)
	if err != nil {
		return nil, nil
	}
	if !slices.Contains(names, secretName) {
		return nil, nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{"host": host, "key_secret_ref": secretName})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantSSHKey, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil, fmt.Errorf("create ssh key grant: %w", gerr)
	}
	return map[string]string{host: gid.String()}, nil
}

// reconcileWorkspaceRun is called when a governed scan run reaches a terminal
// state. If the workspace is STILL in the in-flight state pointing at this run
// — meaning the run's result upload never arrived (the scan binary uploads
// BEFORE the process exits, so by the time the completion watcher fires a
// successful upload has already advanced the workspace) — it reconciles the
// workspace out of the stuck state instead of leaving it hung. The common
// cause is a sandbox that cannot reach the control plane (e.g. Docker-Desktop/
// WSL2 NAT networking).
func (s *Server) reconcileWorkspaceRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	// SOURCE scan runs settle on the source row: same self-heal one tier down,
	// fenced on the source's own active_run_id so a newer claim is untouched.
	if run.SourceID != nil {
		src, serr := s.cfg.Store.GetSource(ctx, *run.SourceID)
		if serr != nil || src.ActiveRunID == nil || *src.ActiveRunID != runID {
			return
		}
		if src.Status == types.WorkspaceScanning {
			_, _ = s.cfg.Store.SetSourceScanResult(ctx, src.ID, src.Profile, types.WorkspaceError, runID, nil)
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "source.scan",
				src.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_facts_uploaded"})))
		}
		return
	}
	if run.WorkspaceID == nil {
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, *run.WorkspaceID)
	if err != nil {
		return
	}
	// Only reconcile when THIS run is still the in-flight one (a newer run or a
	// landed upload would have cleared/changed active_run_id or the status).
	if ws.ActiveRunID == nil || *ws.ActiveRunID != runID {
		return
	}
	// LEGACY ROWS ONLY (STORE-4): for an attachment-backed workspace, hydrate
	// derives ws.Status as the worst-of-attached-sources status
	// (store_sources.go), so WorkspaceScanning here can only mean an attached
	// SOURCE is scanning — already handled above by the run.SourceID != nil
	// branch, which reconciles at the source, not the workspace. This switch
	// still fires for a genuinely pre-split row's own whole-workspace scan
	// run (run.WorkspaceID set, no per-source run).
	switch ws.Status {
	case types.WorkspaceScanning:
		// A repo scan run ended without uploading facts — leave a clear error via a
		// SCOPED write: touch only status + clear the in-flight pointer. The
		// previous full-row UpdateWorkspace replayed a stale pre-read snapshot over
		// EVERY column, clobbering any concurrently-persisted async field (profile,
		// record_results, approvals). Fenced on THIS run still owning the import
		// step: a newer scan that already claimed the slot must not be reverted by
		// this late reconcile.
		_, _, _ = s.cfg.Store.SetWorkspaceImportState(ctx, ws.ID, types.WorkspaceError, nil, &runID)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.scan",
			ws.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_facts_uploaded"})))
	}
}

// settleTerminalLaunch settles a workspace/record run that reached a TERMINAL
// state DURING dispatch — a CreateSandbox error or a STARTING→FAILED grant/MITM
// path CAS's the run to FAILED before Exec runs, so the completion watcher (which
// starts only after a successful Exec) never fires and no reconcile hook would
// otherwise settle the workspace. Without this, a scan strands in `scanning`
// forever and a record capture is never taken.
// Both reconcilers are idempotent + self-scoping — reconcileRecordRun no-ops for a
// non-record run, reconcileWorkspaceRun no-ops unless the workspace still points at
// this run in `scanning` — so calling both is safe for any launch kind.
func (s *Server) settleTerminalLaunch(ctx context.Context, runID uuid.UUID, created types.AgentRun) {
	if !isTerminalRunState(created.State) {
		return
	}
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
}

// recordEmptyCaptureHint explains a capture that produced ZERO egress evidence.
// This is a FAILURE, never "the task needs no egress": the (open) recording
// exists to observe, and the common cause of observing nothing is the proxy's
// decision callbacks not reaching the control plane, not a network-free task.
const recordEmptyCaptureHint = "no egress evidence was captured for this recording — most commonly the " +
	"proxy sidecar's decision callbacks cannot reach the control plane (Docker Desktop + WSL2 NAT needs " +
	"mirrored networking or the compose stack, `make setup`). Treat this recording as failed, NOT as " +
	"proof the task needs no egress."

// maxCaptureAuditEvents bounds a single server-side record/synthesis capture's
// audit-event read. It is a DoS ceiling far ABOVE any realistic recording — its
// only job is to replace QueryAuditEvents' SILENT 1000-row default, which
// truncated a long recording's later egress out of the derived least-privilege
// profile (a scanned-clean-but-actually-incomplete result). It never caps a
// legitimate run; a capture that somehow reaches it stamps captureAuditTruncatedNote
// so the truncation is VISIBLE, never silent.
const maxCaptureAuditEvents = 100000

// captureAuditTruncatedNote is the honest signal stamped on a capture / synthesis
// whose audit read reached maxCaptureAuditEvents — its observations/profile may be
// incomplete for an exceptionally long run.
const captureAuditTruncatedNote = "audit-event capture reached its ceiling; the derived observations/profile " +
	"may be incomplete for an exceptionally long run"

// dispatchFailureReason scans a run's already-fetched audit events for the
// LAST failed run.create/run.dispatch entry and returns its error/note field
// — the honest "why the sandbox never started" for a record run whose
// SandboxRef is empty (W20-W20-capture-store-4). "" when no such event is
// found (a truncated capture, or a failure mode that never audited a reason).
func dispatchFailureReason(events []types.AuditEvent) string {
	reason := ""
	for _, ev := range events {
		if ev.Outcome != "failure" || (ev.Action != "run.create" && ev.Action != "run.dispatch") {
			continue
		}
		var data struct {
			Error string `json:"error"`
			Note  string `json:"note"`
		}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &data)
		}
		switch {
		case data.Error != "":
			reason = data.Error
		case data.Note != "":
			reason = data.Note
		default:
			continue
		}
	}
	if reason == "" {
		return "no dispatch-failure reason was audited"
	}
	return reason
}

// reconcileRecordRun captures a record run's evidence when it reaches a
// terminal state — for ANY reason: auto completion, the operator's "Done
// recording" kill, or a boot reconcile. Capture is server-side and pure
// (recordmode.Capture over the run's already-persisted audit events — never a
// sandbox upload), so it works identically for auto and interactive runs and
// for killed ones. Idempotent: only the task entry still in `recording` and
// pointing at this run is finalized. Record NEVER touches workspace status or
// the verify fields; it only writes its own record_results entry and clears
// the active-run pointer.
func (s *Server) reconcileRecordRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, *run.WorkspaceID)
	if err != nil {
		return
	}
	taskKey := ""
	var res RecordTaskResult
	for k, v := range recordResultsMap(ws) {
		if v.RunID == runID {
			taskKey, res = k, v
			break
		}
	}
	if taskKey == "" || res.Status != recordStatusRecording {
		return // a newer recording superseded this run, or already finalized
	}

	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, maxCaptureAuditEvents)
	if err != nil {
		return // transient store failure: leave `recording`; a later reconcile retries
	}
	obs := recordmode.Capture(events, res.Confined)
	now := s.cfg.Now().UTC()
	res.FinishedAt = &now
	res.Observations = &obs
	res.KernelSensorBlind = run.ConfinementClass == types.CC3
	res.Caveats = []string{recordMaskingCaveat}
	truncated := len(events) >= maxCaptureAuditEvents
	if truncated {
		res.Caveats = append(res.Caveats, captureAuditTruncatedNote)
	}
	// W20-W20-groundtruth-mapper-4: surface the eBPF sensor's own coverage
	// state on the capture itself — before this it lived only on the
	// admin-only /healthz endpoint, nowhere an operator reviewing a recording
	// would see it. Orthogonal to KernelSensorBlind above (that's THIS run's
	// structural CC3 blindness; this is the host sensor's own health/coverage,
	// which can be degraded or partial regardless of confinement class).
	if gt := s.ebpfGroundtruthCaveat(ctx); gt != "" {
		res.Caveats = append(res.Caveats, gt)
	}
	if len(obs.Domains) == 0 {
		res.Status = recordStatusFailed
		// W20-W20-capture-store-4: recordEmptyCaptureHint blames the operator's
		// proxy/WSL2 networking — a fair guess for a run that actually reached
		// RUNNING and then observed nothing. A run whose sandbox never came up
		// AT ALL (SandboxRef is set only once CreateSandbox succeeds —
		// runs_dispatch.go) failed for a DIFFERENT, dispatch-side reason (image
		// build, resource limits, a concurrent kill racing dispatch); naming
		// that instead of the networking guess spares the operator a wasted
		// WSL2-mirrored-networking detour on a session that never even tried
		// to reach the control plane.
		if run.SandboxRef == "" {
			res.FailureHint = "the sandbox never started: " + dispatchFailureReason(events)
		} else {
			res.FailureHint = recordEmptyCaptureHint
		}
	} else {
		res.Status = recordStatusRecorded
		res.SecretNamesMinted = s.mintedSecretNames(ctx, runID, obs.MintedGrantIDs)
		// Clean/Caught are a CONFINED-replay verdict only (Workstream B): an
		// open recording has nothing to be "clean" against, so it gets neither
		// stamp — Clean stays nil (unknown/not-applicable), Caught stays 0.
		if res.Confined {
			caught := 0
			for _, d := range obs.Domains {
				if d.DenyCount > 0 || d.PendingCount > 0 {
					caught++
				}
			}
			res.Caught = caught
			clean := recordmode.CleanReplay(obs.Domains, truncated)
			res.Clean = &clean
		}
	}
	// Compare-and-set on `recording`: idempotent across the watcher/kill/boot/
	// read-repair triggers, and a capture that lost to a concurrent finalizer
	// (or a superseding re-record) writes and audits nothing.
	_, applied, perr := s.putRecordResult(ctx, ws.ID, taskKey, res, recordStatusRecording)
	if perr != nil || !applied {
		return
	}
	// Release the serial import-step slot — conditional, so a step that was
	// concurrently launched and now owns the pointer is never clobbered.
	_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
	// Counts-only audit — observations stay in the workspace row, never in audit.
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.record",
		ws.ID.String(), outcomeBool(res.Status == recordStatusRecorded), mustJSON(map[string]any{
			"task": taskKey, "mode": res.Mode, "domains": len(obs.Domains),
			"minted_grants": len(obs.MintedGrantIDs), "anomalies": len(obs.Anomalies),
			"kernel_sensor_blind": res.KernelSensorBlind,
		})))
}

// outcomeBool renders a bool as the audit outcome string convention
// ("success"/"failure"). Its only remaining caller is reconcileRecordRun; it
// used to be shared with the (now-removed) verify-result upload handler.
func outcomeBool(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

// mintedSecretNames resolves minted grant ids to operator-meaningful names for
// the "proven used" render: an api_key grant's secret_name, otherwise the grant
// kind. Deduped, sorted (stable render), never values.
func (s *Server) mintedSecretNames(ctx context.Context, runID uuid.UUID, minted []uuid.UUID) []string {
	if len(minted) == 0 {
		return nil
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return nil
	}
	byID := make(map[uuid.UUID]types.GrantSpec, len(grants))
	for _, g := range grants {
		byID[g.ID] = g.Spec
	}
	seen := map[string]bool{}
	for _, id := range minted {
		spec, ok := byID[id]
		if !ok {
			continue
		}
		name := string(spec.Kind)
		if spec.Kind == types.GrantAPIKey {
			var scope struct {
				SecretName string `json:"secret_name"`
			}
			if json.Unmarshal(spec.Scope, &scope) == nil && scope.SecretName != "" {
				name = scope.SecretName
			}
		}
		seen[name] = true
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}

// scanIdleCapSec bounds an idle scan run: short, since wardyn-scan clones + scans
// and uploads promptly. Written to both the run row (for the reaper) and the
// dispatch scanPolicy so the two never drift.
const scanIdleCapSec = 600

// gitBrokerManagedHosts are the GitHub clone/API/content hosts that Option C's
// git-broker manages: they are routed through wardyn-proxy (never a run's egress
// allowlist) and must never be promoted into a permanent ApprovedEgress entry (that
// would re-open host-level github egress and defeat the broker's repo-scoping).
var gitBrokerManagedHosts = []string{"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com"}

// gitBrokerKey returns the canonical lowercased "<org>/<repo>" git-broker allowlist
// key for a GitHub HTTPS clone URL, or "" when cloneURL isn't a bare github repo
// path (SSH, non-github, or a deeper path). Used to build the per-run GitGrants
// map for the scan/verify/record clone.
func gitBrokerKey(cloneURL string) string {
	u, err := neturl.Parse(cloneURL)
	if err != nil || u.Hostname() != "github.com" || !isHTTPScheme(u.Scheme) {
		return ""
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if parts := strings.Split(p, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return strings.ToLower(p)
	}
	return ""
}

// gitBrokerKeyFromSlug returns the canonical "<org>/<repo>" git-broker key for a
// run's declared repo slug — a bare "<org>/<repo>" (the github_token clone form) or
// a full https github URL. SSH/scp forms return "" (they use the ssh-over-443 lane,
// not the HTTPS-only broker).
func gitBrokerKeyFromSlug(slug string) string {
	slug = strings.TrimSpace(slug)
	if strings.Contains(slug, "@") || strings.HasPrefix(strings.ToLower(slug), "ssh://") {
		return ""
	}
	if strings.Contains(slug, "://") {
		return gitBrokerKey(slug) // full URL: github https -> key, else ""
	}
	return gitBrokerKey(repoCloneURL(slug)) // bare slug -> github https -> key
}

// gitBrokerGrant builds the single-repo git-broker allowlist for a scan/verify/
// record clone: {"<org>/<repo>": *ghGrantID}, or nil when there's no github grant
// or the clone isn't a broker-covered github repo (ssh/non-github/local dir).
func gitBrokerGrant(cloneURL string, ghGrantID *uuid.UUID) map[string]uuid.UUID {
	if ghGrantID == nil {
		return nil
	}
	if key := gitBrokerKey(cloneURL); key != "" {
		return map[string]uuid.UUID{key: *ghGrantID}
	}
	return nil
}

// isGitHubCloneHost reports whether host is a GitHub clone/API host (github.com,
// ssh.github.com, or a *.github.com subdomain), case-insensitively.
func isGitHubCloneHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return h == "github.com" || h == "ssh.github.com" || strings.HasSuffix(h, ".github.com")
}
