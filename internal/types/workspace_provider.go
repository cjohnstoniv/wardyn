// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// WorkspaceProviders is the org's WORKSPACE-PROVIDER POLICY: which kinds of
// work a sandbox may be given, and the ceilings that work is held to. It is a
// sub-object of the SiteConfig singleton (like Integrations) rather than its
// own table — a closed Go struct validated at the write boundary needs no DDL
// (the doctrine stated at internal/types/governance.go and
// internal/api/capabilities.go), and site-config is the one org->desktop
// channel MDM already delivers (deploy/wardyn-desktop.sh re-applies
// /etc/wardyn/site-config.json on every boot, so a provider block in a table
// would be undeliverable to a laptop).
//
// THE ZERO VALUE IS LEGACY OPEN MODE, and that is load-bearing: an upgraded
// install carries no provider rows, so every predicate over this block answers
// exactly what it answered before the block existed (see the api package's
// providersConfigured / providerFor). Nothing here is ever implied, derived or
// folded from the legacy SiteConfig.ScmHosts list — providers are authored only
// through the providers surface, and ScmHosts is never written by this feature.
//
// URL VALIDATION DELIBERATELY LIVES IN internal/api (validateWorkspaceProviders)
// so it reuses the site-config write gate — validSiteURL / shellSafeSiteString /
// hostrules.ValidApprovedHost — instead of a second opinion about what a safe
// persisted URL is. This file is types and closed enums only.
//
// The ONE internal import here is internal/adoscope, and it is deliberate: the
// Entra lane's ceiling and profile are written in the CLASSIFIER's capability
// vocabulary, and a second spelling of that enum in this package would be a
// second answer to "what does code_write permit". adoscope imports the standard
// library only, so the dependency cannot come back the other way.
type WorkspaceProviders struct {
	// Git are the git-provider rows: which hosts (and which paths on them) a
	// run may clone from, and which credential lanes it may use there.
	Git []GitProvider `json:"git,omitempty"`
	// Storage is the file-system half — ephemeral scratch and user drives. A
	// nil block is legacy behaviour for that half.
	Storage *StorageProviders `json:"storage,omitempty"`
}

// Empty reports whether p carries no policy at all — the shape a caller PUTs to
// CLEAR the block. The write boundary normalizes an empty block to nil so a
// later GET does not render "workspace_providers":{} on every read forever
// (SiteConfig.WorkspaceProviders is a pointer for the same reason: a value
// struct's omitempty is a no-op).
func (p *WorkspaceProviders) Empty() bool {
	if p == nil {
		return true
	}
	if len(p.Git) > 0 {
		return false
	}
	return p.Storage == nil || (p.Storage.Ephemeral == nil && p.Storage.UserDrive == nil)
}

// GitProviderKind is the closed set of git forges Wardyn has bespoke behaviour
// for — the credential lanes, the egress bundles and the host classifiers all
// key on one of these two. A self-hosted forge is NOT a third kind: it is a
// github (GHES) or azure_devops (ADO Server) row whose base URL names its own
// host, because nothing on the tree can tell a GHES host from an ADO Server
// host from a GitLab host by its name alone.
type GitProviderKind string

const (
	// GitProviderGitHub is github.com or a GitHub Enterprise Server host.
	GitProviderGitHub GitProviderKind = "github"
	// GitProviderAzureDevOps is dev.azure.com, a legacy org.visualstudio.com,
	// or an Azure DevOps Server host.
	GitProviderAzureDevOps GitProviderKind = "azure_devops"
)

// ClosedGitProviderKinds is the closed kind set — the only kinds a write may
// name (validateWorkspaceProviders refuses anything else), in the
// ClosedIntegrationKinds shape.
var ClosedGitProviderKinds = map[GitProviderKind]bool{
	GitProviderGitHub: true, GitProviderAzureDevOps: true,
}

// ClosedGitProviderKindList is ClosedGitProviderKinds in a stable order, for
// the "want one of: …" half of a rejected write's error. Sorted so the message
// is deterministic across map iterations.
func ClosedGitProviderKindList() []string {
	kinds := slices.Sorted(maps.Keys(ClosedGitProviderKinds))
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// Valid reports whether k is one of the closed kinds, mirroring
// DriveBackend.Valid — the API write boundary uses it in place of a second
// opinion about the check.
func (k GitProviderKind) Valid() bool { return ClosedGitProviderKinds[k] }

// GitLane is one of the credential lanes a run can clone with: the GitHub App
// broker (repository-scoped, minted per run), a stored PAT, a stored SSH key,
// or the per-person Entra lane. A provider row's Lanes VETO lanes; they never
// mint anything and never create a credential that was not already stored.
type GitLane string

const (
	// GitLaneApp is the GitHub App broker lane (github.com only — the broker
	// mints per <org>/<repo> there and has no Azure DevOps equivalent).
	GitLaneApp GitLane = "app"
	// GitLanePAT is the stored git-pat-<slug> lane.
	GitLanePAT GitLane = "pat"
	// GitLaneSSH is the stored ssh-key-<slug> lane, over the SSH-over-443
	// endpoints github.com and dev.azure.com publish.
	GitLaneSSH GitLane = "ssh"
	// GitLaneEntra is the Microsoft Entra lane: the run is authorized as the
	// PERSON, against the organisation's own tenant, with a token scoped to the
	// capabilities the row permits. Azure DevOps HOSTED only — an Azure DevOps
	// Server host does not accept Entra tokens at all.
	GitLaneEntra GitLane = "entra"
)

// ClosedGitLanes is the closed lane set — see ClosedGitProviderKinds.
var ClosedGitLanes = map[GitLane]bool{
	GitLaneApp: true, GitLanePAT: true, GitLaneSSH: true, GitLaneEntra: true,
}

// LegacyGitLanes are the lanes an EMPTY GitProvider.Lanes list admits: the
// three that existed when "empty means every lane the kind supports" was
// written down.
//
// IT IS NOT "every lane in ClosedGitLanes", and that distinction is the whole
// reason this list exists. Admission expands an empty Lanes list to every
// lane, so growing the closed set alone would have opted EVERY STORED ROW into
// the new lane the moment it was added — an opening nobody authored, delivered
// to every install by the same MDM channel that delivers site-config. A lane
// added after this line must be NAMED on a row to be usable, so this list is
// frozen and never grows again.
var LegacyGitLanes = []GitLane{GitLaneApp, GitLanePAT, GitLaneSSH}

// Legacy reports whether l is one of the lanes an empty Lanes list admits.
// Every site that expands an empty list asks THIS, never ClosedGitLanes.
func (l GitLane) Legacy() bool { return slices.Contains(LegacyGitLanes, l) }

// ClosedGitLaneList is ClosedGitLanes in a stable order, for a rejected
// write's "want one of: …".
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

// ADOTokenMode is HOW an Entra-lane run presents itself to Azure DevOps.
type ADOTokenMode string

const (
	// ADOTokenModeBearer sends the Entra access token itself. The zero value,
	// so an unset field is the bearer mode.
	ADOTokenModeBearer ADOTokenMode = "bearer"
	// ADOTokenModeMintedPAT exchanges the Entra token for a short-lived
	// personal access token on the control plane, and the run sees only the
	// PAT. It exists because some Azure DevOps endpoints and every git
	// credential helper take a PAT and not a bearer token.
	//
	// The MINT needs the token-lifecycle scope (adoscope.ScopeTokens), which is
	// over an area the classifier ALWAYS DENIES to a run. The two are
	// consistent only because the mint happens on the control plane, before a
	// run exists — a run's own token never carries that scope, so a run can
	// never mint itself a second credential.
	ADOTokenModeMintedPAT ADOTokenMode = "minted_pat"
)

// ClosedADOTokenModes is the closed token-mode set — see ClosedGitLanes.
var ClosedADOTokenModes = map[ADOTokenMode]bool{
	ADOTokenModeBearer: true, ADOTokenModeMintedPAT: true,
}

// ClosedADOTokenModeList is ClosedADOTokenModes in a stable order, for a
// rejected write's "want one of: …".
func ClosedADOTokenModeList() []string {
	modes := slices.Sorted(maps.Keys(ClosedADOTokenModes))
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}

// Valid reports whether m is one of the two modes.
func (m ADOTokenMode) Valid() bool { return ClosedADOTokenModes[m] }

// ADOEntraConfig is the Entra lane's configuration on one Azure DevOps row:
// which tenant and app the run authenticates against, the widest access that
// row may ever ask for, what it asks for by default, and how the token is
// presented.
//
// THE CEILING IS NOT A DEFAULT. CapabilityCeiling is the most a run on this
// row could ever be granted, and DefaultProfile is what it gets when nothing
// asks for more — two separate numbers because an admin who is willing to
// permit a bypass ONCE, reviewed, must not thereby make every run able to
// bypass. The write boundary holds DefaultProfile inside CapabilityCeiling
// (validateProviderEntra); an empty DefaultProfile reads as
// adoscope.ProfileRead, so the read-only default is a property of the
// catalogue rather than a field someone remembered to fill in.
type ADOEntraConfig struct {
	// TenantID is the Entra tenant every sign-in for this row goes to, as a
	// GUID. A well-known alias ("common", "organizations") is refused at the
	// write boundary: those mean "whichever tenant the person belongs to",
	// which is the opposite of pinning one.
	TenantID string `json:"tenant_id"`
	// ClientID is the app registration the sign-in names, as a GUID.
	ClientID string `json:"client_id"`
	// CapabilityCeiling is the widest set of capabilities a run on this row may
	// ever hold. Required and non-empty when the block is present — an empty
	// ceiling would have to mean either "nothing" or "everything", and one of
	// those readings is the empty-Lanes trap again.
	CapabilityCeiling []adoscope.Capability `json:"capability_ceiling,omitempty"`
	// DefaultProfile is what a run gets when it asks for nothing. Empty reads
	// as adoscope.ProfileRead.
	DefaultProfile []adoscope.Capability `json:"default_profile,omitempty"`
	// TokenMode is how the token reaches the run. Empty reads as
	// ADOTokenModeBearer.
	TokenMode ADOTokenMode `json:"token_mode,omitempty"`
	// RESTAPI is whether Azure DevOps REST calls are brokered on this lane at
	// all, or only git traffic. A POINTER because the default is TRUE and a
	// bool's zero value is false: nil is "unset, so brokered", which keeps a
	// row written without the field on the behaviour the field describes.
	// Read it through RESTAPIEnabled, never directly.
	RESTAPI *bool `json:"rest_api,omitempty"`
}

// RESTAPIEnabled is the ONE spelling of the rest_api default: unset is
// enabled. A nil receiver answers true as well, so a caller holding a row with
// no Entra block reads the same answer as one whose block says nothing.
func (c *ADOEntraConfig) RESTAPIEnabled() bool {
	return c == nil || c.RESTAPI == nil || *c.RESTAPI
}

// Profile is the capabilities a run on this row gets when it asks for nothing
// — DefaultProfile, or the catalogue's read-only profile when that is empty.
// The fallback lives here so no call site invents its own idea of "default".
func (c *ADOEntraConfig) Profile() []adoscope.Capability {
	if c == nil || len(c.DefaultProfile) == 0 {
		return adoscope.ProfileRead()
	}
	return slices.Clone(c.DefaultProfile)
}

// GitProvider is one git-provider row: a kind, the base URLs it admits, and
// the credential lanes it permits.
type GitProvider struct {
	// ID is an operator-chosen stable slug, unique within Git — the same
	// operator-named identity Integration.ID has (this is a config sub-object
	// inside the SiteConfig singleton, not its own table, so there is no
	// generated uuid), held to integrationRefRE at the write boundary. A
	// capability grant names a provider by this id.
	ID string `json:"id"`
	// Kind says which forge's bespoke behaviour this row carries.
	Kind GitProviderKind `json:"kind"`
	// Disabled turns the row off without deleting its configuration —
	// negative-sense like Integration.Disabled so the zero value is ENABLED.
	// A present-but-disabled row is the admin saying "off": it still CLAIMS its
	// hosts (admission refuses them) rather than falling through to the legacy
	// ScmHosts list.
	Disabled bool `json:"disabled,omitempty"`
	// BaseURLs are the https addresses this row admits: a repo is admitted when
	// its clone URL's scheme and host match one of these and its path equals
	// that base URL's path or extends it at a "/" boundary. At least one, at
	// most eight; no port, no credentials, no query or fragment, at most two
	// path segments (validateWorkspaceProviders).
	BaseURLs []string `json:"base_urls"`
	// Lanes are the credential lanes a run may use for this provider. EMPTY
	// MEANS EVERY LEGACY LANE (LegacyGitLanes) — the field narrows, it never
	// widens, so an absent value is today's behaviour and a lane added later
	// must be named here to be usable.
	Lanes []GitLane `json:"lanes,omitempty"`
	// CredentialSource is WHOSE credential this row's lanes use, in the
	// AgentProvider.CredentialSource vocabulary. Empty reads as
	// CredentialSourceShared — today's behaviour, where one stored PAT or key
	// backs everyone. CredentialSourcePerUser requires GitLaneEntra: it is the
	// only git lane with a per-person authorization path at all.
	CredentialSource CredentialSource `json:"credential_source,omitempty"`
	// Entra is the Entra lane's configuration. Present only on a row whose
	// Lanes name GitLaneEntra, and required on one — a lane with no tenant to
	// sign in to is not a lane.
	Entra *ADOEntraConfig `json:"entra,omitempty"`
}

// StorageProviders is the file-system half of WorkspaceProviders. Each nil
// sub-block is legacy behaviour for that half; the ENFORCEMENT of these
// numbers lives at one dispatch site and one drive-resolver site, not here.
type StorageProviders struct {
	// Ephemeral bounds the writable scratch a run gets when it mounts no drive.
	Ephemeral *EphemeralProvider `json:"ephemeral,omitempty"`
	// UserDrive is the org switch and ceiling over user drives.
	UserDrive *UserDriveProvider `json:"user_drive,omitempty"`
}

// EphemeralProvider is the ephemeral-scratch policy: the size a run gets when
// it asks for none, and the ceiling a run asking for more is CLAMPED to (never
// refused — disk_mib is authored on policies, so a refusal would break every
// existing policy the day a limit is written).
type EphemeralProvider struct {
	// DefaultDiskMiB is filled in at dispatch for a run that requested no size.
	// 0 = unset (today's behaviour: no size is requested).
	DefaultDiskMiB int `json:"default_disk_mib,omitempty"`
	// MaxDiskMiB is the ceiling a larger request is clamped to. 0 = unlimited.
	// A DefaultDiskMiB above a non-zero MaxDiskMiB is refused at the write
	// boundary — a default that is already over the ceiling is not a policy,
	// it is a contradiction.
	MaxDiskMiB int `json:"max_disk_mib,omitempty"`
}

// UserDriveProvider is the org-level user-drive switch and ceiling.
//
// Disabled is a DIFFERENT question from GovernanceLimits.DenyUserDrive, and the
// two are checked in this order: this ORG switch first ("this install offers no
// drives" — a 422 in the refused-backend family, because nobody was denied by a
// profile), then the per-profile DOOR (403 + authz.denied, unchanged).
type UserDriveProvider struct {
	// Disabled turns drives off deployment-wide — negative-sense like
	// GitProvider.Disabled, so the zero value is today.
	Disabled bool `json:"disabled,omitempty"`
	// MaxSizeMiB is the largest drive this deployment allows: an allocation or
	// override above it is refused at the admin write boundary and clamped at
	// resolve. 0 = no ceiling.
	MaxSizeMiB int `json:"max_size_mib,omitempty"`
}
