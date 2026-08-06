// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 The SDK re-exports and references the deprecated
// single-source shape on purpose: it is the wire compatibility surface older
// callers still send.

package client

// This file re-exports — as type aliases and const re-exports — the
// internal/types symbols that appear in pkg/client's exported method
// signatures and request/response structs. The aliases use the `=` form, so
// each is the SAME type as its internal/types counterpart (assignment between
// the two compiles without conversion). The purpose is solely surface: an
// external module CANNOT import github.com/cjohnstoniv/wardyn/internal/types (Go
// forbids importing another module's internal/ tree), so without these the SDK
// would be unusable — callers could invoke methods but could not name the
// values those methods return or accept. With them, github.com/cjohnstoniv/
// wardyn/pkg/client is a self-sufficient public surface.
//
// internal/types remains the single source of truth; these add no new types.

import "github.com/cjohnstoniv/wardyn/internal/types"

// Domain nouns returned or accepted by Client methods.
type (
	// AgentRun is one governed execution of a coding agent. Returned by
	// CreateRun, GetRun, and ListRuns.
	AgentRun = types.AgentRun

	// RunPolicy is a declarative policy attached to runs. Returned by the
	// policy methods (ListPolicies, GetPolicy, CreatePolicy, UpdatePolicy).
	RunPolicy = types.RunPolicy

	// RunPolicySpec is the policy body carried in PolicyRequest.Spec. As a true
	// `=` alias it carries the full wire surface, including AllowAllEgress
	// (json:"allow_all_egress,omitempty") for the "allow all (deny-list only)"
	// egress mode — no separate SDK struct to keep in sync.
	RunPolicySpec = types.RunPolicySpec

	// CredentialGrant is a credential-grant eligibility record. Returned by
	// ListGrants.
	CredentialGrant = types.CredentialGrant

	// GrantSpec is a credential scope description carried in
	// RunPolicySpec.EligibleGrants.
	GrantSpec = types.GrantSpec

	// WorkspaceMount is an operator/policy-controlled host bind mount carried
	// in RunPolicySpec.WorkspaceMounts.
	WorkspaceMount = types.WorkspaceMount

	// ApprovalRequest is a human-in-the-loop approval gate. Returned by
	// ListApprovals, Approve, and Deny.
	ApprovalRequest = types.ApprovalRequest

	// AuditEvent is one append-only audit record. Returned by AuditEvents.
	AuditEvent = types.AuditEvent

	// Workspace is an onboarded local dir / repo / container. Returned by
	// ListWorkspaces, GetWorkspace, CreateWorkspace, and UpdateWorkspace.
	Workspace = types.Workspace

	// WorkspaceLLMCred is the operator-owned model/harness credential binding
	// carried in WorkspaceRequest.LLMCred: IntegrationRef names a
	// SiteConfig.Integrations entry this workspace's model/harness access
	// resolves through. "" (or a nil WorkspaceLLMCred) means no binding.
	WorkspaceLLMCred = types.WorkspaceLLMCred

	// WorkspaceBedrockRef is a Bedrock region/model selection. Aliased because
	// it is a pointer-field shape a caller may need to build (dispatchParams.
	// BedrockRef server-side); no current WorkspaceLLMCred field carries one —
	// that binding resolves through an Integration (see WorkspaceLLMCred) —
	// but the type is kept nameable for that resolution's future wiring.
	WorkspaceBedrockRef = types.WorkspaceBedrockRef

	// Source is one tier-1 library entry: a repo/dir configured once (its own
	// contract + scan) and attached to many workspaces.
	Source = types.Source
	// SourceKind discriminates a library source: local_dir | repo.
	SourceKind = types.SourceKind

	// WorkspaceSource is one entry in a Workspace's composition, carried in
	// WorkspaceRequest.Sources and returned in Workspace.Sources.
	WorkspaceSource = types.WorkspaceSource

	// WorkspaceBaseImage is a Workspace's base-image choice, carried in
	// WorkspaceRequest.BaseImage and returned in Workspace.BaseImage.
	WorkspaceBaseImage = types.WorkspaceBaseImage

	// WorkspaceRequirement is one entry in Workspace.Requirements.
	WorkspaceRequirement = types.WorkspaceRequirement

	// WorkspaceAttachment is one tier-3 composition row (Workspace.Attachments):
	// either a library Source reference (SourceID set) or an inline ephemeral
	// scratch row.
	WorkspaceAttachment = types.WorkspaceAttachment

	// BaseImageEntry is one tier-2 base-image catalog row: a shared, reusable
	// image an operator saved (kind registry|custom|byo).
	BaseImageEntry = types.BaseImageEntry

	// SiteConfig is the operator-wide site config. Returned by GetSiteConfig and
	// accepted by PutSiteConfig.
	SiteConfig = types.SiteConfig

	// ArtifactOverride is one ecosystem's artifact-registry redirect, carried in
	// the deprecated SiteConfig.ArtifactOverrides.
	//
	// Deprecated: superseded by EgressRedirect. Kept aliased so a caller that
	// still builds the legacy shape (PutSiteConfig folds it server-side for one
	// release) can name the element type; GetSiteConfig itself now returns this
	// field empty.
	ArtifactOverride = types.ArtifactOverride

	// EgressRedirect is one outbound redirect (package registry or otherwise),
	// carried in SiteConfig.EgressRedirects. Aliased for the same reason as
	// ArtifactOverride: GetSiteConfig returns a nil slice when unconfigured, so
	// without a nameable element type a caller could not author one at all.
	EgressRedirect = types.EgressRedirect
)

// Enumerated string types named in exported signatures and struct fields.
type (
	// RunState is the AgentRun lifecycle state (AgentRun.State,
	// KillRunResponse.State).
	RunState = types.RunState

	// ApprovalState is the approval lifecycle state; ListApprovals filters on
	// it (ApprovalRequest.State).
	ApprovalState = types.ApprovalState

	// ConfinementClass declares how strongly a sandbox confines an agent
	// (AgentRun.ConfinementClass, RunPolicySpec.MinConfinementClass).
	ConfinementClass = types.ConfinementClass

	// GrantKind enumerates broker-mintable credential kinds (GrantSpec.Kind).
	GrantKind = types.GrantKind

	// ActorType distinguishes who performed an audited action
	// (AuditEvent.ActorType).
	ActorType = types.ActorType

	// ApprovalKind enumerates what a human is being asked to approve
	// (ApprovalRequest.Kind).
	ApprovalKind = types.ApprovalKind

	// WorkspaceKind is what a workspace onboards (Workspace.Kind,
	// WorkspaceRequest.Kind). Deprecated: a DERIVED READ-ONLY MIRROR of
	// Workspace.Sources[0] (single-source only) — see WorkspaceSourceType for
	// the field a multi-source Workspace actually carries per-source.
	WorkspaceKind = types.WorkspaceKind

	// WorkspaceSourceType discriminates a WorkspaceSource's kind (local_dir |
	// repo | ephemeral).
	WorkspaceSourceType = types.WorkspaceSourceType

	// WorkspaceStatus is the onboarding/scan lifecycle of a Workspace or Source
	// (Workspace.Status, Source.Status): not-yet-scanned, mid-scan, scanned
	// (ready to use), or errored.
	WorkspaceStatus = types.WorkspaceStatus
)

// SourceKind values (the tier-1 library's two onboardable kinds).
const (
	SourceLocalDir = types.SourceLocalDir
	SourceRepo     = types.SourceRepo
)

// ApprovalState values. ListApprovals accepts one of these (or "" for all
// states).
const (
	ApprovalPending  = types.ApprovalPending
	ApprovalApproved = types.ApprovalApproved
	ApprovalDenied   = types.ApprovalDenied
	ApprovalExpired  = types.ApprovalExpired
)

// RunState values.
const (
	RunPending   = types.RunPending
	RunStarting  = types.RunStarting
	RunRunning   = types.RunRunning
	RunWaiting   = types.RunWaiting
	RunStopped   = types.RunStopped
	RunArchived  = types.RunArchived
	RunFailed    = types.RunFailed
	RunKilled    = types.RunKilled
	RunCompleted = types.RunCompleted
)

// ConfinementClass values.
const (
	CC1 = types.CC1
	CC2 = types.CC2
	CC3 = types.CC3
)

// GrantKind values.
const (
	GrantGitHubToken = types.GrantGitHubToken
	GrantCloudSTS    = types.GrantCloudSTS
	GrantAPIKey      = types.GrantAPIKey
)

// ApprovalKind values.
const (
	ApprovalCredential   = types.ApprovalCredential
	ApprovalEgressDomain = types.ApprovalEgressDomain
	ApprovalToolCall     = types.ApprovalToolCall
)

// ActorType values.
const (
	ActorHuman  = types.ActorHuman
	ActorAgent  = types.ActorAgent
	ActorSystem = types.ActorSystem
)

// WorkspaceKind values (WorkspaceRequest.Kind). Deprecated: the legacy
// single-source shape; a new caller should build WorkspaceSource entries with
// a WorkspaceSourceType value instead.
const (
	WorkspaceKindLocalDir  = types.WorkspaceKindLocalDir
	WorkspaceKindRepo      = types.WorkspaceKindRepo
	WorkspaceKindContainer = types.WorkspaceKindContainer
)

// WorkspaceSourceType values (WorkspaceSource.Type).
const (
	WorkspaceSourceTypeLocalDir  = types.WorkspaceSourceTypeLocalDir
	WorkspaceSourceTypeRepo      = types.WorkspaceSourceTypeRepo
	WorkspaceSourceTypeEphemeral = types.WorkspaceSourceTypeEphemeral
)

// WorkspaceStatus values (Workspace.Status, Source.Status).
const (
	WorkspacePendingScan = types.WorkspacePendingScan
	WorkspaceScanning    = types.WorkspaceScanning
	WorkspaceScanned     = types.WorkspaceScanned
	WorkspaceError       = types.WorkspaceError
)
