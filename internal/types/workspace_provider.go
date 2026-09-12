// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
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

// GitLane is one of the three credential lanes a run can clone with: the
// GitHub App broker (repository-scoped, minted per run), a stored PAT, or a
// stored SSH key. A provider row's Lanes VETO lanes; they never mint anything
// and never create a credential that was not already stored.
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
)

// ClosedGitLanes is the closed lane set — see ClosedGitProviderKinds.
var ClosedGitLanes = map[GitLane]bool{
	GitLaneApp: true, GitLanePAT: true, GitLaneSSH: true,
}

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

// Valid reports whether l is one of the three lanes.
func (l GitLane) Valid() bool { return ClosedGitLanes[l] }

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
	// MEANS EVERY LANE THE KIND SUPPORTS — the field narrows, it never widens,
	// so an absent value is today's behaviour.
	Lanes []GitLane `json:"lanes,omitempty"`
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
