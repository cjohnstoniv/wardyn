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

// workspace.go carries the WORKSPACE COMPOSITION model (sources + base image
// + requirements contract) and the INTEGRATION entity (a named connection to
// an outside system). Both hold refs/names only, never secret values.

// WorkspaceKind discriminates an onboarded Workspace's legacy, single-source
// shape; a DERIVED READ-ONLY value mirroring Sources[0] (see Workspace.Kind)
// — a multi-source Workspace has no single Kind.
type WorkspaceKind string

const (
	WorkspaceKindLocalDir WorkspaceKind = "local_dir"
	WorkspaceKindRepo     WorkspaceKind = "repo"
	// WorkspaceKindContainer was the pre-composition-model shape (base image, no
	// mount); migration 0029 rewrites every stored row into the composition
	// shape. Deprecated: kept as an anchor for 0029's backfill SQL and any
	// pre-0029 payload that still decodes "container".
	WorkspaceKindContainer WorkspaceKind = "container"
)

// WorkspaceLLMCred is the operator-owned model binding on a workspace: a run
// that picks it uses the pinned model provider unless it names another. Nil or
// ProviderRef="" means no pin.
type WorkspaceLLMCred struct {
	// ProviderRef pins a run to one model provider (by id). A disallowed or
	// ineligible pin refuses the run rather than falling through to another
	// provider.
	ProviderRef string `json:"provider_ref,omitempty"`
	// ProviderUnavailable is set by the server on a READ only, in place of
	// ProviderRef, for a caller "Available to" does not admit to the pinned
	// provider: the workspace is pinned, to a provider they cannot use, and
	// its id is not theirs to learn (#1018). A write carrying it is refused.
	ProviderUnavailable bool `json:"provider_unavailable,omitempty"`
}

// WorkspaceStatus is the onboarding/scan lifecycle of a Workspace: not-yet-
// scanned, mid-scan, scanned, or errored. Later import-pipeline stages are
// RETIRED (migration 0029 collapsed every row back to scanned/pending_scan).
type WorkspaceStatus string

const (
	WorkspacePendingScan WorkspaceStatus = "pending_scan" // onboarded, not yet scanned
	WorkspaceScanning    WorkspaceStatus = "scanning"     // scan run in flight
	WorkspaceScanned     WorkspaceStatus = "scanned"      // profile derived, ready to use
	WorkspaceError       WorkspaceStatus = "error"        // last scan attempt failed
)

// WorkspaceSourceType discriminates one entry in a Workspace's composition.
type WorkspaceSourceType string

const (
	WorkspaceSourceTypeLocalDir WorkspaceSourceType = "local_dir" // binds a host directory
	WorkspaceSourceTypeRepo     WorkspaceSourceType = "repo"      // clones a git repo
	// WorkspaceSourceTypeEphemeral is a sandbox-lifetime scratch dir, the
	// composition floor ensuring "one-or-more sources" never means zero.
	WorkspaceSourceTypeEphemeral WorkspaceSourceType = "ephemeral"
)

// WorkspaceSource is one entry in a Workspace's composition (multiples of the
// same Type allowed). Meaningful fields depend on Type:
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
	// AttachmentOverride* constants). nil means "say nothing" (server carries
	// forward the prior override); an explicit map REPLACES it outright.
	Overrides map[string]string `json:"overrides,omitempty"`
	// Admitted is a server-owned read projection, never persisted or from a
	// write body: does policy admit this clone URL today? A pointer so
	// "no answer" (absent, no provider row) differs from "not admitted".
	Admitted *bool `json:"admitted,omitempty"`
}

// WorkspaceBaseImage is a Workspace's base-image choice: what the sandbox's
// container image is built FROM, independent of its Sources.
type WorkspaceBaseImage struct {
	// Kind is "recommended" (convention image for the detected stack),
	// "registry" (operator-picked, named by Image), "custom" (Image is a base,
	// see Steps), or "byo" (Image used verbatim).
	Kind string `json:"kind"`
	// Image is the base image reference. Meaning depends on Kind: the FROM for
	// "custom", the image itself for "registry"/"byo", unused for "recommended".
	Image string `json:"image,omitempty"`
	// Steps is CATALOG IDENTITY ONLY, never applied to an image. SECURITY:
	// running operator-authored RUN lines would be host-side RCE
	// (assertWrapSafeBase also refuses ONBUILD), so "custom" builds
	// identically to "byo". Must stay: part of base_images' UNIQUE(kind, image)
	// index; removing it mints duplicate rows.
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

// Workspace is an onboarded, admin-reviewed composition of one-or-more
// sources (local_dir | repo | ephemeral — the ephemeral floor means Sources
// is never empty), a base image choice, and a requirements contract. Import
// scans the composition once into an opaque profile (internal/workspacescan
// WorkspaceProfile); runs thereafter reference the workspace by id.
// Profile/BuiltProfileHash are empty until Status leaves pending_scan.
type Workspace struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Sources is the composition: one-or-more local_dir/repo/ephemeral entries.
	Sources []WorkspaceSource `json:"sources"`
	// BaseImage is the base-image choice; nil means the platform default
	// convention image for the detected/scanned stack.
	BaseImage *WorkspaceBaseImage `json:"base_image,omitempty"`
	// Requirements is the declared requirements contract, keyed by a fixed
	// grammar of "<type>:<key>":
	//
	//	secret:NAME       a named secret must be resolvable, e.g. "secret:acme-anthropic-key"
	//	egress:host       the host must be reachable (bare host or "*."-wildcard), e.g. "egress:api.github.com"
	//	write:/host/path  the HOST path must be mounted writable, e.g. "write:/home/user/repo"
	//	integration:ID    the named Integration must be granted, e.g. "integration:git_host:github.com"
	//
	// Split on the FIRST colon only, since a write:/integration: key may itself
	// contain colons. An overlay on attached sources' own contracts;
	// EffectiveRequirements is the folded result a run consumes.
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"`

	// Attachments is the tier-3 composition: ordered source refs and inline
	// ephemeral rows with per-attachment target/writable/overrides. Empty on
	// a pre-split row, falling back to Sources.
	Attachments []WorkspaceAttachment `json:"attachments,omitempty"`
	// BaseImageID references the shared base-image catalog (tier 2); nil means
	// "recommended" (per-workspace derived build) by design, not absence.
	BaseImageID *uuid.UUID `json:"base_image_id,omitempty"`
	// EffectiveRequirements is derived, read-only, never persisted: the
	// FoldWorkspaceContract result over attachments + source contracts + the
	// overlay above (equals Requirements on a pre-split row).
	EffectiveRequirements map[string]WorkspaceRequirement `json:"effective_requirements,omitempty"`

	// Below through DefaultTarget: a derived, read-only mirror of Sources[0]
	// (only when len(Sources)==1); a multi-source workspace leaves these zero.

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
	// BuiltProfileHash is the profile hash ImageRef was built from, the
	// build-once/reuse-many cache key (rebuild only when it changes).
	BuiltProfileHash string `json:"built_profile_hash,omitempty"`
	// ApprovedEgress is the operator-owned list of egress hosts explicitly
	// promoted, unioned into a run's allowlist. Never written by a scan;
	// cleared when the composition changes.
	ApprovedEgress []string `json:"approved_egress,omitempty"`
	// DeniedEgress mirrors ApprovedEgress: hosts explicitly blocked, folded
	// into denied_domains at create (deny beats allow/allow_all_egress/a
	// runtime approval alike). Unlike ApprovedEgress, not cleared on source
	// change (migration 0040) — a deny needs no re-review.
	DeniedEgress []string `json:"denied_egress,omitempty"`
	// EgressEditedAt is when an operator last REPLACED one of the two lists
	// above — read by the boot heal, which without it would let a decided
	// `always` approval re-widen a just-removed host every restart. Stamped
	// only by those two scoped setters.
	EgressEditedAt *time.Time `json:"egress_edited_at,omitempty"`
	// ActiveRunID is the in-flight scan/record run, so the import panel can
	// poll "is my step still running" without scanning all runs.
	ActiveRunID *uuid.UUID `json:"active_run_id,omitempty"`
	// RecordResults is the per-task Record Mode state; opaque
	// map[string]api-owned JSON. Written only via SetWorkspaceRecordResult;
	// cleared on composition change.
	RecordResults json.RawMessage `json:"record_results,omitempty"`
	// LLMCred is the model/harness credential binding a run inherits (see
	// WorkspaceLLMCred). Written only via the scoped SetWorkspaceLLMCred.
	LLMCred *WorkspaceLLMCred `json:"llm_cred,omitempty"`
	// OwnedBy is the member who created this workspace (lowercased OIDC sub or
	// email); empty means operator-owned. TRUST BOUNDARY: ownsWorkspaceOrAdmin
	// keys on non-empty — only that member or an admin may read/write it, else
	// the same 404 as a missing workspace. Set only by handleCreateWorkspace.
	OwnedBy   string          `json:"owned_by,omitempty"`
	Status    WorkspaceStatus `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	// AvailableToYou (#1267): per-caller bit — would the same decide path a
	// launch runs admit THIS workspace for this reader? Computed fresh per GET
	// (workspaceAvailableToCaller), never stored or accepted on write; always
	// true for an operator. Pointer with omitempty: only the two GET stampers
	// set it, so a plain bool would ship a false "unavailable" elsewhere — the
	// console's "absent means fall back" contract needs the omitted key.
	AvailableToYou *bool `json:"available_to_you,omitempty"`
}

// Integration kinds. An integration is a BASE COMPONENT extended by kind: a
// connection (secrets + egress), never an installer. The closed set below is
// every kind with bespoke behavior; any other kind is a GENERIC open slug
// carrying its own secrets/egress/config. UI grouping DERIVES from kind;
// there is no stored category.
const (
	// The four AI kinds (anthropic_api_key, anthropic_subscription, bedrock,
	// openai_api_key) are GONE as of 0.8: model access is a model provider, and
	// migration 0100 converted and deleted every stored row of them.
	IntegrationKindGitHubApp = "github_app"
	IntegrationKindGitHost   = "git_host"
)

// ClosedIntegrationKinds is the closed kind set — the kinds with bespoke
// behavior in code, and as of 0.5 the ONLY kinds a write may name
// (validateIntegrationWrite).
var ClosedIntegrationKinds = map[string]bool{
	IntegrationKindGitHubApp: true, IntegrationKindGitHost: true,
}

// ClosedIntegrationKindList is ClosedIntegrationKinds sorted, for the
// "want one of: …" half of a rejected write's error.
func ClosedIntegrationKindList() []string {
	return slices.Sorted(maps.Keys(ClosedIntegrationKinds))
}

// Integration delivery modes: how one secret reaches the run.
const (
	// DeliveryProxyHeader: the proxy presents the secret in an HTTP header;
	// the sandbox never holds it. The default, never-resident lane.
	DeliveryProxyHeader = "proxy_header"
	// DeliveryResidentFile/DeliveryResidentEnv: materialized as a file, or
	// exported as an env var, inside the sandbox — disclosed exceptions.
	DeliveryResidentFile = "resident_file"
	DeliveryResidentEnv  = "resident_env"
)

// IntegrationDelivery says how ONE secret reaches the run; the mode decides
// which other fields apply.
type IntegrationDelivery struct {
	Mode string `json:"mode"` // proxy_header | resident_file | resident_env
	// Header/Format apply to proxy_header: field name and value template (one
	// %s; empty means the raw secret IS the value).
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
	// Path applies to resident_file: the in-sandbox path the secret lands at.
	Path string `json:"path,omitempty"`
	// Var applies to resident_env: the environment variable name.
	Var string `json:"var,omitempty"`
}

// IntegrationSecret is one required secret of an integration: a role, a ref
// into the secret store (a NAME, never a value), and how it's delivered. A
// nil Delivery is allowed on CLOSED kinds only (their own bespoke transport
// delivers it).
type IntegrationSecret struct {
	Role       string               `json:"role"`
	SecretName string               `json:"secret_name"`
	Delivery   *IntegrationDelivery `json:"delivery,omitempty"`
}

// The verification-probe framework (test-this-connection) was removed in 0.5:
// the Settings cards now only claim what is STORED, never claim to have
// dialed the provider.

// Integration is one operator-configured external connection: a base
// component (required secrets each with its delivery, required egress,
// non-secret config) extended by kind. Secrets holds names, never values;
// the broker/proxy resolve the named secret at dispatch time. UnmarshalJSON
// also accepts the pre-base-component shape, folding it forward one-way
// (write-new; see foldLegacyIntegration).
type Integration struct {
	// ID is an operator-chosen stable slug, unique within
	// SiteConfig.Integrations (no generated uuid — egress redirects'
	// token_integration_ref and workspace requirements point at it).
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is the ONE field saying what this connects to: a closed-set kind
	// (ClosedIntegrationKinds) with bespoke behavior, or any other slug (generic).
	Kind string `json:"kind"`
	// Disabled turns the integration off without deleting its configuration.
	Disabled bool `json:"disabled,omitempty"`
	// Secrets are the required secrets, each with its delivery; refs/names only.
	Secrets []IntegrationSecret `json:"secrets,omitempty"`
	// Egress is WHERE THE SYSTEM LIVES: hosts this integration may reach
	// (proxy.ValidDomainEntry syntax: exact host or leading-"*." wildcard,
	// optionally ":port"). TRUST BOUNDARY: a wildcard opens the path but
	// never carries the credential — proxy-side injection needs an exact
	// entry, so a proxy_header secret is rejected at write time if any
	// egress entry is a wildcard or has a port.
	Egress []string `json:"egress,omitempty"`
	// Config is non-secret, kind-validated configuration; closed kinds accept
	// only known keys (unknown 400s by name at write), generic kinds take any
	// string keys.
	Config map[string]any `json:"config,omitempty"`
	Docs   string         `json:"docs,omitempty"`
	// DisabledCapabilities lists capability names this integration does NOT support.
	DisabledCapabilities []string  `json:"disabled_capabilities,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`

	// legacyTopology marks a row folded from a legacy artifact_mirror/host_proxy
	// category (network topology, not an integration); IntegrationList drops these.
	legacyTopology bool
}

// IntegrationCredentialToken is the secret ROLE a generic header-delivered
// credential uses; closed kinds keep their own role names ("api_key", "pat", "ssh_key").
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
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// integrationJSON is Integration minus its methods, so UnmarshalJSON can
// decode without recursing.
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

// foldLegacyIntegration maps a legacy-shaped row onto the base-component
// shape: kind = old Type (or Category), egress = Hosts, config = Config,
// secrets = Credentials with each
// role's delivery derived from how it actually delivered. A legacy
// artifact_mirror/host_proxy row is marked legacyTopology so IntegrationList
// drops it.
func foldLegacyIntegration(l legacyIntegrationJSON) Integration {
	kind := l.Type
	if kind == "" {
		kind = l.Category
	}
	var secrets []IntegrationSecret
	for _, role := range slices.Sorted(maps.Keys(l.Credentials)) { // deterministic order
		name := l.Credentials[role]
		var d *IntegrationDelivery
		if role == IntegrationCredentialToken && l.Header != "" {
			format := l.Format
			if format == "" {
				format = "%s"
			}
			d = &IntegrationDelivery{Mode: DeliveryProxyHeader, Header: l.Header, Format: format}
		}
		secrets = append(secrets, IntegrationSecret{Role: role, SecretName: name, Delivery: d})
	}
	return Integration{
		ID: l.ID, Name: l.Name, Kind: kind, Disabled: l.Disabled,
		Secrets: secrets, Egress: l.Hosts, Config: l.Config, Docs: l.Docs,
		DisabledCapabilities: l.DisabledCapabilities,
		CreatedAt:            l.CreatedAt, UpdatedAt: l.UpdatedAt,
		legacyTopology: l.Category == "artifact_mirror" || l.Category == "host_proxy",
	}
}

// IntegrationList is SiteConfig's integrations slice with the read-time
// migration applied at decode: rows folded from legacy artifact_mirror/
// host_proxy categories are DROPPED (network topology, homed under
// Corporate network). A new-shape row is never dropped, whatever its kind.
type IntegrationList []Integration

// UnmarshalJSON decodes the rows, then drops legacy topology rows.
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
