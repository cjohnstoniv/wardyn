// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchParams carries the per-run inputs the create / workspace / verify
// handlers thread into dispatchRun. It replaces an 11–12-arg positional
// signature whose two adjacent map[string]string fields — GitPATGrants and
// SSHGrants — were a silent swap hazard: a transposed pair still compiled and
// ran, wiring the wrong credential family onto every host. Named fields make
// each call site self-documenting; the zero value is the "none" case per field.
type dispatchParams struct {
	RunToken           string                  // proxy-verifiable run token (never a usable in-sandbox secret)
	Image              string                  // resolved sandbox OCI image (convention or built devcontainer)
	Policy             types.RunPolicySpec     // egress/resource policy (dispatchRun mutates a local copy)
	FirstGitHubGrantID *uuid.UUID              // surfaced as WARDYN_GITHUB_GRANT_ID; nil when no GitHub grant
	GitGrants          map[string]uuid.UUID    // git-broker allowlist {"<org>/<repo>": grant_id}; proxy-side only
	GitPATGrants       map[string]string       // {host: grant_id} for non-GitHub PAT hosts
	SSHGrants          map[string]string       // {host: grant_id} for SSH clone hosts
	Injections         []runner.InjectionGrant // proxy-side credential injections
	Interactive        bool                    // idle box for `wardyn attach` (no agent exec, no completion watcher)
	TaskMode           string                  // "exec" for the BYOA/CI plain-command lane; "" for the agent harness
	VerifyPlan         json.RawMessage         // non-nil ⇒ VERIFY run (execs wardyn-verify with these commands)
	ComposeEnv         map[string]string       // non-nil ⇒ COMPOSE run: WARDYN_COMPOSE_* env (discriminator + base64 prompt/schema) for the in-sandbox claude compose wire
}

// dispatchWithVerify is the positional entry point retained for the harness-login
// caller (harnesscred.go), which dispatches a blank interactive box with an
// all-nil grant set (no swap hazard). Every other caller builds dispatchParams
// and calls dispatchRun directly; this thin shim avoids churning that out-of-lane
// file. Prefer dispatchRun for new callers.
func (s *Server) dispatchWithVerify(ctx context.Context, run types.AgentRun, runToken, image string, policy types.RunPolicySpec, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, injections []runner.InjectionGrant, interactive bool, taskMode string, verifyPlan json.RawMessage) {
	s.dispatchRun(ctx, run, dispatchParams{
		RunToken:           runToken,
		Image:              image,
		Policy:             policy,
		FirstGitHubGrantID: firstGitHubGrantID,
		GitPATGrants:       gitPATGrants,
		SSHGrants:          sshGrants,
		Injections:         injections,
		Interactive:        interactive,
		TaskMode:           taskMode,
		VerifyPlan:         verifyPlan,
	})
}

// dispatchRun launches the sandbox via the runner and advances run state. On any
// failure it marks the run FAILED and audits — but never returns the failure to
// the create caller (the run row exists and is queryable). p.RunToken is passed
// to the proxy sidecar via ProxyConfig (verifiable, not a usable secret).
// p.Image is the resolved sandbox OCI image (convention image or a devcontainer
// build result). p.FirstGitHubGrantID, when non-nil, is surfaced in sandbox env
// as WARDYN_GITHUB_GRANT_ID so the git-credential helper can request the token
// via the proxy's local mint route without holding the run token directly.
//
// After Exec starts the agent, dispatchRun launches a DETACHED completion watcher
// goroutine (see startCompletionWatcher): it blocks on Runner.Wait(ref) and,
// when the agent process exits, transitions the run to COMPLETED (exit 0) or
// FAILED (non-zero) and tears the sandbox down — but only if the run is still
// RUNNING, so a concurrent kill/stop is never clobbered.
//
// INTERACTIVE MODE: when p.Interactive is true, dispatchRun does CreateSandbox +
// set RUNNING but SKIPS the agent Exec entirely (no `claude -p`) and does NOT
// start the completion watcher (there is no agent process to wait on — the
// watcher would otherwise mark the idle run COMPLETED the moment Wait failed).
// The sandbox comes up idle (the container holds open via `sleep infinity`) so a
// human can `wardyn attach <id>` and drive it. A non-interactive run is
// unchanged. Pair an interactive run with a never-reap policy (AutoStopAfterSec
// < 0) or the idle reaper will stop the idle sandbox.
//
// VERIFY MODE: a non-nil p.VerifyPlan (JSON []workspacescan.SetupCommand) makes
// this a VERIFY run — it execs wardyn-verify (in the built devcontainer image)
// instead of the scanner or the agent, with the commands riding
// WARDYN_VERIFY_COMMANDS. A verify run still sets WorkspaceID (for the trusted
// result linkage), so p.VerifyPlan is the discriminator between scan-only and
// verify-only in the same dispatch.
func (s *Server) dispatchRun(ctx context.Context, run types.AgentRun, p dispatchParams) {
	runToken := p.RunToken
	image := p.Image
	policy := p.Policy // local copy; the phases below mutate policy.AllowedDomains
	firstGitHubGrantID := p.FirstGitHubGrantID
	gitPATGrants := p.GitPATGrants
	sshGrants := p.SSHGrants
	injections := p.Injections
	interactive := p.Interactive
	taskMode := p.TaskMode
	verifyPlan := p.VerifyPlan

	// Client-disconnect isolation: dispatch is invoked synchronously from the
	// create-run handler, so a client disconnect cancels ctx mid-flight — which would
	// also fail the compensating StopSandbox below on the same dead ctx and orphan a
	// live sandbox. Detach from cancellation (values preserved) so the whole
	// provision → CAS → compensate sequence always completes. The completion watcher
	// already runs on BaseCtx, not ctx.
	ctx = context.WithoutCancel(ctx)

	// KILL-RACE GUARD (entry): claim PENDING->STARTING conditionally. A
	// POST /runs/{id}/kill landing in the pre-dispatch window (grant writes, the
	// ListRuns scan, a minutes-long devcontainer build) CASes PENDING->KILLED and
	// tears down identity/broker. A blind ->STARTING write here would RESURRECT that
	// killed run: the later STARTING->RUNNING CAS would then apply and the run would
	// boot and execute despite the 202 kill. So if the claim does not apply, the run
	// is no longer PENDING (killed/stopped) — abort without dispatching. Every
	// dispatch caller passes a freshly-created PENDING run.
	claimed, cerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunPending, types.RunStarting)
	if cerr != nil || !claimed {
		data := map[string]any{"note": "run left PENDING by a concurrent kill/stop before dispatch; dispatch aborted"}
		if cerr != nil {
			data["error"] = cerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// CC3 host-eBPF blindness, surfaced AUTOMATICALLY. The host Tetragon sensor
	// cannot see inside a Kata microVM guest, so a CC3 run is blind to the
	// ground-truth stream. wardynd knows the resolved confinement class here, so
	// it records the one-time kernel.sensor.blind audit event itself — making the
	// gap VISIBLE regardless of whether the operator set the sidecar env var
	// WARDYN_GROUNDTRUTH_BLIND_RUNS (that path is kept too). Matches the data
	// shape the sidecar emits (reason="cc3-kata-host-ebpf-blind", run_id) so the
	// downstream audit/correlation is identical.
	if run.ConfinementClass == types.CC3 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "kernel.sensor.blind",
			run.ID.String(), "success", mustJSON(map[string]any{
				"reason": "cc3-kata-host-ebpf-blind", "run_id": run.ID.String(),
			})))
	}

	// Sandbox env: non-secret values only (invariant 1). The run token never
	// appears here — the proxy holds it via ProxyConfig.RunToken and injects it
	// when forwarding internal API calls from inside the sandbox.
	// Per-run proxy sidecar (docker hostname) unless the config overrides it.
	proxyURL := cmp.Or(s.cfg.ProxyURL, "http://wardyn-proxy:3128")
	sandboxEnv := buildBaseSandboxEnv(run, proxyURL)
	applyDispatchModeEnv(sandboxEnv, run, verifyPlan, interactive, taskMode, firstGitHubGrantID, gitPATGrants, sshGrants)
	applyRepoCloneEnv(sandboxEnv, run, policy)
	// Compose-only mode (AI Run Composer sandbox backend): the WARDYN_COMPOSE_*
	// env (discriminator + base64 prompt/schema) rides here — the same "only a
	// discriminator + non-secret payload changes; clone/grants/EGRESS/recording/
	// LLM-injection are identical" contract as scan/verify/exec. resolveLLMTransport
	// below sees an ordinary (no-WorkspaceID) claude-code run and injects the
	// managed subscription token proxy-side from the launcher's policy.
	for k, v := range p.ComposeEnv {
		sandboxEnv[k] = v
	}

	// Artifact Repository Redirection (operator-wide site-config): for each
	// configured ecosystem, SUBSTITUTE the corp mirror host for the language's
	// public-registry hosts in this run's egress, deliver the per-tool config
	// (URL-only) into the sandbox, and — for a redirect WITH a token secret —
	// author a proxy-side injection so the token is added on the wire (the sandbox
	// never holds it). No-op when no override is configured. Read once here (the
	// one composition layer every run — agent/verify/record/scan — funnels through).
	// Read the operator-wide site-config ONCE per dispatch. Both consumers below
	// (artifact redirection here + the upstream/corp proxy near ProxyConfig) share
	// this snapshot, so a concurrent admin PUT /api/v1/site-config can never compose
	// a single run from two different snapshots (e.g. new SCM hosts with stale
	// artifact overrides). Store is guaranteed non-nil in dispatch (the run-state
	// CAS transitions below are called unconditionally).
	siteCfg, siteCfgErr := s.cfg.Store.GetSiteConfig(ctx)

	var artifactPlan artifactRedirectPlan
	if siteCfgErr == nil {
		policy.AllowedDomains = substituteArtifactEgress(policy.AllowedDomains, siteCfg)
		artifactPlan = s.planArtifactRedirect(ctx, run, siteCfg)
		for k, v := range artifactPlan.env {
			sandboxEnv[k] = v
		}
		if artifactPlan.configB64 != "" {
			sandboxEnv["WARDYN_ARTIFACT_CONFIG_B64"] = artifactPlan.configB64
		}
	}
	artifactInject := len(artifactPlan.injections) > 0

	// LLM transport resolution (precedence: host-staged subscription > managed >
	// Bedrock > api-key gateway): sets the sandbox auth env (+ the codex-cli
	// OpenAI gateway route), may widen policy egress for Bedrock, and reports
	// which proxy-side injections / TLS-MITM this run needs.
	llm := s.resolveLLMTransport(ctx, run, &policy, sandboxEnv, injections, verifyPlan, interactive, proxyURL)

	// Optional TLS-MITM of opaque LLM CONNECT tunnels: provision a per-run CA
	// when ANY consumer needs one — intercept_tls content inspection,
	// subscription/managed credential injection, artifact-token injection, or
	// Bedrock bearer injection. The PRIVATE key reaches ONLY the proxy sidecar
	// (ProxyConfig below); the sandbox trusts the PUBLIC cert. See
	// provisionDispatchMITMCA for the trust-store wiring.
	mitmForInspect := llmInspectMITMEnabled(&policy)
	var mitmCACertPEM, mitmCAKeyPEM string
	if llm.injectSub || llm.injectManaged || mitmForInspect || artifactInject || llm.injectBedrockBearer {
		var ok bool
		if mitmCACertPEM, mitmCAKeyPEM, ok = s.provisionDispatchMITMCA(ctx, run, sandboxEnv); !ok {
			return
		}
	}

	// Subscription / managed: author the proxy-side sentinel credential grant
	// (see authorSubscriptionInjection for the re-mint + api-key-replacement
	// rationale). A failed grant write already marked the run FAILED — stop.
	if llm.injectSub || llm.injectManaged {
		var ok bool
		if injections, ok = s.authorSubscriptionInjection(ctx, run, llm, &policy, injections); !ok {
			return
		}
	}

	// Bedrock BEARER injection + its per-run MITM host (see
	// authorBedrockBearerInjection). Same stop-on-failure contract.
	var bedrockMITMHosts []string
	if llm.injectBedrockBearer {
		var ok bool
		if injections, bedrockMITMHosts, ok = s.authorBedrockBearerInjection(ctx, run, llm, injections); !ok {
			return
		}
	}

	// Artifact-redirect token injections (authored in planArtifactRedirect, whose
	// egress substitution already added each corp host to policy.AllowedDomains, so
	// the injector's exact-allowlist check passes). Appended AFTER the subscription
	// block, which reslices `injections` in place.
	injections = append(injections, artifactPlan.injections...)

	// Fail CLOSED at schedule time when inspection is REQUIRED but the resolved
	// LLM transport is OPAQUE — see enforceInspectableLLM.
	if !s.enforceInspectableLLM(ctx, run, &policy, llm) {
		return
	}

	// Host bind mounts (policy WorkspaceMounts + the host-mode Bedrock ~/.aws
	// read-only mount) — operator-authored, never agent-chosen; see buildRunMounts.
	mounts := buildRunMounts(policy, llm)

	// Operator-wide upstream/corp proxy (site-config → ProxyConfig.UpstreamProxyURL);
	// fail SAFE to "" (direct egress) with an audit event — see resolveRunUpstreamProxy.
	upstreamProxyURL := s.resolveRunUpstreamProxy(ctx, run.ID, siteCfg, siteCfgErr)

	spec := runner.SandboxSpec{
		RunID:            run.ID,
		Image:            image,
		ConfinementClass: run.ConfinementClass,
		Env:              sandboxEnv,
		Mounts:           mounts,
		// Interactive runs come up idle for `wardyn attach`; the driver prepares the
		// workspace (clones the repo into ~/work) on the idle process so the attach
		// shell isn't empty. A non-interactive run's task exec does this itself.
		Interactive: interactive,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        runToken,
			ControlPlaneURL: s.cfg.ControlPlaneURL,
			// The proxy sidecar enforces THIS run's egress policy; a proxy
			// without a policy fails closed (no egress at all).
			Policy:    policy,
			Injection: injections,
			// Per-run TLS-MITM CA (empty unless intercept_tls): private key to the
			// proxy only; the sandbox trusts the public cert via the agent env.
			MITMCACertPEM: mitmCACertPEM,
			MITMCAKeyPEM:  mitmCAKeyPEM,
			// Operator-configured corp artifact hosts the proxy is allowed to
			// TLS-MITM (beyond the built-in LLM hosts) so a registry token injects on
			// the wire. Only hosts with a resolved token injection appear here — a
			// tight per-host allowlist, never a blanket. See isMITMHost widening.
			MITMHosts: append(append([]string{}, artifactPlan.mitmHosts...), bedrockMITMHosts...),
			// MITM the BUILT-IN LLM hosts only when that's actually intended for this
			// run — subscription OAuth injection or intercept_tls content inspection.
			// The CA above may also be minted purely for artifact-token injection, so
			// this keeps an artifact-only run from TLS-terminating a direct CONNECT to
			// Anthropic/OpenAI it never asked to intercept.
			MITMLLM: llm.injectSub || llm.injectManaged || mitmForInspect,
			// Git-broker per-repo allowlist: the proxy's /wardyn/gh/ route serves only
			// these "<org>/<repo>" keys (each -> its github_token grant), minting the
			// scoped token proxy-side so it never enters the sandbox.
			GitGrants: p.GitGrants,
			// Resolved above from site-config.UpstreamProxySecretRef; "" when
			// unconfigured or unresolvable (direct dial, backward-compatible).
			UpstreamProxyURL: upstreamProxyURL,
		},
		// Hard resource caps. A nil policy block (or a zero field) becomes the
		// driver's conservative platform default, so EVERY sandbox is CPU/memory/
		// PID capped even when the policy sets nothing — a fleet of independent
		// agents must not be able to OOM-kill, fork-bomb, or disk-fill the host or
		// each other (C5).
		Resources: resourceLimitsToRunner(policy.Resources),
		Labels: map[string]string{
			"wardyn.run":   run.ID.String(),
			"wardyn.agent": run.Agent,
		},
	}

	sb, err := s.cfg.Runner.CreateSandbox(ctx, spec)
	if err != nil {
		// Conditional: only mark FAILED if still STARTING. A kill landing between the
		// entry claim and this failure moved the run to KILLED — don't clobber that
		// terminal state (mirrors the STARTING->RUNNING guard below).
		s.failAndRevoke(ctx, run.ID, types.RunStarting)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		return
	}
	if err := s.cfg.Store.SetSandboxRef(ctx, run.ID, sb.Ref); err != nil {
		// A lost sandbox ref is not fatal to THIS dispatch (the run proceeds), but on a
		// daemon restart ReconcileOnBoot finds no ref and finalizes the run FAILED while
		// the container keeps running untracked. Log + audit the loss so the orphan is
		// discoverable, rather than discarding the error silently.
		slog.ErrorContext(ctx, "wardynd: SetSandboxRef failed; sandbox may be orphaned/untracked across a daemon restart",
			slog.String("run_id", run.ID.String()), slog.String("sandbox_ref", sb.Ref), slog.Any("err", err))
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"sandbox_ref": sb.Ref, "set_sandbox_ref_error": err.Error()})))
	}

	// KILL-RACE GUARD: advance STARTING->RUNNING CONDITIONALLY. CreateSandbox can
	// be slow (image pull); a concurrent POST /runs/{id}/kill may have moved the
	// run out of STARTING (to KILLED/STOPPED) and already torn down identity +
	// broker while we were creating the sandbox. An unconditional RUNNING write
	// would resurrect a killed run AND leak the just-created container. So if the
	// conditional transition does NOT apply (the run is no longer STARTING), we
	// tear the sandbox we just created back down and stop — never running Exec or
	// the completion watcher. The kill path already revoked identity/broker; we
	// must not undo its work.
	applied, uerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunRunning)
	if uerr != nil || !applied {
		// Killed/stopped mid-dispatch (or a store error). Free the orphaned
		// sandbox and bail without resurrecting the run. The note states only what
		// was observed: whether the teardown actually happened is stopSandboxOrAudit's
		// to report, never this event's to assert.
		s.stopSandboxOrAudit(ctx, run.ID, sb.Ref, "run.dispatch")
		data := map[string]any{
			"sandbox_ref": sb.Ref,
			"note":        "run left STARTING by a concurrent kill/stop during CreateSandbox; dispatch aborted",
		}
		if uerr != nil {
			data["error"] = uerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// INTERACTIVE vs task exec vs BYOI selftest — see startAgentOrIdle.
	s.startAgentOrIdle(ctx, run, sb.Ref, image, interactive)
}

// startAgentOrIdle is dispatch's final phase, after the run is RUNNING.
//
// INTERACTIVE MODE: skip the agent Exec AND the completion watcher. The
// sandbox is RUNNING and idle (the container holds open), ready for a human to
// `wardyn attach`. There is no agent process, so there is nothing for the
// watcher to Wait on — starting it would have it observe an immediate Wait
// failure (no tracked agent exec) and could prematurely terminate the run.
// BYOI runtime preflight: a wrapped arbitrary base is guaranteed to carry the
// runner tools (the wrap COPYs them), but may still lack a shell or the harness
// CLI. Run `agent-run --selftest` and observe its exit — for a batch run, fail
// CLOSED on nonzero (honest FAILED + audit, never a hang or a silent bad run);
// for an interactive/login box, warn-only (a login box legitimately lacks repo
// wiring and the human sees the shell regardless). Keyed off the wardyn-byoi/
// image tag so convention/devcontainer runs are unaffected. Extracted verbatim
// from dispatchWithVerify.
func (s *Server) startAgentOrIdle(ctx context.Context, run types.AgentRun, ref, image string, interactive bool) {
	byoi := strings.HasPrefix(image, "wardyn-byoi/")

	if interactive {
		if byoi {
			s.byoiSelftest(ctx, run, ref, false /* warn-only */)
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.interactive",
			run.ID.String(), "success", mustJSON(map[string]any{
				"sandbox_ref": ref,
				"note":        "interactive run: no agent task exec'd; awaiting attach",
			})))
		return
	}

	// When a task is provided, launch the agent process inside the now-running
	// sandbox. The driver wraps the argv with wardyn-rec (recording) when
	// configured. Exec failure: audit + stop the sandbox + mark FAILED.
	if run.Task != "" {
		// BYOI: gate the task exec on a passing selftest (fail closed).
		if byoi && !s.byoiSelftest(ctx, run, ref, true /* fail-closed */) {
			s.stopSandboxOrAudit(ctx, run.ID, ref, "run.selftest")
			s.failAndRevoke(ctx, run.ID, types.RunRunning)
			return
		}
		argv := []string{"/usr/local/bin/agent-run", run.Task}
		execID, xerr := s.cfg.Runner.Exec(ctx, ref, argv)
		if xerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": xerr.Error()})))
			s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
			// Conditional: a concurrent kill may have moved RUNNING->KILLED; don't
			// clobber it with FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunRunning)
			return
		}
		// Persist the agent exec id so the boot reconciler can observe AGENT liveness
		// (ExecInspect) across a wardynd restart: an idle-container exec run whose
		// agent already exited must finalize + revoke, not strand RUNNING.
		// Best-effort like SetSandboxRef; "" for exec-less substrates (container==agent).
		_ = s.cfg.Store.SetRunAgentExecID(ctx, run.ID, execID)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": argv})))

		// Completion tracking: watch the agent process to exit and propagate its
		// outcome. The watcher runs on a DETACHED context (NOT ctx — that is the
		// request context, cancelled when the create-run handler returns, which
		// would kill the watcher immediately). See startCompletionWatcher.
		s.startCompletionWatcher(run.ID, ref, execID)
	}
}

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
func buildRunMounts(policy types.RunPolicySpec, llm llmTransport) []runner.Mount {
	var mounts []runner.Mount
	for _, wm := range policy.WorkspaceMounts {
		mounts = append(mounts, runner.Mount{
			Source: wm.Source,
			Target: wm.Target,
			// Safe default: omitted read_only => read-only. RW only on explicit
			// read_only=false in the policy.
			ReadOnly: wm.ReadOnlyOrDefault(),
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
// may give the sandbox host NO direct internet route at all —
// site_config.UpstreamProxySecretRef (admin-authored via PUT /api/v1/site-config)
// names the secret holding the corp CONNECT-proxy URL. The resolved cred-bearing
// URL lands in the sidecar's WARDYN_PROXY_CONFIG_JSON env var, the SAME posture
// as RunToken today: proxy-process-only, never on the sandbox side, masked from
// decision-log/stdout by the proxy — a deliberate, already-documented tradeoff
// (see runner.ProxyConfig.UpstreamProxyURL), not a new one. Fail SAFE: an
// unconfigured ref, an unresolvable secret, or a non-http URL all return ""
// (direct egress, today's behavior) plus an audit event; none of them fail the
// run or crash dispatch. Extracted verbatim from dispatchWithVerify.
func (s *Server) resolveRunUpstreamProxy(ctx context.Context, runID uuid.UUID, siteCfg types.SiteConfig, siteCfgErr error) string {
	if siteCfgErr != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			runID.String(), "failure", mustJSON(map[string]any{"reason": "site-config-read-error"})))
		return ""
	}
	if siteCfg.UpstreamProxySecretRef == "" {
		return ""
	}
	var getSecret func(context.Context, string) ([]byte, error)
	if s.cfg.Secrets != nil {
		getSecret = s.cfg.Secrets.Get
	}
	resolved, failReason := resolveUpstreamProxyURL(ctx, siteCfg.UpstreamProxySecretRef, getSecret)
	if failReason != "" {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			runID.String(), "failure", mustJSON(map[string]any{
				"reason": failReason, "secret_ref": siteCfg.UpstreamProxySecretRef,
			})))
		return ""
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
		runID.String(), "success", mustJSON(map[string]any{"secret_ref": siteCfg.UpstreamProxySecretRef})))
	return resolved
}

// buildBaseSandboxEnv assembles dispatchWithVerify's baseline non-secret sandbox
// env (invariant 1: the run token never appears here): proxy routing, the
// toolchain-fidelity proxy config Maven/Gradle need (they ignore HTTP(S)_PROXY),
// and git commit attribution carrying the sub/act delegation chain. Every later
// phase in dispatchWithVerify only adds to this map, never removes from it.
// Extracted verbatim from dispatchWithVerify — pure construction, no branches
// that affect control flow.
func buildBaseSandboxEnv(run types.AgentRun, proxyURL string) map[string]string {
	return map[string]string{
		"WARDYN_RUN_ID":    run.ID.String(),
		"WARDYN_PROXY_URL": proxyURL,
		// Standard proxy env: agents using HTTP_PROXY-aware clients route
		// through the wardyn-proxy automatically (L2 enforcement).
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		// Exclude the proxy itself and loopback from proxy traversal.
		"NO_PROXY": "wardyn-proxy,localhost,127.0.0.1,::1",
		// Toolchain-fidelity env (PLATFORM-wide, every image — not just the fat
		// full toolchain image). These were found across the cross-language toolchains:
		//   GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles+EXECS
		//     its test binaries in $TMPDIR → "permission denied". Point it at the
		//     agent's exec-allowed HOME. (Plain env survives a shell; only PATH is
		//     reset by a login shell — which wardyn-verify no longer uses.)
		//   MAVEN_OPTS: Maven ALONE ignores HTTP(S)_PROXY (npm/pip/cargo/go/git
		//     honor it) → "Unknown host repo.maven.apache.org". The JVM proxy
		//     sysprops route Maven through wardyn-proxy. (The fat image also bakes
		//     a settings.xml <proxy> as belt-and-braces.)
		//   GRADLE_OPTS: Gradle is the same JVM-networking case as Maven — it
		//     resolves dependencies via java.net's proxy selector, which reads the
		//     standard -Dhttp(s).proxyHost/-port/-DnonProxyHosts sysprops (Gradle's
		//     own docs point at these same properties, normally set in
		//     gradle.properties — GRADLE_OPTS is the env-expressible equivalent, so
		//     it's the exact same JVM opts string as MAVEN_OPTS). Reused verbatim.
		//   NOT covered here (need image/build-time FILE config, not env, so out of
		//     scope for this platform-env pass): apt (/etc/apt/apt.conf.d/*.conf)
		//     and a per-project gradle.properties/init.d script for repos that
		//     don't launch via the gradle/gradlew wrapper JVM. npm/pip/cargo/go/git
		//     already honor HTTP(S)_PROXY above and need nothing further.
		"GOTMPDIR":    "/home/agent/.gotmp",
		"GOCACHE":     "/home/agent/.cache/go-build",
		"MAVEN_OPTS":  mavenProxyOpts(proxyURL),
		"GRADLE_OPTS": mavenProxyOpts(proxyURL),
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
	}
}

// applyDispatchModeEnv sets dispatchWithVerify's run-mode discriminator env vars
// (verify-only / scan-only / exec task mode) plus the non-secret grant-id maps
// (WARDYN_GITHUB_GRANT_ID / WARDYN_GIT_PAT_GRANTS / WARDYN_SSH_GRANTS) that let
// the in-sandbox helpers mint the credentials they're eligible for. Extracted
// verbatim from dispatchWithVerify — every branch here only decides which keys
// land in sandboxEnv, none of them change dispatchWithVerify's own control flow.
func applyDispatchModeEnv(sandboxEnv map[string]string, run types.AgentRun, verifyPlan json.RawMessage, interactive bool, taskMode string, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string) {
	// Governed repo SCAN run: after cloning, the entrypoint runs wardyn-scan (which
	// walks ~/work and PUTs ScanFacts to the brokered scan-results route) INSTEAD of
	// the agent. A non-nil WorkspaceID uniquely marks a scan run (ordinary runs never
	// set it); no agent CLI / model call happens.
	// A verify run (verifyPlan present) execs wardyn-verify in the built image;
	// a scan run (WorkspaceID set, no verify plan) execs wardyn-scan. verifyPlan
	// is the discriminator since both set WorkspaceID for the trusted upload
	// linkage. The approved setup commands are non-secret operator-authored
	// values (secrets are proxy-injected, never resident), so they ride env.
	if len(verifyPlan) > 0 {
		sandboxEnv["WARDYN_VERIFY_ONLY"] = "1"
		sandboxEnv["WARDYN_VERIFY_COMMANDS"] = string(verifyPlan)
	} else if run.WorkspaceID != nil && !interactive {
		// An INTERACTIVE workspace-linked run (interactive Record Mode) is a
		// human-driven sandbox, not a scan — never mark it WARDYN_SCAN_ONLY.
		sandboxEnv["WARDYN_SCAN_ONLY"] = "1"
	}
	// exec task mode (BYOA/CI lane): agent-run runs the task as a plain shell
	// command instead of the agent harness. Only the discriminator rides env —
	// everything above/below (clone, grants, egress, recording) is identical.
	if taskMode == "exec" {
		sandboxEnv["WARDYN_TASK_MODE"] = "exec"
	}
	if firstGitHubGrantID != nil {
		sandboxEnv["WARDYN_GITHUB_GRANT_ID"] = firstGitHubGrantID.String()
	}
	// git_pat grants: surface the {host: grant_id} map so the git-credential
	// helper can mint the stored PAT for a matched non-GitHub host. Non-secret
	// (grant ids, not the PAT); the value is returned only through the brokered mint.
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
	if len(sshGrants) > 0 {
		if b, merr := json.Marshal(sshGrants); merr == nil {
			sandboxEnv["WARDYN_SSH_GRANTS"] = string(b)
		}
	}
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

// hasAnthropicAPIKeyInjection reports whether the run already carries an api_key
// injection targeting api.anthropic.com — i.e. the operator/compose set up the
// api-key transport for Anthropic. The managed-subscription gate uses it to stay
// a FALLBACK (fire only when nothing else credentials Anthropic), never a silent
// override of an explicit api-key choice. Mirrors the drop-loop's host check.
func hasAnthropicAPIKeyInjection(injections []runner.InjectionGrant) bool {
	for _, ig := range injections {
		if strings.EqualFold(strings.TrimSuffix(ig.Rule.Host, "."), "api.anthropic.com") {
			return true
		}
	}
	return false
}

// byoiSelftest runs `agent-run --selftest` inside a BYOI sandbox and waits for
// its exit, auditing the outcome. It relies on the runner's "latest Exec wins"
// contract: this exec is tracked and Wait'd BEFORE the real task exec replaces
// it, so the subsequent task's completion watcher is unaffected. Returns true
// when the selftest passed (exit 0). failClosed only governs the audit tone —
// the caller decides what to do with a false (fail the batch run, or warn-only
// for interactive). A selftest that cannot even start (missing shell/binary,
// exit 127) surfaces as a non-nil Exec/Wait error → returns false.
// byoiSelftestTimeout bounds the fail-closed BYOI selftest gate so a hostile or
// broken base image whose agent-run --selftest hangs cannot block the dispatch
// goroutine forever — on timeout the gate fails closed (returns false).
const byoiSelftestTimeout = 2 * time.Minute

func (s *Server) byoiSelftest(ctx context.Context, run types.AgentRun, ref string, failClosed bool) bool {
	ctx, cancel := context.WithTimeout(ctx, byoiSelftestTimeout)
	defer cancel()
	if _, xerr := s.cfg.Runner.Exec(ctx, ref, []string{"/usr/local/bin/agent-run", "--selftest"}); xerr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": xerr.Error(), "fail_closed": failClosed,
				"detail": "BYOI image could not run agent-run --selftest (missing shell or harness binary?)",
			})))
		return false
	}
	code, werr := s.cfg.Runner.Wait(ctx, ref)
	if werr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": werr.Error(), "fail_closed": failClosed,
			})))
		return false
	}
	if code != 0 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"exit_code": code, "fail_closed": failClosed,
				"detail": "BYOI image failed the agent-run contract selftest",
			})))
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
		run.ID.String(), "success", mustJSON(map[string]any{"exit_code": 0})))
	return true
}
