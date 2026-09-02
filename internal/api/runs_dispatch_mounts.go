// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// buildRunMounts assembles the sandbox bind mounts.
//
// Host bind mounts: copy the resolved POLICY's WorkspaceMounts into the spec.
// Mounts may be authored on a stored policy (admin-gated policy CRUD) OR
// INLINE on the create request by an admin / SSO-gated human operator
// (createRunRequest.InlinePolicy) — both flow through the SAME resolved
// RunPolicySpec here, so this is still the only path that populates
// spec.Mounts. They are NEVER chosen by the in-sandbox agent: the agent-run
// entrypoint has no access to this surface, so a prompt-injected agent can
// never pick a host mount (invariants 1 & 3). Every mount was already
// deny-list-validated by runner.ValidateMount at policy-write/inline-validate
// time (validatePolicySpec); the docker driver re-validates it
// defense-in-depth at sandbox-create time. runner.ValidateMount is unchanged.
//
// Bedrock ~/.aws mount (operator config, not agent-chosen; same trust and the
// same driver deny-list re-validation as the WorkspaceMounts above). READ-ONLY:
// the sandbox reads the SSO cache / config but can never write to the operator's
// host AWS state. Present whenever BedrockAWSConfigDir is set and the dir exists
// (resolveBedrockAuth) — host mode auto-detects it, the compose stack opts in via
// the WARDYN_BEDROCK_AWS_DIR bind; it is env-driven with no host/compose branch.
// A single-user / self-hosted choice, not for a shared multi-tenant service.
// Extracted verbatim from dispatchWithVerify.
//
// member is the run's member-mount posture (memberMountPosture, workspace_refs.go).
// Its Sources decide which binds carry runner.Mount.MemberAuthored — the flag the
// driver's bind-time within-roots check keys on. Everything NOT in that set is
// operator/Wardyn-authored (the blessed credential mounts copied from the
// ceiling, the Bedrock ~/.aws dir below, an operator-owned workspace's dir) and
// lives under no member root by construction, so stamping it would refuse the
// very credential mounts a member-owned workspace's model run needs. The zero
// posture (every operator run) stamps nothing.
func buildRunMounts(policy types.RunPolicySpec, llm llmTransport, member memberMountPosture) []runner.Mount {
	var mounts []runner.Mount
	for _, wm := range policy.WorkspaceMounts {
		// W5-S1-5: the resident ~/.claude subscription mount is a MODEL-RUN-ONLY
		// credential (THREAT-MODEL.md 5.1a) — a task-mode=exec or non-interactive
		// scan run makes no model call and must get NO LLM credential, even when
		// the resolved POLICY still carries the mount (e.g. a subscription-blessed
		// default/named policy reused for a plain exec task with no per-run
		// integration consent). Every other injection mode already gates on
		// llm.modelRun; this is the one path that read the policy verbatim.
		if !llm.modelRun && (wm.Target == claudeCredTarget || wm.Target == claudeCredJSONTarget) {
			continue
		}
		mounts = append(mounts, runner.Mount{
			Source: wm.Source,
			Target: wm.Target,
			// Safe default: omitted read_only => read-only. RW only on explicit
			// read_only=false in the policy.
			ReadOnly: wm.ReadOnlyOrDefault(),
			// Keyed on SOURCE, the same key validateWorkspaceSources resolves the
			// owning workspace by — so a bind is member-authored here exactly when
			// the run-create gate treated it as member-authored.
			MemberAuthored: member.Sources[wm.Source],
		})
	}
	if llm.bedrockReady && llm.bedrock.awsMount {
		mounts = append(mounts, runner.Mount{
			Source:   llm.bedrock.awsMountSource,
			Target:   sandboxAWSDir,
			ReadOnly: true,
		})
	}
	return mounts
}

// resolveRunUpstreamProxy resolves the operator-wide upstream/corp proxy
// (site-config → ProxyConfig.UpstreamProxyURL). A locked-down corporate network
// may give the sandbox host NO direct internet route at all — the operator
// configures the corp CONNECT-proxy URL via PUT /api/v1/site-config either as a
// plain site_config.UpstreamProxyURL or (when it carries a credential) as
// site_config.UpstreamProxySecretRef naming the secret holding it;
// resolveUpstreamProxyURL prefers the plain URL when set. The resolved
// cred-bearing URL lands in the sidecar's WARDYN_PROXY_CONFIG_JSON env var, the
// SAME posture as RunToken today: proxy-process-only, never on the sandbox
// side, masked from decision-log/stdout by the proxy — a deliberate,
// already-documented tradeoff (see runner.ProxyConfig.UpstreamProxyURL), not a
// new one. Fail SAFE: neither field configured, an unresolvable secret, or a
// non-http URL (from either source) all return "" (direct egress, today's
// behavior) plus an audit event; none of them fail the run or crash dispatch.
// Extracted verbatim from dispatchWithVerify.
func (s *Server) resolveRunUpstreamProxy(ctx context.Context, runID uuid.UUID, siteCfg types.SiteConfig, siteCfgErr error) string {
	if siteCfgErr != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			runID.String(), "failure", mustJSON(map[string]any{"reason": "site-config-read-error"})))
		return ""
	}
	if siteCfg.UpstreamProxyURL == "" && siteCfg.UpstreamProxySecretRef == "" {
		return ""
	}
	var getSecret func(context.Context, string) ([]byte, error)
	if s.cfg.Secrets != nil {
		// Operator namespace ONLY (0.7): under an upstream the sidecar skips
		// VetHost entirely, so a member-substitutable secret here would be an
		// SSRF-guard bypass, not a convenience.
		getSecret = s.cfg.Secrets.For("").Get
	}
	detail := map[string]any{
		"secret_ref": siteCfg.UpstreamProxySecretRef, "url_configured": siteCfg.UpstreamProxyURL != "",
	}
	resolved, failReason := resolveUpstreamProxyURL(ctx, siteCfg.UpstreamProxyURL, siteCfg.UpstreamProxySecretRef, getSecret)
	if failReason != "" {
		detail["reason"] = failReason
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			runID.String(), "failure", mustJSON(detail)))
		return ""
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
		runID.String(), "success", mustJSON(detail)))
	return resolved
}

// envEnabled reports whether an operator env-toggle string is truthy
// (1/true/yes/on, case-insensitive). Empty/unset/anything else is false.
func envEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// buildBaseSandboxEnv assembles dispatchWithVerify's baseline non-secret sandbox
// env (invariant 1: the run token never appears here): proxy routing, the
// toolchain-fidelity env the run's workspaces actually need (needs — Go's
// tempdir/cache redirect, the JVM proxy sysprops Maven/Gradle need because
// they ignore HTTP(S)_PROXY; nil needs = no workspace context, full set), and
// git commit attribution carrying the sub/act delegation chain. Every later
// phase in dispatchWithVerify only adds to this map, never removes from it.
func buildBaseSandboxEnv(run types.AgentRun, proxyURL string, needs *toolchainNeeds) map[string]string {
	env := map[string]string{
		"WARDYN_RUN_ID":    run.ID.String(),
		"WARDYN_PROXY_URL": proxyURL,
		// Standard proxy env: agents using HTTP_PROXY-aware clients route
		// through the wardyn-proxy automatically (L2 enforcement). BOTH cases:
		// curl (and others, post-httpoxy) deliberately IGNORE uppercase
		// HTTP_PROXY for plain-http URLs and honor only the lowercase form —
		// without it, an http:// fetch bypasses the proxy, fails DNS in the
		// sandbox, and the header-injection path never fires.
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"http_proxy":  proxyURL,
		"https_proxy": proxyURL,
		// Exclude the proxy itself and loopback from proxy traversal.
		"NO_PROXY": "wardyn-proxy,localhost,127.0.0.1,::1",
		"no_proxy": "wardyn-proxy,localhost,127.0.0.1,::1",
		// Git commit attribution: carry the sub/act delegation chain into the commit
		// graph so an agent's commits are traceable to the governed run — AUTHOR is
		// the human who authorized the run (sub), COMMITTER is the agent run (act).
		// git reads these env vars without touching the image. (Deterministic
		// Run-Id/On-Behalf-Of commit trailers need an in-image prepare-commit-msg
		// hook — tracked as a follow-up.)
		"GIT_AUTHOR_NAME":     run.CreatedBy,
		"GIT_AUTHOR_EMAIL":    gitEmailLocal(run.CreatedBy) + "@wardyn.local",
		"GIT_COMMITTER_NAME":  "wardyn-agent:" + run.Agent,
		"GIT_COMMITTER_EMAIL": run.ID.String() + "@agent.wardyn.local",
		// The pre-exec clone (agent-run-lib.sh's clone_one) runs on a TTY exec
		// (driver.go's ExecCreateOptions.TTY — needed for the agent CLI itself),
		// so an unauthorized/blocked/misconfigured URL that would otherwise fail
		// fast instead makes git see a terminal and sit at a credential prompt
		// FOREVER (nobody is there to answer it — clone_one's own stdin isn't
		// wired to a human). These three turn that hang back into the documented
		// log-and-continue: git exits non-zero immediately, clone_one's `else`
		// branch logs it as the governance signal it is, and the run proceeds.
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
	}
	// Agent-CLI telemetry, suppressed by default. Claude Code phones home to a
	// Datadog host on first run; in a Confined run that host is the FIRST pending
	// egress approval a pilot sees — before the host their task actually needs —
	// which reads as if Wardyn itself is exfiltrating. Suppress it at the source so
	// the approval queue shows only task-relevant egress. An operator who WANTS
	// agent telemetry sets WARDYN_ALLOW_AGENT_TELEMETRY (1/true/yes/on) to omit
	// these; default-unset keeps the suppression on.
	if !envEnabled(os.Getenv("WARDYN_ALLOW_AGENT_TELEMETRY")) {
		env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		env["DISABLE_TELEMETRY"] = "1"
	}
	// Toolchain-fidelity env — REQUIREMENTS-DRIVEN, never platform-wide: a
	// workspace run gets exactly what its scans detected (needs), and only a
	// run with no workspace context at all (needs == nil: ad-hoc/BYO/scan/
	// login/composer) keeps the full set, because nothing was scanned and
	// nothing declared. The runtime consumers are env-driven no-ops when a
	// key is absent (agent-run's make_toolchain_dirs, the attach shell guard).
	if needs == nil || needs.goTools {
		// GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles+EXECS
		// its test binaries in $TMPDIR → "permission denied". Point it (and the
		// build cache) at the agent's exec-allowed HOME. (Plain env survives a
		// shell; only PATH is reset by a login shell.)
		env["GOTMPDIR"] = "/home/agent/.gotmp"
		env["GOCACHE"] = "/home/agent/.cache/go-build"
	}
	if needs == nil || needs.jvmTools {
		// MAVEN_OPTS: Maven ALONE ignores HTTP(S)_PROXY (npm/pip/cargo/go/git
		// honor it) → "Unknown host repo.maven.apache.org". The JVM proxy
		// sysprops route Maven through wardyn-proxy. (The fat image also bakes
		// a settings.xml <proxy> as belt-and-braces.)
		// GRADLE_OPTS: Gradle is the same JVM-networking case — java.net's proxy
		// selector reads the same sysprops, so the exact same opts string.
		// NOT covered here (need image/build-time FILE config, not env): apt
		// and a per-project gradle.properties for repos that don't launch via
		// the gradle/gradlew wrapper JVM.
		env["MAVEN_OPTS"] = mavenProxyOpts(proxyURL)
		env["GRADLE_OPTS"] = mavenProxyOpts(proxyURL)
	}
	return env
}

// applyDispatchModeEnv sets dispatchRun's run-mode discriminator env vars
// (scan-only / exec task mode / interactive-start / boot-seed / tool-approval
// posture) plus the non-secret grant-id maps (WARDYN_GITHUB_GRANT_ID /
// WARDYN_GIT_PAT_GRANTS / WARDYN_SSH_GRANTS) that let the in-sandbox helpers
// mint the credentials they're eligible for, plus WARDYN_GIT_BROKER_REPOS
// (which of those are served by the git broker instead). Every branch here
// only decides which keys land in sandboxEnv, none of them change
// dispatchRun's own control flow.
//
// Returns the ssh_key and git_pat grant hosts it withheld because the run is
// BROKERED for that forge (dropBrokeredGrants) — both nil in the ordinary case.
// The caller warns and audits each; neither must ever be silent.
func applyDispatchModeEnv(sandboxEnv map[string]string, run types.AgentRun, interactive bool, taskMode, interactiveStart string, seedAutoTools bool, toolApprovals string, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, gitGrants map[string]uuid.UUID, patBroker bool) (droppedSSH, droppedPAT []string) {
	// Governed repo SCAN run: after cloning, the entrypoint runs wardyn-scan (which
	// walks ~/work and PUTs ScanFacts to the brokered scan-results route) INSTEAD of
	// the agent. A non-nil WorkspaceID marks a scan run — UNLESS the run is
	// interactive (an interactive workspace-linked run is Record Mode, a
	// human-driven sandbox, never a scan); no agent CLI / model call happens on a
	// scan.
	if (run.WorkspaceID != nil || run.SourceID != nil) && !interactive {
		sandboxEnv["WARDYN_SCAN_ONLY"] = "1"
	}
	// exec task mode (BYOA/CI lane): agent-run runs the task as a plain shell
	// command instead of the agent harness. Only the discriminator rides env —
	// everything above/below (clone, grants, egress, recording) is identical.
	if taskMode == "exec" {
		sandboxEnv["WARDYN_TASK_MODE"] = "exec"
	}
	// interactive_start=agent: the attach shell opens IN the image's agent CLI
	// instead of a bare shell. Consumed by attach-bashrc.sh (the image's
	// ~/.bashrc), which the tmux/bash attach chain sources — the same seam its
	// existing prep-done wait already rides. Gated on `interactive` HERE rather
	// than in a doc comment, so "ignored for a non-interactive run" is
	// structurally true: a batch run can never carry this env no matter what
	// the request said.
	if interactive && interactiveStart == "agent" {
		sandboxEnv["WARDYN_INTERACTIVE_START"] = "agent"
	}
	// Boot seed (Part A1): an interactive run's Task, when non-empty, is no
	// longer discarded — it fires once at sandbox boot, in the same persistent
	// session the human's attach later joins (interactiveStart above decides
	// whether it reads as an initial prompt or a startup command; that
	// interpretation lives entirely image-side, in agent-run's --boot-seed
	// branch — nothing here needs to know which). The reservedRunTasks
	// exclusion is load-bearing, not defensive: server-launched record/verify/
	// login runs (runs_create_validate.go, same package) are INTERACTIVE runs
	// that carry a non-empty, server-set Task ("workspace record", etc.) —
	// without this guard they would boot-seed `claude "workspace record"` into
	// what is supposed to be a plain record-mode sandbox, and the login box
	// would boot-seed over its own login flow.
	if interactive && !reservedRunTasks[run.Task] {
		if seed := strings.TrimSpace(run.Task); seed != "" {
			sandboxEnv["WARDYN_INTERACTIVE_SEED"] = run.Task
			if seedAutoTools {
				sandboxEnv["WARDYN_SEED_AUTO_TOOLS"] = "1"
			}
		}
	}
	// Tool-approval posture (Part C1/C2): "hold" routes an AUTONOMOUS run's own
	// tool calls to a Wardyn approval instead of running unsupervised —
	// consumed by agent-run's autonomous branch (its hold branch runs claude
	// under --permission-mode manual with wardyn-toolgate as the permission
	// prompt tool). Gated on `!interactive` for the same structural reason
	// InteractiveStart above is gated on `interactive`: an interactive run's
	// supervised-seed posture is SeedAutoTools's job, so this can't ride one no
	// matter what the request said.
	if !interactive && toolApprovals == "hold" {
		sandboxEnv["WARDYN_TOOL_APPROVALS"] = "hold"
	}
	if firstGitHubGrantID != nil {
		sandboxEnv["WARDYN_GITHUB_GRANT_ID"] = firstGitHubGrantID.String()
	}
	// The repos this run is BROKERED for — the SAME map confineGitBrokerEgress
	// keys on, so the sandbox's answer to "is my GitHub access brokered?" cannot
	// drift from the control plane's. wardyn-git-helper refuses to mint a GitHub
	// credential exactly when this is set (resolveGrantForHost): non-empty means
	// the /wardyn/gh/ route exists AND the broker-managed hosts are denied, so
	// the token belongs proxy-side only. A github_token grant with no repos
	// declared anywhere is NOT brokered — no route, no deny — and keeps the
	// helper as its credential path, unchanged. Non-secret: repo names only,
	// space-separated canonical "<org>/<repo>", sorted for a stable env value.
	if len(gitGrants) > 0 {
		repos := slices.Sorted(maps.Keys(gitGrants))
		sandboxEnv["WARDYN_GIT_BROKER_REPOS"] = strings.Join(repos, " ")
	}
	// git_pat grants: surface the {host: grant_id} map so the git-credential
	// helper can mint the stored PAT for a matched non-GitHub host. Non-secret
	// (grant ids, not the PAT); the value is returned only through the brokered mint.
	// A PAT for a BROKERED forge is withheld for the same reason the ssh_key is —
	// see dropBrokeredGrants.
	gitPATGrants, droppedPAT = dropBrokeredGrants(gitPATGrants, gitGrants, brokeredForgeHost)
	// THE POINT OF THE PAT BROKER, and the half that is easy to leave out: when
	// the never-resident lane is on, the grant ids must NOT reach the sandbox.
	// Leaving them here would let the in-sandbox credential helper mint the PAT
	// exactly as before, and the credential would be resident despite the broker
	// — the feature would look like it worked while changing nothing.
	//
	// agent-run learns which hosts to route through the broker from
	// WARDYN_GIT_PAT_BROKER_HOSTS below, which carries HOST NAMES ONLY and no
	// grant id, so it cannot be used to mint anything.
	if patBroker && len(gitPATGrants) > 0 {
		hosts := slices.Sorted(maps.Keys(gitPATGrants))
		sandboxEnv["WARDYN_GIT_PAT_BROKER_HOSTS"] = strings.Join(hosts, " ")
		gitPATGrants = nil
	}
	if len(gitPATGrants) > 0 {
		if b, merr := json.Marshal(gitPATGrants); merr == nil {
			sandboxEnv["WARDYN_GIT_PAT_GRANTS"] = string(b)
		}
	}
	// ssh_key grants: surface the {host: grant_id} map so agent-run can mint the
	// resident SSH private key at clone time (SSH has NO credential-helper seam,
	// so the key is written to a 0400 file and wiped after the clone). Non-secret
	// (grant ids, not the key); the key material is returned only via the brokered
	// mint and never touches env. See GrantSSHKey.
	sshGrants, droppedSSH = dropBrokeredGrants(sshGrants, gitGrants, brokeredForgeSSHHost)
	if len(sshGrants) > 0 {
		if b, merr := json.Marshal(sshGrants); merr == nil {
			sandboxEnv["WARDYN_SSH_GRANTS"] = string(b)
		}
	}
	return droppedSSH, droppedPAT
}

// applyRepoCloneEnv surfaces the repo(s) to clone (the legacy single run.Repo
// plus each onboarded WorkspaceRepo on the resolved policy) as sandbox env the
// agent-run launcher reads before running the agent — non-secret; invariant 1
// preserved. Extracted verbatim from dispatchWithVerify — pure map mutation, no
// branch here changes control flow. See buildRepoRecords for the validation
// (repoFieldSafe, allowed-prefix targets, dedup) it relies on.
func applyRepoCloneEnv(sandboxEnv map[string]string, run types.AgentRun, policy types.RunPolicySpec) {
	if slug := strings.TrimSpace(run.Repo); slug != "" && repoFieldSafe(slug) {
		sandboxEnv["WARDYN_REPO_SLUG"] = slug
		if url := repoCloneURL(slug); url != "" {
			sandboxEnv["WARDYN_REPO_URL"] = url
		}
	}
	if repos := buildRepoRecords(run.Repo, policy.WorkspaceRepos); repos != "" {
		sandboxEnv["WARDYN_REPOS"] = repos
	}
}

// applyEphemeralDirsEnv surfaces a run's ephemeral workspace-source targets as
// WARDYN_EPHEMERAL_DIRS (comma-separated in-sandbox paths with no host mount
// and no clone — the sandbox entrypoint mkdirs them). No-op when there are
// none.
// ponytail: a plain mkdir'd directory has no size cap; a tmpfs mount (with a
// size limit) is the upgrade path if an unbounded scratch dir ever needs one.
func applyEphemeralDirsEnv(sandboxEnv map[string]string, dirs []string) {
	if len(dirs) == 0 {
		return
	}
	sandboxEnv["WARDYN_EPHEMERAL_DIRS"] = strings.Join(dirs, ",")
}

// applyUserDriveEnv announces the mounted USER DRIVE to the sandbox as
// "<target>:ro" or "<target>:rw" — the one in-sandbox signal that a run has
// persistent storage and whether it may write to it. No-op for the runs that
// carry no drive, which is most of them.
//
// It is an ANNOUNCEMENT, never the mechanism: the mount itself is made by the
// driver from SandboxSpec.Drive, so an agent that ignores this variable still
// gets the drive, and one that fabricates it still gets nothing. That split is
// what lets it be non-secret env (invariant 1) beside WARDYN_EPHEMERAL_DIRS —
// and it is why the value names the in-container target and the mode and
// nothing else: the object name, the host path and the drive's name are
// admin-facing, and a member's own run must not be able to read back the
// storage object it was allocated.
//
// The mode suffix is `ro`/`rw` rather than a boolean because that is what the
// mount reads as everywhere else a human sees one (`docker inspect`, `mount`,
// the console's own chip), and a variable an agent is expected to print in a
// startup banner should not need a translation table.
func applyUserDriveEnv(sandboxEnv map[string]string, drive *types.DriveMount) {
	if drive == nil {
		return
	}
	sandboxEnv["WARDYN_USER_DRIVE"] = drive.Target + ":" + driveAuditMode(drive.ReadOnly)
}

// auditDriveMount records run.drive.mount: dispatch attached a member's user
// drive to this sandbox. Silent when no drive was attached — an audit action
// that fires on every run is noise an operator learns to skip past.
//
// Called AFTER CreateSandbox has returned, never beside the spec assembly: the
// driver re-runs the drive's ceiling and deny matrix at bind time and can still
// refuse, so an earlier emit wrote `success` for a mount the next event
// (run.create failure) contradicted. See the call site in runs_dispatch.go.
//
// Its own event rather than a field on the run's policy snapshot, because the
// drive is the ONE thing in a run's spec that OUTLIVES the run: "which run
// mounted whose storage, in which mode" is a question asked months later about
// data that is still there. Actor SYSTEM — a member ticked a checkbox, and what
// is recorded is dispatch's own resolution of that flag into an object.
//
// TARGET IS THE STORAGE OBJECT, not the run id — the run is already named by
// the event's own run id, so spending the target on it a second time made the
// row's one rendered detail redundant. The console's Audit tab renders a row as
// time, actor, action and `target`, and nothing at all from `data`
// (`AuditTab`), so the object name reaches a screen only from here: an operator
// (or the member, reading their own run's rows through GET /audit?run_id=) sees
// WHICH volume or share directory this run was handed, which is the whole
// question the row exists to answer months later.
//
// `object` repeats it in the payload, where the other four fields live, and
// `drive` is the per-person HOME SEGMENT — not the drive object's name, which
// is what the same key carries on the drive.grant.* rows
// (userDriveGrantAuditData). Both are admin-facing in the sense that they name
// storage rather than a request: neither ever enters the sandbox env, which
// carries the target and the mode and nothing else (applyUserDriveEnv), so an
// agent cannot read back the object it was allocated even though the human
// whose run it is can. `enforcement` is what actually binds the drive's bytes
// (types.StorageEnforcement), logged beside the mount so a size read back in a
// later dispute carries its caveat instead of reading as a promise. Five
// fields, matching docs/AUDIT-ACTIONS.md exactly.
//
// The nil test lives HERE rather than at the assembly site for a mechanical
// reason worth stating: dispatchRun sits at its gocyclo ceiling, so one more
// branch there is a lint failure. It belongs with the payload anyway.
func (s *Server) auditDriveMount(ctx context.Context, runID uuid.UUID, drive *types.DriveMount) {
	if drive == nil {
		return
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.drive.mount",
		drive.ObjectName, "success", mustJSON(map[string]any{
			"backend":     drive.Backend,
			"drive":       drive.HomeName,
			"enforcement": drive.Enforcement,
			"mode":        driveAuditMode(drive.ReadOnly),
			"object":      drive.ObjectName,
		})))
}

// driveAuditMode renders a drive's mode as the SAME two words the sandbox env
// (applyUserDriveEnv) and the run.drive.mount audit row use — one vocabulary
// for the machine-facing surfaces, so a log line and a run's env agree.
//
// NOT the console, which is a HUMAN surface and says "Read-only"/"Writable"
// (MODE_RO/MODE_RW in ui/src/app/lib/user-drives-copy.ts, frozen copy). Those
// two vocabularies are deliberately different and must not be reconciled: `ro`
// is what a mount reads as in `docker inspect` and in `mount`, and a chip in a
// table is prose.
func driveAuditMode(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}
