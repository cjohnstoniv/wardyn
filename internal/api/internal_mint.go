// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Continues internal.go's /api/v1/internal/* sidecar surface (see that
// file's own header) — split into a second file only because #656 final
// review round L5 found internal.go sitting exactly at
// scripts/check-file-size.sh's 1000-line threshold, one edit from tripping
// it. This half is the credential-mint door and the token renew door;
// internal.go keeps the decision-log, ground-truth and approval-request
// intake. No behavior changes — a Go function's visibility is package-wide
// regardless of which file declares it.

// handleInternalMint is the broker chokepoint over HTTP. The caller's verified
// claims (run + SPIFFE id) are passed to the broker, which enforces ownership,
// the approval gate, and the no-widening invariant inside a single transaction.
// Responses: 200 minted | 401 unauthorized | 409 {"approval_id"} pending/denied.
func (s *Server) handleInternalMint(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	if s.cfg.Broker == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonBrokerNotConfigured, "broker not configured")
		return
	}
	var body mintRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSidecarBody)).Decode(&body); err != nil || body.GrantID == uuid.Nil {
		writeErrorReason(w, http.StatusBadRequest, reasonMintGrantIDRequired, "grant_id is required")
		return
	}

	// Single-lane, at mint time. A run that also holds a github_token grant may
	// not mint an ssh_key OR a git_pat for the brokered forge (see
	// brokeredForgeMintKind). This is the residual-only half of a rule already
	// enforced three times over — policy-write refuses the pair
	// (validateGrantLaneExclusivity), dispatch denies the forge in egress and
	// withholds the grant id from the sandbox env — and it covers exactly one
	// case: a policy STORED BEFORE that change, whose ssh_key/git_pat grant row
	// still exists, minted by a caller who somehow learned its id. That caller is
	// the whole point for git_pat: the proxy's own mint refusal
	// (isBrokeredGitGrant) matches github_token grant ids only, so this route is
	// where a direct POST for a brokered forge's PAT was answered with the PAT.
	// Refusing HERE, before MintForGrant, means no transaction is opened and no
	// approval-gated grant's single-use minted_jti is consumed.
	// NOTE: handleInternalInjection (injection.go) also calls MintForGrant and is
	// deliberately NOT guarded — the sandbox has no network path to it, and
	// neither an ssh_key nor a git_pat populates Minted.Injection, so it cannot
	// return either credential.
	if kind, host, refuse := s.brokeredForgeMintKind(r.Context(), claims.RunID, body.GrantID); refuse {
		// kind == "" is the UNVERIFIABLE refusal: the grant list could not be
		// read, so the check could not run at all. It gets its own audit
		// reason and its own status — 503, because nothing about this run is
		// known to be wrong and the caller should retry — rather than the
		// single-lane 403, whose message asserts a github_token grant this
		// request never managed to observe.
		if kind == "" {
			s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "credential.mint",
				body.GrantID.String(), "denied", mustJSON(map[string]any{
					"grant_id": body.GrantID.String(),
					"reason":   reasonBrokeredForgeSingleLaneUnverifiable,
				})))
			writeErrorReason(w, http.StatusServiceUnavailable, reasonBrokeredForgeSingleLaneUnverifiable,
				"wardyn: could not read this run's grants to check the brokered-forge single-lane rule, "+
					"so the mint is refused rather than answered unchecked. Retry.")
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "credential.mint",
			body.GrantID.String(), "denied", mustJSON(map[string]any{
				"grant_id": body.GrantID.String(),
				"kind":     string(kind),
				"host":     host,
				"reason":   reasonBrokeredForgeSingleLane,
			})))
		second := "a resident SSH key gives the same run a second push path SSH makes unparseable"
		alt := "your own key"
		if kind == types.GrantGitPAT {
			second = "a resident PAT gives the same run a second push path that is an opaque CONNECT tunnel no parser can read"
			alt = "your own PAT"
		}
		writeErrorReason(w, http.StatusForbidden, reasonBrokeredForgeSingleLane, "wardyn: this run also holds a github_token grant, and a brokered forge is single-lane — "+
			"the git-broker route is its only route to "+host+" by name, so every push it carries is parsed and confined to "+
			"refs/heads/wardyn/<run-id>/, while "+second+". "+
			"Push through the git broker, or re-run from a policy without the github_token grant to push with "+alt)
		return
	}

	minted, err := s.cfg.Broker.MintForGrant(r.Context(), claims, body.GrantID)
	if err != nil {
		s.writeMintError(w, r, err)
		return
	}
	s.metrics.credentialMinted()
	writeJSON(w, http.StatusOK, mintResponse{
		Kind:       minted.Kind,
		Token:      minted.Token,
		Username:   minted.Username,
		JTI:        minted.JTI,
		ExpiresAt:  minted.ExpiresAt.UTC().Format(time.RFC3339),
		Injection:  s.mintedRule(r.Context(), claims, body.GrantID, minted),
		KnownHosts: minted.KnownHosts,
	})
}

// mintedRule is the injection rule a mint answers with. The proxy relays this
// answer into the sandbox as it is (its mint route passes status and body
// through), and nothing on that path reads the rule's secret name: the proxy
// takes its rules from its own config and resolves a value by grant id
// (handleInternalInjection). So the rule of a `shared` grant, or of a grant
// whose scope could not be read and may be one, goes out without the name —
// what an organisation's secret is called is the operator's, here as in the
// run's audit rows (sharedRefs).
func (s *Server) mintedRule(ctx context.Context, claims *identity.Claims, grantID uuid.UUID, minted broker.Minted) *egress.InjectionRule {
	if minted.Injection == nil {
		return nil
	}
	if read, _ := s.injectionReadFor(ctx, claims, grantID, minted); !read.shared && !read.unknown {
		return minted.Injection
	}
	rule := *minted.Injection
	rule.SecretName = ""
	return &rule
}

// brokeredForgeMintKind reports whether grantID is an ssh_key or git_pat grant
// for a brokered forge on a run that ALSO holds a github_token grant, returning
// the grant's kind and host for the refusal message. It is the mint-time sibling of
// validateGrantLaneExclusivity (policy.go) and carries the SAME ceiling that
// comment already states: the run's real broker map (GitGrants) is built
// in-memory on the create path and handed straight to dispatchRun in the same
// request — it reaches no store and no persisted type — so "brokered" means only
// "a github_token grant exists on this run". That is strictly broader than
// dispatch's brokered-ness — it can OVER-refuse (a github_token whose scope.repos
// and whose run's clone set never touched github.com) and never under-refuse,
// which is the correct direction for a defence-in-depth check.
//
// A nil Store FAILS OPEN on purpose (every newHarness-based test runs without
// one); a LIST ERROR fails CLOSED, signalled to the caller as refuse=true with an
// EMPTY kind, which the handler answers 503 rather than the single-lane 403.
//
// The broker transaction's own authority checks (ownership, approval,
// no-widening) do not cover the case this check exists for — a policy
// STORED BEFORE validateGrantLaneExclusivity, whose ssh_key/git_pat grant row for
// a brokered forge is still mintable by anyone who learns the grant id (see the
// handler's own comment above). For exactly that residual, this check IS the only
// belt: answering it with the raw credential whenever one SELECT fails would make
// the residual re-openable for the length of a store hiccup.
//
// The nil-Store arm stays open because it is not a failure at all — it is the
// configuration in which there is no grant store to consult, and no persisted
// pre-exclusivity policy can exist to be exploited.
//
// A pre-transaction read is as authoritative as one inside the broker's FOR
// UPDATE lock: credential_grants is INSERT-ONLY (no UPDATE/DELETE anywhere in the
// tree, no status column), so the set of a run's grants is fixed at run creation.
func (s *Server) brokeredForgeMintKind(ctx context.Context, runID, grantID uuid.UUID) (kind types.GrantKind, host string, refuse bool) {
	if s.cfg.Store == nil {
		return "", "", false
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list run grants for the single-lane mint check; REFUSING the mint (this check is the only belt covering that residual)",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return "", "", true
	}
	var target *types.GrantSpec
	brokered := false
	for i := range grants {
		if grants[i].ID == grantID {
			target = &grants[i].Spec
		}
		if grants[i].Spec.Kind == types.GrantGitHubToken {
			brokered = true
		}
	}
	// Unknown or another run's grant id: fall through unguarded — MintForGrant
	// answers not-found/ownership (ErrGrantNotFound/ErrRunMismatch) and this check
	// has no business pre-empting it. A malformed scope is likewise not ours.
	if target == nil || !brokered {
		return "", "", false
	}
	// Same per-kind same-forge tests as policy-write and dispatch
	// (brokeredForgeSSHHost / brokeredForgeHost, runs_dispatch.go), so all three
	// seams refuse exactly the same set of hosts.
	switch target.Kind {
	case types.GrantSSHKey:
		if h, _, _, _, derr := sshKeyScopeFields(target.Scope); derr == nil && brokeredForgeSSHHost(h) {
			return types.GrantSSHKey, h, true
		}
	case types.GrantGitPAT:
		if sc, derr := types.DecodeGitPATScope(target.Scope); derr == nil && brokeredForgeHost(sc.Host) {
			return types.GrantGitPAT, sc.Host, true
		}
	}
	return "", "", false
}

// Mint 409-conflict "code" values, bound to the ONE home both sides of the wire
// contract read (types.MintConflict*, internal/types/mint_wire.go). These are
// bindings, not a second declaration: the literal strings live in exactly one
// place, so this block and cmd/wardyn-git-helper's cannot drift.
const (
	mintConflictPending       = types.MintConflictPending
	mintConflictDenied        = types.MintConflictDenied
	mintConflictScopeMismatch = types.MintConflictScopeMismatch
	mintConflictAlreadyMinted = types.MintConflictAlreadyMinted
)

// writeMintError maps broker errors to the documented fail-closed HTTP shape.
//
// Every 409 here carries an explicit "code" field
// (mintConflictCode*) alongside the historical shape (approval_id/denied/
// reason kept for the two callers that already read them), so
// cmd/wardyn-git-helper's callMint can tell pending, denied, scope_mismatch,
// and already_minted apart — including the SECOND git operation of an
// approval-gated run, which legitimately 409s with ErrAlreadyMinted
// (docs/adoption/corp-network-onboarding-findings.md B2).
func (s *Server) writeMintError(w http.ResponseWriter, r *http.Request, err error) {
	var pending broker.ErrApprovalPending
	if errors.As(err, &pending) {
		// Approval still open: 409 with the approval id so the caller can poll.
		writeJSON(w, http.StatusConflict, map[string]any{"code": mintConflictPending, "approval_id": pending.ApprovalID})
		return
	}
	var denied broker.ErrApprovalDenied
	if errors.As(err, &denied) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": mintConflictDenied, "approval_id": denied.ApprovalID, "denied": true, "reason": denied.Reason,
		})
		return
	}
	switch {
	case errors.Is(err, broker.ErrRunMismatch):
		writeErrorReason(w, http.StatusForbidden, reasonGrantRunMismatch, "caller run does not own this grant")
	case errors.Is(err, broker.ErrGrantNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonGrantNotFound, "grant not found")
	case errors.Is(err, broker.ErrRequiresSPIRE):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonGrantRequiresSPIRE, "grant requires the spire identity provider")
	case errors.Is(err, broker.ErrScopeMismatch):
		writeJSON(w, http.StatusConflict, map[string]any{"code": mintConflictScopeMismatch, "error": "requested scope does not match grant (no-widening)"})
	case errors.Is(err, broker.ErrAlreadyMinted):
		writeJSON(w, http.StatusConflict, map[string]any{"code": mintConflictAlreadyMinted, "error": "credential already minted (single-use)"})
	case mintUnreachable(err):
		// Transient, like the store's own outage: a run's proxy rides it out
		// on its last-good header (K8) instead of dropping it at once. The
		// SAME reason the credential-injection sinks use for the identical shape.
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSinkStoreUnavailable, sinkStoreUnreachable)
	default:
		writeServerError(w, r, "mint", err)
	}
}

// mintUnreachable reports whether a mint failed because nothing answered: a
// connection that could not be made or was lost, or a timeout. A database
// that answered with an error (a *pgconn.PgError) is not one.
func mintUnreachable(err error) bool {
	var connErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connErr) || errors.As(err, &netErr) || pgconn.Timeout(err)
}

// tokenRenewResponse is the POST /api/v1/internal/token/renew success body: a
// FRESH run token carrying the same short TTL, plus its expiry so the caller can
// schedule the next renew before this one lapses.
type tokenRenewResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// handleInternalTokenRenew issues a FRESH run token to a caller presenting a
// still-valid one for the same run — the missing producer for the per-run
// internal-audience token.
//
// Why this exists: a run token is minted ONCE at dispatch with a 1h TTL and
// handed to the per-run proxy sidecar in its config env. A process env is fixed
// after exec, so with no renew producer EVERY run outliving that TTL began
// getting 401s on /internal/* — silently losing credential mints, approvals,
// decision-log posts and subscription re-resolves, with no recovery. (The
// ground-truth sensor already had a producer — see wardynd's token rotator —
// but the per-run internal token had no counterpart.)
//
// The fix is a renew route, not a longer TTL. The short TTL is the security
// property, not the bug: it forces a re-authorization checkpoint roughly every
// half-life, at which revocation and run state are re-checked. Raising it would
// trade away exactly the invariant the design rests on, so tokenTTL stays put
// and the token is instead re-issued while authority still holds.
//
// No new credential: renewal is authenticated by the CURRENT, still-valid run
// token, so the caller (the out-of-sandbox proxy, already the token's sole
// holder) needs no additional long-lived secret. The sandbox never holds a run
// token at all and gains nothing here: the brokered local routes forward only
// mint/approvals/recordings, never this route.
//
// Fail closed, twice over:
//  1. internalAuth has already verified signature, expiry, audience AND the
//     identity revocation list (a RevocationStore error is itself treated as
//     revoked). Revocation is RUN-scoped: the kill cascade's Identity.RevokeRun
//     marks the whole run, so every token of that run — including one renewed
//     after the kill — fails Verify. A revoked run can never reach this handler.
//  2. Independently, the run's own state is re-read here and a TERMINAL run is
//     refused. This is NOT redundant with (1): revokeRunCascade is best-effort
//     (its error is audited, never propagated), so a run that went terminal while
//     its revocation write failed would still present a verifiable token. This
//     gate closes that window, which is precisely the one that matters — a run
//     whose authority ended but whose denial did not land.
//
// A renewed token is a NEW jti with a NEW TTL. The presented token is left to
// expire on its own rather than being revoked, so a request already in flight
// with it is never broken by a concurrent renew.
func (s *Server) handleInternalTokenRenew(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	// Fail closed: with no run store we cannot prove the run is still alive, so
	// we refuse rather than renew on an unverifiable authority.
	if s.cfg.Store == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewStoreUnavailable, "run store not configured")
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 403, not 404: same reason as refuseTerminalRun — the caller's own
			// presented token names claims.RunID, so a missing run is that token's
			// authority gone, not a path a member could probe. The SAME wire
			// reason notFoundIf's own "run" case uses, at a different status.
			s.auditRenewDenied(r, claims, reasonRunNotFound)
			writeErrorReason(w, http.StatusForbidden, reasonRunNotFound, "run not found")
			return
		}
		// Transient store failure: refuse (fail closed) but signal retryable, so a
		// Postgres blip costs a renew attempt and not the run's credentials.
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewReadFailed, loggedMsg(r.Context(), "read run", err))
		return
	}
	if isTerminalRunState(run.State) {
		s.auditRenewDenied(r, claims, "run_terminal:"+string(run.State))
		// The SAME registered reason refuseTerminalRun's own terminal arm writes.
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunTerminal), "run is terminal")
		return
	}
	// A kept run (ended or lost) has no proxy on purpose; only a revive gives
	// it one, with a token of its own. Nothing may carry its identity forward.
	if runIsKept(run) {
		s.auditRenewDenied(r, claims, "run_lost:"+string(run.LostReason))
		// The SAME registered reason refuseTerminalRun's own kept-run arm writes.
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunKept), "run is lost")
		return
	}

	id, err := s.cfg.Identity.MintRunIdentity(r.Context(), claims.RunID, claims.Sub, claims.Sponsor, internalAudience, claims.OperatorOwned)
	if err != nil {
		writeServerError(w, r, "renew run identity", err)
		return
	}
	// The stamp is what the lapsed-token sweep reads (run_lost.go), so it is
	// written after the mint and the token is handed out only once it lands: a
	// stamp can then never be older than the token the proxy holds. A failed
	// stamp is retryable, like a store blip on the read above; a run marked
	// lost since that read is refused.
	if loser, ok := s.cfg.Store.(store.RunLoser); ok {
		stamped, serr := loser.StampRunTokenRenewed(r.Context(), claims.RunID)
		if serr != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewStampFailed, loggedMsg(r.Context(), "stamp run token renewed", serr))
			return
		}
		if !stamped {
			s.auditRenewDenied(r, claims, "run_lost")
			writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunKept), "run is lost")
			return
		}
	}

	// Honest trail: the provider records its own identity.mint for the new token;
	// this SEPARATE identity.renew names the run, the retiring jti and the fresh
	// one, so a long run reads as an explicit chain of re-authorized ~1h segments
	// rather than as an unexplained second mint out of nowhere.
	ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"identity.renew", id.JTI, "success", mustJSON(map[string]any{
			"prev_jti":   claims.JTI,
			"expires_at": id.Expiry.UTC().Format(time.RFC3339),
			"run_state":  run.State,
		}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)

	writeJSON(w, http.StatusOK, tokenRenewResponse{
		Token:     id.Token,
		ExpiresAt: id.Expiry.UTC().Format(time.RFC3339),
	})
}

// auditRenewDenied records a REFUSED renew. A token asking to outlive its run's
// authority is security-relevant whether it is a dead sidecar or an attacker
// replaying a stolen token, so the refusal is never silent.
func (s *Server) auditRenewDenied(r *http.Request, claims *identity.Claims, reason string) {
	ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"identity.renew", claims.JTI, "denied", mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// decisionOutcome maps an egress decision to an audit outcome. Only a Deny is a
// refusal: an Allow proceeded, and a Pending is held for a human rather than
// turned away, so both record as a success.
func decisionOutcome(d egress.Decision) string {
	if d == egress.Deny {
		return "denied"
	}
	return "success"
}
