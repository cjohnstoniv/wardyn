// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// workspace.go carries the WORKSPACE COMPOSITION model and the INTEGRATION
// entity — the two shapes this redesign added, split out of types.go when that
// file crossed the size gate. A workspace is one-or-more sources plus a base
// image plus a requirements contract; an integration is a named connection to
// a system outside Wardyn. Both are refs/NAMES only, never secret values.

// WorkspaceKind discriminates an onboarded Workspace's (legacy, single-source)
// shape. As of the composition model it is a DERIVED READ-ONLY value (see
// Workspace.Kind) mirroring Sources[0] — a multi-source Workspace has no
// single Kind.
type WorkspaceKind string

const (
	WorkspaceKindLocalDir WorkspaceKind = "local_dir"
	WorkspaceKindRepo     WorkspaceKind = "repo"
	// WorkspaceKindContainer was the pre-composition-model shape: an onboarded
	// base IMAGE with no mount (Source was the image ref). The composition
	// model replaces it — a "container" workspace is now an ephemeral Source
	// plus a custom BaseImage — so this value is never derived by
	// deriveWorkspaceMirrors and never written for a new workspace; migration
	// 0029 rewrites every stored 'container' row into that shape.
	//
	// Deprecated: kept for one release so 0029's backfill SQL ('container') has
	// a readable Go anchor, and so any pre-0029 client/fixture payload that
	// still carries "kind":"container" keeps decoding.
	WorkspaceKindContainer WorkspaceKind = "container"
)

// WorkspaceLLMCredMode selected how a run that picks this workspace/container
// was credentialed for model/harness access, pre-Integration. WorkspaceLLMCred
// no longer has a Mode field (it names an Integration instead); this type is
// retained only because internal/api's mode-specific dispatch switches on it —
// a later wave that folds that dispatch into the Integration model can retire
// it.
type WorkspaceLLMCredMode string

const (
	WorkspaceLLMCredNone    WorkspaceLLMCredMode = ""
	WorkspaceLLMCredManaged WorkspaceLLMCredMode = "managed" // Wardyn-managed Claude subscription (setup-token), proxy-injected
	WorkspaceLLMCredAPIKey  WorkspaceLLMCredMode = "api_key" // a named secret, proxy-injected as an api_key grant
	WorkspaceLLMCredBedrock WorkspaceLLMCredMode = "bedrock" // AWS Bedrock (region/model/profile)
)

// WorkspaceLLMCred is the OPERATOR-owned model/harness credential BINDING on a
// workspace: a run that picks this workspace inherits this model access via
// the named Integration (IntegrationCategory=ai_provider) — the generalized
// replacement for the old inline {mode, api_key_secret, bedrock} shape. Refs/
// NAMES only, never secret values (the SiteConfig precedent): the actual
// secret lives in the store, resolved/injected via the Integration at
// dispatch. Mirrors ApprovedEgress's operator-owned discipline (never scan-
// written; cleared on source change). Nil / IntegrationRef="" => no binding;
// the run uses the global provider config, or is a plain governed command when
// it needs no model.
type WorkspaceLLMCred struct {
	// IntegrationRef names the Integration (SiteConfig.Integrations[i].ID) this
	// workspace's model/harness access resolves through.
	IntegrationRef string `json:"integration_ref,omitempty"`
}

// UnmarshalJSON decodes WorkspaceLLMCred, tolerating the pre-Integration wire
// shape a workspace's llm_cred JSONB column may still hold from before this
// type change ({"mode":"managed"|"api_key"|"bedrock","api_key_secret":"...",
// "bedrock":{...}}): those fields have no home on this type anymore, so they
// are silently dropped and the result is an EMPTY IntegrationRef ("no
// binding") rather than a decode error. There is deliberately no reverse
// mapping (mode=api_key -> a synthesized integration ref) — a legacy binding
// names no Integration, so it degrades to unbound rather than guessing one.
func (c *WorkspaceLLMCred) UnmarshalJSON(b []byte) error {
	var wire struct {
		IntegrationRef string `json:"integration_ref"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	c.IntegrationRef = wire.IntegrationRef
	return nil
}

// WorkspaceBedrockRef is a workspace's Bedrock model selection (non-secret; the
// AWS credentials themselves come from the store / mounted ~/.aws, unchanged).
type WorkspaceBedrockRef struct {
	Region     string `json:"region,omitempty"`
	Model      string `json:"model,omitempty"`
	AWSProfile string `json:"aws_profile,omitempty"`
}

// WorkspaceStatus is the onboarding/scan lifecycle of a Workspace. The
// import-pipeline stages (building/verifying/verify_failed/build_error/ready)
// a later "Workspace Import v2" wave had added are RETIRED as of the
// composition model — migration 0029 collapses every stored row back to
// scanned/pending_scan — so a workspace is either not-yet-scanned, mid-scan,
// scanned (ready to use), or errored.
type WorkspaceStatus string

const (
	// WorkspacePendingScan is the initial state: onboarded but not yet scanned.
	WorkspacePendingScan WorkspaceStatus = "pending_scan"
	// WorkspaceScanning: a scan run is in flight (repo scan).
	WorkspaceScanning WorkspaceStatus = "scanning"
	// WorkspaceScanned: profile derived; the workspace is ready to use.
	WorkspaceScanned WorkspaceStatus = "scanned"
	// WorkspaceError means the last scan attempt failed.
	WorkspaceError WorkspaceStatus = "error"
)

// WorkspaceSourceType discriminates one entry in a Workspace's composition.
type WorkspaceSourceType string

const (
	// WorkspaceSourceTypeLocalDir binds a host directory into the sandbox.
	WorkspaceSourceTypeLocalDir WorkspaceSourceType = "local_dir"
	// WorkspaceSourceTypeRepo clones a git repo into the sandbox.
	WorkspaceSourceTypeRepo WorkspaceSourceType = "repo"
	// WorkspaceSourceTypeEphemeral is a scratch directory that exists only for
	// the sandbox's lifetime — no host mount, no clone. It is the composition
	// floor: a Workspace with no other source still has this one, so "one-or-
	// more sources" never means zero.
	WorkspaceSourceTypeEphemeral WorkspaceSourceType = "ephemeral"
)

// WorkspaceSource is one entry in a Workspace's composition — a Workspace is
// one-or-more of these (multiples of the same Type are allowed, e.g. two
// local_dir sources mounted at different targets). Which fields are
// meaningful depends on Type:
//
//	local_dir  Path (host path), Target, Writable
//	repo       Source (slug/URL), Ref, Target
//	ephemeral  Target only — no Path, no Source, never Writable (there is no
//	           host location for a scratch dir to be writable back TO)
type WorkspaceSource struct {
	Type WorkspaceSourceType `json:"type"`
	// Path is the host directory path. local_dir only.
	Path string `json:"path,omitempty"`
	// Source is the repo slug/URL. repo only.
	Source string `json:"source,omitempty"`
	// Ref is an optional git ref (branch/tag/sha). repo only.
	Ref string `json:"ref,omitempty"`
	// Target is the mount/clone/scratch-dir path inside the sandbox.
	Target string `json:"target,omitempty"`
	// Writable opts a local_dir source into read-write (default false =
	// read-only, matching WorkspaceMount.ReadOnly's safe default — this field is
	// a plain bool rather than a *bool because false already IS the safe
	// default, so there is no unsafe zero value to guard against). local_dir
	// only.
	Writable bool `json:"writable,omitempty"`
}

// WorkspaceBaseImage is a Workspace's base-image choice: what the sandbox's
// container image is built FROM, independent of its Sources.
type WorkspaceBaseImage struct {
	// Kind is "recommended" (Wardyn's own convention image for the detected
	// stack), "registry" (an operator-picked published image, named by Image),
	// "custom" (Image is a base, Steps layers Dockerfile lines on top), or
	// "byo" (Image is used verbatim, no layering).
	Kind string `json:"kind"`
	// Image is the base image reference. Meaning depends on Kind: the FROM for
	// "custom", the image itself for "registry"/"byo", unused for "recommended".
	Image string `json:"image,omitempty"`
	// Steps are additional Dockerfile RUN/ENV/ARG lines layered on Image.
	// "custom" only.
	Steps []string `json:"steps,omitempty"`
}

// WorkspaceRequirement is one entry in a Workspace's requirements contract: a
// declared need for a secret, an egress host, or host write access, keyed by
// the grammar documented on Workspace.Requirements.
type WorkspaceRequirement struct {
	// Level is "required" (a run against this workspace cannot proceed without
	// it) or "optional" (a run may proceed without it, degraded).
	Level string `json:"level"`
	// Provenance is "scan_seeded" (the workspace scanner detected the need) or
	// "operator_set" (an operator declared it directly).
	Provenance string `json:"provenance"`
}

// Workspace is an onboarded, admin-reviewed COMPOSITION of one-or-more
// sources (local_dir | repo | ephemeral — an ephemeral scratch dir is the
// floor, so Sources is never empty) a run may attach, plus a base image choice
// and a requirements contract. Import scans/reviews the composition ONCE and
// persists a profile; runs thereafter reference the workspace by id instead of
// a free-text host path or repo slug. Repos are re-cloned fresh per run but
// reuse the scan-once Profile.
//
// Profile is internal/workspacescan's WorkspaceProfile serialized opaquely:
// this type never interprets it, only persists/returns it — internal/workspacescan
// owns the shape and BuiltProfileHash cache-keying. Both are empty
// until Status transitions out of pending_scan.
type Workspace struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Sources is the workspace's composition: one-or-more local_dir/repo/
	// ephemeral sources.
	Sources []WorkspaceSource `json:"sources"`
	// BaseImage is the workspace's base-image choice. Nil means the platform
	// default convention image for the detected/scanned stack.
	BaseImage *WorkspaceBaseImage `json:"base_image,omitempty"`
	// Requirements is the workspace's declared requirements contract: what
	// secrets/egress/write-access a run against this workspace needs, keyed by
	// a fixed grammar of "<type>:<key>" where type is one of:
	//
	//	secret:NAME       a named secret must be resolvable (the store secret
	//	                  NAME), e.g. "secret:acme-anthropic-key"
	//	egress:host       the host must be reachable from the sandbox (a bare
	//	                  host or "*."-wildcard, matching RunPolicySpec.
	//	                  AllowedDomains grammar), e.g. "egress:api.github.com" —
	//	                  unioned into a run's allowlist alongside ApprovedEgress
	//	write:/host/path  the given HOST path must be mounted writable, e.g.
	//	                  "write:/home/user/repo" — matches a Sources[] entry
	//	                  whose Path equals /host/path and Writable=true
	//
	// The map key is exactly "<type>:<key>", type and key joined by ONE colon.
	// A key parser MUST split on the FIRST colon only (never the last): the
	// type prefix is always one of the three fixed tokens above, so splitting
	// on the first colon unambiguously separates it from the key even though a
	// host path (a write: key) may itself legally contain colons.
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"`

	// --- Everything below this point through DefaultTarget is a DERIVED
	// READ-ONLY MIRROR of Sources[0]. NEVER persisted (there is no backing
	// column) and NEVER read on write — populated ONLY by the store's read path
	// (scanWorkspace -> deriveWorkspaceMirrors) from Sources[0], and ONLY when
	// len(Sources)==1. Kept solely so pkg/client and `wardyn workspace list`
	// keep rendering a single-source workspace the way they always have; a
	// multi-source workspace has no single "the" kind/source/ref/target, so
	// these are left zero rather than arbitrarily picking one — callers that
	// need the full picture must read Sources.

	// Kind mirrors Sources[0].Type (single-source only).
	Kind WorkspaceKind `json:"kind"`
	// Source mirrors Sources[0]'s host path (local_dir) or repo slug/URL (repo)
	// (single-source only).
	Source string `json:"source"`
	// Ref mirrors Sources[0].Ref (single-source repo only).
	Ref string `json:"ref,omitempty"`
	// DefaultTarget mirrors Sources[0].Target (single-source only).
	DefaultTarget string `json:"default_target,omitempty"`

	// Profile is internal/workspacescan's WorkspaceProfile, opaque here. Nil/empty
	// until scanned.
	Profile json.RawMessage `json:"profile,omitempty"`
	// ImageRef is the resolved/generated image for this workspace's profile
	// empty until scanned/built.
	ImageRef string `json:"image_ref,omitempty"`
	// BuiltProfileHash is the profile hash ImageRef was built from — the
	// build-once/reuse-many cache key (rebuild only when the profile hash changes).
	BuiltProfileHash string `json:"built_profile_hash,omitempty"`
	// ApprovedEgress is the OPERATOR-owned list of egress hosts explicitly
	// promoted for this workspace (typically from the scanner's content-derived
	// SuggestedEgress, which is advisory and never auto-allowed). Unioned into a
	// run's allowlist alongside the scanned profile's EgressDomains. Never
	// written by a scan; cleared when the composition changes.
	ApprovedEgress []string `json:"approved_egress,omitempty"`
	// ActiveRunID is the in-flight scan/record run for this workspace, so the
	// import panel can poll "is my step still running" without scanning all
	// runs. Nil when no import step is executing.
	ActiveRunID *uuid.UUID `json:"active_run_id,omitempty"`
	// RecordResults is the per-task Record Mode state (taskKey → result: run
	// pointer, mode, status, observations captured server-side from the run's
	// audit events). Opaque map[string]api-owned JSON; written only via the
	// scoped SetWorkspaceRecordResult; cleared when the composition changes
	// (recordings were reviewed against the OLD sources).
	RecordResults json.RawMessage `json:"record_results,omitempty"`
	// LLMCred is the OPERATOR-owned model/harness credential binding: a run that
	// picks this workspace inherits it (refs/names only). Nil => no binding.
	// Folded into the run policy at create (applyWorkspaceCreds); written only
	// via the scoped SetWorkspaceLLMCred, mirroring ApprovedEgress.
	LLMCred   *WorkspaceLLMCred `json:"llm_cred,omitempty"`
	Status    WorkspaceStatus   `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// IntegrationCategory classifies what an Integration connects Wardyn to.
type IntegrationCategory string

const (
	IntegrationAIProvider     IntegrationCategory = "ai_provider"
	IntegrationSCMHost        IntegrationCategory = "scm_host"
	IntegrationArtifactMirror IntegrationCategory = "artifact_mirror"
	IntegrationHostProxy      IntegrationCategory = "host_proxy"
)

// Integration is one operator-configured external connection — an AI
// provider, an SCM host, an artifact mirror, or the corporate host proxy. It
// generalizes the ad hoc per-feature refs SiteConfig carried before it
// (UpstreamProxySecretRef, ArtifactOverride.TokenSecretRef): one shape for "an
// external system Wardyn talks to, plus the secret(s) that authenticate it."
// Like every other SiteConfig-doctrine type, Credentials holds secret NAMES
// (refs), never VALUES — the broker/proxy resolve the named secret at
// dispatch/injection time.
type Integration struct {
	// ID is an operator-chosen stable slug, unique within SiteConfig.Integrations
	// (this is a config sub-object inside the SiteConfig singleton, not its own
	// table, so there is no generated uuid — the operator names it, and
	// WorkspaceLLMCred.IntegrationRef / DefaultFor point at this ID).
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Category IntegrationCategory `json:"category"`
	// Type is the specific provider within Category, e.g. "anthropic"|"bedrock"
	// (ai_provider), "github"|"gitlab"|"azure_devops" (scm_host). Open-ended on
	// purpose — a new provider type is just a new string, no schema change.
	Type string `json:"type"`
	// Disabled turns the integration off without deleting its configuration.
	Disabled bool `json:"disabled,omitempty"`
	// Credentials maps a role (e.g. "api_key", "token") to a store secret NAME —
	// never a value.
	Credentials map[string]string `json:"credentials,omitempty"`
	// Config is provider-specific, non-secret configuration (opaque here, like
	// Workspace.Profile).
	Config json.RawMessage `json:"config,omitempty"`
	// DisabledCapabilities lists capability names this integration does NOT
	// support, so callers don't need a hardcoded per-provider capability matrix.
	DisabledCapabilities []string `json:"disabled_capabilities,omitempty"`
	// DefaultFor lists what this integration is the operator-chosen default
	// for: "agent_runs" (a new run picks this absent an explicit choice) and/or
	// "wardyn_features" (Wardyn's own internal usage, e.g. scan/record runs).
	DefaultFor []string  `json:"default_for,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
