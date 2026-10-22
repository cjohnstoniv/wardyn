// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchParams carries the per-run inputs the create / workspace handlers
// thread into dispatchRun. It replaces an 11–12-arg positional signature whose
// two adjacent map[string]string fields — GitPATGrants and SSHGrants — were a
// silent swap hazard: a transposed pair still compiled and ran, wiring the
// wrong credential family onto every host. Named fields make each call site
// self-documenting; the zero value is the "none" case per field.
type dispatchParams struct {
	TrustedOutput      bool                 // server-authored probe provenance; never a public request field
	RunToken           string               // proxy-verifiable run token (never a usable in-sandbox secret)
	Image              string               // resolved sandbox OCI image (convention or built devcontainer)
	Policy             types.RunPolicySpec  // egress/resource policy (dispatchRun mutates a local copy)
	FirstGitHubGrantID *uuid.UUID           // surfaced as WARDYN_GITHUB_GRANT_ID; nil when no GitHub grant
	GitGrants          map[string]uuid.UUID // git-broker allowlist {"<org>/<repo>": grant_id}; proxy-side only
	// PATBroker reports whether the never-resident git_pat lane is on: the PAT is
	// minted PROXY-SIDE and no grant id reaches the sandbox env.
	//
	// Set by dispatchRun, never by a caller — it overwrites whatever arrives here
	// from Config.DisableGitPATBroker (WARDYN_GIT_PAT_BROKER), because the
	// posture is a DEPLOYMENT-wide operator escape hatch rather than a per-run
	// choice. It lives on the struct only because applyDispatchModeEnv takes the
	// whole parameter object; a value a lane sets is ignored.
	//
	// That overwrite IS the fix: PATBroker must never be a per-lane opt-in whose
	// default silently reverts to the resident WARDYN_GIT_PAT_GRANTS posture
	// (docs/ENV.md, docs/POLICIES.md: only `off` restores it). A per-lane opt-in
	// that defaults to the weaker posture is a control whose default is "off by
	// omission", and the omission is invisible.
	PATBroker        bool
	PATAPI           bool                    // set by dispatchRun, never a caller: a git_pat grant sets api, so the run needs the MITM CA
	GitPATGrants     map[string]string       // {host: grant_id} for non-GitHub PAT hosts
	SSHGrants        map[string]string       // {host: grant_id} for SSH clone hosts
	Injections       []runner.InjectionGrant // proxy-side credential injections
	Interactive      bool                    // idle box for `wardyn attach` (no agent exec, no completion watcher)
	TaskMode         string                  // "exec" for the BYOA/CI plain-command lane; "" for the agent harness
	InteractiveStart string                  // "agent" opens the attach shell in the image's agent CLI; "" / "shell" = a bare shell. Interactive runs only.
	SeedAutoTools    bool                    // true lets an interactive run's boot seed use tools before attach (--dangerously-skip-permissions for that pre-attach span). Interactive + agent-started + non-empty seed only.
	ToolApprovals    string                  // "hold" routes an AUTONOMOUS run's tool calls to a Wardyn approval instead of running unsupervised. "" / "auto" = today's skip-permissions. Non-interactive runs only.
	// ApprovalExpiryAfter mirrors Config.ApprovalExpiryAfter (dispatchRun sets
	// it from s.cfg) — the SAME ceiling the approval-expiry sweeper actually
	// expires a PENDING approval at. Carried onto the sandbox env only when
	// ToolApprovals=="hold" (WARDYN_APPROVAL_EXPIRY_AFTER, applyDispatchModeEnv)
	// so wardyn-toolgate's -deadline default and agent-run's MCP_TOOL_TIMEOUT
	// track the real ceiling instead of their own hardcoded literal (RL-1).
	ApprovalExpiryAfter time.Duration
	ExtraEnv            map[string]string // extra NON-SECRET sandbox env: the pre-login WARDYN_AWS_SSO_CONFIG_B64 for an AWS harness login, the site-config probe's own settings
	// Toolchains is the requirements-driven subset of the toolchain-fidelity
	// env this run needs (runToolchainNeeds over its workspaces' profiles).
	// nil = the run has NO workspace context (ad-hoc/BYO/scan/login/composer
	// runs): nothing was scanned and nothing declared, so dispatch keeps the
	// full accommodation set — "unknown" must not break the proven CI and
	// ad-hoc lanes. Non-nil = only what the scans actually detected lands.
	Toolchains *toolchainNeeds
	// UserMounts, when its Roots are non-nil, marks this as a run against a
	// MEMBER-OWNED workspace and carries the operator/MDM-set roots that member's
	// local_dir binds must resolve inside plus which sources those binds are
	// (userMountPosture, workspace_refs.go). Roots ride straight onto
	// SandboxSpec.UserMountRoots and Sources stamp runner.Mount.MemberAuthored,
	// so the driver re-checks every MEMBER-authored bind against the canonicalized
	// real path as late as this process can. The ZERO value is every operator run
	// — the driver then takes exactly today's path.
	UserMounts userMountPosture
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
	// Drives are the acting principal's drives, resolved and narrowed at create
	// time by stepDrive (migrations 0054, 0169). EMPTY for every run that
	// did not ask for one — which is every run on a deployment that has
	// allocated no drives, and every scan/probe/harness lane, none of which
	// carries a member principal to resolve a drive for.
	//
	// A create-time snapshot by necessity, not by preference: resolution keys on
	// capabilitySubjects (the caller's OIDC sub/email/groups) and the run row
	// carries only CreatedBy, so there is nothing here to re-resolve from —
	// exactly the constraint dispatchCeiling is a snapshot for. dispatchRun
	// runs inline in the create request, so the snapshot has no staleness window
	// to be stale in. See user_drives_run.go.
	Drives []types.DriveMount
	// MemberDrives are the Drives that are people's own, id to owner, which dispatch re-reads (memberDrivesStillThere).
	MemberDrives map[uuid.UUID]string
	// MainDrive is the main drive create's fold decided (stepMainDrive): its size and whether it
	// is soft. NIL for every lane that has no fold decision (scans, record, verify, logins, probes).
	MainDrive *mainDriveDecision
	// Components is the dispatch half of the run's components, from the gate's
	// decision at create (runs_dispatch_components.go). Zero for every other lane.
	Components componentDispatch
	// PolicyOverridden marks a run whose person edited the New Run cards: the
	// proxy's refusal then names the policy as changed for this run
	// (policyref.Ref.Overridden).
	PolicyOverridden bool
	// Provenance is which layer set each value of the run's policy as create
	// resolved it, recorded on run.policy.resolve. Nil for every lane with no
	// create resolution (scans, record, verify, logins, probes).
	Provenance knobProvenance
	// PushRuleSets are the push content rules per SCM entry the run brings
	// (pushRuleSetsFor), the product after the ceiling. When set, the proxy
	// carries them INSTEAD of the policy's run-wide push_rules.
	PushRuleSets []types.PushRuleSet
	// HarnessToolRules are the run's tool rules per included harness
	// (applyHarnessToolRules). When set, the proxy carries them, and a credential
	// per harness, INSTEAD of the policy's run-wide tool_rules.
	HarnessToolRules []types.HarnessToolRules
	// Harnesses are the run's run_harnesses rows, nil without the harness set;
	// each that names a provider gets its own model credential lane.
	Harnesses []types.RunHarness
	// ADOLaneRepo is the run's own repository as the per-person Azure DevOps
	// lane may serve it. Set only by finishCreateRunLaunch (the create and
	// pool-queue doors) from the request the fold graded; a server-authored lane
	// leaves it empty and resolves from WorkspaceRepos alone.
	ADOLaneRepo string
	// RedirectProbe is the stored egress redirect row the redirect Test probes,
	// nil for every other run. Set only by handleTestSiteConfigRedirect: dispatch
	// plans that one row's token as for a run that reaches its public host, so
	// the probe presents the token proxy-side exactly as a run does.
	RedirectProbe *types.EgressRedirect
	// SigninImageUngranted: Image is its person's own choice and its sign-in
	// variant is not granted to them, so the run keeps Image and gets no
	// desktop sign-ins (signinVariantUngranted). Set only by
	// finishCreateRunLaunch, from the fold.
	SigninImageUngranted bool
}
