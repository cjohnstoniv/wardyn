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
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/audit"
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

// reasonIdentityDirectoryUnreadable is the audit reason an erase records when it
// could not read who a name is, so nothing was erased.
const reasonIdentityDirectoryUnreadable = "identity_directory_unreadable"

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
// when that scope fails part way. want, when non-nil, is the person's names the
// caller proved before anything ran (erasureNamesOf); the steps act on no others.
func (s *Server) erasureOrchestrator(credRep *secretstore.EraseReport, want *erasureNames) *erasure.Orchestrator {
	// Launch intents hold task text and inline component definitions, so the
	// run_tasks and components steps erase them too, and so does run_setups.
	steps := map[erasure.Scope]erasure.Step{
		erasure.RunTasks:   s.alsoEraseLaunchIntents(s.eraseRunTasks),
		erasure.RunOutputs: s.eraseRunOutputsOf,
		erasure.Recordings: s.eraseRecordingsOf,
	}
	if s.cfg.Secrets != nil {
		steps[erasure.Credentials] = func(ctx context.Context, person string) (any, error) {
			var rep secretstore.EraseReport
			history := map[string]any{}
			fenced, err := s.eraseCredentialsScope(ctx, person, want, &rep)
			if err == nil {
				err = s.eraseLoginGroups(ctx, person, want)
			}
			if err == nil {
				err = s.eraseTokenHistory(ctx, person, want, history)
			}
			if credRep != nil {
				*credRep = rep
			}
			data := credentialEraseData(rep)
			maps.Copy(data, history)
			if s.cfg.MaskManifests != nil {
				data["runs_fenced"] = fenced
			}
			return data, err
		}
	}
	if _, ok := s.cfg.Store.(store.ComponentStore); ok {
		steps[erasure.Components] = s.alsoEraseLaunchIntents(s.eraseComponentsOf)
	}
	if ts, ok := s.cfg.Store.(store.TemplateStore); ok {
		// run_setups: the person's own templates, their grants, and their name on
		// shared templates. The person is matched in the canonical form every
		// template row stores.
		steps[erasure.RunSetups] = s.alsoEraseLaunchIntents(func(ctx context.Context, person string) (any, error) {
			return ts.EraseRunSetupsOf(ctx, types.CanonicalUserSubject(person))
		})
	}
	if s.cfg.MaskManifests != nil {
		steps[erasure.MaskCopies] = func(ctx context.Context, person string) (any, error) {
			return s.eraseMaskCopies(ctx, person, want)
		}
	}
	if _, ok := s.cfg.Store.(store.RunnerPoolStore); ok {
		steps[erasure.RunnerPools] = s.eraseRunnerPoolsOf
	}
	if _, ok := s.cfg.Store.(store.MemberDriveStore); ok {
		steps[erasure.MemberDrives] = s.eraseMemberDrivesOf
	}
	if s.cfg.SubjectKeys != nil {
		steps[erasure.AuditPersonalFields] = func(ctx context.Context, person string) (any, error) {
			return s.eraseAuditSealKeys(ctx, person, want)
		}
	}
	return &erasure.Orchestrator{Steps: steps}
}

// The two answers credential.erase gives for what held back a writer that began before it:
// the owner's erase generation, which refuses it in any process, or the credential locks alone,
// for a store that keeps no generation.
const (
	writeFenceGeneration = "generation"
	writeFenceLocksOnly  = "locks_only"
)

// credentialEraseData is what a credentials erase reports: the credential.erase
// row's fields and the route's response. crypto_erased are the rows under the
// person's destroyed principal key; deleted are the rest (v1 rows, rows
// written with principal keys off, external pointers), which are gone only to
// the backup horizon.
func credentialEraseData(rep secretstore.EraseReport) map[string]any {
	data := map[string]any{"count": rep.Count, "crypto_erased": rep.CryptoErased, "deleted": rep.Count - rep.CryptoErased, "write_fence": writeFenceLocksOnly}
	if rep.Fenced {
		data["write_fence"] = writeFenceGeneration
	}
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
func (s *Server) eraseCredentialsScope(ctx context.Context, owner string, want *erasureNames, rep *secretstore.EraseReport) (int, error) {
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
	fenced := 0
	if s.cfg.MaskManifests != nil && owner != "" { // "" is refused by the erase below
		// Every namespace eraseLocked erases: a run started under an entra form owns its manifest by it.
		namespaces, err := s.personNamespaces(ctx, owner, want)
		if err != nil {
			return 0, err
		}
		for _, ns := range namespaces {
			runs, err := s.cfg.MaskManifests.FenceSubject(ctx, ns)
			fenced += len(runs)
			if err != nil {
				return fenced, err
			}
		}
	}
	return fenced, s.eraseLocked(ctx, owner, rowID, want, rep)
}

// alsoEraseLaunchIntents runs step, then erases the person's launch intents and
// counts them in the step's detail. The intents are matched on the runs'
// created_by, as run_tasks matches the runs. A store that keeps no intents has
// none to erase.
func (s *Server) alsoEraseLaunchIntents(step erasure.Step) erasure.Step {
	return func(ctx context.Context, person string) (any, error) {
		detail, err := step(ctx, person)
		ls, ok := s.cfg.Store.(store.LaunchIntentStore)
		if err != nil || !ok {
			return detail, err
		}
		n, err := ls.DeleteLaunchIntentsCreatedBy(ctx, person)
		switch d := detail.(type) {
		case map[string]any:
			d["launch_intents"] = n
		case store.RunSetupsErased:
			d.LaunchIntents = n
			detail = d
		}
		return detail, err
	}
}

func (s *Server) personErasureStore() (store.PersonErasureStore, error) {
	if pe, ok := s.cfg.Store.(store.PersonErasureStore); ok {
		return pe, nil
	}
	return nil, fmt.Errorf("%w: the store cannot list a person's runs", erasure.ErrNotAvailable)
}

// eraseRunTasks blanks the task text, title and description of every run the person created. A run of theirs still waiting
// in a pool's queue is cancelled first: its queue row holds the whole request, task and identity
// snapshot included, and it would start under the very person being erased.
func (s *Server) eraseRunTasks(ctx context.Context, person string) (any, error) {
	pe, err := s.personErasureStore()
	if err != nil {
		return nil, err
	}
	cancelled, err := s.cancelQueuedRunsOf(ctx, person, "person_erased")
	if err != nil {
		return nil, err
	}
	n, err := pe.BlankRunTasks(ctx, person)
	out := map[string]any{"runs": n}
	if cancelled > 0 {
		out["queued_cancelled"] = cancelled
	}
	return out, err
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

// errErasureNameUnproved refuses an erasure that would act on a name derived for the person (an
// entra:<tenant>:<object> form of their sign-ins) that the directory names as no one: whose records are
// under it is unknown, so nothing is erased and nothing is reported erased.
var errErasureNameUnproved = errors.New("a name derived for the person is not proved to be theirs alone")

// errErasureNamesChanged fails a destructive step whose proved names are not the ones the erasure began
// with: a sign-in was bound or released in between.
var errErasureNamesChanged = errors.New("the person's names changed while the erasure ran")

// erasureNames are the names besides the principal that an erasure of the person acts on, each one the
// directory (PrincipalsNamed) resolves to that person alone: the entra:<tenant>:<object> forms of their
// sign-ins, and the email aliases they signed in with. notErased are the other principals the erasure
// leaves alone although they share the person's tenant and object id (another issuer's sign-in, or the
// principal a derived form resolves to): often the same human, so the operator is told to erase them by
// subject (notErasedDetail).
type erasureNames struct {
	entra, aliases, notErased []string
}

// notErasedDetail is how an erasure's or a purge's response and audit row list the principals it left
// alone, when there are any.
func notErasedDetail(subjects []string) (map[string]any, bool) {
	if len(subjects) == 0 {
		return nil, false
	}
	return map[string]any{"subjects": subjects, "note": "not erased: erase by subject"}, true
}

// derivedVerdict is what the identity directory says of an entra:<tenant>:<object> form derived for a
// person from their own tenant and object ids.
type derivedVerdict int

const (
	derivedOwned   derivedVerdict = iota // the directory names the person alone
	derivedOthers                        // it names another principal, or the person and another
	derivedUnknown                       // it names no one
)

// deriveOwner proves one derived form c for person. Equal tenant and object ids are no proof, since a
// sign-in under another issuer can share them: c is the person's only when the directory resolves it to
// them alone, as for a people row of the same issuer they signed in under. Otherwise it is someone else's,
// or shared, and the subjects to list as not erased are c itself and every principal it names besides
// person; or it names no one.
func deriveOwner(ctx context.Context, dir store.PrincipalDirectoryStore, person, c string) (derivedVerdict, []string, error) {
	owners, err := dir.PrincipalsNamed(ctx, c)
	switch {
	case err != nil:
		return derivedUnknown, nil, err
	case slices.Equal(owners, []string{person}):
		return derivedOwned, nil, nil
	case len(owners) == 0:
		return derivedUnknown, nil, nil
	}
	return derivedOthers, append(slices.DeleteFunc(owners, func(o string) bool { return o == person }), c), nil
}

// erasureNamesOf proves erasureNames for person. Each entra form goes through deriveOwner: one the person
// owns is acted on, one that names another principal (alone or beside the person) is skipped and listed in
// notErased with those principals, and one that names no one is errErasureNameUnproved. An alias is claim
// text that can spell anyone, so one that names anyone else is skipped.
func (s *Server) erasureNamesOf(ctx context.Context, person string) (n erasureNames, err error) {
	pe, err := s.personErasureStore()
	if err != nil || person == "" {
		return n, nil
	}
	entra, err := pe.PrincipalEntraNames(ctx, person)
	if err != nil {
		return n, err
	}
	aliases, err := pe.PrincipalAliases(ctx, person)
	if err != nil {
		return n, err
	}
	if n.notErased, err = pe.PrincipalsSharingObject(ctx, person); err != nil {
		return n, err
	}
	defer func() { n.notErased = slices.Compact(slices.Sorted(slices.Values(n.notErased))) }()
	dir, _ := s.cfg.Store.(store.PrincipalDirectoryStore)
	for _, c := range entra {
		if c == person {
			continue
		}
		if dir == nil {
			return n, fmt.Errorf("%w: %s", errErasureNameUnproved, c)
		}
		v, others, err := deriveOwner(ctx, dir, person, c)
		switch {
		case err != nil:
			return n, err
		case v == derivedOwned:
			n.entra = append(n.entra, c)
		case v == derivedUnknown:
			return n, fmt.Errorf("%w: %s", errErasureNameUnproved, c)
		default:
			n.notErased = append(n.notErased, others...)
		}
	}
	for _, a := range aliases {
		if dir == nil || a == person || slices.Contains(entra, a) {
			continue
		}
		owners, err := dir.PrincipalsNamed(ctx, a)
		if err != nil {
			return n, err
		}
		if slices.Equal(owners, []string{person}) {
			n.aliases = append(n.aliases, a)
		}
	}
	return n, nil
}

// provedNames is erasureNamesOf at a destructive step. An erasure that proved want before it began acts
// only if the directory still proves the same names, so a binding that changed in between cannot widen it.
func (s *Server) provedNames(ctx context.Context, person string, want *erasureNames) (erasureNames, error) {
	got, err := s.erasureNamesOf(ctx, person)
	if err == nil && want != nil && (!slices.Equal(got.entra, want.entra) || !slices.Equal(got.aliases, want.aliases)) {
		err = errErasureNamesChanged
	}
	return got, err
}

// eraseAuditSealKeys is the audit_personal_fields scope for person, the principal the route resolved
// (resolveSecretOwner): it destroys, in every version, the key the person's sealed audit fields are
// under, and clears the person from the governance changes they proposed or decided (migration 0126)
// and from the approvals they decided, with the reason they typed.
// The fields are sealed under the person's principal; a name the directory did not yet know when a row
// was written is under its own key, so the keys of the person's proved names (erasureNames) are
// destroyed too (a destroy that finds no live key writes only a tombstone). A governance change matches on its
// recorded principal by those names, and on its recorded email by the proved aliases alone: an email
// claim can spell the principal, so the principal is never matched against one.
func (s *Server) eraseAuditSealKeys(ctx context.Context, person string, want *erasureNames) (any, error) {
	proved, err := s.provedNames(ctx, person, want)
	if err != nil {
		return nil, err
	}
	names := slices.Concat([]string{person}, proved.entra, proved.aliases)
	keyOwners := names
	// Under WARDYN_AUDIT_SEAL=full a row's actor is the person's subject id, which an identity row
	// unbound and bound again hands to someone else: destroying under the subject too keeps the
	// rows written before now erased whoever the subject names later (audit.Sealer renderSubject).
	if sd, ok := s.cfg.Store.(audit.SubjectDirectory); ok {
		id, found, err := sd.AuditSubjectFor(ctx, person)
		if err != nil {
			return nil, err
		}
		if found {
			keyOwners = append(slices.Clone(names), audit.SubjectActorPrefix+id)
		}
	}
	generations := 0
	for _, name := range keyOwners {
		gens, err := s.cfg.SubjectKeys.Destroy(ctx, name, subjectkey.PurposeAuditSeal)
		if err != nil {
			return nil, err
		}
		generations += len(gens)
	}
	detail := map[string]any{"generations": generations}
	if pe, err := s.personErasureStore(); err == nil {
		// A pending change whose proposer is erased leaves pending in the same statement, so a change
		// with no recorded proposer can never be approved.
		n, err := pe.EraseGovernanceChangePersonalFields(ctx, names, proved.aliases)
		if err != nil {
			return nil, err
		}
		detail["governance_changes"] = n
	}
	if ae, ok := s.cfg.Store.(approvalDecisionEraser); ok {
		n, err := ae.EraseApprovalDecisionPersonalFields(ctx, names)
		if err != nil {
			return nil, err
		}
		detail["approvals"] = n
	}
	return detail, nil
}

// approvalDecisionEraser clears who decided an approval and the reason they typed (store.PG).
type approvalDecisionEraser interface {
	EraseApprovalDecisionPersonalFields(ctx context.Context, principals []string) (int, error)
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

// erasureTargetsCaller is erasureSelfTarget over the names the erasure's own
// steps act on. The target is owner, the principal the route resolved
// (resolveSecretOwner), with its aliases. The caller is their principal and
// email, each resolved through the same answer, with that principal's aliases,
// so an entra:<tenant>:<object> form or an email of the caller's, or another
// identity of theirs their email names, still counts as the caller. names, when
// non-nil, are the target's proved names the steps will act on (erasureNamesOf). The caller
// is the target when any name of the target's matches any name of the caller's,
// compared case-folded (a refusal that is too wide is the safe side). An error
// reading the directory is returned: the erasure does not go ahead unchecked.
func (s *Server) erasureTargetsCaller(ctx context.Context, actorType types.ActorType, principal, email, owner, raw string, names *erasureNames) (bool, error) {
	if actorType == types.ActorSystem {
		return false, nil
	}
	if erasureSelfTarget(actorType, principal, email, owner, raw) {
		return true, nil
	}
	pe, err := s.personErasureStore()
	if err != nil {
		return false, nil // no identity directory: the names above are all there are
	}
	target, err := pe.PrincipalAliases(ctx, owner)
	if err != nil {
		return false, err
	}
	target = append(target, owner, raw)
	if names != nil {
		target = slices.Concat(target, names.entra, names.aliases)
	}
	var caller []string
	for _, n := range []string{principal, email} {
		if n == "" {
			continue
		}
		p, _, refusal, err := s.resolveSecretOwner(ctx, n)
		if err != nil {
			return false, err
		}
		if refusal != "" {
			p = n // ambiguous or unresolved: the name alone
		}
		aliases, err := pe.PrincipalAliases(ctx, p)
		if err != nil {
			return false, err
		}
		caller = append(append(caller, n, p), aliases...)
	}
	return slices.ContainsFunc(target, func(t string) bool {
		return t != "" && slices.ContainsFunc(caller, func(c string) bool { return strings.EqualFold(t, c) })
	}), nil
}

// handleErasePerson is POST /people/{principal}/erasure on the security tier.
// The body names the scopes (a non-empty list of known names). The principal
// resolves once, through resolveSecretOwner as the credential erase resolves it,
// and every scope acts on that one principal; the operator namespace and the
// reserved names that are no person (admin-token, local:, device:, a portal) are
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
	if s.refuseReservedErasure(w, r, "person.erasure", raw) {
		return
	}
	owner, known, refusal, err := s.resolveSecretOwner(r.Context(), raw)
	if err != nil {
		record(raw, "failure", map[string]any{"reason": reasonIdentityDirectoryUnreadable, "scopes": scopeNames(scopes)})
		writeServerError(w, r, "resolve whom an erasure names", err)
		return
	}
	if refusal != "" {
		reason := ownerRefusalReason(refusal)
		s.auditOwnerRefusal(r, "person.erasure", raw, reason)
		writeErrorReason(w, http.StatusUnprocessableEntity, reason, eraseRefusalMsg(refusal))
		return
	}
	if s.refuseReservedErasure(w, r, "person.erasure", owner) {
		return
	}
	// The names the credentials and audit steps act on are proved once, before anything runs, and the
	// self check below covers them. A name that is not proved fails only the scopes that act on names.
	names, unproved := s.erasureNamesOf(r.Context(), owner)
	if unproved != nil && !errors.Is(unproved, errErasureNameUnproved) {
		record(owner, "failure", map[string]any{"reason": reasonIdentityDirectoryUnreadable, "scopes": scopeNames(scopes)})
		writeServerError(w, r, "prove whose names an erasure acts on", unproved)
		return
	}
	if !slices.ContainsFunc(scopes, func(sc erasure.Scope) bool { return sc == erasure.Credentials || sc == erasure.AuditPersonalFields }) {
		unproved = nil
	}
	if slices.ContainsFunc(scopes, func(sc erasure.Scope) bool { return sc != erasure.Credentials }) {
		self, err := s.erasureTargetsCaller(r.Context(), actorType, principal, oidcEmailFromContext(r.Context()), owner, raw, &names)
		if err != nil {
			record(owner, "failure", map[string]any{"reason": reasonIdentityDirectoryUnreadable, "scopes": scopeNames(scopes)})
			writeServerError(w, r, "check who an erasure names", err)
			return
		}
		if self {
			record(owner, "failure", map[string]any{"reason": reasonErasureSelfRefused, "scopes": scopeNames(scopes)})
			writeErrorReason(w, http.StatusForbidden, reasonErasureSelfRefused,
				"you cannot erase your own records, except your credentials; ask another security admin. Nothing was erased")
			return
		}
	}

	var rep erasure.Report
	if unproved != nil {
		err = &erasure.IncompleteError{Remaining: sortedScopes(scopes), Cause: unproved}
	} else {
		rep, err = s.erasureOrchestrator(nil, &names).Orchestrate(withRequestActor(r), owner, scopes)
	}
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
	if ne, ok := notErasedDetail(names.notErased); ok {
		data["not_erased"] = ne
	}
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
			msg := "erasure is not complete; retry with the same scopes to finish the rest"
			if errors.Is(err, errErasureNameUnproved) {
				msg = "erasure did not run: a name derived for this person names no one this deployment knows, so nothing was erased"
			}
			writeJSON(w, http.StatusInternalServerError, erasureIncompleteBody{
				Error: msg, Reason: reasonErasureIncomplete, Done: scopeNames(inc.Done), Remaining: scopeNames(inc.Remaining),
			})
			return
		}
		record(owner, "failure", data)
		writeServerError(w, r, "erase a person", err)
		return
	}
	record(owner, "success", data)
	resp := map[string]any{"person": owner, "scopes": data["scopes"], "outcome": outcomes, "detail": detail}
	if ne, ok := data["not_erased"]; ok {
		resp["not_erased"] = ne
	}
	writeJSON(w, http.StatusOK, resp)
}

// refuseReservedErasure answers 422 reserved_principal, and audits the refusal
// under action, when name is a reserved identity (isReservedPrincipal): the
// admin token, the local operator, a device or a portal is no person, and the
// runs, outputs and recordings stamped with it belong to the deployment's
// automation, not to someone who can ask to be forgotten. It reports whether it
// answered.
func (s *Server) refuseReservedErasure(w http.ResponseWriter, r *http.Request, action, name string) bool {
	if !s.isReservedPrincipal(name) {
		return false
	}
	s.auditOwnerRefusal(r, action, name, reasonReservedPrincipal)
	writeErrorReason(w, http.StatusUnprocessableEntity, reasonReservedPrincipal,
		"that names an identity that is not a person (the admin token, a local operator, a device or a portal); nothing was erased")
	return true
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
