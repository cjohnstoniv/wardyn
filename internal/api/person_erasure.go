// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// person_erasure.go is POST /people/{principal}/erasure: one security-tier act
// that erases a person's retained records by explicit scope (internal/erasure
// owns the order and the partial-failure answer; the steps here are this
// server's own). DELETE /people/{principal}/credentials is the one-scope form
// and runs through the same orchestrator, so one code path erases credentials.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errSignInConfigUnreadable is the credentials scope's refusal when the Azure
// DevOps sign-in configuration cannot be read, so the erase could not take the
// sign-in's lock (credential_erase.go).
var errSignInConfigUnreadable = errors.New("the Azure DevOps sign-in configuration could not be read")

type erasePersonRequest struct {
	Scopes []string `json:"scopes"`
}

// erasureIncompleteBody is the 500 a partial erasure answers: what is done and
// what is left, so a retry names the same scopes.
type erasureIncompleteBody struct {
	Error     string   `json:"error"`
	Reason    string   `json:"reason"`
	Done      []string `json:"done"`
	Remaining []string `json:"remaining"`
}

// erasureOrchestrator is this server's steps. A scope whose backing is not
// configured has no step, so asking for it fails instead of reporting it
// erased. credRep, when non-nil, receives the credentials scope's report even
// when that scope fails part way.
func (s *Server) erasureOrchestrator(credRep *secretstore.EraseReport) *erasure.Orchestrator {
	steps := map[erasure.Scope]erasure.Step{
		erasure.RunTasks:   s.eraseRunTasks,
		erasure.RunOutputs: s.eraseRunOutputsOf,
		erasure.Recordings: s.eraseRecordingsOf,
	}
	if s.cfg.Secrets != nil {
		steps[erasure.Credentials] = func(ctx context.Context, person string) (any, error) {
			var rep secretstore.EraseReport
			fenced, err := s.eraseCredentialsScope(ctx, person, &rep)
			if credRep != nil {
				*credRep = rep
			}
			data := credentialEraseData(rep)
			if s.cfg.MaskManifests != nil {
				data["runs_fenced"] = fenced
			}
			return data, err
		}
	}
	if _, ok := s.cfg.Store.(store.ComponentStore); ok {
		steps[erasure.Components] = s.eraseComponentsOf
	}
	if s.cfg.MaskManifests != nil {
		steps[erasure.MaskCopies] = s.eraseMaskCopies
	}
	if s.cfg.SubjectKeys != nil {
		steps[erasure.AuditPersonalFields] = s.eraseAuditSealKeys
	}
	return &erasure.Orchestrator{Steps: steps}
}

// credentialEraseData is what a credentials erase reports: the credential.erase
// row's fields and the route's response. crypto_erased are the rows under the
// person's destroyed principal key; deleted are the rest (v1 rows, rows
// written with principal keys off, external pointers), which are gone only to
// the backup horizon.
func credentialEraseData(rep secretstore.EraseReport) map[string]any {
	data := map[string]any{"count": rep.Count, "crypto_erased": rep.CryptoErased, "deleted": rep.Count - rep.CryptoErased}
	if rep.Store != "" {
		data["store"], data["purged"] = rep.Store, rep.Purged
		if !rep.Purged && rep.RecoverableDays > 0 {
			data["recoverable_days"] = rep.RecoverableDays
		}
	}
	return data
}

// eraseCredentialsScope deletes every credential in owner's namespace, the
// credentials scope. The sign-in's row id is read BEFORE anything is erased,
// because the erase must hold the redemption lock for it: a configuration that
// cannot be read refuses the scope rather than proceeding without the lock.
//
// The person's runs are fenced (FenceSubject) before the cred key goes: their
// masking values are sealed under it, so a replica that has not opened them
// could no longer vouch for those runs' masking. It returns how many runs it
// fenced. Their history and sandboxes are untouched.
func (s *Server) eraseCredentialsScope(ctx context.Context, owner string, rep *secretstore.EraseReport) (int, error) {
	rowID, cfgErr := s.adoSignInRowID(ctx)
	if cfgErr != nil {
		return 0, errSignInConfigUnreadable
	}
	// Revoke the person's live Azure DevOps tokens first: the erase takes the
	// sign-in that revoking them needs. That runs BEFORE and OUTSIDE the three
	// locks eraseLocked takes, because its paths take the Entra redemption lock
	// themselves (ado_pat_client.go) and a nested take would deadlock.
	// end runs even on a panic, or the person's mints would self-revoke until restart.
	finish, err := s.beginADOSignInEnd(ctx, owner, adoPATRevokeOffboarding)
	if err != nil {
		return 0, err
	}
	defer finish()
	s.revokeOwnerRunPATs(ctx, owner, adoPATRevokeOffboarding)
	var runs []uuid.UUID
	if s.cfg.MaskManifests != nil && owner != "" { // "" is refused by the erase below
		if runs, err = s.cfg.MaskManifests.FenceSubject(ctx, owner); err != nil {
			return 0, err
		}
	}
	return len(runs), s.eraseLocked(ctx, owner, rowID, rep)
}

func (s *Server) personErasureStore() (store.PersonErasureStore, error) {
	if pe, ok := s.cfg.Store.(store.PersonErasureStore); ok {
		return pe, nil
	}
	return nil, fmt.Errorf("%w: the store cannot list a person's runs", erasure.ErrNotAvailable)
}

// eraseRunTasks blanks the task text of every run the person created.
func (s *Server) eraseRunTasks(ctx context.Context, person string) (any, error) {
	pe, err := s.personErasureStore()
	if err != nil {
		return nil, err
	}
	n, err := pe.BlankRunTasks(ctx, person)
	return map[string]any{"runs": n}, err
}

// eraseRunOutputsOf erases the stored output of every run the person created:
// out-o2's tombstone, which every replica honours on its next touch.
func (s *Server) eraseRunOutputsOf(ctx context.Context, person string) (any, error) {
	pe, err := s.personErasureStore()
	if err != nil {
		return nil, err
	}
	ids, err := pe.RunIDsCreatedBy(ctx, person)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := s.EraseRunOutputs(ctx, ids); err != nil {
			return nil, err
		}
	}
	return map[string]any{"runs": len(ids)}, nil
}

// eraseAuditSealKeys is the audit_personal_fields scope: it destroys, in every version, the key the
// person's sealed audit fields are under, and clears the person from the governance changes they
// proposed or decided (migration 0126). The fields are sealed under the person's principal; a name the
// directory did not yet know when a row was written is under its own key, so the principal the
// asked-for name resolves to and every alias the directory holds for it are destroyed too (a destroy
// that finds no live key writes nothing). The same names match a governance change's recorded
// principal or email.
func (s *Server) eraseAuditSealKeys(ctx context.Context, person string) (any, error) {
	names := []string{person}
	pe, peErr := s.personErasureStore()
	if peErr == nil {
		principal, err := pe.PrincipalForName(ctx, person)
		if err != nil {
			return nil, err
		}
		aliases, err := pe.PrincipalAliases(ctx, principal)
		if err != nil {
			return nil, err
		}
		for _, n := range append([]string{principal}, aliases...) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	generations := 0
	for _, name := range names {
		gens, err := s.cfg.SubjectKeys.Destroy(ctx, name, subjectkey.PurposeAuditSeal)
		if err != nil {
			return nil, err
		}
		generations += len(gens)
	}
	detail := map[string]any{"generations": generations}
	if peErr == nil {
		// A pending change whose proposer is erased leaves pending in the same statement, so a change
		// with no recorded proposer can never be approved.
		n, err := pe.EraseGovernanceChangePersonalFields(ctx, names)
		if err != nil {
			return nil, err
		}
		detail["governance_changes"] = n
	}
	return detail, nil
}

// erasureSelfTarget reports whether the person an erasure names is the caller:
// the resolved principal or the name as typed is the caller's principal or
// email, the comparison requireSecondHuman makes. A system actor (the admin
// token) is no person, so it is never the target.
func erasureSelfTarget(actorType types.ActorType, principal, email, owner, raw string) bool {
	if actorType == types.ActorSystem {
		return false
	}
	if principal != "" && (principal == owner || principal == raw) {
		return true
	}
	return email != "" && (strings.EqualFold(email, owner) || strings.EqualFold(email, raw))
}

// handleErasePerson is POST /people/{principal}/erasure on the security tier.
// The body names the scopes (a non-empty list of known names). The principal
// resolves as the credential erase resolves it, the operator namespace is
// refused for every scope before anything runs, and nobody erases themself
// except their own credentials (the admin token, which is no person, may).
// The person.erasure row names the person in the clear, lists the scopes and
// each one's outcome, and is never sealed, so the record of an erasure
// survives the keys it destroys.
func (s *Server) handleErasePerson(w http.ResponseWriter, r *http.Request) {
	actorType, principal := actorFromRequest(r)
	record := func(target, outcome string, data map[string]any) {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, principal, "person.erasure", target, outcome, mustJSON(data)))
	}
	raw := strings.TrimSpace(principalParam(r))
	var req erasePersonRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	scopes, err := erasure.ParseScopes(req.Scopes)
	if err != nil {
		record(raw, "failure", map[string]any{"reason": reasonErasureScopeUnknown})
		writeErrorReason(w, http.StatusBadRequest, reasonErasureScopeUnknown, "scopes must be a non-empty list of known names: "+strings.Join(scopeNames(erasure.Scopes()), ", "))
		return
	}
	if raw == "" {
		record("", "failure", map[string]any{"reason": reasonErasureOperatorNamespace})
		writeErrorReason(w, http.StatusBadRequest, reasonErasureOperatorNamespace, "that names the operator namespace, which is not a person's; nothing was erased")
		return
	}
	owner, known, refusal := s.resolveSecretOwner(r.Context(), raw)
	if refusal != "" {
		reason := ownerRefusalReason(refusal)
		s.auditOwnerRefusal(r, "person.erasure", raw, reason)
		writeErrorReason(w, http.StatusUnprocessableEntity, reason, eraseRefusalMsg(refusal))
		return
	}
	if erasureSelfTarget(actorType, principal, oidcEmailFromContext(r.Context()), owner, raw) &&
		slices.ContainsFunc(scopes, func(sc erasure.Scope) bool { return sc != erasure.Credentials }) {
		record(owner, "failure", map[string]any{"reason": reasonErasureSelfRefused, "scopes": scopeNames(scopes)})
		writeErrorReason(w, http.StatusForbidden, reasonErasureSelfRefused,
			"you cannot erase your own records, except your credentials; ask another security admin. Nothing was erased")
		return
	}

	rep, err := s.erasureOrchestrator(nil).Orchestrate(r.Context(), owner, scopes)
	data := map[string]any{"scopes": scopeNames(sortedScopes(scopes))}
	outcomes := map[string]string{}
	for _, sc := range scopes {
		outcomes[string(sc)] = "not_run"
	}
	for _, sc := range rep.Done {
		outcomes[string(sc)] = "done"
	}
	detail := map[string]any{}
	for sc, d := range rep.Details {
		detail[string(sc)] = d
	}
	data["outcome"], data["detail"] = outcomes, detail
	if !known {
		data["owner_known"] = false
	}
	if err != nil {
		var inc *erasure.IncompleteError
		if errors.As(err, &inc) {
			outcomes[string(inc.Remaining[0])] = "failed"
			data["error"] = err.Error()
			record(owner, "failure", data)
			slog.ErrorContext(r.Context(), "wardyn: person erasure incomplete", slog.String("target", owner), slog.Any("err", err))
			if db.LockRefused(err) {
				writeLockRefused(w, r, err)
				return
			}
			writeJSON(w, http.StatusInternalServerError, erasureIncompleteBody{
				Error:  "erasure is not complete; retry with the same scopes to finish the rest",
				Reason: reasonErasureIncomplete, Done: scopeNames(inc.Done), Remaining: scopeNames(inc.Remaining),
			})
			return
		}
		record(owner, "failure", data)
		writeServerError(w, r, "erase a person", err)
		return
	}
	record(owner, "success", data)
	writeJSON(w, http.StatusOK, map[string]any{"person": owner, "scopes": data["scopes"], "outcome": outcomes, "detail": detail})
}

func scopeNames(ss []erasure.Scope) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// sortedScopes is scopes deduplicated, in the order Orchestrate runs them.
func sortedScopes(scopes []erasure.Scope) []erasure.Scope {
	var out []erasure.Scope
	for _, s := range erasure.Scopes() {
		if slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	return out
}
