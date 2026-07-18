// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package composer turns a natural-language task description into a PROPOSED
// Wardyn run setup — the same {run, inline_policy} shape the New Run wizard emits —
// that a human reviews and approves before launch.
//
// Trust model (load-bearing): the composer processes UNTRUSTED input (the
// operator's prompt plus uploaded attachment text and source URLs that may
// themselves contain prompt-injection). Three invariants make that safe:
//
//  1. ADVISORY ONLY. A Proposal is never launched automatically; the human
//     approves it through the existing create-run path.
//  2. RISK IS GRADED BY Wardyn, DETERMINISTICALLY (see risk.go). The grade is
//     computed from the proposed spec fields — never read from anything the LLM
//     says about its own output — so a prompt-injected attachment cannot talk
//     the grader into "low risk".
//  3. CLAMPED TO OPERATOR POLICY (see clamp.go). The proposal is tightened to
//     the operator's ceiling before it is ever returned, so the composer can
//     never propose beyond what an operator allows.
//
// The control plane NEVER fetches the source URLs: they are passed to the
// analyzer as hints only, adding no new control-plane egress / SSRF surface.
package composer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Input size caps. The analyzer input is untrusted and may be operator-pasted or
// uploaded; bound it so a single compose call cannot exhaust memory or blow past
// the backend's context window.
const (
	MaxPromptBytes      = 16 * 1024   // 16 KiB of task description
	MaxAttachmentBytes  = 256 * 1024  // 256 KiB per attachment
	MaxTotalInputBytes  = 1024 * 1024 // 1 MiB across prompt + all attachments
	MaxAttachmentsCount = 32          // at most 32 attachments
	MaxSources          = 32          // at most 32 source-URL hints
	MaxSourceURLBytes   = 2048        // a single URL hint

	// Interactive clarify-step caps (the Q&A transcript the UI carries between
	// rounds, stateless on the server). They bound the extra LLM cost.
	MaxTranscriptBytes  = 64 * 1024 // total Q + A text in the transcript
	MaxTranscriptQAs    = 24        // at most this many answered questions
	MaxClarifyRounds    = 3         // endpoint forces a proposal after this many rounds
	MaxClarifyQuestions = 6         // at most this many questions per clarify round

	// MaxAuditFieldBytes bounds any single free-text field the compose pipeline
	// writes into an advisory audit event's Data (JSONB): the prompt, the clarify
	// transcript, the serialized proposal. Unlike the input caps above (which
	// reject an oversized REQUEST outright, before any backend call), the audit
	// trail must never fail the operation it is recording — so an over-budget
	// field is TRUNCATED with an explicit marker (CapAuditText) instead.
	MaxAuditFieldBytes = 2 * 1024 // 2 KiB per field
)

// auditTruncatedMarker is appended by CapAuditText when it truncates, so a
// reader of the audit trail can never mistake a cut string for the complete
// value. The audit path has NO free-text masking — this is truncation only.
const auditTruncatedMarker = "...[truncated]"

// ErrInputTooLarge is returned by ValidateRequest when caps are exceeded.
var ErrInputTooLarge = errors.New("composer: input exceeds size limits")

// CapAuditText bounds s to MaxAuditFieldBytes for embedding in a compose audit
// event, appending auditTruncatedMarker when it truncates. One place for the
// cap + marker so every free-text audit field (prompt, transcript, the
// serialized proposal) is capped identically.
//
// a plain byte-slice cut can split a multi-byte UTF-8 rune at the
// boundary; encoding/json replaces the resulting partial rune with U+FFFD
// rather than erroring, so the marshaled audit event stays valid JSON — a
// cosmetic ceiling only (a rune-boundary-aware cut would be a few more lines,
// not worth it for a truncated audit field).
func CapAuditText(s string) string {
	if len(s) <= MaxAuditFieldBytes {
		return s
	}
	cut := MaxAuditFieldBytes - len(auditTruncatedMarker)
	if cut < 0 {
		cut = 0
	}
	return s[:cut] + auditTruncatedMarker
}

// Attachment is uploaded text the operator attached as context. Content is the
// raw text (the UI reads files client-side); the control plane never fetches it.
type Attachment struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// WorkspaceKind selects the run's working-directory source. Exactly one is
// REQUIRED on a compose request, and it is OPERATOR-set (trusted) — never chosen
// by the LLM. This is the ONLY way a host directory enters a composed run: the
// proposal schema has no mount field, and the clamp drops any mount the model
// somehow emits, so a workspace mount can come only from this operator choice.
type WorkspaceKind string

const (
	// WorkspaceLocal bind-mounts a host directory at the agent's working dir
	// (/home/agent/work) so the agent operates in and sees that directory.
	WorkspaceLocal WorkspaceKind = "local"
	// WorkspaceGit clones a git repo into the sandbox.
	WorkspaceGit WorkspaceKind = "git"
	// WorkspaceEphemeral runs in an empty sandbox working dir that is wiped on
	// teardown (no persistence).
	WorkspaceEphemeral WorkspaceKind = "ephemeral"
)

// Workspace is the operator-chosen working directory for a composed run.
type Workspace struct {
	Kind      WorkspaceKind `json:"kind"`
	Path      string        `json:"path,omitempty"`       // local: absolute host directory
	ReadWrite bool          `json:"read_write,omitempty"` // local: false => read-only (safe default)
	Repo      string        `json:"repo,omitempty"`       // git: repo slug or clone URL
}

// ComposeRequest is the analyzer input: a REQUIRED operator-chosen workspace plus
// a task description and optional uploaded attachment text and source-URL HINTS
// (never fetched).
type ComposeRequest struct {
	Prompt      string       `json:"prompt"`
	Workspace   Workspace    `json:"workspace"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Sources     []string     `json:"sources,omitempty"`

	// WorkspaceGitHubRepos / WorkspaceOtherRemotes are filled by the SERVER (not
	// the client) for a local workspace: the git remotes Wardyn detected in the
	// directory. They ground the analyzer so it doesn't guess a repo; Wardyn also
	// enforces them deterministically on the proposal regardless.
	WorkspaceGitHubRepos  []string `json:"-"`
	WorkspaceOtherRemotes []string `json:"-"`

	// Transcript carries the prior clarify Q&A (the UI accumulates and resends it
	// each round — the server holds no compose session). Round is the 0-based
	// clarify round. Both feed BuildUserMessage so clarify AND the final propose
	// see the operator's answers.
	Transcript []QA `json:"transcript,omitempty"`
	Round      int  `json:"round,omitempty"`

	// ClarifyAlways is set by the server for a clarify call when the operator
	// chose "Always ask" (round 0 only); it nudges the analyzer to ask at least
	// one question even if the task looks clear. Never set on the propose call.
	ClarifyAlways bool `json:"-"`

	// SessionID is the CLIENT-owned stable id for one compose conversation (one
	// per describe-mode entry, resent unchanged on every round — mirrors how
	// Transcript is carried). The server holds no session state (Decision 1: the
	// stateless round-trip protocol is kept; persistence is via enriched audit
	// events correlated on this id, not a server-side session store). Empty is
	// valid (the endpoint mints a fallback correlation id); non-empty MUST be a
	// UUID (ValidateRequest) so it can't smuggle arbitrary audit-log content.
	SessionID string `json:"session_id,omitempty"`
}

// QA is one answered clarifying question carried in the compose transcript.
type QA struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Question is one clarifying question the analyzer asks before proposing. Options
// empty ⇒ a free-text answer; non-empty ⇒ choose from Options (the UI ALSO always
// offers a free-text "Other"); Multi ⇒ choose any vs choose one.
type Question struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Why      string   `json:"why"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multi"`

	// OPTIONAL plain-language enrichment for a mixed audience (novice↔expert). The
	// analyzer fills these ONLY when confident (empty otherwise); the UI shows them
	// in an info popover so most "what is this / is it safe?" questions need no
	// follow-up. Carries NO authority: inert display text, never risk-graded or
	// clamped — same trust posture as Why/Notes.
	Help           string   `json:"help,omitempty"`           // one-sentence plain definition
	Risk           string   `json:"risk,omitempty"`           // what the riskier answer costs
	Examples       []string `json:"examples,omitempty"`       // what each option concretely enables
	Misconceptions []string `json:"misconceptions,omitempty"` // correct a likely wrong assumption
}

// Clarification is the analyzer's interview-step output. Ready=true means it has
// enough to propose; otherwise Questions holds what it needs answered. It carries
// NO authority (only questions/assumptions) so it is never risk-graded or clamped —
// all enforcement stays in the Propose→ground→clamp→grade pipeline.
type Clarification struct {
	Ready       bool       `json:"ready"`
	Questions   []Question `json:"questions"`
	Assumptions []string   `json:"assumptions"`
	Notes       string     `json:"notes"`
}

// RunInput is the scalar create-run fields of a proposal — the same fields the
// wizard's buildSpec puts on `run`. It is mapped onto the create-run request by
// the API layer (the composer package must not import internal/api).
type RunInput struct {
	Agent            string `json:"agent"`
	Repo             string `json:"repo"`
	Task             string `json:"task"`
	ConfinementClass string `json:"confinement_class,omitempty"`
	Interactive      bool   `json:"interactive,omitempty"`
	DevcontainerRepo string `json:"devcontainer_repo,omitempty"`
}

// Proposal is the analyzer's advisory output BEFORE risk grading and clamping.
// The API endpoint clamps InlinePolicy to operator policy and attaches the
// deterministic risk assessment before returning it to the human.
type Proposal struct {
	Run          RunInput            `json:"run"`
	InlinePolicy types.RunPolicySpec `json:"inline_policy"`
	Summary      string              `json:"summary"`
	Warnings     []string            `json:"warnings,omitempty"`
}

// Composer proposes a run setup from a natural-language request. Implementations
// (Claude API, Claude CLI, a local model, or a deterministic fake) all satisfy
// this one interface; the endpoint, grader, and clamp are backend-agnostic.
type Composer interface {
	Propose(ctx context.Context, req ComposeRequest) (Proposal, error)
}

// Clarifier is the OPTIONAL interview capability: a backend that implements it can
// ask clarifying questions before Propose. The registry treats a backend that does
// NOT implement it as "always ready" (straight to Propose), so a BYO backend
// without clarify still works.
type Clarifier interface {
	Clarify(ctx context.Context, req ComposeRequest) (Clarification, error)
}

// ComposeEventType tags a ComposeEvent emitted as the compose pipeline runs.
type ComposeEventType string

const (
	// EvStage: a pipeline stage is starting (Stage holds the internal key). The UI
	// maps the key to human copy (server never sends UX strings).
	EvStage ComposeEventType = "stage"
	// EvResult: terminal — the clarify/proposal payload (Result). For a non-stream
	// caller this is the single JSON body; for SSE it is the last frame.
	EvResult ComposeEventType = "result"
	// EvError: terminal — the pipeline failed AFTER the response began streaming, so
	// it can no longer be an HTTP status. Pre-flush validation stays a real 4xx.
	EvError ComposeEventType = "error"
)

// ComposeEvent is one item in the compose pipeline's progress stream. The API
// handler emits these through a transport: a single JSON EvResult for CLI/tests,
// or one SSE frame per event for the UI. Result is `any` so this package need not
// import internal/api (the handler fills it with its own response type).
type ComposeEvent struct {
	Type   ComposeEventType `json:"type"`
	Stage  string           `json:"stage,omitempty"`
	Result any              `json:"result,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// ValidateRequest enforces the input size caps. It is called by the endpoint
// BEFORE any backend is invoked so oversized/abusive input is rejected cheaply
// and never reaches the analyzer.
func ValidateRequest(req ComposeRequest) error {
	if strings.TrimSpace(req.Prompt) == "" && len(req.Attachments) == 0 {
		return errors.New("composer: a prompt or at least one attachment is required")
	}
	// A workspace is REQUIRED — the operator must choose where the run works.
	// (Path/repo are further validated against the mount/repo rules in the API
	// layer; here we only enforce presence + shape.)
	switch req.Workspace.Kind {
	case WorkspaceLocal:
		if strings.TrimSpace(req.Workspace.Path) == "" {
			return errors.New("composer: a local workspace requires a directory path")
		}
	case WorkspaceGit:
		if strings.TrimSpace(req.Workspace.Repo) == "" {
			return errors.New("composer: a git workspace requires a repo")
		}
	case WorkspaceEphemeral:
		// no fields required
	case "":
		return errors.New("composer: a workspace is required (local | git | ephemeral)")
	default:
		return fmt.Errorf("composer: unknown workspace kind %q", req.Workspace.Kind)
	}
	if len(req.Prompt) > MaxPromptBytes {
		return fmt.Errorf("%w: prompt %d > %d bytes", ErrInputTooLarge, len(req.Prompt), MaxPromptBytes)
	}
	if len(req.Attachments) > MaxAttachmentsCount {
		return fmt.Errorf("%w: %d attachments > %d", ErrInputTooLarge, len(req.Attachments), MaxAttachmentsCount)
	}
	if len(req.Sources) > MaxSources {
		return fmt.Errorf("%w: %d sources > %d", ErrInputTooLarge, len(req.Sources), MaxSources)
	}
	total := len(req.Prompt)
	for _, a := range req.Attachments {
		if len(a.Content) > MaxAttachmentBytes {
			return fmt.Errorf("%w: attachment %q %d > %d bytes", ErrInputTooLarge, a.Name, len(a.Content), MaxAttachmentBytes)
		}
		total += len(a.Content)
	}
	if total > MaxTotalInputBytes {
		return fmt.Errorf("%w: total input %d > %d bytes", ErrInputTooLarge, total, MaxTotalInputBytes)
	}
	for _, s := range req.Sources {
		if len(s) > MaxSourceURLBytes {
			return fmt.Errorf("%w: a source URL exceeds %d bytes", ErrInputTooLarge, MaxSourceURLBytes)
		}
	}
	if len(req.Transcript) > MaxTranscriptQAs {
		return fmt.Errorf("%w: %d transcript entries > %d", ErrInputTooLarge, len(req.Transcript), MaxTranscriptQAs)
	}
	tb := 0
	for _, qa := range req.Transcript {
		tb += len(qa.Question) + len(qa.Answer)
	}
	if tb > MaxTranscriptBytes {
		return fmt.Errorf("%w: transcript %d > %d bytes", ErrInputTooLarge, tb, MaxTranscriptBytes)
	}
	// SessionID is client-minted (crypto.randomUUID() in the UI) and carries no
	// authority — it is a correlation id only — but it lands in the audit trail
	// verbatim, so it must be a UUID rather than an arbitrary attacker string.
	// Empty is fine (the endpoint mints a fallback).
	if req.SessionID != "" {
		if _, err := uuid.Parse(req.SessionID); err != nil {
			return fmt.Errorf("composer: session_id must be a UUID: %w", err)
		}
	}
	return nil
}

// FakeComposer is a deterministic Composer for tests: it returns a preset
// Proposal (or error) regardless of input, and records the last request it saw
// so tests can assert the endpoint passed input through unchanged. It performs
// NO network I/O.
type FakeComposer struct {
	Result Proposal
	Err    error
	Last   ComposeRequest

	// ClarifyEnabled makes the fake implement the interview step: it asks
	// ClarifyResult once (on the first round, when Transcript is empty) and is
	// "ready" thereafter. With ClarifyEnabled=false it is always ready (one-shot,
	// today's behavior).
	ClarifyEnabled bool
	ClarifyResult  Clarification
	ClarifyErr     error
}

// Propose records the request and returns the preset result.
func (f *FakeComposer) Propose(_ context.Context, req ComposeRequest) (Proposal, error) {
	f.Last = req
	if f.Err != nil {
		return Proposal{}, f.Err
	}
	return f.Result, nil
}

// Clarify records the request and returns the preset interview result. It asks on
// the first round only (empty transcript), then reports ready so a test loop
// converges deterministically.
func (f *FakeComposer) Clarify(_ context.Context, req ComposeRequest) (Clarification, error) {
	f.Last = req
	if f.ClarifyErr != nil {
		return Clarification{}, f.ClarifyErr
	}
	if f.ClarifyEnabled && len(req.Transcript) == 0 && (len(f.ClarifyResult.Questions) > 0 || !f.ClarifyResult.Ready) {
		return f.ClarifyResult, nil
	}
	return Clarification{Ready: true}, nil
}
