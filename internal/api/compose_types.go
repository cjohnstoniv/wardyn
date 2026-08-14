// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// composeRequest is the POST /api/v1/runs/compose body: a natural-language task
// description plus optional uploaded attachment TEXT and source-URL HINTS, and
// an optional backend name (empty = the registry default). The control plane NEVER
// fetches the sources — they are passed to the analyzer as hints only, adding no
// new egress/SSRF surface.
type composeRequest struct {
	Prompt string `json:"prompt"`
	// Workspace is the legacy single operator-chosen workspace; Workspaces is the
	// multi-select form (onboarded dirs + repos). When Workspaces is set it wins,
	// and Workspace is normalized to its first entry (the PRIMARY) so the analyzer /
	// git-detect / grounding operate on it; every entry is mounted/cloned.
	Workspace   composer.Workspace    `json:"workspace"`
	Workspaces  []composer.Workspace  `json:"workspaces,omitempty"`
	Attachments []composer.Attachment `json:"attachments,omitempty"`
	Sources     []string              `json:"sources,omitempty"`
	Backend     string                `json:"backend,omitempty"`

	// Interactive clarify-step fields. Mode is "auto" (default; the model decides
	// whether to ask), "always" (force at least round 0), or "skip" (one-shot —
	// straight to a proposal). Transcript carries the prior Q&A (the UI accumulates
	// and resends it each round; the server holds no session). Round is the 0-based
	// clarify round.
	Mode       string        `json:"mode,omitempty"`
	Transcript []composer.QA `json:"transcript,omitempty"`
	Round      int           `json:"round,omitempty"`

	// Interactive is the operator's UPFRONT run-mode choice (true = interactive:
	// the sandbox comes up idle for `wardyn attach`; false = background: the agent
	// runs the task unattended). This is the OPERATOR's decision, not the model's —
	// it is enforced deterministically on the proposal below, overriding any guess.
	Interactive bool `json:"interactive,omitempty"`

	// ConfinementFloor is the operator's Getting Started DEFAULT tier, sent per-run
	// as a raise-only MINIMUM. The server raises the policy confinement floor to it
	// for this compose, but only up to the strongest class THIS host can enforce
	// (capped server-side — the dialog sends the raw pick with no health probe), so
	// a stronger-than-available floor degrades instead of 422ing at launch. Weaker
	// than the proposal ⇒ no-op; empty ⇒ the policy minimum stands.
	ConfinementFloor types.ConfinementClass `json:"confinement_floor,omitempty"`

	// SessionID is the client-owned stable id for this compose SESSION (mirrors
	// composer.ComposeRequest.SessionID — see there for why: Decision 1 keeps the
	// server stateless, so persistence is via this id correlating the audit trail
	// across rounds, not a session store). Validated as a UUID by ValidateRequest.
	SessionID string `json:"session_id,omitempty"`

	// IntegrationID, when set, pins the proposal's model/harness credential to
	// a SPECIFIC AI-provider Integration (see client.CreateRunRequest.
	// IntegrationID — same field, same rule: a non-AI-provider id is a 400).
	// Empty falls through resolveRunIntegration's remaining tiers (llmcred.go).
	IntegrationID string `json:"integration_id,omitempty"`

	// WorkspaceSelections carries the per-workspace requirements-contract
	// opt-ins CreateRunRequest.Workspaces does (client.WorkspaceSelection: id +
	// enabled_optional + read_only), for the onboarded workspaces named by
	// Workspace/Workspaces above. A field DISTINCT from Workspaces on purpose:
	// Workspaces is a SOURCE descriptor list ({kind,path,repo}) applyWorkspaces
	// mounts/clones, which loses the onboarded id (resolveComposeWorkspace,
	// ui/src/app/lib/api/compose.ts flattens a picked Workspace down to its bare
	// kind/path/repo) — this is that id riding alongside it, so it isn't
	// discarded. The compose pipeline itself folds it too (compose.go's own
	// applyWorkspaceRequirements call, via selectionsByWorkspaceID) so the
	// PREVIEW already reflects these opt-ins; it is ALSO echoed back verbatim
	// on the proposal (composeProposed.WorkspaceSelections) so approveLaunch can
	// forward it to POST /runs unchanged, exactly like Proposed.Run/InlinePolicy
	// already launch verbatim, where resolveWorkspaceSelections/
	// applyWorkspaceRequirements fold it again for the REAL run.
	WorkspaceSelections []client.WorkspaceSelection `json:"workspace_selections,omitempty"`
}

// composeModeSkip / composeModeAlways select the clarify behavior; "" / anything
// else is auto (the model decides).
const (
	composeModeSkip   = "skip"
	composeModeAlways = "always"
)

// clarifyResponse is the discriminated "the analyzer needs answers" response: the
// UI shows these questions, collects answers, and re-POSTs with the answers
// appended to the transcript. It carries NO proposal and NO authority.
type clarifyResponse struct {
	Kind        string              `json:"kind"` // always "questions"
	Questions   []composer.Question `json:"questions"`
	Assumptions []string            `json:"assumptions,omitempty"`
	Notes       string              `json:"notes,omitempty"`
	Round       int                 `json:"round"`
}

// composeProposed is the proposed setup in the EXACT shape the New Run wizard's
// buildSpec emits, so the UI launches it via the unchanged createRun path.
type composeProposed struct {
	Run          composer.RunInput   `json:"run"`
	InlinePolicy types.RunPolicySpec `json:"inline_policy"`
	// IntegrationID echoes composeRequest.IntegrationID (PARITY-6) so approveLaunch
	// can forward it to POST /runs unchanged. Tier 1 of resolveRunIntegration steers
	// the WHOLE model-access fold on the compose path; without carrying it, a
	// compose→launch round-trip silently re-resolved model access from a DIFFERENT
	// precedence tier (the workspace binding or the operator default), so a run
	// previewed against `int-bedrock-eu` could launch against the shared Anthropic
	// key in another account/region. NOTE (UI): new-run-dialog.tsx's approveLaunch
	// must send this alongside `workspaces` (later UI batch).
	IntegrationID string `json:"integration_id,omitempty"`
	// BedrockRef is the pinned integration's region/model override, when this
	// proposal resolved a bedrock AI-provider integration with one (nil
	// otherwise). Advisory only, like the rest of this payload: the real run
	// created from this proposal re-resolves its own bedrockRef from
	// integration_id at launch (foldRunIntegration, runs.go) — a link that
	// now actually holds, because IntegrationID above rides the proposal. This field
	// lets the review surface show which region/model that will be instead of only
	// the AllowedDomains side effect.
	BedrockRef *types.WorkspaceBedrockRef `json:"bedrock_ref,omitempty"`
	// WorkspaceSelections is composeRequest.WorkspaceSelections echoed back
	// verbatim (see its doc comment) — the vehicle that lets approveLaunch
	// forward the operator's per-workspace opt-ins to POST /runs without
	// re-deriving them from separate UI state.
	WorkspaceSelections []client.WorkspaceSelection `json:"workspace_selections,omitempty"`
}

// composeResponse is advisory output for human review: the proposed setup, Wardyn's
// DETERMINISTIC risk assessment (never the LLM's self-assessment), a summary, and
// any warnings (including every clamp Wardyn applied to fit operator policy).
type composeResponse struct {
	Kind           string              `json:"kind"` // always "proposal"
	Proposed       composeProposed     `json:"proposed"`
	RiskAssessment []composer.RiskItem `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel  `json:"overall_risk"`
	Summary        string              `json:"summary"`
	// Warnings are DETERMINISTIC policy actions (clamp/ground/workspace/confinement) —
	// what the engine actually DID to the proposal. Shown as "Tightened by policy:".
	Warnings []string `json:"warnings,omitempty"`
	// ModelNotes are the LLM's OWN advisory remarks (prop.Warnings). Kept SEPARATE from
	// Warnings so untrusted model prose is never displayed as an enforced policy action (M7).
	ModelNotes []string `json:"model_notes,omitempty"`
	// LLMAccess is the deterministic FINAL-state model-access verdict for a composed
	// LLM run (reconcileLLMAccess). Provisioned=false means the run will launch but its
	// first model call 404s — the review surfaces this as its OWN distinct destructive
	// banner (non-blocking), separate from the benign clamp notices in Warnings. Absent
	// for a non-LLM agent (nothing to verify).
	LLMAccess *composeLLMAccess `json:"llm_access,omitempty"`
	// SetupItems is the deterministic setup checklist (deriveSetupItems,
	// compose_setup.go): what this proposal needs configured vs. what actually
	// is, so the review UI can guide the operator through fixing gaps.
	SetupItems []SetupItem `json:"setup_items,omitempty"`
}

// composeLLMAccess is the structured no-model-access signal so the review UI need
// never prose-sniff a warning to tell "this run will do nothing" from "tightened by
// policy".
type composeLLMAccess struct {
	Provisioned bool   `json:"provisioned"`
	Note        string `json:"note"`
}
