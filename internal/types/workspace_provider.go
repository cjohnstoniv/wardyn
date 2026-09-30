// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// WorkspaceProviders is the org's workspace-provider policy: what kinds of
// work a sandbox may be given and the ceilings it's held to. The zero value
// is legacy open mode (load-bearing for upgraded installs), never derived
// from legacy SiteConfig.ScmHosts; URL validation lives in internal/api.
type WorkspaceProviders struct {
	// Git rows: hosts a run may clone from and the credential lanes usable there.
	Git []GitProvider `json:"git,omitempty"`
	// Storage is the file-system half. A nil block is legacy behaviour.
	Storage *StorageProviders `json:"storage,omitempty"`
	// GitPatBrokerEnabled is read-only and server-owned: projected from
	// WARDYN_GIT_PAT_BROKER on every GET, never stored or accepted on a PUT.
	// A pointer since nil ("never projected") differs from a present false.
	GitPatBrokerEnabled *bool `json:"git_pat_broker_enabled,omitempty"`
}

// Empty reports whether p carries no policy — the shape a PUT uses to clear
// the block; the write boundary normalizes an empty block to nil.
func (p *WorkspaceProviders) Empty() bool {
	if p == nil {
		return true
	}
	if len(p.Git) > 0 {
		return false
	}
	return p.Storage == nil || (p.Storage.Ephemeral == nil && p.Storage.UserDrive == nil)
}

// CredentialSource says WHOSE credential a git provider's lanes use.
type CredentialSource string

const (
	// CredentialSourceShared: one credential, captured by an admin, backs
	// every run. The zero value, so an unset field keeps legacy behaviour.
	CredentialSourceShared CredentialSource = "shared"
	// CredentialSourcePerUser: one credential per principal, captured or
	// stored under their own namespace.
	CredentialSourcePerUser CredentialSource = "per_user"
)

// ClosedCredentialSources is the closed source set — the only values a write may name.
var ClosedCredentialSources = map[CredentialSource]bool{
	CredentialSourceShared: true, CredentialSourcePerUser: true,
}

// ClosedCredentialSourceList is ClosedCredentialSources in a stable order, for a
// rejected write's "want one of: …".
func ClosedCredentialSourceList() []string {
	ss := slices.Sorted(maps.Keys(ClosedCredentialSources))
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// Valid reports whether s is one of the two sources.
func (s CredentialSource) Valid() bool { return ClosedCredentialSources[s] }

// GitProviderKind is the closed set of git forges Wardyn has bespoke behaviour
// for; a self-hosted forge is a github (GHES) or azure_devops (ADO Server) row, not a third kind.
type GitProviderKind string

const (
	GitProviderGitHub      GitProviderKind = "github"       // github.com or GitHub Enterprise Server
	GitProviderAzureDevOps GitProviderKind = "azure_devops" // dev.azure.com, org.visualstudio.com, or ADO Server
)

// ClosedGitProviderKinds is the only kinds a write may name.
var ClosedGitProviderKinds = map[GitProviderKind]bool{
	GitProviderGitHub: true, GitProviderAzureDevOps: true,
}

// ClosedGitProviderKindList is ClosedGitProviderKinds in a stable order.
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
// row's Lanes only veto lanes; they never mint or create a new credential.
type GitLane string

const (
	GitLaneApp   GitLane = "app"   // GitHub App broker lane (github.com only)
	GitLanePAT   GitLane = "pat"   // stored git-pat-<slug> lane
	GitLaneSSH   GitLane = "ssh"   // stored ssh-key-<slug> lane
	GitLaneEntra GitLane = "entra" // Microsoft Entra lane: run authorizes as the person, against the org's own tenant; Azure DevOps hosted only
)

// ClosedGitLanes is the closed lane set — see ClosedGitProviderKinds.
var ClosedGitLanes = map[GitLane]bool{
	GitLaneApp: true, GitLanePAT: true, GitLaneSSH: true, GitLaneEntra: true,
}

// LegacyGitLanes are the lanes an empty GitProvider.Lanes list admits: not
// "every lane in ClosedGitLanes" but the three from when this rule was
// written, frozen forever so growing the closed set never silently opts a
// stored row into a new lane.
var LegacyGitLanes = []GitLane{GitLaneApp, GitLanePAT, GitLaneSSH}

// Legacy reports whether l is admitted by an empty Lanes list; expanding one must ask this, never ClosedGitLanes.
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

// ADOTokenMode is how a run presents itself to Azure DevOps on an Entra-lane
// row: the Entra access token itself, a personal access token Wardyn creates
// for the run, or one the person pasted. A named field, so a refused mode says
// why rather than an opaque "unknown field" 400.
type ADOTokenMode string

const (
	ADOTokenModeBearer ADOTokenMode = "bearer" // sends the Entra access token itself; the zero value
	// ADOTokenModeMintedPAT creates a short-lived personal access token per
	// run, in the person's name, through the sign-in app's own grant. Named
	// and closed, but the write boundary still refuses it: it is not yet
	// available.
	ADOTokenModeMintedPAT ADOTokenMode = "minted_pat"
	// ADOTokenModeOwnPAT is a personal access token the person pasted in
	// themselves. It needs no tenant or client: nothing signs in. SO EVERY
	// PICKER OF THE ONE SIGN-IN ROW MUST SKIP IT — the console-login capture
	// (adoEntraRow in cmd/wardynd) and the dispatch row lookup take any
	// enabled per_user entra-lane row today, and a row with no tenant or
	// client would fail their validation on every login and could shadow the
	// row that does sign in. Skipping own_pat there is L1's change (#1428).
	ADOTokenModeOwnPAT ADOTokenMode = "own_pat"
)

// ClosedADOTokenModes is the closed token-mode set — see ClosedGitLanes.
var ClosedADOTokenModes = map[ADOTokenMode]bool{
	ADOTokenModeBearer: true, ADOTokenModeMintedPAT: true, ADOTokenModeOwnPAT: true,
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

// Bounds and defaults for the personal-access-token lifetimes on ADOEntraConfig.
const (
	ADOPATMaxHoursDefault = 8   // minted_pat: PATMaxHours 0 reads as this
	ADOPATMaxHoursLimit   = 168 // minted_pat: the longest a token may live, in hours
	ADOPATMaxDaysDefault  = 30  // own_pat: PATMaxDays 0 reads as this
	ADOPATMaxDaysLimit    = 90  // own_pat: the longest a pasted token may live, in days
)

// ADOEntraConfig is the Entra lane's configuration on one Azure DevOps row:
// tenant, app, widest ceiling, default profile, and token presentation. The
// ceiling is not a default: CapabilityCeiling is the most a run could ever be
// granted; DefaultProfile is what it gets by default.
type ADOEntraConfig struct {
	// TenantID is the Entra tenant every sign-in for this row goes to (GUID); a well-known alias ("common", "organizations") is refused.
	TenantID string `json:"tenant_id"`
	ClientID string `json:"client_id"` // app registration the sign-in names, as a GUID
	// CapabilityCeiling is the widest capability set a run on this row may
	// ever hold; required and non-empty when the block is present.
	CapabilityCeiling []adoscope.Capability `json:"capability_ceiling,omitempty"`
	DefaultProfile    []adoscope.Capability `json:"default_profile,omitempty"` // what a run gets by default; empty reads as adoscope.ProfileDefault
	TokenMode         ADOTokenMode          `json:"token_mode,omitempty"`      // empty reads as ADOTokenModeBearer
	// PATMaxHours is the longest a minted_pat run's token lives (1-168). 0
	// reads as 8; read via PATHours, never directly.
	PATMaxHours int `json:"pat_max_hours,omitempty"`
	// PATMaxDays is the furthest expiry an own_pat token may carry (1-90). 0
	// reads as 30; read via PATDays, never directly. A Server row has no entra
	// block, so it cannot set this and always reads the default, 30.
	PATMaxDays int `json:"pat_max_days,omitempty"`
	// RESTAPI: brokered REST calls, or only git traffic. Default true; read
	// via RESTAPIEnabled, never directly.
	RESTAPI *bool `json:"rest_api,omitempty"`
}

// RESTAPIEnabled: the rest_api default (unset or nil receiver) is enabled.
func (c *ADOEntraConfig) RESTAPIEnabled() bool {
	return c == nil || c.RESTAPI == nil || *c.RESTAPI
}

// PATHours is PATMaxHours with the unset default applied.
func (c *ADOEntraConfig) PATHours() int {
	if c == nil || c.PATMaxHours == 0 {
		return ADOPATMaxHoursDefault
	}
	return c.PATMaxHours
}

// PATDays is PATMaxDays with the unset default applied.
func (c *ADOEntraConfig) PATDays() int {
	if c == nil || c.PATMaxDays == 0 {
		return ADOPATMaxDaysDefault
	}
	return c.PATMaxDays
}

// Profile is the capabilities a run gets when it asks for nothing:
// DefaultProfile, or adoscope.ProfileDefault when that is empty.
func (c *ADOEntraConfig) Profile() []adoscope.Capability {
	if c == nil || len(c.DefaultProfile) == 0 {
		return adoscope.ProfileDefault()
	}
	return slices.Clone(c.DefaultProfile)
}

// GitProvider is one git-provider row: a kind, the base URLs it admits, and
// the credential lanes it permits.
type GitProvider struct {
	ID   string          `json:"id"`   // operator-chosen stable slug, unique within Git, held to integrationRefRE; a capability grant names a provider by this id
	Kind GitProviderKind `json:"kind"` // which forge's bespoke behaviour this row carries
	// Disabled turns the row off without deleting it (zero value enabled); a
	// disabled row still claims its hosts rather than falling through to ScmHosts.
	Disabled bool `json:"disabled,omitempty"`
	// BaseURLs are the https addresses this row admits: a repo is admitted
	// when its clone URL's scheme/host match one of these and its path equals
	// or extends that base URL's path at a "/" boundary. 1-8 entries; no
	// port, credentials, query or fragment; at most two path segments.
	BaseURLs []string `json:"base_urls"`
	// Lanes are the credential lanes usable for this provider; empty means
	// every legacy lane (LegacyGitLanes) — the field narrows, never widens.
	Lanes            []GitLane        `json:"lanes,omitempty"`
	CredentialSource CredentialSource `json:"credential_source,omitempty"` // whose credential the lanes use; empty = CredentialSourceShared, CredentialSourcePerUser requires GitLaneEntra
	Entra            *ADOEntraConfig  `json:"entra,omitempty"`             // present only on, and required by, a row whose Lanes name GitLaneEntra
}

// StorageProviders is the file-system half of WorkspaceProviders; each nil sub-block is legacy behaviour for that half.
type StorageProviders struct {
	Ephemeral *EphemeralProvider `json:"ephemeral,omitempty"`  // bounds writable scratch when a run mounts no drive
	UserDrive *UserDriveProvider `json:"user_drive,omitempty"` // org switch and ceiling over user drives
}

// EphemeralProvider is the ephemeral-scratch policy: default size, and the
// ceiling a larger request is clamped to (never refused).
type EphemeralProvider struct {
	DefaultDiskMiB int `json:"default_disk_mib,omitempty"` // 0 = unset
	// MaxDiskMiB: ceiling a larger request is clamped to (0 = unlimited); a
	// DefaultDiskMiB above it is refused at the write boundary.
	MaxDiskMiB int `json:"max_disk_mib,omitempty"`
}

// UserDriveProvider is the org-level user-drive switch and ceiling. Disabled
// is a different question from GovernanceLimits.DenyUserDrive; the two are
// checked in order: this org switch first (422, refused-backend), then the
// per-profile door (403 + authz.denied).
type UserDriveProvider struct {
	Disabled   bool `json:"disabled,omitempty"`     // turns drives off deployment-wide, negative-sense like GitProvider.Disabled, so zero value is today
	MaxSizeMiB int  `json:"max_size_mib,omitempty"` // largest drive allowed, refused at admin write boundary and clamped at resolve; 0 = no ceiling
}
