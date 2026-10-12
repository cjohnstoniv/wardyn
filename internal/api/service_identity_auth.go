// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Service identities (#1508), the authentication half: a wdn_ token whose principal is "service:<name>"
// authenticates here instead of as a person. The identity is read on every request, so suspending or
// retiring it, or changing its sponsor, ends its tokens at once; so does the sponsor's session state.
// The service is published at member reach on a fixed route allow-list, and it is never an operator.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The auth.fail reasons of a service token refused at the door: its identity is missing, suspended or
// retired or the token predates the sponsor, or its sponsor's session is not live.
const (
	authFailedServiceIdentityInactive = "service_identity_inactive"
	authFailedServiceSponsorInactive  = "service_sponsor_inactive"
)

// serviceIdentityCtxKey carries the service identity that authenticated the request and its sponsor.
type serviceIdentityCtxKey struct{}

type serviceIdentityCtx struct{ name, sponsor string }

// withServiceIdentity marks ctx as a request made by service identity name, sponsored by sponsor. The
// sponsor rides audit.WithSponsor too, so the recorder chain stamps data.sponsor on every row.
func withServiceIdentity(ctx context.Context, name, sponsor string) context.Context {
	ctx = context.WithValue(ctx, serviceIdentityCtxKey{}, serviceIdentityCtx{name: name, sponsor: sponsor})
	return audit.WithSponsor(ctx, sponsor)
}

// serviceIdentityFromContext reports the service identity ctx's request was made by, when it was.
func serviceIdentityFromContext(ctx context.Context) (name, sponsor string, ok bool) {
	v, ok := ctx.Value(serviceIdentityCtxKey{}).(serviceIdentityCtx)
	return v.name, v.sponsor, ok
}

// serviceActorType is the audit actor type of a service request. It is types.ActorService; the design's
// fallback form (D8) records the same actor as types.ActorSystem, and every decision about a service keys
// on serviceIdentityFromContext, never on this value, so both forms behave the same. Tests set the fallback.
var serviceActorType = types.ActorService

// serviceAllowed is the whole of what a service identity's token may reach, keyed as chi reports a
// matched route: "METHOD pattern". Anything not here, including any route added later, answers 403
// service_scope. Absent on purpose: attach, approvals, secrets, tokens, SSH keys, invitations, runner
// claims and every admin route.
var serviceAllowed = map[string]bool{
	"POST /api/v1/runs":                              true,
	"POST /api/v1/runs/preflight":                    true,
	"GET /api/v1/runs":                               true,
	"GET /api/v1/runs/{id}":                          true,
	"GET /api/v1/runs/{id}/events":                   true,
	"GET /api/v1/runs/{id}/output":                   true,
	"PATCH /api/v1/runs/{id}":                        true,
	"POST /api/v1/runs/{id}/kill":                    true,
	"GET /api/v1/me":                                 true,
	"GET /api/v1/setup/status":                       true,
	"PUT /api/v1/model-providers/{id}/credential":    true,
	"DELETE /api/v1/model-providers/{id}/credential": true,
}

// serviceScopeRefusal is the 403 body a service identity's token gets on any other route.
const serviceScopeRefusal = "a service identity cannot reach this action"

// serviceTokenAuth authenticates a wdn_ token whose principal is a service identity's, in apiTokenAuth's
// place for it. A refusal falls through to fallback with its auth.fail reason set, so every refusal is the
// one 401 of an unknown token (no oracle for which check failed); a store failure is a 503.
func (s *Server) serviceTokenAuth(w http.ResponseWriter, r *http.Request, t types.APIToken, next, fallback http.Handler) {
	ctx := r.Context()
	reject := func(reason string) {
		fallback.ServeHTTP(w, r.WithContext(oidc.WithSessionRejected(ctx, reason)))
	}
	unavailable := func(what string, err error) {
		slog.ErrorContext(ctx, "api: "+what+" lookup failed; this service token could not be authenticated", "error", err, "path", r.URL.Path)
		s.metrics.authStoreErrorInc()
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "api token lookup failed")
	}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		reject(authFailedServiceIdentityInactive)
		return
	}
	si, err := ss.GetServiceIdentity(ctx, strings.TrimPrefix(t.Principal, types.ServicePrincipalPrefix))
	if errors.Is(err, store.ErrNotFound) {
		reject(authFailedServiceIdentityInactive)
		return
	}
	if err != nil {
		unavailable("service identity", err)
		return
	}
	// A token at or before sponsor_since is not a credential: a mint whose row committed after a sponsor
	// change's revoke snapshot still never authenticates, the cutoff rule apiTokenAuth applies to a session.
	if !si.Usable() || !t.CreatedAt.After(si.SponsorSince) {
		reject(authFailedServiceIdentityInactive)
		return
	}
	// The sponsor's session state, read on every request (epoch -1: a session-only cut leaves a token
	// working, as for the sponsor's own). A deployment with no revocation store has no sponsor to ask:
	// fail closed.
	if s.cfg.SessionRevocations == nil {
		reject(authFailedServiceSponsorInactive)
		return
	}
	status, rerr := oidc.CheckSession(ctx, s.cfg.SessionRevocations, si.Sponsor, "", oidc.Issue{At: t.CreatedAt, Seq: t.RevSeq}, -1)
	if rerr != nil {
		unavailable("session-revocation", rerr)
		return
	}
	if status != oidc.SessionLive {
		reject(authFailedServiceSponsorInactive)
		return
	}
	_ = s.cfg.Store.TouchAPIToken(ctx, t.ID, s.cfg.Now().UTC())
	// Role and type come from the live identity on every request, not a sign-in stamp, so refuseStaleRoleStamp
	// has nothing to compare. Member reach, no groups, a complete (empty) snapshot.
	ctx = withHumanIdentity(ctx, t.Principal, "", oidc.RoleUser, si.UserType, []string{}, false)
	r = r.WithContext(withAPITokenID(withServiceIdentity(ctx, si.Name, si.Sponsor), t.ID))
	if rctx := chi.RouteContext(r.Context()); rctx == nil || !serviceAllowed[r.Method+" "+rctx.RoutePattern()] {
		d := authz.Deny(authz.ReasonServiceScope, r.URL.Path, serviceScopeRefusal)
		writeErrorReason(w, d.Status, string(d.Reason), d.Sentence)
		s.recordRefusal(r.Context(), r, d)
		return
	}
	next.ServeHTTP(w, r)
}

// isServicePrincipal reports whether p is spelled as a service identity's principal.
func isServicePrincipal(p string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), types.ServicePrincipalPrefix)
}

// requestSponsor is the sponsor claim of a run created by this request: the sponsor of the service
// identity that made it, else creator itself.
func requestSponsor(ctx context.Context, creator string) string {
	if _, sponsor, ok := serviceIdentityFromContext(ctx); ok {
		return sponsor
	}
	return creator
}

// errServiceRetired is runSponsor's refusal for a run of a retired service identity: it stops renewing and
// cannot be revived, as the identity's tokens and credentials are gone.
var errServiceRetired = errors.New("the service identity is retired")

// runSponsor is the sponsor a re-minted run identity for creator carries, read from the service identity's
// row so a sponsor change reaches it; creator itself for a person. A retired service has none: errServiceRetired.
func (s *Server) runSponsor(ctx context.Context, creator string) (string, error) {
	name, isService := strings.CutPrefix(creator, types.ServicePrincipalPrefix)
	if !isService {
		return creator, nil
	}
	si, err := s.serviceRecord(ctx, name)
	if err != nil {
		return "", err
	}
	if si.RetiredAt != nil {
		return "", errServiceRetired
	}
	return si.Sponsor, nil
}

func (s *Server) serviceRecord(ctx context.Context, name string) (types.ServiceIdentity, error) {
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		return types.ServiceIdentity{}, errors.New("service identities require the Postgres store backend")
	}
	return ss.GetServiceIdentity(ctx, name)
}

// errRunAuthorUnrecorded is runAuthoredBy's answer for a service identity's run with no recorded human
// author (one created before agent_runs.human_author): no decider can be shown not to be its author.
var errRunAuthorUnrecorded = errors.New("the run has no recorded human author")

// runAuthoredBy reports whether principal is the author of run for the second-human rule: its creator, or,
// for a service identity's run, the person sponsoring that service now (retired or not) or the sponsor
// recorded when the run was created, so neither launching through a service nor a later sponsor change
// makes that person a second human. A service whose sponsor cannot be read is an error, and a service run
// with no recorded author is errRunAuthorUnrecorded; the callers treat both as a refusal.
func (s *Server) runAuthoredBy(ctx context.Context, run types.AgentRun, principal string) (bool, error) {
	if run.CreatedBy == "" {
		return false, nil
	}
	if run.CreatedBy == principal {
		return true, nil
	}
	name, isService := strings.CutPrefix(run.CreatedBy, types.ServicePrincipalPrefix)
	if !isService {
		return false, nil
	}
	si, err := s.serviceRecord(ctx, name)
	if err != nil {
		return false, err
	}
	if si.Sponsor == principal || run.HumanAuthor == principal {
		return true, nil
	}
	if run.HumanAuthor == "" {
		return false, errRunAuthorUnrecorded
	}
	return false, nil
}

// rateLimitedActor reports who a per-person limiter counts the request against, and whether it counts it
// at all: a person, and a service identity (its name is its bucket); never the admin token or a device,
// which are shared actors.
func rateLimitedActor(r *http.Request) (who string, limited bool) {
	t, who := actorFromRequest(r)
	if _, _, ok := serviceIdentityFromContext(r.Context()); ok {
		return who, true
	}
	return who, t == types.ActorHuman
}

// serviceEndedSince reports whether a service identity's open stream must end: the identity is gone,
// suspended or retired, its sponsor changed since openedAt, or the sponsor's session is not live. An
// unanswerable check ends it too; the reconnect re-authenticates.
func (s *Server) serviceEndedSince(r *http.Request, name, sponsor string, openedAt time.Time) bool {
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		return true
	}
	// The store compares openedAt on the database clock sponsor_since is written on.
	if still, err := ss.ServiceStillSponsored(r.Context(), name, sponsor, openedAt); err != nil || !still {
		return true
	}
	if s.cfg.SessionRevocations == nil {
		return true
	}
	status, err := oidc.CheckSession(r.Context(), s.cfg.SessionRevocations, sponsor, "", oidc.Issue{At: openedAt}, -1)
	return err != nil || status != oidc.SessionLive
}
