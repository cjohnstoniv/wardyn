// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// WorkspaceProviders is the org's WORKSPACE-PROVIDER POLICY: which kinds of
// work a sandbox may be given, and the ceilings that work is held to.
// THE ZERO VALUE IS LEGACY OPEN MODE (load-bearing for upgraded installs) and
// is never derived from the legacy SiteConfig.ScmHosts list. URL validation
// lives in internal/api (validateWorkspaceProviders); this file is types and
// closed enums only.
type WorkspaceProviders struct {
	// Git are the git-provider rows: which hosts a run may clone from and
	// which credential lanes it may use there.
	Git []GitProvider `json:"git,omitempty"`
	// Storage is the file-system half. A nil block is legacy behaviour.
	Storage *StorageProviders `json:"storage,omitempty"`
	// GitPatBrokerEnabled is READ-ONLY and SERVER-OWNED: projected onto every
	// GET from this deployment's WARDYN_GIT_PAT_BROKER switch, never stored or
	// taken from a PUT body. A POINTER, not a bool: nil means "never
	// projected", distinct from a present false.
	GitPatBrokerEnabled *bool `json:"git_pat_broker_enabled,omitempty"`
}

// Empty reports whether p carries no policy at all — the shape a caller PUTs
// to CLEAR the block; the write boundary normalizes an empty block to nil.
func (p *WorkspaceProviders) Empty() bool {
	if p == nil {
		return true
	}
	if len(p.Git) > 0 {
		return false
	}
	return p.Storage == nil || (p.Storage.Ephemeral == nil && p.Storage.UserDrive == nil)
}

// GitProviderKind is the closed set of git forges Wardyn has bespoke
// behaviour for. A self-hosted forge is NOT a third kind: it is a github
// (GHES) or azure_devops (ADO Server) row whose base URL names its own host.
type GitProviderKind string

const (
	// GitProviderGitHub is github.com or a GitHub Enterprise Server host.
	GitProviderGitHub GitProviderKind = "github"
	// GitProviderAzureDevOps is dev.azure.com, a legacy org.visualstudio.com,
	// or an Azure DevOps Server host.
	GitProviderAzureDevOps GitProviderKind = "azure_devops"
)

// ClosedGitProviderKinds is the only kinds a write may name
// (validateWorkspaceProviders refuses anything else).
var ClosedGitProviderKinds = map[GitProviderKind]bool{
	GitProviderGitHub: true, GitProviderAzureDevOps: true,
}

// ClosedGitProviderKindList is ClosedGitProviderKinds in a stable order, for
// a rejected write's "want one of: …".
func ClosedGitProviderKindList() []string {
	kinds := slices.Sorted(maps.Keys(ClosedGitProviderKinds))
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// Valid reports whether k is one of the closed kinds.
func (k GitProviderKind) Valid() bool { return ClosedGitProviderKinds[k] }

// GitLane is one of the credential lanes a run can clone with. A provider
// row's Lanes VETO lanes; they never mint or create a credential that was
// not already stored.
type GitLane string

const (
	// GitLaneApp is the GitHub App broker lane (github.com only).
	GitLaneApp GitLane = "app"
	// GitLanePAT is the stored git-pat-<slug> lane.
	GitLanePAT GitLane = "pat"
	// GitLaneSSH is the stored ssh-key-<slug> lane.
	GitLaneSSH GitLane = "ssh"
	// GitLaneEntra is the Microsoft Entra lane: the run authorizes as the
	// PERSON against the org's own tenant. Azure DevOps HOSTED only.
	GitLaneEntra GitLane = "entra"
)

// ClosedGitLanes is the closed lane set — see ClosedGitProviderKinds.
var ClosedGitLanes = map[GitLane]bool{
	GitLaneApp: true, GitLanePAT: true, GitLaneSSH: true, GitLaneEntra: true,
}

// LegacyGitLanes are the lanes an EMPTY GitProvider.Lanes list admits — the
// three that existed when that rule was written. IT IS NOT "every lane in
// ClosedGitLanes": growing the closed set must never silently opt every
// stored row into a new lane, so this list is frozen and never grows again.
// A lane added later must be NAMED on a row to be usable.
var LegacyGitLanes = []GitLane{GitLaneApp, GitLanePAT, GitLaneSSH}

// Legacy reports whether l is one of the lanes an empty Lanes list admits.
// Every site that expands an empty list asks THIS, never ClosedGitLanes.
func (l GitLane) Legacy() bool { return slices.Contains(LegacyGitLanes, l) }

// ClosedGitLaneList is ClosedGitLanes in a stable order.
func ClosedGitLaneList() []string {
	lanes := slices.Sorted(maps.Keys(ClosedGitLanes))
	out := make([]string, len(lanes))
	for i, l := range lanes {
		out[i] = string(l)
	}
	return out
}

// Valid reports whether l is one of the closed lanes.
func (l GitLane) Valid() bool { return ClosedGitLanes[l] }

// ADOTokenMode is HOW an Entra-lane run presents itself to Azure DevOps. It
// has ONE legal value today, kept as a named field so a refused mode is
// refused with its REASON rather than an opaque "unknown field" 400.
type ADOTokenMode string

const (
	// ADOTokenModeBearer sends the Entra access token itself. The zero value,
	// and the only mode accepted.
	ADOTokenModeBearer ADOTokenMode = "bearer"
	// ADOTokenModeMintedPAT would exchange the Entra token for a short-lived
	// PAT per run. REFUSED at the write boundary and absent from
	// ClosedADOTokenModes: Azure DevOps mints PATs only for Microsoft's own
	// first-party clients (measured: 401 TF400813). Stays NAMED so the
	// refusal can say why.
	ADOTokenModeMintedPAT ADOTokenMode = "minted_pat"
)

// ClosedADOTokenModes is the closed token-mode set — see ClosedGitLanes.
var ClosedADOTokenModes = map[ADOTokenMode]bool{
	ADOTokenModeBearer: true,
}

// ClosedADOTokenModeList is ClosedADOTokenModes in a stable order.
func ClosedADOTokenModeList() []string {
	modes := slices.Sorted(maps.Keys(ClosedADOTokenModes))
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}

// Valid reports whether m is an accepted mode.
func (m ADOTokenMode) Valid() bool { return ClosedADOTokenModes[m] }

// ADOEntraConfig is the Entra lane's configuration on one Azure DevOps row:
// tenant, app, widest ceiling, default profile, and token presentation.
// THE CEILING IS NOT A DEFAULT: CapabilityCeiling is the most a run could
// ever be granted, DefaultProfile (held inside it at the write boundary) is
// what it gets by default.
type ADOEntraConfig struct {
	// TenantID is the Entra tenant every sign-in for this row goes to, as a
	// GUID. A well-known alias ("common", "organizations") is refused.
	TenantID string `json:"tenant_id"`
	// ClientID is the app registration the sign-in names, as a GUID.
	ClientID string `json:"client_id"`
	// CapabilityCeiling is the widest set of capabilities a run on this row may
	// ever hold. Required and non-empty when the block is present.
	CapabilityCeiling []adoscope.Capability `json:"capability_ceiling,omitempty"`
	// DefaultProfile is what a run gets when it asks for nothing. Empty reads
	// as adoscope.ProfileRead.
	DefaultProfile []adoscope.Capability `json:"default_profile,omitempty"`
	// TokenMode is how the token reaches the run. Empty reads as ADOTokenModeBearer.
	TokenMode ADOTokenMode `json:"token_mode,omitempty"`
	// RESTAPI: brokered REST calls, or only git traffic. Default TRUE; read
	// through RESTAPIEnabled, never directly.
	RESTAPI *bool `json:"rest_api,omitempty"`
}

// RESTAPIEnabled is the ONE spelling of the rest_api default: unset (or a nil
// receiver) is enabled.
func (c *ADOEntraConfig) RESTAPIEnabled() bool {
	return c == nil || c.RESTAPI == nil || *c.RESTAPI
}

// Profile is the capabilities a run gets when it asks for nothing:
// DefaultProfile, or adoscope.ProfileRead when that is empty.
func (c *ADOEntraConfig) Profile() []adoscope.Capability {
	if c == nil || len(c.DefaultProfile) == 0 {
		return adoscope.ProfileRead()
	}
	return slices.Clone(c.DefaultProfile)
}

// GitProvider is one git-provider row: a kind, the base URLs it admits, and
// the credential lanes it permits.
type GitProvider struct {
	// ID is an operator-chosen stable slug, unique within Git, held to
	// integrationRefRE at the write boundary. A capability grant names a
	// provider by this id.
	ID string `json:"id"`
	// Kind says which forge's bespoke behaviour this row carries.
	Kind GitProviderKind `json:"kind"`
	// Disabled turns the row off without deleting it (zero value ENABLED). A
	// disabled row still CLAIMS its hosts rather than falling through to ScmHosts.
	Disabled bool `json:"disabled,omitempty"`
	// BaseURLs are the https addresses this row admits: a repo is admitted
	// when its clone URL's scheme and host match one of these and its path
	// equals or extends that base URL's path at a "/" boundary. At least one,
	// at most eight; no port, credentials, query or fragment; at most two
	// path segments (validateWorkspaceProviders).
	BaseURLs []string `json:"base_urls"`
	// Lanes are the credential lanes a run may use for this provider. EMPTY
	// MEANS EVERY LEGACY LANE (LegacyGitLanes) — the field narrows, never widens.
	Lanes []GitLane `json:"lanes,omitempty"`
	// CredentialSource is WHOSE credential this row's lanes use. Empty reads
	// as CredentialSourceShared; CredentialSourcePerUser requires GitLaneEntra.
	CredentialSource CredentialSource `json:"credential_source,omitempty"`
	// Entra is present only on, and required by, a row whose Lanes name GitLaneEntra.
	Entra *ADOEntraConfig `json:"entra,omitempty"`
}

// StorageProviders is the file-system half of WorkspaceProviders. Each nil
// sub-block is legacy behaviour for that half.
type StorageProviders struct {
	// Ephemeral bounds the writable scratch a run gets when it mounts no drive.
	Ephemeral *EphemeralProvider `json:"ephemeral,omitempty"`
	// UserDrive is the org switch and ceiling over user drives.
	UserDrive *UserDriveProvider `json:"user_drive,omitempty"`
}

// EphemeralProvider is the ephemeral-scratch policy: the default size, and
// the ceiling a larger request is CLAMPED to (never refused).
type EphemeralProvider struct {
	DefaultDiskMiB int `json:"default_disk_mib,omitempty"` // 0 = unset
	// MaxDiskMiB is the ceiling a larger request is clamped to (0 =
	// unlimited). A DefaultDiskMiB above it is refused at the write boundary.
	MaxDiskMiB int `json:"max_disk_mib,omitempty"`
}

// UserDriveProvider is the org-level user-drive switch and ceiling.
// Disabled is a DIFFERENT question from GovernanceLimits.DenyUserDrive, and
// the two are checked in this order: this ORG switch first (422,
// refused-backend), then the per-profile DOOR (403 + authz.denied).
type UserDriveProvider struct {
	// Disabled turns drives off deployment-wide — negative-sense like
	// GitProvider.Disabled, so the zero value is today.
	Disabled bool `json:"disabled,omitempty"`
	// MaxSizeMiB is the largest drive this deployment allows, refused at the
	// admin write boundary and clamped at resolve. 0 = no ceiling.
	MaxSizeMiB int `json:"max_size_mib,omitempty"`
}
