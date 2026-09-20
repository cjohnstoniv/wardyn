// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Length ceilings for the create-run request's free-text fields. Every one of
// them is a trust boundary: the values land in TEXT columns, in every run row
// the console renders, in the hash-chained audit row, and — for repo — in the
// clone the sandbox performs. Generous enough that no real value is refused.
//
// title/description were already capped; repo, task and agent were bounded only
// by the 1 MiB body, so a 1 MiB "repo" reached the run row, every list payload
// and the audit trail. localPrincipalOverride's doc comment
// (runs_policy.go) claims every other caller-supplied string on this path is
// capped and control-character-checked — these were the exceptions it did not
// know about.
const (
	maxRunRepoLen  = 512
	maxRunTaskLen  = 32000
	maxRunAgentLen = 128
)

// DRAFT (M2 canon pending)
const (
	// runFieldControlCharRefusal is the 400 for a control character in a
	// create-run text field. NUL is the one that mattered: Postgres rejects it
	// outright, so a NUL in a title became a 500 from the driver instead of a
	// 400 naming the field the caller can fix.
	runFieldControlCharRefusal = "%s contains a control character, which is not allowed"
	// runFieldTooLongRefusal is the EXISTING title/description sentence,
	// spelled once so the three fields that never had a cap answer in the
	// caller's own words rather than inventing a second shape.
	runFieldTooLongRefusal = "%s is too long (%d chars, max %d)"
)

// runTextField is one create-run free-text field and the two things a trust
// boundary has to know about it: how long it may be, and whether it is prose
// (newlines and tabs are content) or a single-line identifier (they are not).
type runTextField struct {
	name  string
	value string
	max   int
	// multiline: \n and \t are legitimate content here. A task is what the human
	// typed for the agent and a description is a note — both are pasted from
	// editors and both round-trip through a TEXT column unharmed. Every other
	// field is a single-line identifier where a newline is either a paste
	// accident or an attempt to forge a second line in something that renders
	// the value.
	multiline bool
}

// validateRunTextFields is the ONE loop over every caller-supplied free-text
// field on a create-run request: a rune cap and a control-character check,
// answering the 400 shape title/description already answered. It writes its own
// error and returns false once it has responded.
//
// A method on Server, although it reads nothing from the receiver: preflight
// parity is enforced structurally over `s.<Gate>(…)` calls
// (TestPreflightMirrorsLaunchGates), so a package-level function would be a gate
// that guard cannot see — which is exactly how the gap survived. Both doors
// call this one; neither can drift.
//
// Runes, not bytes, for the reason the title cap already gives: the message says
// "chars", and a CJK or emoji value well under the limit was refused with a byte
// count the operator could not reconcile with what they typed.
func (s *Server) validateRunTextFields(w http.ResponseWriter, req createRunRequest) bool {
	for _, f := range []runTextField{
		{name: "title", value: req.Title, max: maxRunTitleLen},
		{name: "description", value: req.Description, max: maxRunDescriptionLen, multiline: true},
		{name: "repo", value: req.Repo, max: maxRunRepoLen},
		{name: "devcontainer_repo", value: req.DevcontainerRepo, max: maxRunRepoLen},
		{name: "task", value: req.Task, max: maxRunTaskLen, multiline: true},
		{name: "agent", value: req.Agent, max: maxRunAgentLen},
	} {
		if n := utf8.RuneCountInString(f.value); n > f.max {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldTooLongRefusal, f.name, n, f.max))
			return false
		}
		if !runFieldCharsAllowed(f.value, f.multiline) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldControlCharRefusal, f.name))
			return false
		}
	}
	return true
}

// runFieldCharsAllowed is controlCharFree (permissions.go — C0, DEL and the C1
// range, the same hygiene every capability-grant field gets) with newline,
// CARRIAGE RETURN and tab exempted for the two prose fields: a description
// pasted out of a Windows editor arrives CRLF, and refusing it would be a 400
// nobody could act on.
func runFieldCharsAllowed(v string, multiline bool) bool {
	if multiline {
		return !strings.ContainsFunc(v, func(r rune) bool {
			return r != '\n' && r != '\t' && r != '\r' && unicode.IsControl(r)
		})
	}
	return controlCharFree(v)
}

// recordCreateFolds emits the audit rows for the two folds handleCreateRun runs
// ABOVE the confinement floor (SPINE-2): the run's model-access binding and each
// referenced workspace's requirements contract.
//
// The folds themselves are audit-FREE by design — preflight calls the same ones
// and persists nothing — so the rows are emitted here, at the one caller that
// has a run id to bind them to. Extracted because handleCreateRun sits at the
// funlen ratchet (.golangci.yml), which is what its neighbours' own comments ask
// the next lane to do.
func (s *Server) recordCreateFolds(ctx context.Context, runID uuid.UUID,
	foldInteg types.Integration, foldKind string, reqEvents []requirementAuditEntry,
) {
	if foldKind != "" {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.creds",
			runID.String(), "success", mustJSON(map[string]any{"integration_ref": foldInteg.ID, "type": foldKind})))
	}
	for _, ev := range reqEvents {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", ev.action, ev.target, "success", mustJSON(ev.data)))
	}
}

// abortHalfBuiltRun is the compensator every post-CreateRun early return in
// handleCreateRun goes through: it fails the persisted run PENDING->FAILED with
// an operator-facing hint and runs the revoke cascade, so a 500 answered after
// the run row exists cannot leave a ghost PENDING run holding a live run token
// until the undispatched sweep reaps it.
//
// context.WithoutCancel is applied INSIDE the returned closure, not at the
// client-disconnect detach further down handleCreateRun: the compensator runs on
// the REQUEST's context, and a 500 is very often answered to a client that has
// already gone — its cancelled context cannot write the FAILED state the
// compensator exists to write, so the run would strand PENDING with un-revoked
// credentials on exactly the path this exists for.
func (s *Server) abortHalfBuiltRun(ctx context.Context, runID uuid.UUID) func(hint string) {
	return func(hint string) {
		s.failAndRevoke(context.WithoutCancel(ctx), runID, types.RunPending, hint)
	}
}

// appendDevcontainerNoBuilderWarning says on the 201 what resolveCreateRunImage
// does silently: with no ImageBuilder wired a devcontainer_repo run falls
// through to the convention image, so the sandbox is not the one the caller
// asked for. The fall-through itself stays — a hard refusal would break
// every no-builder deployment that has been launching this way — and a workspace
// base_image still fails CLOSED there, which is the difference this
// sentence exists to make visible.
func (s *Server) appendDevcontainerNoBuilderWarning(warnings []string, req createRunRequest) []string {
	if req.DevcontainerRepo != "" && s.cfg.ImageBuilder == nil {
		return append(warnings, devcontainerNoBuilderWarning)
	}
	return warnings
}
