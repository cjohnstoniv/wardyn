// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// GovernanceChangeReview is what the server read of a held change when it was proposed: whether it widens
// anything, why a change that widens nothing is still held, and the risk grade of a policy-shaped change.
// Verdict is widens, narrows_held or not_checked. A change proposed before the server wrote a review, and a
// request, carries none: read that as not_checked.
type GovernanceChangeReview struct {
	Verdict     string                     `json:"verdict"`
	Widens      []GovernanceChangeWidening `json:"widens,omitempty"`
	HeldBecause []string                   `json:"held_because,omitempty"`
	Risk        *GovernanceChangeRisk      `json:"risk,omitempty"`
}

// GovernanceChangeWidening is one field a change makes more permissive, with the entries it adds. Field is a
// policy field's JSON name, or limits, member_cap (Entries are the raised keys), members, withheld_grants,
// vcs or owning_unit.
type GovernanceChangeWidening struct {
	Field   string   `json:"field"`
	Entries []string `json:"entries,omitzero"`
}

// GovernanceChangeRisk is the deterministic risk grade of a change, before and after. Before is nil for a
// create.
type GovernanceChangeRisk struct {
	Before *GovernanceRiskGrade `json:"before"`
	After  GovernanceRiskGrade  `json:"after"`
}

// GovernanceRiskGrade is one side's grade: its highest level and the graded choices.
type GovernanceRiskGrade struct {
	Overall string               `json:"overall"`
	Items   []GovernanceRiskItem `json:"items"`
}

// GovernanceRiskItem is one graded config choice.
type GovernanceRiskItem struct {
	Field        string `json:"field"`
	Value        string `json:"value"`
	RiskLevel    string `json:"risk_level"`
	Rationale    string `json:"rationale"`
	InvariantRef string `json:"invariant_ref,omitempty"`
}

// GovernanceChangeImpact is what approving a pending change would do now. Computed is tree, boundary or
// none; with none, NotComputed is kind, stale or refused. People, LiveRuns and LiveRunsTotal are set for a
// tree impact, Boundary for a boundary one.
type GovernanceChangeImpact struct {
	Computed      string                    `json:"computed"`
	NotComputed   string                    `json:"not_computed,omitempty"`
	AsOf          time.Time                 `json:"as_of"`
	People        []GovernanceImpactPerson  `json:"people,omitzero"`
	LiveRuns      []GovernanceImpactRun     `json:"live_runs,omitzero"`
	LiveRunsTotal *int                      `json:"live_runs_total,omitempty"`
	Activates     []GovernanceUnitRef       `json:"activates,omitempty"`
	Vacates       []GovernanceUnitRef       `json:"vacates,omitempty"`
	Boundary      *GovernanceImpactBoundary `json:"boundary,omitempty"`
}

// GovernanceUnitRef names a unit that may become, or stop being, someone's context.
type GovernanceUnitRef struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
}

// GovernanceImpactChange is one knob an impact moves.
type GovernanceImpactChange struct {
	Knob   string `json:"knob"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// GovernanceImpactPerson is one membership an impact reaches: the context it is (From) and would be, and the
// knobs that differ. Error names a context that would not compose.
type GovernanceImpactPerson struct {
	MembershipID uuid.UUID                `json:"membership_id"`
	SubjectType  CapabilitySubjectType    `json:"subject_type"`
	Subject      string                   `json:"subject"`
	FromUnitID   uuid.UUID                `json:"from_unit_id"`
	FromUnitName string                   `json:"from_unit_name"`
	UnitID       uuid.UUID                `json:"unit_id"`
	UnitName     string                   `json:"unit_name"`
	Changes      []GovernanceImpactChange `json:"changes"`
	Error        string                   `json:"error,omitempty"`
}

// GovernanceImpactRun is one live run an impact reaches. Title is sent only to the run's owner and to admins.
type GovernanceImpactRun struct {
	ID        uuid.UUID `json:"id"`
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"started_at"`
	Title     string    `json:"title,omitempty"`
}

// GovernanceImpactBoundary is the rows a drafted boundary would narrow.
type GovernanceImpactBoundary struct {
	Target         string                   `json:"target"`
	Deployment     []GovernanceImpactChange `json:"deployment"`
	Units          []GovernanceImpactRow    `json:"units"`
	StoredPolicies []GovernanceImpactRow    `json:"stored_policies"`
	Memberships    []GovernanceImpactAssign `json:"memberships"`
	ProviderRows   []GovernanceImpactCap    `json:"provider_rows"`
}

// GovernanceImpactRow is a unit or stored policy a boundary narrows.
type GovernanceImpactRow struct {
	ID      uuid.UUID                `json:"id"`
	Name    string                   `json:"name"`
	Changes []GovernanceImpactChange `json:"changes"`
}

// GovernanceImpactAssign is an assignment a boundary narrows.
type GovernanceImpactAssign struct {
	ID          uuid.UUID                `json:"id"`
	SubjectType string                   `json:"subject_type"`
	Subject     string                   `json:"subject"`
	UnitID      uuid.UUID                `json:"unit_id"`
	UnitName    string                   `json:"unit_name"`
	Changes     []GovernanceImpactChange `json:"changes"`
}

// GovernanceImpactCap is an Azure DevOps provider row a drafted capability cap narrows.
type GovernanceImpactCap struct {
	ID                    string   `json:"id"`
	CeilingRemoved        []string `json:"ceiling_removed"`
	DefaultProfileRemoved []string `json:"default_profile_removed,omitempty"`
}

// GovernanceChangeApprovals is where a pending change stands when more than one approval is required. It is
// absent when one person decides. A list row carries only Required, Have, RequireSecurity and Viewer.
type GovernanceChangeApprovals struct {
	Required        int  `json:"required"`
	RequireSecurity bool `json:"require_security"`
	Have            int  `json:"have"`
	// Entries are the approvals given so far, a dropped one included. The slices that follow use omitzero, so an
	// empty list the server sent stays in the output and an absent one stays absent.
	Entries []GovernanceApprovalEntry `json:"entries,omitzero"`
	// Eligible are the people who can still approve; Uncountable says what the count leaves out.
	Eligible    []GovernanceApprovalEligible    `json:"eligible,omitzero"`
	Uncountable []GovernanceApprovalUncountable `json:"uncountable,omitzero"`
	// Reach is ok, cant_reach or cant_confirm. CantReachReason (count or security) is set only with cant_reach.
	Reach           string `json:"reach,omitempty"`
	CantReachReason string `json:"cant_reach_reason,omitempty"`
	// The four fields below are on the read of one change only, and a zero or false is a real answer there,
	// so they are pointers: a value the server sent stays in the output, and a list row's absence stays absent.
	StillNeeded          *int                      `json:"still_needed,omitempty"`
	RemainingEligible    *int                      `json:"remaining_eligible,omitempty"`
	ProposerActive       *bool                     `json:"proposer_active,omitempty"`
	RejectReasonRequired *bool                     `json:"reject_reason_required,omitempty"`
	Viewer               *GovernanceApprovalViewer `json:"viewer,omitempty"`
}

// GovernanceApprovalEntry is one approval. Counts is false for a dropped one, and Dropped says why:
// deactivated, token_revoked, session_revoked, role_removed, grant_removed or cant_confirm.
type GovernanceApprovalEntry struct {
	Approver   string     `json:"approver"`
	Tier       string     `json:"tier"`
	UnitName   string     `json:"unit_name,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	ApprovedAt time.Time  `json:"approved_at"`
	Counts     bool       `json:"counts"`
	Dropped    string     `json:"dropped,omitempty"`
	DroppedAt  *time.Time `json:"dropped_at,omitempty"`
}

// GovernanceApprovalEligible is one person who can still approve. Email is the people directory's, when it
// has one.
type GovernanceApprovalEligible struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	Tier      string `json:"tier"`
}

// GovernanceApprovalUncountable is something the people count leaves out: Why is unconfirmed_people (Count of
// them) or too_many_people.
type GovernanceApprovalUncountable struct {
	Why   string `json:"why"`
	Count int    `json:"count,omitempty"`
}

// GovernanceApprovalViewer is the caller's own standing: State is can_approve, approved, proposer,
// cannot_be_last or not_eligible. It is absent for the admin token.
type GovernanceApprovalViewer struct {
	State      string     `json:"state"`
	Last       bool       `json:"last,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// ApprovalCounts is how many approvals each kind of governance change needs. Minimum is the deploy-time
// floor (0 when it is not set) and Highest the largest count in force.
type ApprovalCounts struct {
	Minimum int                  `json:"minimum"`
	Highest int                  `json:"highest"`
	Default ApprovalCountsRule   `json:"default"`
	Kinds   []ApprovalCountsKind `json:"kinds"`
}

// ApprovalCountsRule is a count and whether a security admin must be among the approvers.
type ApprovalCountsRule struct {
	Required        int  `json:"required"`
	RequireSecurity bool `json:"require_security"`
}

// ApprovalCountsKind is one kind's row. Own is false when the kind follows the default. Approvers is
// super_admin, security or security_or_unit_admin.
type ApprovalCountsKind struct {
	Kind            string             `json:"kind"`
	Own             bool               `json:"own"`
	Required        int                `json:"required"`
	RequireSecurity bool               `json:"require_security"`
	Approvers       string             `json:"approvers"`
	Pool            ApprovalCountsPool `json:"pool"`
}

// ApprovalCountsPool is who can approve a kind now. Status is ok, exact, short or cant_confirm; Why
// (unconfirmed_people or too_many_people) is set with cant_confirm.
type ApprovalCountsPool struct {
	Status   string `json:"status"`
	Count    int    `json:"count"`
	UnitName string `json:"unit_name,omitempty"`
	Why      string `json:"why,omitempty"`
}

// ApprovalCountsWrite is the body of PutApprovalCounts. It replaces every row: a kind it omits follows
// the default.
type ApprovalCountsWrite struct {
	Default ApprovalCountsRule        `json:"default"`
	Kinds   []ApprovalCountsWriteKind `json:"kinds"`
}

// ApprovalCountsWriteKind is one kind's own count in an ApprovalCountsWrite.
type ApprovalCountsWriteKind struct {
	Kind            string `json:"kind"`
	Required        int    `json:"required"`
	RequireSecurity bool   `json:"require_security"`
}

// GetGovernanceChangeImpact returns who and what approving a pending change would reach. The server answers
// 404 when the caller cannot see the change and 409 when it is no longer pending. GET
// /api/v1/governance/changes/{id}/impact.
func (c *Client) GetGovernanceChangeImpact(ctx context.Context, id uuid.UUID) (GovernanceChangeImpact, error) {
	var out GovernanceChangeImpact
	err := c.do(ctx, http.MethodGet, "/api/v1/governance/changes/"+id.String()+"/impact", nil, &out)
	return out, err
}

// ApproveGovernanceChangeWithReason is ApproveGovernanceChange with the approver's reason ("" omits it). An
// approval that does not yet apply the change comes back with the change still pending. POST
// /api/v1/governance/changes/{id}/approve.
func (c *Client) ApproveGovernanceChangeWithReason(ctx context.Context, id uuid.UUID, reason string) (GovernanceChange, error) {
	var out GovernanceChange
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason}
	err := c.do(ctx, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/approve", body, &out)
	return out, err
}

// TakeBackGovernanceApproval withdraws the caller's own counting approval of a pending change. The server
// answers 404 when the caller has none. DELETE /api/v1/governance/changes/{id}/approval.
func (c *Client) TakeBackGovernanceApproval(ctx context.Context, id uuid.UUID) (GovernanceChange, error) {
	var out GovernanceChange
	err := c.do(ctx, http.MethodDelete, "/api/v1/governance/changes/"+id.String()+"/approval", nil, &out)
	return out, err
}

// GetApprovalCounts returns how many approvals each kind of governance change needs. GET
// /api/v1/governance/approval-counts.
func (c *Client) GetApprovalCounts(ctx context.Context) (ApprovalCounts, error) {
	var out ApprovalCounts
	err := c.do(ctx, http.MethodGet, "/api/v1/governance/approval-counts", nil, &out)
	return out, err
}

// PutApprovalCounts replaces the approval counts. Every caller but the admin token has the edit held for a
// second approver, which comes back as *PendingApprovalError. The write replaces every row, so no kinds is
// sent as an empty list, never as null. PUT /api/v1/governance/approval-counts.
func (c *Client) PutApprovalCounts(ctx context.Context, w ApprovalCountsWrite) (ApprovalCounts, error) {
	if w.Kinds == nil {
		w.Kinds = []ApprovalCountsWriteKind{}
	}
	var out ApprovalCounts
	err := c.do(ctx, http.MethodPut, "/api/v1/governance/approval-counts", w, &out)
	return out, err
}
