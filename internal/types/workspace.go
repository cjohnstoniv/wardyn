// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"sort"
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

// WorkspaceLLMCred is the OPERATOR-owned model/harness credential BINDING on a
// workspace: a run that picks this workspace inherits this model access via
// the named Integration (one of the AI-provider kinds) — the generalized
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
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
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
	// Overrides is this attachment's stance on requirement keys ITS SOURCE
	// declares: reqKey -> off|optional|required (workspace_contract.go's
	// AttachmentOverride* constants); local_dir/repo only (ephemeral has no
	// library contract to override). nil (the field omitted) means "say
	// nothing here" — the server carries forward whatever this source's
	// attachment already had (upsertAndAttach), so a plain composition edit
	// (e.g. a rename) can never silently wipe an override set on an earlier
	// PUT; an explicit map, empty or not, REPLACES it outright.
	Overrides map[string]string `json:"overrides,omitempty"`
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
	// secrets/egress/write-access/integrations a run against this workspace
	// needs, keyed by a fixed grammar of "<type>:<key>" where type is one of:
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
	//	integration:ID    the named Integration (SiteConfig.Integrations[i].ID)
	//	                  must be granted — its hosts opened and its credential
	//	                  (if any) presented, e.g. "integration:git_host:
	//	                  github.com" (ID itself may contain colons; see the
	//	                  split rule below)
	//
	// The map key is exactly "<type>:<key>", type and key joined by ONE colon.
	// A key parser MUST split on the FIRST colon only (never the last): the
	// type prefix is always one of the four fixed tokens above, so splitting
	// on the first colon unambiguously separates it from the key even though a
	// host path (a write: key) or an adopted legacy id (an integration: key)
	// may itself legally contain colons.
	//
	// THREE-TIER SPLIT: this map is now the workspace's OVERLAY — what this
	// workspace additionally declares or restates on top of what its attached
	// sources' own contracts contribute. EffectiveRequirements below is the
	// folded result a run actually consumes.
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"`

	// Attachments is the tier-3 composition: ordered source references and
	// inline ephemeral rows, each with per-attachment target/writable and the
	// workspace's requirement OVERRIDES for that source (workspace_contract.go).
	// Empty on a pre-split row that hasn't migrated its embedded Sources yet —
	// the store's hydrate pass falls back to the embedded column then.
	Attachments []WorkspaceAttachment `json:"attachments,omitempty"`
	// BaseImageID references the shared base-image catalog (tier 2). Nil means
	// "recommended" — the per-workspace derived build — by design, not absence.
	BaseImageID *uuid.UUID `json:"base_image_id,omitempty"`
	// EffectiveRequirements is DERIVED, read-only, never persisted: the
	// FoldWorkspaceContract result over attachments + attached source
	// contracts + the overlay above. This is the map run-create and preflight
	// consume. Populated by the store's hydrate pass; on a pre-split row it
	// equals Requirements verbatim (the fold's zero-source identity).
	EffectiveRequirements map[string]WorkspaceRequirement `json:"effective_requirements,omitempty"`

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
	// DeniedEgress is the OPERATOR-owned mirror of ApprovedEgress: hosts
	// explicitly blocked for this workspace, written by a `deny · always`
	// approval decision (AddWorkspaceEgressDecision) or the denied-egress PUT
	// (SetWorkspaceDeniedEgress). Folded into a run's denied_domains at
	// create time, where deny beats allow, allow_all_egress, and a runtime
	// first-use approval alike. UNLIKE ApprovedEgress, this is NOT cleared
	// when the workspace's sources change (see migration 0040): that reset
	// rule is right for a WIDENING (a stale allow should fail closed), but
	// applying it to a NARROWING would silently drop an operator's permanent
	// deny the moment content changed — a deny needs no re-review against new
	// content.
	DeniedEgress []string `json:"denied_egress,omitempty"`
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

// Integration kinds. An integration is a BASE COMPONENT extended by kind: a
// connection — secrets + egress — never an installer. The closed set below is
// every kind Wardyn has bespoke behavior for (provider transports, brokered
// clone lanes); ANY other kind is a GENERIC open slug (shape-validated), whose
// row carries its own secrets/egress/config — which is everything the runtime
// needs. UI grouping DERIVES from kind (AI set → "AI providers", github_app/
// git_host → "Source control", else → "Connections"); there is no stored
// category.
const (
	IntegrationKindAnthropicAPIKey       = "anthropic_api_key"
	IntegrationKindAnthropicSubscription = "anthropic_subscription"
	IntegrationKindBedrock               = "bedrock"
	IntegrationKindOpenAIAPIKey          = "openai_api_key"
	// IntegrationKindAzureOpenAI is GONE as of 0.5. Its single capability was
	// powering Wardyn's own features — the AI Run Composer — which no longer
	// exists; no agent tool can be pointed at an Azure OpenAI deployment (see
	// harness.go's reasonXAzureHarness). A stored row of the old kind now fails
	// closed at validateIntegrationWrite like any other unknown kind, and an
	// azure-openai-key secret is left untouched but inert.
	IntegrationKindGitHubApp             = "github_app"
	IntegrationKindGitHost               = "git_host"
)

// ClosedIntegrationKinds is the closed kind set — the kinds with bespoke
// behavior in code, and as of 0.5 the ONLY kinds a write may name. A write
// naming one is validated against that kind's contract (config keys, DefaultFor
// eligibility); anything else is refused (validateIntegrationWrite).
var ClosedIntegrationKinds = map[string]bool{
	IntegrationKindAnthropicAPIKey: true, IntegrationKindAnthropicSubscription: true,
	IntegrationKindBedrock: true, IntegrationKindOpenAIAPIKey: true,
	IntegrationKindGitHubApp: true, IntegrationKindGitHost: true,
}

// ClosedIntegrationKindList is ClosedIntegrationKinds in a stable order, for the
// "want one of: …" half of a rejected write's error. Sorted so the message is
// deterministic across map iterations.
func ClosedIntegrationKindList() []string {
	out := make([]string, 0, len(ClosedIntegrationKinds))
	for k := range ClosedIntegrationKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AIProviderKind reports whether kind is one of the five AI provider flavors —
// the set eligible for DefaultFor marks and the run-time model-credential fold
// (applyIntegrationCreds), and the "AI providers" UI group.
func AIProviderKind(kind string) bool {
	switch kind {
	case IntegrationKindAnthropicAPIKey, IntegrationKindAnthropicSubscription,
		IntegrationKindBedrock, IntegrationKindOpenAIAPIKey:
		return true
	}
	return false
}

// Integration delivery modes: how one secret reaches the run.
const (
	// DeliveryProxyHeader: the egress proxy presents the secret in an HTTP
	// header on requests bound for the integration's egress hosts. The sandbox
	// never holds it. The default, never-resident lane.
	DeliveryProxyHeader = "proxy_header"
	// DeliveryResidentFile: the secret is materialized as a file inside the
	// sandbox — a disclosed exception to never-resident.
	DeliveryResidentFile = "resident_file"
	// DeliveryResidentEnv: the secret is exported as an environment variable
	// inside the sandbox — a disclosed exception to never-resident.
	DeliveryResidentEnv = "resident_env"
)

// IntegrationDelivery says how ONE secret reaches the run. Exactly one mode;
// the mode decides which of the other fields apply.
type IntegrationDelivery struct {
	Mode string `json:"mode"` // proxy_header | resident_file | resident_env
	// Header/Format apply to proxy_header: the HTTP field name the proxy adds,
	// and the value template it wraps the secret in (exactly one %s; empty
	// means the raw secret IS the value, i.e. "%s").
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
	// Path applies to resident_file: the in-sandbox path the secret lands at.
	Path string `json:"path,omitempty"`
	// Var applies to resident_env: the environment variable name.
	Var string `json:"var,omitempty"`
}

// IntegrationSecret is one required secret of an integration: a role (what it
// is to this system), a ref into the secret store (a NAME, never a value), and
// how it is delivered. A nil Delivery is allowed on CLOSED kinds only and
// means the kind's own bespoke transport delivers it (the brokered github_app
// halves, git_host clone credentials) — hand-written per provider, not
// declarable here.
type IntegrationSecret struct {
	Role       string               `json:"role"`
	SecretName string               `json:"secret_name"`
	Delivery   *IntegrationDelivery `json:"delivery,omitempty"`
}

// The verification-probe framework (IntegrationProbe, IntegrationProbeStatus,
// POST /integrations/{id}/test) was removed in 0.5 with the integration catalog
// it served. It spent a throwaway confined sandbox to traverse ONE row's egress
// and credential injection; the Settings cards state what is STORED and say so
// plainly ("Wardyn stores this — it doesn't dial the provider to check it"),
// which is the honest claim about a credential nobody has used yet. A real run
// is the real test.

// Integration is one operator-configured external connection: a base
// component — required secrets (each with its delivery), required egress,
// non-secret config, an optional verification probe — extended by kind. Like
// every other SiteConfig-doctrine type, Secrets holds secret NAMES (refs),
// never VALUES — the broker/proxy resolve the named secret at dispatch/
// injection time.
//
// STORAGE COMPATIBILITY: UnmarshalJSON also accepts the pre-base-component
// shape ({category, type, hosts, header, format, credentials, config}) and
// folds it into this one at read time — one-way, write-new (marshal emits
// only this shape). See foldLegacyIntegration.
type Integration struct {
	// ID is an operator-chosen stable slug, unique within SiteConfig.Integrations
	// (this is a config sub-object inside the SiteConfig singleton, not its own
	// table, so there is no generated uuid — the operator names it, and
	// WorkspaceLLMCred.IntegrationRef / DefaultFor point at this ID).
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is the ONE field that says what this connects to: a closed-set kind
	// (ClosedIntegrationKinds) with bespoke behavior, or any other slug — a
	// generic connection whose row carries its whole contract.
	Kind string `json:"kind"`
	// Disabled turns the integration off without deleting its configuration.
	Disabled bool `json:"disabled,omitempty"`
	// Secrets are the required secrets, each with its delivery. Refs/names only.
	Secrets []IntegrationSecret `json:"secrets,omitempty"`
	// Egress is WHERE THE SYSTEM LIVES: the host entries a run granted this
	// integration may reach. This is the reason a host is on a run's egress
	// allowlist, instead of being hand-listed in every workspace that needs it.
	// Entries use the policy allowlist's own syntax (proxy.ValidDomainEntry):
	// an exact host, a leading-"*." wildcard, either optionally ":port".
	//
	// A WILDCARD OPENS THE PATH BUT NEVER CARRIES THE CREDENTIAL: proxy-side
	// injection requires an EXACT allowlist entry (Policy.AllowedExactHost), so
	// a secret can never leak to a wildcard-matched host. An integration with a
	// proxy_header-delivered secret is therefore rejected at write time if any
	// of its egress entries is a wildcard or carries a port — the alternative
	// is a row that looks credentialed and silently isn't.
	Egress []string `json:"egress,omitempty"`
	// Config is non-secret, kind-validated configuration. Closed kinds accept
	// only their known keys (bedrock ⇒ region/model/auth_lane, github_app ⇒
	// app_id/installation_id/host, anthropic_subscription ⇒ lane — an unknown
	// key 400s by name at write); generic kinds take any string keys.
	Config map[string]any `json:"config,omitempty"`
	// Docs optionally points at whatever documents this system, so whoever
	// comes after the operator who added it can find out what it is.
	Docs string `json:"docs,omitempty"`
	// DisabledCapabilities lists capability names this integration does NOT
	// support, so callers don't need a hardcoded per-provider capability matrix.
	DisabledCapabilities []string `json:"disabled_capabilities,omitempty"`
	// DefaultFor lists what this integration is the operator-chosen default
	// for: "agent_runs" (a new run picks this absent an explicit choice) and/or
	// "wardyn_features" (Wardyn's own internal usage, e.g. scan/record runs).
	DefaultFor []string  `json:"default_for,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// legacyTopology marks a row that FOLDED from a legacy-shaped
	// artifact_mirror/host_proxy category — network topology, not an
	// integration; its config lives (and stays) under Corporate network.
	// IntegrationList's decode drops such rows from this surface. Unexported:
	// never serialized, never set on a new-shape row.
	legacyTopology bool
}

// IntegrationCredentialToken is the secret ROLE a generic header-delivered
// credential uses — the proxy-injected lane every header-authenticating
// system shares. The closed kinds keep their own role names, which encode a
// lane the runtime treats differently ("api_key", "pat", "ssh_key").
const IntegrationCredentialToken = "token"

// CredentialsMap flattens Secrets into a role → secret_name map. ONE consumer
// left, and it wants exactly this shape: the integration.delete audit payload,
// recording which credential REFS a deleted row named (the operator's secrets
// themselves are not deleted). Never a read path — anything that ACTS on a
// secret reads the Secrets rows, because only they carry the delivery that
// says how it reaches a run.
func (in Integration) CredentialsMap() map[string]string {
	if len(in.Secrets) == 0 {
		return nil
	}
	m := make(map[string]string, len(in.Secrets))
	for _, s := range in.Secrets {
		m[s.Role] = s.SecretName
	}
	return m
}

// RoleSecret returns the secret NAME stored for role ("" when the role is not
// configured).
func (in Integration) RoleSecret(role string) string {
	for _, s := range in.Secrets {
		if s.Role == role {
			return s.SecretName
		}
	}
	return ""
}

// HeaderSecret returns the first proxy_header-delivered secret row — the
// generic proxy-injected credential lane — as the (secretName, header, format)
// triple injection consumes. An empty stored format reads as "%s" (the raw
// secret IS the header value; injectionRuleFromScope would otherwise default
// "" to "Bearer %s", which is wrong for every custom credential header).
// ok=false when no secret is proxy_header-delivered.
func (in Integration) HeaderSecret() (secretName, header, format string, ok bool) {
	for _, s := range in.Secrets {
		if s.Delivery == nil || s.Delivery.Mode != DeliveryProxyHeader {
			continue
		}
		format = s.Delivery.Format
		if format == "" {
			format = "%s"
		}
		return s.SecretName, s.Delivery.Header, format, true
	}
	return "", "", "", false
}

// AIKeyDelivery is the proxy-header injection convention an AI api-key kind's
// "api_key" secret rides — the same convention the harness catalog's Gateway
// rows encode (internal/api/harness.go), recorded here so a folded/derived row
// states the delivery that actually happens. nil for a kind whose api_key has
// no proxy-header lane.
func AIKeyDelivery(kind string) *IntegrationDelivery {
	switch kind {
	case IntegrationKindAnthropicAPIKey:
		return &IntegrationDelivery{Mode: DeliveryProxyHeader, Header: "x-api-key", Format: "%s"}
	case IntegrationKindOpenAIAPIKey:
		return &IntegrationDelivery{Mode: DeliveryProxyHeader, Header: "Authorization", Format: "Bearer %s"}
	}
	return nil
}

// legacyIntegrationJSON is the pre-base-component wire/storage shape, decoded
// only by the read-time fold below. The field set is frozen — this is a
// decode-compat shadow, never written.
type legacyIntegrationJSON struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Category             string            `json:"category"`
	Type                 string            `json:"type"`
	Disabled             bool              `json:"disabled"`
	Hosts                []string          `json:"hosts"`
	Header               string            `json:"header"`
	Format               string            `json:"format"`
	Docs                 string            `json:"docs"`
	Credentials          map[string]string `json:"credentials"`
	Config               map[string]any    `json:"config"`
	DisabledCapabilities []string          `json:"disabled_capabilities"`
	DefaultFor           []string          `json:"default_for"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// integrationJSON is Integration minus its methods, so UnmarshalJSON can
// decode the new shape without recursing.
type integrationJSON Integration

// UnmarshalJSON accepts BOTH the base-component shape (discriminated by its
// "kind" key) and the legacy {category, type, hosts, header, credentials}
// shape, folding the latter forward (foldLegacyIntegration). This is the ONE
// read-time migration chokepoint: every decode path — the store's SiteConfig
// document, a saved wire body — folds here, and marshal emits only the new
// shape (one-way, write-new). Precedent: foldLegacyArtifactOverrides
// (internal/api/site_config.go).
func (in *Integration) UnmarshalJSON(b []byte) error {
	var probe struct {
		Kind     *string `json:"kind"`
		Category *string `json:"category"`
		Type     *string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	if probe.Kind == nil && (probe.Category != nil || probe.Type != nil) {
		var legacy legacyIntegrationJSON
		if err := json.Unmarshal(b, &legacy); err != nil {
			return err
		}
		*in = foldLegacyIntegration(legacy)
		return nil
	}
	var row integrationJSON
	if err := json.Unmarshal(b, &row); err != nil {
		return err
	}
	*in = Integration(row)
	return nil
}

// foldLegacyIntegration maps one legacy-shaped row onto the base-component
// shape: kind = the old Type (which for a generic category WAS the open slug;
// falls back to Category for a degenerate type-less row), egress = Hosts,
// config = Config (bedrock's old "lane" key renamed to its contract name
// "auth_lane"), and secrets = the Credentials map with each role's delivery
// derived from what actually delivered it — the row's own Header/Format for
// the generic token lane, the provider convention for an AI api_key, and nil
// (the kind's bespoke brokered lane) for everything else. A legacy
// artifact_mirror/host_proxy row is marked legacyTopology so IntegrationList
// drops it from this surface (its config lives under Corporate network).
func foldLegacyIntegration(l legacyIntegrationJSON) Integration {
	kind := l.Type
	if kind == "" {
		kind = l.Category
	}
	cfg := l.Config
	if kind == IntegrationKindBedrock {
		if lane, ok := cfg["lane"]; ok {
			cfg = cloneAnyMap(cfg)
			delete(cfg, "lane")
			cfg["auth_lane"] = lane
		}
	}
	var secrets []IntegrationSecret
	for _, role := range sortedKeys(l.Credentials) {
		name := l.Credentials[role]
		var d *IntegrationDelivery
		switch {
		case role == IntegrationCredentialToken && l.Header != "":
			format := l.Format
			if format == "" {
				format = "%s"
			}
			d = &IntegrationDelivery{Mode: DeliveryProxyHeader, Header: l.Header, Format: format}
		case role == "api_key":
			d = AIKeyDelivery(kind)
		}
		secrets = append(secrets, IntegrationSecret{Role: role, SecretName: name, Delivery: d})
	}
	return Integration{
		ID: l.ID, Name: l.Name, Kind: kind, Disabled: l.Disabled,
		Secrets: secrets, Egress: l.Hosts, Config: cfg, Docs: l.Docs,
		DisabledCapabilities: l.DisabledCapabilities, DefaultFor: l.DefaultFor,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		legacyTopology: l.Category == "artifact_mirror" || l.Category == "host_proxy",
	}
}

// cloneAnyMap shallow-copies m so the fold never mutates a caller's map.
func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedKeys returns m's keys sorted, so the fold's secrets order is
// deterministic regardless of map iteration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// IntegrationList is SiteConfig's integrations slice with the read-time
// migration applied at decode: each row folds individually (see
// Integration.UnmarshalJSON), and rows folded from the legacy
// artifact_mirror/host_proxy categories are DROPPED — they are network
// topology, homed under Corporate network, and this surface stops carrying
// them. A NEW-shape row is never dropped, whatever its kind slug.
type IntegrationList []Integration

// UnmarshalJSON decodes the rows then drops legacy topology rows.
func (l *IntegrationList) UnmarshalJSON(b []byte) error {
	var rows []Integration
	if err := json.Unmarshal(b, &rows); err != nil {
		return err
	}
	kept := rows[:0]
	for _, r := range rows {
		if r.legacyTopology {
			continue
		}
		kept = append(kept, r)
	}
	*l = IntegrationList(kept)
	return nil
}
