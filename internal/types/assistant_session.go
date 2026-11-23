// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// The assistant session's states. A session holds in fetching before its run exists, when its files are
// still being read, and every other live session holds a run.
const (
	AssistantSessionFetching = "fetching"
	AssistantSessionStarting = "starting"
	AssistantSessionReady    = "ready"
	AssistantSessionEnded    = "ended"
)

// The reasons a session ends.
const (
	AssistantEndPerson        = "person"
	AssistantEndIdle          = "idle"
	AssistantEndMaxLifetime   = "max_lifetime"
	AssistantEndRunEnded      = "run_ended"
	AssistantEndProviderError = "provider_error"
	AssistantEndReplaced      = "replaced"
	AssistantEndRoleDenied    = "role_denied"
	AssistantEndTurnedOff     = "turned_off"
	AssistantEndErased        = "erased"
)

// AssistantErasedPrincipal stands in for the person of an erased session, so the row still parses and no
// call can ever act as that person.
const AssistantErasedPrincipal = "erased"

// What a session works on. A compose session drafts a run, a unit change or a template; a review session
// explains a change that waits for approval.
const (
	AssistantTargetRun      = "run"
	AssistantTargetUnit     = "unit"
	AssistantTargetTemplate = "template"
	AssistantTargetChange   = "change"
)

// AssistantTarget is what a session or a preview is about. ID is empty for a new draft.
type AssistantTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// ValidFor reports whether the pair is one the assistant serves: compose with a run, unit or template,
// review with a change.
func (t AssistantTarget) ValidFor(p AssistantPurpose) bool {
	switch p {
	case AssistantCompose:
		return t.Kind == AssistantTargetRun || t.Kind == AssistantTargetUnit || t.Kind == AssistantTargetTemplate
	case AssistantReview:
		return t.Kind == AssistantTargetChange
	}
	return false
}

// AssistantBinding is the one destination a preview and its session are bound to: the provider, that
// provider row's server-owned UID, and the model. A change in any of the three is a different binding.
type AssistantBinding struct {
	ProviderID  string `json:"provider_id"`
	ProviderUID string `json:"provider_uid"`
	Model       string `json:"model"`
}

// AssistantSection is one block of the records sent with the first message. Untrusted marks text the model
// is told to read as data.
type AssistantSection struct {
	Label     string `json:"label"`
	Text      string `json:"text"`
	Untrusted bool   `json:"untrusted"`
}

// AssistantPreview is the answer of POST /assistant/preview: what the first message will carry, and where
// it goes. Letters maps each letter to the name it stands for, for the browser only.
type AssistantPreview struct {
	Provider  string             `json:"provider"`
	Model     string             `json:"model"`
	Sections  []AssistantSection `json:"sections"`
	Digest    string             `json:"digest"`
	Letters   map[string]string  `json:"letters"`
	NeverSent []string           `json:"never_sent"`
	Tools     []string           `json:"tools"`
}

// AssistantSession is one person's conversation with the assistant, with the snapshot of who they were when
// it began and the serving binding.
type AssistantSession struct {
	ID    uuid.UUID  `json:"id"`
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// Principal, Email, Role, UserType, Groups and GroupsTruncated are what the creating request published
	// about the person; every tool call acts as them, capped at the security tier.
	Principal       string   `json:"-"`
	Email           string   `json:"-"`
	Role            string   `json:"-"`
	UserType        string   `json:"-"`
	Groups          []string `json:"-"`
	GroupsTruncated bool     `json:"-"`

	Purpose     AssistantPurpose `json:"purpose"`
	TargetKind  string           `json:"target_kind"`
	TargetKey   string           `json:"target_key"`
	WorkspaceID *uuid.UUID       `json:"workspace_id,omitempty"`
	Repo        *string          `json:"repo,omitempty"`

	Binding       AssistantBinding  `json:"-"`
	State         string            `json:"state"`
	Draft         []byte            `json:"-"`
	PersonLetters map[string]string `json:"-"`
	PreviewDigest string            `json:"preview_digest"`

	CreatedAt  time.Time  `json:"created_at"`
	LastTurnAt *time.Time `json:"last_turn_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	EndReason  string     `json:"end_reason,omitempty"`
}

// Live reports whether the session has not ended.
func (s AssistantSession) Live() bool { return s.State != AssistantSessionEnded }

// The kinds of a session event. The run writes the first group; the server alone writes the second, and
// the internal route refuses it from the run.
const (
	AssistantEventState      = "state"
	AssistantEventText       = "text"
	AssistantEventToolCall   = "tool_call"
	AssistantEventToolResult = "tool_result"
	AssistantEventNotice     = "notice"
	AssistantEventTurnEnd    = "turn_end"
	AssistantEventError      = "error"

	AssistantEventUserMessage = "user_message"
	AssistantEventPersonEdit  = "person_edit"
	AssistantEventProposal    = "proposal"
	AssistantEventToolRecord  = "tool_record"
	AssistantEventBuildFiles  = "build_files"
	AssistantEventSuggestion  = "suggestion"
)

// AssistantRunEventKind reports whether the run may post an event of kind k.
func AssistantRunEventKind(k string) bool {
	switch k {
	case AssistantEventState, AssistantEventText, AssistantEventToolCall, AssistantEventToolResult,
		AssistantEventNotice, AssistantEventTurnEnd, AssistantEventError:
		return true
	}
	return false
}

// AssistantEvent is one row of a session's transcript. Seq counts from 1 within the session.
type AssistantEvent struct {
	SessionID uuid.UUID `json:"session_id"`
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// The kinds of a turn the relay is given.
const (
	AssistantTurnUser  = "user"
	AssistantTurnReset = "reset"
	AssistantTurnEnd   = "end"
)

// AssistantTurn is what the relay reads next: a person's message, a reset to a new session, or the end.
// The records and the system prompt ride on the first turn and on a reset.
type AssistantTurn struct {
	Seq       int64              `json:"seq"`
	Kind      string             `json:"kind"`
	SessionID uuid.UUID          `json:"session_id"`
	Model     string             `json:"model,omitempty"`
	Text      string             `json:"text,omitempty"`
	System    string             `json:"system,omitempty"`
	Records   []AssistantSection `json:"records,omitempty"`
	Tools     []string           `json:"tools,omitempty"`
}
