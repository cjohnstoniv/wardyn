// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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
