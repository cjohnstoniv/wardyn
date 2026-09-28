// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
)

// workspace.go carries the WORKSPACE COMPOSITION model and the INTEGRATION
// entity. A workspace is one-or-more sources plus a base image plus a
// requirements contract; an integration is a named connection to a system
// outside Wardyn. Both are refs/NAMES only, never secret values.

// WorkspaceKind discriminates an onboarded Workspace's (legacy, single-source)
// shape; it is a DERIVED READ-ONLY value (see Workspace.Kind) mirroring
// Sources[0] — a multi-source Workspace has no single Kind.
type WorkspaceKind string

const (
	WorkspaceKindLocalDir WorkspaceKind = "local_dir"
	WorkspaceKindRepo     WorkspaceKind = "repo"
	// WorkspaceKindContainer was the pre-composition-model shape (an onboarded
	// base IMAGE with no mount); never derived or written for a new workspace,
	// and migration 0029 rewrites every stored row into the composition shape.
	//
	// Deprecated: kept for one release as a readable Go anchor for 0029's
	// backfill SQL and any pre-0029 payload that still decodes "container".
	WorkspaceKindContainer WorkspaceKind = "container"
)

// WorkspaceLLMCred is the OPERATOR-owned model/harness credential BINDING on a
// workspace: a run that picks this workspace inherits model access via the
// named Integration. Refs/NAMES only, never secret values. Nil /
// IntegrationRef="" => no binding; the run uses the global provider config.
type WorkspaceLLMCred struct {
	// IntegrationRef names the Integration this workspace's model/harness access resolves through.
	IntegrationRef string `json:"integration_ref,omitempty"`
	// ProviderRef pins a run to one model provider (by id); replaces
	// IntegrationRef once AI-kind integrations retire. A disallowed/ineligible
	// pin refuses the run rather than falling through to another provider.
	ProviderRef string `json:"provider_ref,omitempty"`
}

// UnmarshalJSON decodes WorkspaceLLMCred, tolerating the pre-Integration wire
// shape ({"mode":...,"api_key_secret":...,"bedrock":{...}}); those fields have
// no home on this type, so they are silently dropped, yielding an EMPTY
// IntegrationRef rather than a decode error. No reverse mapping is attempted.
func (c *WorkspaceLLMCred) UnmarshalJSON(b []byte) error {
	var wire struct {
		IntegrationRef string `json:"integration_ref"`
		ProviderRef    string `json:"provider_ref"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	c.IntegrationRef, c.ProviderRef = wire.IntegrationRef, wire.ProviderRef
	return nil
}

// WorkspaceBedrockRef is a workspace's Bedrock model selection (non-secret; AWS credentials are unchanged).
type WorkspaceBedrockRef struct {
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
}

// WorkspaceStatus is the onboarding/scan lifecycle of a Workspace: not-yet-
// scanned, mid-scan, scanned (ready to use), or errored. The later
// import-pipeline stages are RETIRED (migration 0029 collapsed every stored
// row back to scanned/pending_scan).
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
	// WorkspaceSourceTypeEphemeral is a scratch dir for the sandbox's lifetime
	// only — the composition floor ensuring "one-or-more sources" never means zero.
	WorkspaceSourceTypeEphemeral WorkspaceSourceType = "ephemeral"
)

// WorkspaceSource is one entry in a Workspace's composition (multiples of the
// same Type are allowed). Which fields are meaningful depends on Type:
//
//	local_dir  Path (host path), Target, Writable
//	repo       Source (slug/URL), Ref, Target
//	ephemeral  Target only — no Path, no Source, never Writable
type WorkspaceSource struct {
	Type WorkspaceSourceType `json:"type"`
	// Path is the host directory path (local_dir only).
	Path string `json:"path,omitempty"`
	// Source is the repo slug/URL (repo only).
	Source string `json:"source,omitempty"`
	// Ref is an optional git ref (branch/tag/sha, repo only).
	Ref string `json:"ref,omitempty"`
	// Target is the mount/clone/scratch-dir path inside the sandbox.
	Target string `json:"target,omitempty"`
	// Writable opts a local_dir source into read-write (default false =
	// read-only; a plain bool since false is already the safe default).
	Writable bool `json:"writable,omitempty"`
	// Overrides is this attachment's stance on requirement keys its source
	// declares: reqKey -> off|optional|required (workspace_contract.go's
	// AttachmentOverride* constants). nil means "say nothing" — the server
	// carries forward the prior override; an explicit map REPLACES it outright.
	Overrides map[string]string `json:"overrides,omitempty"`
	// Admitted is a SERVER-OWNED READ PROJECTION, never persisted or taken from
	// a write body: does the workspace-provider policy admit this repo source's
	// clone URL today? A POINTER so "no answer" stays distinguishable from
	// "not admitted"; ABSENT where no provider row exists.
	Admitted *bool `json:"admitted,omitempty"`
}

// WorkspaceBaseImage is a Workspace's base-image choice: what the sandbox's
// container image is built FROM, independent of its Sources.
type WorkspaceBaseImage struct {
	// Kind is "recommended" (Wardyn's convention image for the detected stack),
	// "registry" (operator-picked, named by Image), "custom" (Image is a base;
	// see Steps), or "byo" (Image used verbatim).
	Kind string `json:"kind"`
	// Image is the base image reference. Meaning depends on Kind: the FROM for
	// "custom", the image itself for "registry"/"byo", unused for "recommended".
	Image string `json:"image,omitempty"`
	// Steps is CATALOG IDENTITY ONLY — never applied to an image (a deliberate
	// refusal: running operator-authored RUN lines would be host-side RCE, the
	// same class assertWrapSafeBase refuses for ONBUILD), so "custom" builds
	// identically to "byo". The field must stay: it's part of the base_images
	// UNIQUE (kind, image) index, and removing it would mint duplicate rows.
	Steps []string `json:"steps,omitempty"`
}

// WorkspaceRequirement is one entry in a Workspace's requirements contract,
// keyed by the grammar documented on Workspace.Requirements.
type WorkspaceRequirement struct {
	// Level is "required" (a run cannot proceed without it) or "optional"
	// (a run may proceed without it, degraded).
	Level string `json:"level"`
	// Provenance is "scan_seeded" (the scanner detected the need) or
	// "operator_set" (an operator declared it directly).
	Provenance string `json:"provenance"`
}

// Workspace is an onboarded, admin-reviewed COMPOSITION of one-or-more
// sources (local_dir | repo | ephemeral — the ephemeral floor means Sources
// is never empty), plus a base image choice and a requirements contract.
// Import scans/reviews the composition ONCE and persists a profile (opaque
// internal/workspacescan WorkspaceProfile); runs thereafter reference the
// workspace by id. Profile/BuiltProfileHash are empty until Status transitions
// out of pending_scan.
type Workspace struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Sources is the workspace's composition: one-or-more local_dir/repo/ephemeral sources.
	Sources []WorkspaceSource `json:"sources"`
	// BaseImage is the workspace's base-image choice; nil means the platform
	// default convention image for the detected/scanned stack.
	BaseImage *WorkspaceBaseImage `json:"base_image,omitempty"`
	// Requirements is the workspace's declared requirements contract, keyed by
	// a fixed grammar of "<type>:<key>":
	//
	//	secret:NAME       a named secret must be resolvable, e.g. "secret:acme-anthropic-key"
	//	egress:host       the host must be reachable (bare host or "*."-wildcard), e.g. "egress:api.github.com"
	//	write:/host/path  the HOST path must be mounted writable, e.g. "write:/home/user/repo"
	//	integration:ID    the named Integration must be granted, e.g. "integration:git_host:github.com"
	//
	// Split on the FIRST colon only, since a write:/integration: key may itself
	// contain colons. This map is the workspace's OVERLAY on its attached
	// sources' own contracts; EffectiveRequirements is the folded result a run consumes.
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"`

	// Attachments is the tier-3 composition: ordered source references and
	// inline ephemeral rows, each with per-attachment target/writable and
	// requirement OVERRIDES. Empty on a pre-split row, which falls back to the
	// embedded Sources column via the store's hydrate pass.
	Attachments []WorkspaceAttachment `json:"attachments,omitempty"`
	// BaseImageID references the shared base-image catalog (tier 2); nil means
	// "recommended" (the per-workspace derived build) by design, not absence.
	BaseImageID *uuid.UUID `json:"base_image_id,omitempty"`
	// EffectiveRequirements is DERIVED, read-only, never persisted: the
	// FoldWorkspaceContract result over attachments + source contracts + the
	// overlay above, populated by the store's hydrate pass (equals Requirements
	// verbatim on a pre-split row).
	EffectiveRequirements map[string]WorkspaceRequirement `json:"effective_requirements,omitempty"`

	// --- Everything below through DefaultTarget is a DERIVED READ-ONLY MIRROR
	// of Sources[0] (never persisted, only when len(Sources)==1); a
	// multi-source workspace leaves these zero — read Sources instead. ---

	// Kind mirrors Sources[0].Type (single-source only).
	Kind WorkspaceKind `json:"kind"`
	// Source mirrors Sources[0]'s host path (local_dir) or repo slug/URL (repo, single-source only).
	Source string `json:"source"`
	// Ref mirrors Sources[0].Ref (single-source repo only).
	Ref string `json:"ref,omitempty"`
	// DefaultTarget mirrors Sources[0].Target (single-source only).
	DefaultTarget string `json:"default_target,omitempty"`

	// Profile is internal/workspacescan's WorkspaceProfile, opaque here; nil/empty until scanned.
	Profile json.RawMessage `json:"profile,omitempty"`
	// ImageRef is the resolved/generated image for this workspace's profile; empty until scanned/built.
	ImageRef string `json:"image_ref,omitempty"`
	// BuiltProfileHash is the profile hash ImageRef was built from — the
	// build-once/reuse-many cache key (rebuild only when it changes).
	BuiltProfileHash string `json:"built_profile_hash,omitempty"`
	// ApprovedEgress is the OPERATOR-owned list of egress hosts explicitly
	// promoted for this workspace, unioned into a run's allowlist. Never
	// written by a scan; cleared when the composition changes.
	ApprovedEgress []string `json:"approved_egress,omitempty"`
	// DeniedEgress is the OPERATOR-owned mirror of ApprovedEgress: hosts
	// explicitly blocked, folded into a run's denied_domains at create time
	// where deny beats allow, allow_all_egress, and a runtime approval alike.
	// UNLIKE ApprovedEgress, this is NOT cleared when sources change (migration
	// 0040): a deny needs no re-review against new content.
	DeniedEgress []string `json:"denied_egress,omitempty"`
	// EgressEditedAt is when an operator last REPLACED one of the two lists
	// above through the approved-egress/denied-egress PUTs. It exists for one
	// reader, the boot heal (ReconcileWorkspaceEgressDecisions): without this
	// mark, a decided `always` approval would re-widen a host the operator had
	// just removed at every restart. Stamped ONLY by those two scoped setters,
	// never by a decision or a composition edit.
	EgressEditedAt *time.Time `json:"egress_edited_at,omitempty"`
	// ActiveRunID is the in-flight scan/record run for this workspace, so the
	// import panel can poll "is my step still running" without scanning all runs.
	ActiveRunID *uuid.UUID `json:"active_run_id,omitempty"`
	// RecordResults is the per-task Record Mode state (taskKey → result: run
	// pointer, mode, status, observations). Opaque map[string]api-owned JSON;
	// written only via the scoped SetWorkspaceRecordResult; cleared when the
	// composition changes.
	RecordResults json.RawMessage `json:"record_results,omitempty"`
	// LLMCred is the OPERATOR-owned model/harness credential binding: a run that
	// picks this workspace inherits it (refs/names only). Nil => no binding.
	// Written only via the scoped SetWorkspaceLLMCred, mirroring ApprovedEgress.
	LLMCred *WorkspaceLLMCred `json:"llm_cred,omitempty"`
	// OwnedBy is the MEMBER who created this workspace (lowercased OIDC sub or
	// email). EMPTY means OPERATOR-OWNED — every admin-created or pre-0048
	// workspace, member-readable and admin-writable as before. A non-empty
	// value is the ownership relation ownsWorkspaceOrAdmin keys on: only that
	// member or an admin may read/write it, and a foreign member gets the same
	// 404 a missing workspace gets. Set ONLY by handleCreateWorkspace; never
	// accepted from a request body or rewritten by an edit.
	OwnedBy   string          `json:"owned_by,omitempty"`
	Status    WorkspaceStatus `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Integration kinds. An integration is a BASE COMPONENT extended by kind: a
// connection (secrets + egress), never an installer. The closed set below is
// every kind with bespoke behavior; any other kind is a GENERIC open slug
// whose row carries its own secrets/egress/config. UI grouping DERIVES from
// kind; there is no stored category.
const (
	IntegrationKindAnthropicAPIKey       = "anthropic_api_key"
	IntegrationKindAnthropicSubscription = "anthropic_subscription"
	IntegrationKindBedrock               = "bedrock"
	IntegrationKindOpenAIAPIKey          = "openai_api_key"
	// IntegrationKindAzureOpenAI is GONE as of 0.5 (its one caller, the AI Run
	// Composer, no longer exists). A stored row of this kind now fails closed
	// at validateIntegrationWrite like any other unknown kind.
	IntegrationKindGitHubApp = "github_app"
	IntegrationKindGitHost   = "git_host"
)

// ClosedIntegrationKinds is the closed kind set — the kinds with bespoke
// behavior in code, and as of 0.5 the ONLY kinds a write may name
// (validateIntegrationWrite).
var ClosedIntegrationKinds = map[string]bool{
	IntegrationKindAnthropicAPIKey: true, IntegrationKindAnthropicSubscription: true,
	IntegrationKindBedrock: true, IntegrationKindOpenAIAPIKey: true,
	IntegrationKindGitHubApp: true, IntegrationKindGitHost: true,
}

// ClosedIntegrationKindList is ClosedIntegrationKinds in a stable, sorted order
// for the "want one of: …" half of a rejected write's error.
func ClosedIntegrationKindList() []string {
	return slices.Sorted(maps.Keys(ClosedIntegrationKinds))
}

// AIProviderKind reports whether kind is one of the AI provider flavors —
// eligible for DefaultFor and the run-time model-credential fold.
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
	// DeliveryProxyHeader: the proxy presents the secret in an HTTP header;
	// the sandbox never holds it. The default, never-resident lane.
	DeliveryProxyHeader = "proxy_header"
	// DeliveryResidentFile: materialized as a file inside the sandbox — a disclosed exception.
	DeliveryResidentFile = "resident_file"
	// DeliveryResidentEnv: exported as an environment variable inside the sandbox — a disclosed exception.
	DeliveryResidentEnv = "resident_env"
)

// IntegrationDelivery says how ONE secret reaches the run; exactly one mode
// decides which of the other fields apply.
type IntegrationDelivery struct {
	Mode string `json:"mode"` // proxy_header | resident_file | resident_env
	// Header/Format apply to proxy_header: the HTTP field name and the value
	// template (one %s; empty means the raw secret IS the value, i.e. "%s").
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
	// Path applies to resident_file: the in-sandbox path the secret lands at.
	Path string `json:"path,omitempty"`
	// Var applies to resident_env: the environment variable name.
	Var string `json:"var,omitempty"`
}

// IntegrationSecret is one required secret of an integration: a role, a ref
// into the secret store (a NAME, never a value), and how it is delivered. A
// nil Delivery is allowed on CLOSED kinds only (the kind's own bespoke transport delivers it).
type IntegrationSecret struct {
	Role       string               `json:"role"`
	SecretName string               `json:"secret_name"`
	Delivery   *IntegrationDelivery `json:"delivery,omitempty"`
}

// The verification-probe framework (test-this-connection) was removed in 0.5:
// the Settings cards now only claim what is STORED, never claim to have
// dialed the provider.

// Integration is one operator-configured external connection: a base
// component — required secrets (each with its delivery), required egress,
// non-secret config — extended by kind. Secrets holds NAMES, never VALUES;
// the broker/proxy resolve the named secret at dispatch time.
//
// STORAGE COMPATIBILITY: UnmarshalJSON also accepts the pre-base-component
// shape, folding it forward one-way (write-new). See foldLegacyIntegration.
type Integration struct {
	// ID is an operator-chosen stable slug, unique within SiteConfig.Integrations
	// (no generated uuid — WorkspaceLLMCred.IntegrationRef/DefaultFor point at this ID).
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is the ONE field that says what this connects to: a closed-set kind
	// (ClosedIntegrationKinds) with bespoke behavior, or any other slug — a generic connection.
	Kind string `json:"kind"`
	// Disabled turns the integration off without deleting its configuration.
	Disabled bool `json:"disabled,omitempty"`
	// Secrets are the required secrets, each with its delivery. Refs/names only.
	Secrets []IntegrationSecret `json:"secrets,omitempty"`
	// Egress is WHERE THE SYSTEM LIVES: the host entries a run granted this
	// integration may reach (proxy.ValidDomainEntry syntax: exact host or
	// leading-"*." wildcard, optionally ":port").
	//
	// A WILDCARD OPENS THE PATH BUT NEVER CARRIES THE CREDENTIAL: proxy-side
	// injection requires an EXACT entry, so a proxy_header-delivered secret is
	// rejected at write time if any egress entry is a wildcard or has a port.
	Egress []string `json:"egress,omitempty"`
	// Config is non-secret, kind-validated configuration; closed kinds accept
	// only their known keys (an unknown key 400s by name at write), generic
	// kinds take any string keys.
	Config map[string]any `json:"config,omitempty"`
	// Docs optionally points at whatever documents this system.
	Docs string `json:"docs,omitempty"`
	// DisabledCapabilities lists capability names this integration does NOT support.
	DisabledCapabilities []string `json:"disabled_capabilities,omitempty"`
	// DefaultFor lists what this integration is the operator-chosen default for:
	// "agent_runs" and/or "wardyn_features" (Wardyn's own internal usage).
	DefaultFor []string  `json:"default_for,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// legacyTopology marks a row folded from a legacy artifact_mirror/host_proxy
	// category (network topology, not an integration); IntegrationList's decode drops such rows.
	legacyTopology bool
}

// IntegrationCredentialToken is the secret ROLE a generic header-delivered
// credential uses. Closed kinds keep their own role names instead ("api_key", "pat", "ssh_key").
const IntegrationCredentialToken = "token"

// CredentialsMap flattens Secrets into a role → secret_name map for the ONE
// consumer that wants this shape: the integration.delete audit payload.
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

// RoleSecret returns the secret NAME stored for role ("" when not configured).
func (in Integration) RoleSecret(role string) string {
	for _, s := range in.Secrets {
		if s.Role == role {
			return s.SecretName
		}
	}
	return ""
}

// HeaderSecret returns the first proxy_header-delivered secret row as the
// (secretName, header, format) triple injection consumes. An empty stored
// format reads as "%s", not injectionRuleFromScope's "Bearer %s" default.
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
// "api_key" secret rides (mirrors the harness catalog's Gateway rows,
// internal/api/harness.go). nil for a kind with no proxy-header lane.
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
// only by the read-time fold below; the field set is frozen, never written.
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
// shape, folding the latter forward (foldLegacyIntegration) — the ONE
// read-time migration chokepoint; marshal emits only the new shape.
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
// shape: kind = old Type (falls back to Category), egress = Hosts, config =
// Config (bedrock's "lane" renamed to "auth_lane"), secrets = Credentials with
// each role's delivery derived from what actually delivered it. A legacy
// artifact_mirror/host_proxy row is marked legacyTopology so IntegrationList
// drops it.
func foldLegacyIntegration(l legacyIntegrationJSON) Integration {
	kind := l.Type
	if kind == "" {
		kind = l.Category
	}
	cfg := l.Config
	if kind == IntegrationKindBedrock {
		if lane, ok := cfg["lane"]; ok {
			// Shallow, so the fold never mutates the caller's map.
			cfg = maps.Clone(cfg)
			delete(cfg, "lane")
			cfg["auth_lane"] = lane
		}
	}
	var secrets []IntegrationSecret
	for _, role := range slices.Sorted(maps.Keys(l.Credentials)) { // deterministic secrets order
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

// IntegrationList is SiteConfig's integrations slice with the read-time
// migration applied at decode: rows folded from legacy artifact_mirror/
// host_proxy categories are DROPPED (network topology, homed under Corporate
// network). A NEW-shape row is never dropped, whatever its kind slug.
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
