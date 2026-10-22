// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// CLI sign-in (wardyn login), the server half: the CLI asks for a sign-in, the person approves it in
// the console, and the CLI redeems the approval for a CLI session (cli_sessions.go). Off unless the
// operator sets WARDYN_CLI_SIGN_IN; it needs SSO, because the approval is a browser session's.
//
// The CLI never talks to the identity provider. The request row holds no secret: the PKCE S256
// challenge, the device key's public half, and, once approved, the hash of the single-use code K.
// Approve runs mintGuards, the token call mints through prepareMint from the approval's snapshot with
// the same late revocation re-check, so a CLI sign-in reaches no further than a personal token would.
package api

import (
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CLI session lifetimes: the defaults and the bounds boot enforces on WARDYN_CLI_SESSION_TTL.
const (
	DefaultCLISessionTTL  = 30 * 24 * time.Hour
	DefaultCLISessionIdle = 7 * 24 * time.Hour
	MinCLISessionTTL      = time.Hour
	MaxCLISessionTTL      = 90 * 24 * time.Hour
)

// cliSignInMaxPending bounds the sign-in requests waiting at once, deployment-wide, so the anonymous
// create cannot fill the table faster than requests expire.
const cliSignInMaxPending = 1000

// cliMachineMaxLen bounds the computer name the approve page shows and the token names carry.
const cliMachineMaxLen = 100

// CLISignInConfig is CLI sign-in's operator settings (docs/ENV.md). The zero value is off.
type CLISignInConfig struct {
	// Enabled is WARDYN_CLI_SIGN_IN. The routes mount, and /healthz.cli_sign_in is true, only when it
	// is on and SSO is configured.
	Enabled bool
	// Paste is WARDYN_CLI_PASTE_SIGN_IN: browserless paste-back sign-in (wardyn login --no-browser).
	Paste bool
	// FileStoreOff is WARDYN_CLI_FILE_STORE=off: refuse a CLI that reports the file store.
	FileStoreOff bool
	// SessionTTL and SessionIdle are WARDYN_CLI_SESSION_TTL and WARDYN_CLI_SESSION_IDLE; zero is the
	// default. Boot bounds them (MinCLISessionTTL..MaxCLISessionTTL, idle at most the TTL).
	SessionTTL, SessionIdle time.Duration
	// Origin is the organisation URL a device proof signs (types.CLIProofMessage): the SSO redirect
	// URL's scheme, host and base path, which is where the CLI signed in.
	Origin string
}

// sessionTTL is a session's absolute lifetime for store: the file store is capped at 7 days.
func (c CLISignInConfig) sessionTTL(st string) time.Duration {
	ttl := cmp.Or(c.SessionTTL, DefaultCLISessionTTL)
	if st == types.CLIStoreFile {
		ttl = min(ttl, types.CLIFileStoreSessionCap)
	}
	return ttl
}

// sessionIdle is a session's idle lifetime for store, never above its absolute one.
func (c CLISignInConfig) sessionIdle(st string) time.Duration {
	return min(cmp.Or(c.SessionIdle, DefaultCLISessionIdle), c.sessionTTL(st))
}

// cliSignInState is CLI sign-in's process-local rate limiters. Anonymous creates and token calls share
// one bucket per client address; a refresh spends one bucket per address and one per session.
type cliSignInState struct {
	cliPeerLimiter, cliRefreshPeerLimiter, cliRefreshSessionLimiter principalLimiter
}

func newCLISignInState() cliSignInState {
	return cliSignInState{
		cliPeerLimiter:           principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers},
		cliRefreshPeerLimiter:    principalLimiter{rate: 60.0 / 3600, burst: 30, max: enrolLimiterMaxPeers},
		cliRefreshSessionLimiter: principalLimiter{rate: 10.0 / 3600, burst: 10, max: enrolLimiterMaxPeers},
	}
}

// cliStore is the CLI sign-in store, or nil when CLI sign-in is off or the store cannot hold it.
func (s *Server) cliStore() store.CLISignInStore {
	if !s.cfg.CLISignIn.Enabled || s.cfg.OIDC == nil {
		return nil
	}
	cs, _ := s.cfg.Store.(store.CLISignInStore)
	return cs
}

// mountCLISignInRoutes mounts nothing while CLI sign-in is off, so every route is a 404:
//
//	POST   /cli/sign-ins                    anonymous: the CLI asks for a sign-in
//	POST   /cli/sign-ins/{id}/token         anonymous: code, verifier and device proof in, a session out
//	POST   /cli/sessions/{id}/refresh       anonymous: secret and device proof in, a rotated session out
//	GET    /me/cli-sign-ins/{id}            the approve page's data
//	POST   /me/cli-sign-ins/{id}/approve    Approve (a browser session; mintGuards)
//	POST   /me/cli-sign-ins/{id}/deny       Deny
//	GET    /me/cli-sessions                 Signed-in CLIs
//	DELETE /me/cli-sessions/{id}            Sign out one computer (also wardyn logout)
func (s *Server) mountCLISignInRoutes(r chi.Router) {
	if s.cliStore() == nil {
		return
	}
	r.Post("/cli/sign-ins", s.handleCreateCLISignIn)
	r.Post("/cli/sign-ins/{id}/token", s.handleCLISignInToken)
	r.Post("/cli/sessions/{id}/refresh", s.handleCLISessionRefresh)
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth, s.expectedPrincipalGuard)
		r.Get("/me/cli-sign-ins/{id}", s.handleGetCLISignIn)
		r.Post("/me/cli-sign-ins/{id}/approve", s.handleApproveCLISignIn)
		r.Post("/me/cli-sign-ins/{id}/deny", s.handleDenyCLISignIn)
		r.Get("/me/cli-sessions", s.handleListCLISessions)
		r.Delete("/me/cli-sessions/{id}", s.handleEndCLISession)
	})
}

// requestAddress is the client address as realip.go resolved it (r.RemoteAddr after
// trustedProxyClient), without the port.
func requestAddress(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// userCodeAlphabet leaves out 0/O, 1/I/L and U, which read alike or spell words.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// newUserCode is the code the terminal prints and the approve page repeats, XXXX-XXXX.
func newUserCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// cliSignInRequestRefusal is the 400 sentence for a malformed create body, by field.
func cliSignInRequestRefusal(req types.CLISignInCreateRequest) string {
	if c, err := base64.RawURLEncoding.DecodeString(req.Challenge); err != nil || len(c) != sha256.Size {
		return "challenge: must be base64url(SHA-256(verifier)) with no padding"
	}
	if k, err := base64.RawURLEncoding.DecodeString(req.PublicKey); err != nil || len(k) != ed25519.PublicKeySize {
		return "public_key: must be an Ed25519 public key, base64url with no padding"
	}
	if m := strings.TrimSpace(req.Machine); m == "" || len(m) > cliMachineMaxLen || !controlCharFree(m) {
		return fmt.Sprintf("machine: must be 1 to %d printable characters", cliMachineMaxLen)
	}
	if !slices.Contains(types.CLISignInModes, req.Mode) {
		return `mode: must be "loopback" or "paste"`
	}
	if (req.Mode == types.CLISignInModeLoopback) != (req.Port >= 1 && req.Port <= 65535) || (req.Mode == types.CLISignInModePaste && req.Port != 0) {
		return "port: a loopback sign-in needs its port (1 to 65535) and a paste sign-in has none"
	}
	if !slices.Contains(types.CLIStores, req.Store) {
		return `store: must be "keychain" or "file"`
	}
	if req.Admin != (req.SessionID != nil) {
		return "session_id: an admin sign-in names the CLI session on this computer, and only an admin sign-in does"
	}
	return ""
}

// handleCreateCLISignIn is POST /api/v1/cli/sign-ins, anonymous: rate-limited per client address and
// capped deployment-wide (cliSignInMaxPending).
func (s *Server) handleCreateCLISignIn(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	if !s.cliPeerLimiter.allow(peerKey(r.RemoteAddr), now) {
		w.Header().Set("Retry-After", "10")
		writeErrorReason(w, http.StatusTooManyRequests, reasonCLISignInRateLimited, "too many sign-in attempts from this address; retry later")
		return
	}
	var req types.CLISignInCreateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if msg := cliSignInRequestRefusal(req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonCLISignInInvalidRequest, msg)
		return
	}
	if req.Mode == types.CLISignInModePaste && !s.cfg.CLISignIn.Paste {
		writeErrorReason(w, http.StatusForbidden, reasonCLIPasteSignInOff,
			"this organisation has not turned on sign-in without a browser (WARDYN_CLI_PASTE_SIGN_IN): sign in on a computer with a browser, or set WARDYN_TOKEN")
		return
	}
	if req.Store == types.CLIStoreFile && s.cfg.CLISignIn.FileStoreOff {
		writeErrorReason(w, http.StatusForbidden, reasonCLIFileStoreOff,
			"this organisation does not allow a CLI sign-in kept in a file (WARDYN_CLI_FILE_STORE=off), and this computer has no keychain: set WARDYN_TOKEN instead")
		return
	}
	key, _ := base64.RawURLEncoding.DecodeString(req.PublicKey)
	si := store.CLISignIn{ID: uuid.New(), UserCode: newUserCode(), Challenge: req.Challenge, Port: req.Port,
		Machine: strings.TrimSpace(req.Machine), PublicKey: key, Mode: req.Mode, Store: req.Store, Admin: req.Admin,
		SessionID: req.SessionID, RequestAddr: requestAddress(r)}
	created, err := s.cliStore().CreateCLISignIn(r.Context(), si, cliSignInMaxPending, now)
	if err != nil {
		writeServerError(w, r, "create cli sign-in", err)
		return
	}
	if !created {
		w.Header().Set("Retry-After", "60")
		writeErrorReason(w, http.StatusTooManyRequests, reasonCLISignInPendingCap, "too many sign-ins are waiting for approval; retry later")
		return
	}
	ev := s.auditEvent(nil, types.ActorSystem, "cli-sign-in:"+si.ID.String(), "cli.sign_in.request", si.ID.String(), "success",
		mustJSON(map[string]any{"machine": si.Machine, "mode": si.Mode, "store": si.Store, "admin": si.Admin, "request_address": si.RequestAddr}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	writeJSON(w, http.StatusCreated, types.CLISignInCreateResponse{ID: si.ID, UserCode: si.UserCode,
		ExpiresIn: int(types.CLISignInRequestLife / time.Second)})
}

// pendingCLISignIn loads {id} for the approve page and its two decisions: 404 when unknown, 410 with
// its reason once used or decided (cli_sign_in_used) or expired (cli_sign_in_expired).
func (s *Server) pendingCLISignIn(w http.ResponseWriter, r *http.Request) (store.CLISignIn, bool) {
	id, ok := parseIDParam(w, r, "id", "sign-in")
	if !ok {
		return store.CLISignIn{}, false
	}
	si, err := s.cliStore().GetCLISignIn(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonCLISignInNotFound, "no such sign-in: run wardyn login again")
		return store.CLISignIn{}, false
	}
	if err != nil {
		writeServerError(w, r, "read cli sign-in", err)
		return store.CLISignIn{}, false
	}
	switch {
	case si.Decision != "":
		writeErrorReason(w, http.StatusGone, reasonCLISignInUsed, "this sign-in link was already used: run wardyn login again")
		return store.CLISignIn{}, false
	case !s.cfg.Now().Before(si.ExpiresAt):
		writeErrorReason(w, http.StatusGone, reasonCLISignInExpired, "this sign-in link expired: run wardyn login again")
		return store.CLISignIn{}, false
	}
	return si, true
}

// cliAccessLife is the access (and admin) token lifetime: 1 hour, or WARDYN_API_TOKEN_MAX_TTL when
// that is shorter.
func (s *Server) cliAccessLife() time.Duration {
	if m := s.cfg.APITokenMaxTTL; m > 0 && m < types.CLIAccessTokenLife {
		return m
	}
	return types.CLIAccessTokenLife
}

// handleGetCLISignIn is GET /api/v1/me/cli-sign-ins/{id}: the approve page's data, with the operator's
// actual lifetimes and both addresses.
func (s *Server) handleGetCLISignIn(w http.ResponseWriter, r *http.Request) {
	if oidcHumanFromContext(r.Context()) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	si, ok := s.pendingCLISignIn(w, r)
	if !ok {
		return
	}
	cfg := s.cfg.CLISignIn
	approver := requestAddress(r)
	writeJSON(w, http.StatusOK, types.CLISignInView{ID: si.ID, Machine: si.Machine, UserCode: si.UserCode, Mode: si.Mode,
		Store: si.Store, Admin: si.Admin, RequestAddress: si.RequestAddr, ApproverAddress: approver,
		AddressMatch: si.RequestAddr == approver, CreatedAt: si.CreatedAt, ExpiresAt: si.ExpiresAt,
		SessionTTLSeconds: int64(cfg.sessionTTL(si.Store) / time.Second), SessionIdleSeconds: int64(cfg.sessionIdle(si.Store) / time.Second),
		AccessTTLSeconds: int64(s.cliAccessLife() / time.Second)})
}

// handleApproveCLISignIn is POST /api/v1/me/cli-sign-ins/{id}/approve. It runs mintGuards unchanged and
// records the approver's identity snapshot, which the token call mints from.
func (s *Server) handleApproveCLISignIn(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	sub, ok := s.mintGuards(w, r, "cli.sign_ins")
	if !ok {
		return
	}
	si, ok := s.pendingCLISignIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if si.Admin && !s.cliAdminSessionOK(w, r, si, sub) {
		return
	}
	issue, err := s.mintIssue(ctx, sub, authorizedAt)
	if err != nil {
		writeServerError(w, r, "approve cli sign-in", err)
		return
	}
	epoch, _ := oidc.AuthorityEpochFromContext(ctx)
	truncated := oidcGroupsTruncatedFromContext(ctx)
	code := newCLICode()
	life := types.CLISignInLoopbackCode
	if si.Mode == types.CLISignInModePaste {
		life = types.CLISignInPasteCode
	}
	_, err = s.cliStore().DecideCLISignIn(ctx, si.ID, store.CLISignInDecision{Decision: types.CLISignInApproved, Now: authorizedAt,
		AuthorizedAt: authorizedAt, Code: code, CodeExpiresAt: authorizedAt.Add(life),
		Approver: store.CLIIdentity{Principal: sub, Email: oidcEmailFromContext(ctx), Role: cmp.Or(oidcRoleFromContext(ctx), oidc.RoleUser),
			UserType: cmp.Or(oidcUserTypeFromContext(ctx), types.UserTypeStandard), Groups: oidcGroupsFromContext(ctx),
			GroupsTruncated: &truncated, RevSeq: issue.Seq, AuthorityEpoch: epoch}})
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusGone, reasonCLISignInUsed, "this sign-in link was already used: run wardyn login again")
		return
	}
	if err != nil {
		writeServerError(w, r, "approve cli sign-in", err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "cli.sign_in.approve", si.ID.String(), "success",
		mustJSON(cliSignInAuditData(si, requestAddress(r)))))
	resp := types.CLISignInApproveResponse{Code: code, CodeExpiresIn: int(life / time.Second)}
	if si.Mode == types.CLISignInModeLoopback {
		resp.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback?id=%s&code=%s", si.Port, si.ID, code)
	}
	writeJSON(w, http.StatusOK, resp)
}

// cliAdminSessionOK is the step-up's own check: the request names a live CLI session of the approver
// whose device key is the one this request carries. False after writing the 409.
func (s *Server) cliAdminSessionOK(w http.ResponseWriter, r *http.Request, si store.CLISignIn, sub string) bool {
	sess, err := s.cliStore().GetCLISession(r.Context(), *si.SessionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read cli session", err)
		return false
	}
	if err != nil || sess.EndedAt != nil || !s.cfg.Now().Before(sess.ExpiresAt) || sess.Identity.Principal != sub ||
		subtle.ConstantTimeCompare(sess.PublicKey, si.PublicKey) != 1 {
		writeErrorReason(w, http.StatusConflict, reasonCLIAdminNeedsSession,
			"admin access needs a live CLI sign-in of yours on that computer: run wardyn login there first")
		return false
	}
	return true
}

// cliSignInAuditData is the data of the approve and deny rows.
func cliSignInAuditData(si store.CLISignIn, approver string) map[string]any {
	return map[string]any{"machine": si.Machine, "mode": si.Mode, "store": si.Store, "admin": si.Admin,
		"request_address": si.RequestAddr, "approver_address": approver, "address_match": si.RequestAddr == approver}
}

// newCLICode is K: 128 random bits, hex. It is useless without the verifier and the device key.
func newCLICode() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handleDenyCLISignIn is POST /api/v1/me/cli-sign-ins/{id}/deny.
func (s *Server) handleDenyCLISignIn(w http.ResponseWriter, r *http.Request) {
	if oidcHumanFromContext(r.Context()) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	si, ok := s.pendingCLISignIn(w, r)
	if !ok {
		return
	}
	_, err := s.cliStore().DecideCLISignIn(r.Context(), si.ID, store.CLISignInDecision{Decision: types.CLISignInDenied, Now: s.cfg.Now().UTC()})
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusGone, reasonCLISignInUsed, "this sign-in link was already used: run wardyn login again")
		return
	}
	if err != nil {
		writeServerError(w, r, "deny cli sign-in", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "cli.sign_in.deny", si.ID.String(), "success",
		mustJSON(cliSignInAuditData(si, requestAddress(r)))))
	var resp types.CLISignInDenyResponse
	if si.Mode == types.CLISignInModeLoopback {
		resp.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback?id=%s&error=denied", si.Port, si.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// cliSignInInvalid is the one 401 the token call gives for every refusal about the request, the code,
// the verifier or the proof, so it is not an oracle for which of them was wrong.
func cliSignInInvalid(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusUnauthorized, reasonCLISignInInvalid, "this sign-in cannot be completed: run wardyn login again")
}

// s256Matches is PKCE S256: base64url(SHA-256(verifier)) == challenge, compared in constant time.
func s256Matches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return verifier != "" && subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// handleCLISignInToken is POST /api/v1/cli/sign-ins/{id}/token, anonymous: K, the verifier and a
// device proof in; a CLI session and its first access token, or a cli_admin token, out. K is spent
// only after the verifier and the proof check, so a wrong guess cannot burn someone else's code.
func (s *Server) handleCLISignInToken(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	if !s.cliPeerLimiter.allow(peerKey(r.RemoteAddr), now) {
		w.Header().Set("Retry-After", "10")
		writeErrorReason(w, http.StatusTooManyRequests, reasonCLISignInRateLimited, "too many sign-in attempts from this address; retry later")
		return
	}
	id, ok := parseIDParam(w, r, "id", "sign-in")
	if !ok {
		return
	}
	var req types.CLISignInTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	cs := s.cliStore()
	si, err := cs.GetCLISignIn(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read cli sign-in", err)
		return
	}
	if err != nil || !s256Matches(req.Verifier, si.Challenge) ||
		!s.cliProofValid(si.PublicKey, "/api/v1/cli/sign-ins/"+id.String()+"/token", id, req.Code, req.Proof, now) {
		cliSignInInvalid(w)
		return
	}
	si, err = cs.RedeemCLISignIn(r.Context(), id, req.Code, now)
	if errors.Is(err, store.ErrNotFound) {
		cliSignInInvalid(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "redeem cli sign-in", err)
		return
	}
	if si.Admin {
		s.mintCLIAdmin(w, r, si)
		return
	}
	s.createCLISession(w, r, si)
}
