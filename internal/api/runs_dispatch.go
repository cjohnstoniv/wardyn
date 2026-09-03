// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// dispatchParams carries the per-run inputs the create / workspace handlers
// thread into dispatchRun. It replaces an 11–12-arg positional signature whose
// two adjacent map[string]string fields — GitPATGrants and SSHGrants — were a
// silent swap hazard: a transposed pair still compiled and ran, wiring the
// wrong credential family onto every host. Named fields make each call site
// self-documenting; the zero value is the "none" case per field.
type dispatchParams struct {
	RunToken           string               // proxy-verifiable run token (never a usable in-sandbox secret)
	Image              string               // resolved sandbox OCI image (convention or built devcontainer)
	Policy             types.RunPolicySpec  // egress/resource policy (dispatchRun mutates a local copy)
	FirstGitHubGrantID *uuid.UUID           // surfaced as WARDYN_GITHUB_GRANT_ID; nil when no GitHub grant
	GitGrants          map[string]uuid.UUID // git-broker allowlist {"<org>/<repo>": grant_id}; proxy-side only
	// The never-resident git_pat posture is deliberately NOT a field here: it is
	// a DEPLOYMENT-wide operator escape hatch (Config.DisableGitPATBroker,
	// WARDYN_GIT_PAT_BROKER), so dispatchRun derives it once rather than every
	// lane remembering to pass it. It used to be an optional field, and no
	// production caller set it — see dispatchRun's derivation for what that cost.
	GitPATGrants     map[string]string          // {host: grant_id} for non-GitHub PAT hosts
	SSHGrants        map[string]string          // {host: grant_id} for SSH clone hosts
	Injections       []runner.InjectionGrant    // proxy-side credential injections
	Interactive      bool                       // idle box for `wardyn attach` (no agent exec, no completion watcher)
	TaskMode         string                     // "exec" for the BYOA/CI plain-command lane; "" for the agent harness
	InteractiveStart string                     // "agent" opens the attach shell in the image's agent CLI; "" / "shell" = a bare shell. Interactive runs only.
	SeedAutoTools    bool                       // true lets an interactive run's boot seed use tools before attach (--dangerously-skip-permissions for that pre-attach span). Interactive + agent-started + non-empty seed only.
	ToolApprovals    string                     // "hold" routes an AUTONOMOUS run's tool calls to a Wardyn approval instead of running unsupervised. "" / "auto" = today's skip-permissions. Non-interactive runs only.
	BedrockRef       *types.WorkspaceBedrockRef // picked workspace's Bedrock region/model override; nil => global config
	ExtraEnv         map[string]string          // extra NON-SECRET sandbox env: the pre-login WARDYN_AWS_SSO_CONFIG_B64 for an AWS harness login, the site-config probe's own settings
	// Toolchains is the requirements-driven subset of the toolchain-fidelity
	// env this run needs (runToolchainNeeds over its workspaces' profiles).
	// nil = the run has NO workspace context (ad-hoc/BYO/scan/login/composer
	// runs): nothing was scanned and nothing declared, so dispatch keeps the
	// full accommodation set — "unknown" must not break the proven CI and
	// ad-hoc lanes. Non-nil = only what the scans actually detected lands.
	Toolchains *toolchainNeeds
	// MemberMounts, when its Roots are non-nil, marks this as a run against a
	// MEMBER-OWNED workspace and carries the operator/MDM-set roots that member's
	// local_dir binds must resolve inside plus which sources those binds are
	// (memberMountPosture, workspace_refs.go). Roots ride straight onto
	// SandboxSpec.MemberMountRoots and Sources stamp runner.Mount.MemberAuthored,
	// so the driver re-checks every MEMBER-authored bind against the canonicalized
	// real path as late as this process can. The ZERO value is every operator run
	// — the driver then takes exactly today's path.
	MemberMounts memberMountPosture
	// EphemeralDirs are the in-sandbox scratch-directory targets this run's
	// ephemeral workspace source(s) declare — no host mount, no clone; the
	// sandbox just needs the directory to exist. Surfaced as
	// WARDYN_EPHEMERAL_DIRS (comma-separated); nil/empty adds nothing.
	EphemeralDirs []string
	// The acting principal's ceiling is deliberately NOT a field here: it is a
	// required positional argument of dispatchRun/dispatchAndSettle, because an
	// optional field defaulting to "no ceiling" made enforcement opt-in per call
	// site and three of five lanes had opted out. See dispatchCeiling
	// (runs_dispatch_ceiling.go).
	//
	// Drive is the acting principal's USER DRIVE, resolved and narrowed at
	// create time by seedRequestDrive (migration 0054). NIL for every run that
	// did not ask for one — which is every run on a deployment that has
	// allocated no drives, and every scan/probe/harness lane, none of which
	// carries a member principal to resolve a drive for.
	//
	// A CREATE-TIME SNAPSHOT by necessity, not by preference: resolution keys on
	// capabilitySubjects (the caller's OIDC sub/email/groups) and the run row
	// carries only CreatedBy, so there is nothing here to re-resolve from —
	// exactly the constraint dispatchCeiling is a snapshot for. dispatchRun
	// runs inline in the create request, so the snapshot has no staleness window
	// to be stale in. See user_drives_run.go.
	Drive *types.DriveMount
	// ResolvedManaged, when non-nil, is filled in by dispatchRun with whether
	// the ACTUAL resolved llmTransport used the Wardyn-managed subscription
	// lane (llm.injectManaged — resolveLLMTransport's MANAGED subscription
	// section). W20-llm-transport-matrix-2: launchRecordRun's pre-dispatch
	// llm_mode guess for the session entry is a mount/integration check that
	// cannot see this lane at all (it resolves only here, inside dispatch,
	// gated on s.managedInjectReady) — that guess would otherwise say "none"
	// for a session the managed subscription actually credentialed. nil for
	// every other caller: a no-op.
	ResolvedManaged *bool
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
// PHASE ORDER IS THE CONTRACT: the policy phases below narrow `policy` in
// sequence, and confineGitBrokerEgress runs LAST of the phases that touch the
// ALLOWLIST so nothing above it can re-add a broker-managed host;
// reassertCeilingDenies then runs after it (it only adds denies and only
// removes credentials, so it cannot un-confine anything, and it has to sit
// below every WIDENING phase — see its own ordering argument). The ProxyConfig
// snapshot then captures that final policy, and the run.policy.effective audit
// event discloses it. Keep new phases inside this sequence, in the right place
// — a phase hoisted into a caller silently loses the ordering guarantee.
//
//nolint:funlen // Deliberate: one linear provision → CAS → compensate sequence whose phase ORDER is the security contract (see above). Each phase already lives in its own helper; splitting the sequence would hide the ordering behind a call graph and make it unauditable in one scope. Low branching — passes gocyclo/gocognit, just long.
func (s *Server) dispatchRun(ctx context.Context, run types.AgentRun, ceiling dispatchCeiling, p dispatchParams) {
	// FAIL CLOSED on a ceiling nobody resolved. The compiler already forces a
	// lane to pass SOMETHING; this refuses the one thing it could pass without
	// deciding — the zero value — so "a new dispatch lane forgot the ceiling"
	// surfaces as a failed run with an audit row rather than as a sandbox that
	// quietly ran with no profile enforcement and no run.ceiling.reassert to
	// show for it. Unreachable from any lane in tree (a compile-time enumeration
	// of the construction sites is TestDispatchCeilingIsRequiredAtEveryLane).
	if !ceiling.resolved {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"note": "dispatch was handed an unresolved governance ceiling (the dispatchCeiling zero value); " +
					"refusing to launch rather than running with no profile enforcement",
			})))
		s.failAndRevoke(context.WithoutCancel(ctx), run.ID, types.RunPending,
			"this run was not launched: its dispatch lane did not resolve the acting principal's governance ceiling")
		return
	}
	// Only the values a phase below REBINDS get a local alias; everything else is
	// read straight off p (the named-field struct is already self-documenting).
	image := p.Image
	policy := p.Policy // local copy; the phases below mutate policy.AllowedDomains
	injections := p.Injections
	interactive := p.Interactive

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
	claimed, cerr := s.casRunState(ctx, run.ID, types.RunPending, types.RunStarting)
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
	sandboxEnv := buildBaseSandboxEnv(run, proxyURL, p.Toolchains)
	// The withheld ssh_key / git_pat hosts are NEVER silent: the operator asked for
	// a credential and is not getting it, so say why — same shape as the codex-cli
	// drop (applySSHLaneWarnings, runs_create.go), minus the response warning,
	// which dispatch has no caller to return one to.
	// THE NEVER-RESIDENT git_pat LANE, derived from the deployment's own flag
	// rather than taken from the caller. It was a dispatchParams field that NO
	// production literal ever set, so every real dispatch ran with it false:
	// patBrokerGrants returned nil, ProxyConfig.PATGrants stayed empty, and
	// applyDispatchModeEnv left WARDYN_GIT_PAT_GRANTS in the sandbox env — i.e.
	// exactly the pre-0.7 resident posture docs/ENV.md and docs/POLICIES.md say
	// only `WARDYN_GIT_PAT_BROKER=off` restores. Config.DisableGitPATBroker, the
	// field the flag resolves into, was read by nothing at all. The promise was
	// documented in three places and delivered in none.
	//
	// Derived HERE, once, for the reason the ceiling argument above is required:
	// a per-lane opt-in that defaults to the weaker posture is a control whose
	// default is "off by omission", and the omission is invisible.
	patBroker := !s.cfg.DisableGitPATBroker
	droppedSSH, droppedPAT := applyDispatchModeEnv(sandboxEnv, run, interactive, p.TaskMode, p.InteractiveStart, p.SeedAutoTools, p.ToolApprovals, p.FirstGitHubGrantID, p.GitPATGrants, p.SSHGrants, p.GitGrants, patBroker)
	s.auditBrokeredGrantDrop(ctx, run.ID, "ssh_key", "run.ssh.brokered_forge", droppedSSH,
		"this run is brokered for a repo on this forge, so the git-broker route is its only route to it BY NAME "+
			"(confineGitBrokerEgress denies the forge and its SSH endpoint). Withholding the key is load-bearing, not "+
			"belt-and-braces: those denies are name-keyed, so under allow_all_egress a resident key could still have "+
			"reached the forge by raw IP. Drop the github_token grant to push with your own key instead")
	s.auditBrokeredGrantDrop(ctx, run.ID, "git_pat", "run.git_pat.brokered_forge", droppedPAT,
		"this run is brokered for a repo on this forge, so the git-broker route is its only route to it BY NAME "+
			"(confineGitBrokerEgress denies the forge's HTTPS hosts). Withholding the PAT is load-bearing, not "+
			"belt-and-braces: wardyn-git-helper's refusal only binds a caller that goes through git, and those denies "+
			"are name-keyed, so under allow_all_egress a resident PAT could still have reached the forge by raw IP — "+
			"and a GitHub PAT is typically a user PAT, wider than the repo-scoped installation token beside it. "+
			"Drop the github_token grant to push with your own PAT instead")
	applyRepoCloneEnv(sandboxEnv, run, policy)
	applyEphemeralDirsEnv(sandboxEnv, p.EphemeralDirs)
	// Caller-supplied non-secret env (p.ExtraEnv): the AWS harness login's
	// pre-login WARDYN_AWS_SSO_CONFIG_B64, or the site-config probe's own
	// settings — the same "only a discriminator + non-secret payload changes;
	// clone/grants/EGRESS/recording/LLM-injection are identical" contract as
	// scan/verify/exec. resolveLLMTransport below sees an ordinary
	// (no-WorkspaceID) claude-code run and injects the managed subscription
	// token proxy-side from the launcher's policy.
	for k, v := range p.ExtraEnv {
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
		// Capture the run's PRE-substitution egress: it decides which redirects are
		// in scope for THIS run (GAP-EGRESS-2), used by both the egress substitution
		// and the token-injection plan so they can never disagree.
		preDomains := append([]string(nil), policy.AllowedDomains...)
		policy.AllowedDomains = substituteArtifactEgress(policy.AllowedDomains, siteCfg)
		// Deny each network-only redirect's From host so an allow_all_egress run
		// cannot keep reaching the public host the operator redirected away
		// (GAP-EGRESS-4); deny beats allow-all in the proxy's evaluator.
		policy.DeniedDomains = appendNetworkRedirectDenials(policy.DeniedDomains, siteCfg)
		artifactPlan = s.planArtifactRedirect(ctx, run, siteCfg, preDomains)
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
	llm := s.resolveLLMTransport(ctx, run, &policy, sandboxEnv, injections, interactive, p.TaskMode, proxyURL, p.BedrockRef)
	if p.ResolvedManaged != nil {
		*p.ResolvedManaged = llm.injectManaged
	}

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

	// Corporate CA trust (WARDYN_TRUSTED_CA_FILE): append the operator's PEM to
	// this run's sandbox CA trust exactly as provisionDispatchMITMCA does for
	// the per-run MITM cert — see installSandboxTrustedCA. Runs unconditionally
	// (no-op when the knob is unset) so a non-MITM run gets it too. Placed
	// before resolveEnvSecretGrants below, so a user env_secret named
	// SSL_CERT_FILE can never clobber the bundle this just staged.
	installSandboxTrustedCA(s.cfg.TrustedCAPEM, sandboxEnv)

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

	// BROKERED GIT: make the broker route the only route to the managed host names.
	// Last of the policy
	// phases so nothing above can re-add a managed host. See
	// confineGitBrokerEgress; the run.policy.effective audit below records the
	// narrowed envelope, so the removal is disclosed, not silent.
	if confined := confineGitBrokerEgress(&policy, p.GitGrants); len(confined) > 0 {
		slog.InfoContext(ctx, "wardynd: git-broker run — broker-managed hosts confined to the /wardyn/gh/ route",
			slog.String("run_id", run.ID.String()), slog.Any("hosts", confined))
	}

	// GOVERNANCE CEILING RE-ASSERTION: union the acting principal's assigned
	// profile's denies into the policy, drop every injection rule and BROKERED
	// credential lane that reaches a denied host. Placed here, after
	// confineGitBrokerEgress and after every widening phase above, because a
	// create-time deny is defeated by those widenings — the artifact-redirect
	// phase adds corp hosts AND authors token injections for them mid-dispatch.
	// A no-op with no assigned profile. See reassertCeilingDenies.
	s.reassertCeilingDenies(ctx, run, &policy, &injections, ceiling, &p, sandboxEnv)

	// Host bind mounts (policy WorkspaceMounts + the host-mode Bedrock ~/.aws
	// read-only mount) — operator-authored, never agent-chosen; see buildRunMounts.
	mounts := buildRunMounts(policy, llm, p.MemberMounts)

	// Operator-wide upstream/corp proxy (site-config → ProxyConfig.UpstreamProxyURL);
	// fail SAFE to "" (direct egress) with an audit event — see resolveRunUpstreamProxy.
	upstreamProxyURL := s.resolveRunUpstreamProxy(ctx, run.ID, siteCfg, siteCfgErr)

	// LLM-inspection detection corpus: resolve WorkspaceSecretNames -> VALUES
	// from the secret store onto THIS dispatch's local policy copy only (never
	// a stored/ceiling spec) — see resolveLLMInspectionSecrets. Last-mile,
	// right before the ProxyConfig snapshot below captures policy: the proxy
	// sidecar is the only consumer that ever needs the resolved values.
	s.resolveLLMInspectionSecrets(ctx, run, &policy)

	// env_secret grants: stored secret -> sandbox ENV VAR, mask-registered. LAST
	// in the env composition (after applyDispatchModeEnv, the artifact config,
	// p.ExtraEnv and resolveLLMTransport's auth vars) so its refusal to overwrite
	// an already-set variable covers every platform-authored key, not just the
	// ones written above it. See resolveEnvSecretGrants.
	s.resolveEnvSecretGrants(ctx, run, policy, sandboxEnv)

	spec := runner.SandboxSpec{
		RunID:            run.ID,
		Image:            image,
		ConfinementClass: run.ConfinementClass,
		Env:              sandboxEnv,
		Mounts:           mounts,
		Drive:            p.Drive,
		// nil for an operator run (the driver then behaves exactly as it does
		// today); non-nil marks a member-owned-workspace run whose MEMBER-AUTHORED
		// binds (stamped above by buildRunMounts) the driver re-checks against
		// these roots — see runner/member_mount.go.
		MemberMountRoots: p.MemberMounts.Roots,
		// Interactive runs come up idle for `wardyn attach`; the driver prepares the
		// workspace (clones the repo into ~/work) on the idle process so the attach
		// shell isn't empty. A non-interactive run's task exec does this itself.
		Interactive: interactive,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        p.RunToken,
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
			// git_pat per-HOST allowlist: the /wardyn/git/ route serves only these
			// hosts, minting the stored PAT proxy-side so it never enters the
			// sandbox. Empty when the lane is off, which makes the route 403 —
			// the same state as a run with no PAT grants at all.
			PATGrants: patBrokerGrants(p.GitPATGrants, patBroker),
			// Resolved above from site-config.UpstreamProxySecretRef; "" when
			// unconfigured or unresolvable (direct dial, backward-compatible).
			UpstreamProxyURL: upstreamProxyURL,
			// WARDYN_TRUSTED_CA_FILE, forwarded verbatim so the sidecar's own
			// outbound TLS trusts it too. "" when the operator knob is unset.
			TrustedCAPEM: s.cfg.TrustedCAPEM,
			// Operator-declared internal hostnames eligible for the proxy's
			// private-IP-guard lift (site-config, read once above as siteCfg;
			// nil on a GetSiteConfig error — fail safe, no lift).
			InternalHosts:        siteCfg.InternalHosts,
			UpstreamProxyNoProxy: siteCfg.UpstreamProxyNoProxy,
			// Operator-configured internal model gateway(s) — WARDYN_ANTHROPIC_
			// BASE_URL/WARDYN_OPENAI_BASE_URL, validated at boot. Empty => every
			// brokered LLM route dials the vendor host, byte-identical to today.
			LLMUpstreams: s.cfg.LLMGateways,
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

	// AUTHORIZATION ENVELOPE — the append-only answer to "what was this agent
	// actually allowed to do?". The run row cannot answer it: agent_runs.policy_id
	// has no FK and no spec column, run_policies.spec is overwritten in place, and
	// an inline/default policy has no stored row at all. Recorded HERE, at the one
	// funnel every dispatch flavour (agent/scan/verify/record/compose/exec) passes
	// through, and AFTER every widening phase above (artifact substitution, LLM
	// transport, subscription/Bedrock injection, the inspection gate) — so this is
	// the envelope the proxy really enforces (ProxyConfig.Policy below), never a
	// pre-union guess. One snapshot covers egress + first_use_approval +
	// LLMInspection + mount read-only flags + resource caps. auditablePolicy
	// redacts LLMInspection.WorkspaceSecretValues (which resolveLLMInspectionSecrets
	// just populated with the REAL resolved corpus for the proxy above) — that
	// field's own doc comment says NEVER logged (W12-A-2), so the audited copy is a
	// Clone with the values replaced by a count; the live `policy` the ProxyConfig
	// snapshot below references still carries the real values.
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.effective",
		run.ID.String(), "success", mustJSON(auditablePolicy(policy))))

	// Stamped BEFORE CreateSandbox, not after (review round 2, L7): the row
	// still carries the heartbeat it was born with, which any image
	// build/pull longer than the stale window has already let expire — so
	// without an early stamp the next reconcile sweep on any replica can
	// adopt a run this dispatch is still setting up (reconcile.go). It used
	// to be stamped once CreateSandbox returned ("the run now has something
	// to watch"), but CreateSandbox itself can now block for
	// canaryWaitTimeout (the k8s substrate's agent-pod readiness wait, on
	// top of whatever image pull it was already doing) — stamping first
	// covers that latency too instead of leaving it entirely un-leased.
	// stampRunWatcherLease only touches run.ID (idempotent heartbeat write;
	// no dependency on sb.Ref), so moving it earlier is safe.
	s.stampRunWatcherLease(ctx, run.ID)

	sb, err := s.cfg.Runner.CreateSandbox(ctx, spec)
	if err != nil {
		// Conditional: only mark FAILED if still STARTING. A kill landing between the
		// entry claim and this failure moved the run to KILLED — don't clobber that
		// terminal state (mirrors the STARTING->RUNNING guard below).
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "the sandbox could not be created: "+err.Error())
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		return
	}
	// HOLD the run's watcher lease for the rest of dispatch — starting the moment
	// there is a sandbox to watch and BEFORE SetSandboxRef publishes its ref, so a
	// run whose sandbox_ref is set is ALWAYS backed by a fresh lease while its
	// dispatcher lives. A single stamp (the old behavior) went stale after
	// watcherLeaseStaleAfter if the SetSandboxRef→completion-watcher window ran long
	// (a slow BYOI selftest before Exec), and a stale lease on a still-dispatching
	// run is what let another replica's sweep adopt — and, with the strand guard's
	// age gate now removed, FINALIZE — a run this dispatch was still setting up.
	// Holding it continuously makes a stale lease UNAMBIGUOUS: the dispatcher is
	// gone. That is exactly what lets sweepRunWatchers' strand guard finalize a
	// never-exec'd run with NO age gate — closing the fast-crash/multi-replica C3
	// strand the age gate left open (GAP-RECONCILE-2). The completion watcher starts
	// its OWN hold before this one stops (startCompletionWatcher), so lease coverage
	// is continuous across the handoff; this hold is released when dispatch returns.
	stopDispatchLease := s.holdRunWatcherLease(ctx, run.ID)
	defer stopDispatchLease()

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
	applied, uerr := s.casRunState(ctx, run.ID, types.RunStarting, types.RunRunning)
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

	// The run is live: record how long creation -> RUNNING took (image pull,
	// devcontainer build, sandbox create). This is the one operational number the
	// audit stream does not carry.
	s.metrics.sandboxLaunched(s.cfg.Now().Sub(run.CreatedAt))

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
//
// mainProcessExecID is the agent_exec_id persisted when Runner.Exec succeeds
// with an EMPTY id ("", nil) — an EXEC-LESS substrate (krun runtime; see
// runtimeSupportsExec in the docker driver, which krun-vs-kata backs on
// EITHER a CC2 or CC3 confinement class depending on the host/operator
// pin, so this can never be inferred from run.ConfinementClass): the
// workload runs as the container's own main process, so container Status is
// already authoritative and there is no separate exec to track.
//
// W15-c: a bare "" used to be persisted for this case, but reconcile.go's
// watcher-sweep strand guard also reads "" as "never exec'd, no agent will
// ever run" — one crash-recovery signal doing two jobs — so it finalized
// FAILED and tore down healthy exec-less runs the moment their watcher lease
// went stale. The sentinel gives each meaning its own value: "" now means
// ONLY "SetRunAgentExecID was never called" (genuinely stranded); this
// constant means "called, deliberately empty". The docker driver's
// AgentStatus (internal/runner/docker/driver.go) maps the sentinel back onto
// container Status exactly like "" always has — duplicated there (same
// literal) rather than imported, because internal/api sits above the
// concrete runner substrate and must stay target-agnostic.
const mainProcessExecID = "main-process"

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
		// BYOI: gate the task exec on a passing selftest (fail closed) — but
		// refuse UP FRONT, before even attempting the selftest, on an
		// exec-less (krun) substrate: see byoiExecLessRefused.
		if byoi {
			if s.byoiExecLessRefused(ctx, run, ref) {
				return
			}
			if !s.byoiSelftest(ctx, run, ref, true /* fail-closed */) {
				s.stopSandboxOrAudit(ctx, run.ID, ref, "run.selftest")
				s.failAndRevoke(ctx, run.ID, types.RunRunning,
					"the BYOI image failed its agent-run --selftest (missing shell/harness binary, or a nonzero selftest exit)")
				return
			}
		}
		argv := []string{"/usr/local/bin/agent-run", run.Task}
		execID, xerr := s.cfg.Runner.Exec(ctx, ref, argv)
		if xerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": xerr.Error()})))
			s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
			// Conditional: a concurrent kill may have moved RUNNING->KILLED; don't
			// clobber it with FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunRunning, "the agent process could not be started in the sandbox: "+xerr.Error())
			return
		}
		// Persist the agent exec id so the boot reconciler can observe AGENT liveness
		// (ExecInspect) across a wardynd restart: an idle-container exec run whose
		// agent already exited must finalize + revoke, not strand RUNNING.
		// Best-effort like SetSandboxRef. execID=="" (no error) is the exec-less
		// substrate (container==agent) — persist the mainProcessExecID sentinel
		// instead of the bare "" reconcile.go's strand guard reserves for a run
		// that never got this far (see mainProcessExecID's doc comment, W15-c).
		persistExecID := execID
		if persistExecID == "" {
			persistExecID = mainProcessExecID
		}
		_ = s.cfg.Store.SetRunAgentExecID(ctx, run.ID, persistExecID)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": argv})))

		// Completion tracking: watch the agent process to exit and propagate its
		// outcome. The watcher runs on a DETACHED context (NOT ctx — that is the
		// request context, cancelled when the create-run handler returns, which
		// would kill the watcher immediately). See startCompletionWatcher. Passed
		// the RAW execID, not the sentinel: this local value only ever reaches
		// AgentStatus via reconcileWatch's Wait-error fallback (runs_lifecycle.go),
		// where "" already means "use container Status" — the sentinel only
		// matters for a value that gets persisted and re-read after a restart.
		s.startCompletionWatcher(run.ID, ref, execID)
	}
}

// resolveLLMInspectionSecrets resolves the llm_inspection detection corpus
// store->proxy AT DISPATCH: WorkspaceSecretNames (the field a policy is
// actually allowed to author — see types.LLMInspectionSpec, validatePolicySpec
// refuses a raw value on any write) is looked up in the secret store and
// appended to WorkspaceSecretValues on THIS dispatch's local policy copy only
// — never a stored/ceiling spec, never re-read, never logged (the
// run.policy.effective audit above redacts it to a count).
//
// Belt-and-braces (W12-A-2): every resolved value is ALSO registered with the
// run's mask registry, so a verbatim leak into PTY capture, a session
// recording, or any OTHER audit event's Data/Target is scrubbed the same way
// any other run secret is (cmd/wardynd's maskingRecorder) — not merely kept
// out of this one event.
//
// Fail-open per name: a name that no longer resolves (deleted secret, no
// store configured) is skipped and audited by NAME only (never a value), and
// dispatch continues — this is a detection guardrail, not an access-control
// gate (types.LLMInspectionSpec's own doc: "a guardrail + visibility layer,
// NOT exfiltration prevention").
func (s *Server) resolveLLMInspectionSecrets(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec) {
	li := policy.LLMInspection
	if li == nil || len(li.WorkspaceSecretNames) == 0 {
		return
	}
	if s.cfg.Secrets == nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm_inspection.secrets_resolve",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"reason": "no secret store configured", "names": li.WorkspaceSecretNames,
			})))
		return
	}
	var resolved, missing int
	// run.CreatedBy: the run's own owner's row wins, falling back to the
	// operator's (secretOwnerFromRequest.stamped rows never collide with an
	// operator's own run identity string — see injection.go's Get for the
	// same reasoning).
	for _, name := range li.WorkspaceSecretNames {
		val, err := s.cfg.Secrets.For(run.CreatedBy).Get(ctx, name)
		if err != nil || len(val) == 0 {
			missing++
			continue
		}
		resolved++
		li.WorkspaceSecretValues = append(li.WorkspaceSecretValues, string(val))
		if s.cfg.MaskRegistry != nil {
			s.cfg.MaskRegistry.Add(run.ID, val)
		}
	}
	outcome := "success"
	if missing > 0 {
		outcome = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm_inspection.secrets_resolve",
		run.ID.String(), outcome, mustJSON(map[string]any{
			"resolved": resolved, "missing": missing, "names": li.WorkspaceSecretNames,
		})))
}

// envAllowMemberEnvSecret opts a deployment IN to letting MEMBERS hold
// env_secret grants. DEFAULT CLOSED: unset means a member's env_secret grant is
// dropped by filterMemberGrants even when the operator's ceiling lists the exact
// (name, secret) pairing. An operator's own runs are unaffected — the ceiling
// authority is never clamped by its own ceiling.
const envAllowMemberEnvSecret = "WARDYN_ALLOW_MEMBER_ENV_SECRET"

// resolveEnvSecretGrants resolves this run's env_secret grants store->sandbox
// env at dispatch: each grant's scope names a stored secret and the variable to
// put its VALUE under (envSecretScopeFields). This is the whole delivery
// mechanism for the kind — there is no mint, no approval and no broker
// involvement (mintKind refuses env_secret outright), which is why the closest
// precedent is resolveLLMInspectionSecrets and not any of the git lanes.
//
// Every resolved value is registered with the run's mask registry, so a verbatim
// leak into PTY capture, a session recording, or any audit event's Data is
// scrubbed like any other run secret. Values NEVER enter the audit stream: the
// events below carry the variable name and the secret NAME only.
//
// FAIL-CLOSED PER GRANT, deliberately the opposite of resolveLLMInspectionSecrets'
// fail-open: that one feeds a detection corpus, where a missing entry costs
// detection coverage; this one delivers a credential the task needs, where a
// silently absent variable surfaces as an unauthenticated API call the agent
// then reports as a task failure. So a grant is SKIPPED and audited (never
// substituted, never blank-set) when its scope is unreadable, its secret is
// reserved, the store is missing or the value is gone.
//
// It also refuses to OVERWRITE a variable dispatch already set to a NON-EMPTY
// value. Everything in sandboxEnv by this point is platform-authored (the
// harness's own WARDYN_*, the LLM transport's ANTHROPIC_*, artifact-redirect
// config, p.ExtraEnv), and a grant that could replace one would be a config
// override wearing a credential's clothes — the WARDYN_ prefix is already
// refused at write time, and this closes the rest of the set without having to
// enumerate it. Runs LAST in dispatch's env composition so "already set" means
// all of it, not just the part written so far. Non-empty, not merely present:
// an empty value carries no configuration to protect, and treating it as
// occupied would make a placeholder key unfillable for no gain.
func (s *Server) resolveEnvSecretGrants(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, sandboxEnv map[string]string) {
	for _, g := range policy.EligibleGrants {
		if g.Kind != types.GrantEnvSecret {
			continue
		}
		name, secretName, err := envSecretScopeFields(g.Scope)
		skip := ""
		switch {
		case err != nil:
			skip = "scope invalid: " + err.Error()
		case sinkReservedSecret(secretName):
			skip = "references a reserved platform-internal secret name"
		case s.cfg.Secrets == nil:
			skip = "no secret store configured"
		case sandboxEnv[name] != "":
			skip = "the sandbox env already sets this variable; a grant may not override platform-authored env"
		}
		if skip == "" {
			// run.CreatedBy: same owner-then-operator-fallback rule as
			// resolveLLMInspectionSecrets above.
			val, gerr := s.cfg.Secrets.For(run.CreatedBy).Get(ctx, secretName)
			if gerr != nil || len(val) == 0 {
				skip = "secret could not be resolved"
			} else {
				sandboxEnv[name] = string(val)
				if s.cfg.MaskRegistry != nil {
					s.cfg.MaskRegistry.Add(run.ID, val)
				}
			}
		}
		data := map[string]any{"name": name, "secret_name": secretName}
		outcome := "success"
		if skip != "" {
			outcome, data["reason"] = "failure", skip
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.env_secret.resolve",
			run.ID.String(), outcome, mustJSON(data)))
	}
}

// auditablePolicy returns a Clone of policy safe to write to the append-only
// audit log: LLMInspection.WorkspaceSecretValues (the real resolved corpus, whose
// own doc comment says NEVER logged, W12-A-2) is replaced by a redacted count.
// The caller's live policy is never mutated.
func auditablePolicy(policy types.RunPolicySpec) types.RunPolicySpec {
	out := policy.Clone()
	if out.LLMInspection != nil && len(out.LLMInspection.WorkspaceSecretValues) > 0 {
		out.LLMInspection.WorkspaceSecretValues = []string{
			fmt.Sprintf("<%d value(s) redacted>", len(out.LLMInspection.WorkspaceSecretValues)),
		}
	}
	return out
}

// toolchainNeeds is dispatchParams.Toolchains' shape: which of the two
// toolchain-fidelity env groups this run's workspace scans actually detected.
type toolchainNeeds struct{ goTools, jvmTools bool }

// runToolchainNeeds derives a run's toolchainNeeds from its resolved
// workspaces: the union of every attached profile's detections
// (workspacescan.ToolchainNeeds — the same signals the devcontainer emission
// keys on). No workspaces at all, or ANY attached workspace lacking a
// decodable profile (unattached/unscanned/malformed) → nil (UNKNOWN; dispatch
// keeps the full set) — one unscanned workspace in an otherwise-scanned set
// must dominate the same way an all-unscanned set does, or its undeclared
// needs would silently ride on the scanned workspaces' narrower env. Only
// once EVERY workspace decodes a profile does empty needs apply: a workspace
// run's env then states what its requirements ground, nothing more.
func runToolchainNeeds(wsRefs []types.Workspace) *toolchainNeeds {
	profiles := make([]workspacescan.WorkspaceProfile, 0, len(wsRefs))
	for _, ws := range wsRefs {
		if p, ok := workspaceProfile(ws); ok {
			profiles = append(profiles, p)
		}
	}
	if len(wsRefs) == 0 || len(profiles) != len(wsRefs) {
		return nil
	}
	goNeeded, jvmNeeded := workspacescan.ToolchainNeeds(profiles...)
	return &toolchainNeeds{goTools: goNeeded, jvmTools: jvmNeeded}
}

// hasAnthropicAPIKeyInjection reports whether the run already carries an api_key
// injection targeting Anthropic's api-key host — i.e. the operator/compose set up
// the api-key transport for Anthropic. The managed-subscription gate uses it to
// stay a FALLBACK (fire only when nothing else credentials Anthropic), never a
// silent override of an explicit api-key choice. Mirrors the drop-loop's host
// check. Gateway-aware (s.llmProviderFor(agent).host): under a configured
// WARDYN_ANTHROPIC_BASE_URL the grant targets the gateway host, not
// api.anthropic.com — comparing against the hardcoded public host would make
// managed silently fire alongside an explicit api-key choice.
func (s *Server) hasAnthropicAPIKeyInjection(agent string, injections []runner.InjectionGrant) bool {
	p, ok := s.llmProviderFor(agent)
	if !ok {
		return false
	}
	for _, ig := range injections {
		if strings.EqualFold(strings.TrimSuffix(ig.Rule.Host, "."), p.host) {
			return true
		}
	}
	return false
}

// byoiExecLessRefused is the W15-W15f-exec-lane-runtime-4 guard: a BYOI image
// on an exec-less (krun microVM) substrate must never even ATTEMPT the
// selftest. runAsMainProcess (internal/runner/docker/driver.go) makes the
// sandbox's container process ITSELF the agent on that substrate — there is
// no separate exec slot — so byoiSelftest's own Exec would consume the
// sandbox's one process, guaranteeing the task Exec that follows it fails
// against an already-exited container (Exec would be called twice: once for
// the selftest, once for the task). Refuses up front (audit + teardown +
// FAILED) instead of wasting the slot finding that out the hard way.
// Capabilities().Resolved[cc] carries an "oci/krun" runtime label on that
// substrate (Kata/CC3 stays exec-capable: its Resolved label carries no such
// prefix). Reports whether it refused; the caller must return immediately.
func (s *Server) byoiExecLessRefused(ctx context.Context, run types.AgentRun, ref string) bool {
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil || !strings.HasPrefix(caps.Resolved[run.ConfinementClass], "oci/krun") {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"confinement_class": run.ConfinementClass,
			"detail": "BYOI images are refused on an exec-less (krun) runtime: the selftest's own exec " +
				"would consume the sandbox's only process, guaranteeing the task exec that follows it fails",
		})))
	s.stopSandboxOrAudit(ctx, run.ID, ref, "run.selftest")
	s.failAndRevoke(ctx, run.ID, types.RunRunning,
		"BYOI images are not supported on this exec-less (krun) runtime")
	return true
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

// patBrokerGrants converts the run's {host: grant_id} PAT grants into the
// proxy's per-host broker allowlist, or nil when the lane is off.
//
// The username is left empty here on purpose: the control plane resolves the
// host's git username at MINT time (ADO wants "pat", GitLab "oauth2", and an
// operator may override), so duplicating that resolution at dispatch would be a
// second place for it to drift. The proxy uses whatever the mint returns.
func patBrokerGrants(pat map[string]string, on bool) map[string]proxy.PATGrant {
	if !on || len(pat) == 0 {
		return nil
	}
	out := make(map[string]proxy.PATGrant, len(pat))
	for host, id := range pat {
		gid, err := uuid.Parse(id)
		if err != nil {
			continue // a malformed id cannot broker; the host simply is not granted
		}
		out[strings.ToLower(strings.TrimSpace(host))] = proxy.PATGrant{GrantID: gid}
	}
	return out
}
