// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalService is the narrow approval FSM surface the API depends on. It is
// satisfied by package-level wrappers over internal/approval (see wardynd wiring),
// keeping the API decoupled from concrete storage.
type ApprovalService interface {
	Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error)
	// Decide transitions an approval to decision.State (APPROVED or DENIED —
	// the caller picks; there is no separate approve bool, since
	// types.ApprovalDecision.State already says which). decidedByType is kept
	// as its own parameter rather than folded into ApprovalDecision: it is
	// audit attribution (who/what decided), not a property of the decision
	// itself.
	Decide(ctx context.Context, id uuid.UUID, decidedByType types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error)
	Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error)
	// CancelForRun moves every still-PENDING approval of a run that has just
	// reached a terminal state to CANCELLED, returning how many moved per kind. It is
	// part of the terminal cascade, beside identity/broker revocation: an
	// approval whose run has ended is a control that cannot function, and a row
	// left PENDING renders live Approve/Deny buttons in the console. reason names
	// the transition ("run_killed", "run_completed", ...). Idempotent by
	// construction — a second call finds nothing PENDING and emits nothing.
	CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error)
	// ExpireOne moves one still-PENDING approval to EXPIRED (a no-op once decided):
	// wardyn-toolgate's give-up signal (#811). actor is the withdrawing agent.
	ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error
	// CountForRun returns how many approvals a run has raised, in ANY state —
	// the per-run cap handleInternalRequestApproval enforces. A sandbox chooses
	// the hosts it asks about, so without that cap the number of rows one run can
	// create is bounded by nothing.
	CountForRun(ctx context.Context, runID uuid.UUID) (int, error)
}

// MintBroker is the credential-mint surface the API depends on (internal/broker).
type MintBroker interface {
	MintForGrant(ctx context.Context, caller *identity.Claims, grantID uuid.UUID) (broker.Minted, error)
	RevokeRun(ctx context.Context, runID uuid.UUID) error
}

// RefRulesetVerifier answers whether GitHub itself confines the App's writes on
// a repo to the run branch namespace. Satisfied by broker.GitHubMinter. It is a
// SEPARATE, optional Config field rather than a method on MintBroker because it
// is the only thing in the API layer that calls an external network service, and
// the /setup/status checklist must degrade to "unknown" — never "fail" — when it
// is absent or errors.
type RefRulesetVerifier interface {
	VerifyRefRuleset(ctx context.Context, repo string) (confined bool, detail string, err error)
}

// ImageBuilder builds a per-run sandbox image from a devcontainer repo. It is
// target-agnostic (the parity rule): the concrete envbuilder implementation is
// wired in wardynd behind the "docker" build tag, so the control-plane default
// build carries zero target-specific code. Nil disables devcontainer builds.
type ImageBuilder interface {
	// BuildDevcontainer builds the devcontainer for repoURL@ref and returns the
	// local image reference to run. outputTag is the deterministic per-run tag
	// the result is committed under. logSink, when non-nil, receives build
	// output lines as they happen (e.g. the wizard Build step's in-memory log);
	// nil preserves the implementation's own default (wardynd's slog).
	BuildDevcontainer(ctx context.Context, repoURL, ref, outputTag string, logSink io.Writer) (imageRef string, err error)
	// BuildFromDevcontainerFiles builds an image from IN-MEMORY generated
	// devcontainer files (relative path -> content, e.g.
	// ".devcontainer/devcontainer.json") rather than a repo checkout, returning
	// the local image reference. It drives the SAME hardened envbuilder path as
	// BuildDevcontainer. Used for an onboarded workspace WITHOUT a wired
	// devcontainer, where internal/workspacescan generates a minimal one from the
	// detected profile. outputTag is the deterministic profile-hash-keyed tag
	// the result is committed under. logSink: see BuildDevcontainer.
	BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string, logSink io.Writer) (imageRef string, err error)
	// FinalizeBase wraps an arbitrary USER-supplied base image (Bring Your Own
	// Image) with Wardyn's runner tools + a cleared ENTRYPOINT, returning the
	// runnable local image reference. No untrusted build, no registry push — just
	// the trusted FROM+COPY finalize stage; the base is pulled only if absent, so
	// a host-pre-pulled private image works. outputTag is the per-run tag.
	// logSink: see BuildDevcontainer.
	FinalizeBase(ctx context.Context, baseRef, outputTag string, logSink io.Writer) (imageRef string, err error)
}
