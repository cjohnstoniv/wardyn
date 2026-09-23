// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_entra_store.go owns the STORED half of a per-user Azure DevOps sign-in:
// the blob, its per-principal namespace discipline, and the control-plane-owned
// redemption that turns its refresh token into a narrowed access token.
//
// WHY THE CONTROL PLANE REDEEMS. Entra ROTATES the refresh token on every
// redemption: the response carries a new one and the presented one stops
// working. Anything that redeems and then dies — a sandbox with an ephemeral
// home directory, a per-request helper — loses the rotated token while the
// stored one is already spent, which is one heal followed by a permanently dead
// credential that setup status keeps calling renewable. Wardyn is the only party
// that can PERSIST the rotation, so Wardyn is the only party that may redeem
// it: one refresher per credential, single-flight per owner. That is the same
// single-owner rule the captured AWS SSO lane already states for itself.
//
// WHY THE READ LISTS FIRST. Store.For(owner).Get FALLS BACK to the operator's
// row by contract, so the obvious For(owner).Get would serve an ADMIN's
// captured Azure DevOps session to a member who has captured nothing — the
// exact substitution a per-person credential exists to refuse. Every read here
// lists the owner's own namespace first and returns absent unless the name is
// in it, byte-for-byte the discipline the AWS SSO read follows.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoEntraProviderPrefix is the reserved-name segment identifying this lane's
// blobs. The full name is adoEntraSecretName's, and it lands inside the
// `wardyn-harness-*-oauth` PATTERN reservedSecret already seals — so the
// generic secrets API cannot read, overwrite or delete one, and no credential
// sink will resolve it as a raw value, without a new entry anywhere.
const adoEntraProviderPrefix = "ado"

// adoEntraRowIDPattern is the grammar a provider row id must satisfy before it
// is concatenated into a secret name. A row id is operator-supplied data, and a
// name is a key in a shared namespace: an unchecked one could collide with
// a different reserved name (a trailing "-oauth", an embedded
// "wardyn-harness-") or carry whitespace a store key must not hold. Checked at
// every door rather than trusted from the row.
var adoEntraRowIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// adoEntraRedeemTimeout bounds ONE token request. A caller is waiting on it, so
// an unresponsive authority must fail the lane rather than hang the caller.
const adoEntraRedeemTimeout = 10 * time.Second

// adoEntraMaxScopes is how many scopes one request may name. It is a bound on a
// caller-supplied slice that is joined into a request, not a policy: the
// ceiling itself is the provider row's, and this only stops an unbounded list
// from being composed into a URL.
const adoEntraMaxScopes = 32

// The Entra error identifiers this lane classifies on. The OAuth `error` code
// alone is not enough: a consent refusal arrives as `invalid_grant` on some
// paths and `consent_required` on others, and only the AADSTS number in the
// description tells them apart. Treating a consent refusal as a dead credential
// would sign a whole estate out over an admin toggling one app permission.
const (
	// entraConsentRequiredCode is "the user or administrator has not consented
	// to use the application" — the scope was never granted, or the grant was
	// revoked. The remedy is a new interactive sign-in, not a retry.
	entraConsentRequiredCode = "AADSTS65001"
	// entraOAuthConsentRequired / entraOAuthInteractionRequired are the two
	// OAuth codes with the same meanings.
	entraOAuthConsentRequired     = "consent_required"
	entraOAuthInteractionRequired = "interaction_required"
)

// The four failure classes a caller answers differently, as sentinels so a
// caller can switch on them with errors.Is and a log line still names the code
// the authority sent.
//
// They are DISTINCT because the answers are: a dead credential must be
// re-captured, a consent refusal needs an admin or a fresh consent prompt, an
// interaction refusal needs the human at a keyboard on a compliant device, and
// an unavailable authority needs nothing but a retry. Collapsing any pair of
// them turns a transient outage into a fleet-wide re-sign-in.
var (
	// ErrADOEntraNotCaptured: this principal has no stored Azure DevOps
	// sign-in. Distinct from a dead one — nothing was ever there.
	ErrADOEntraNotCaptured = errors.New("no Azure DevOps sign-in has been captured for this principal")
	// ErrADOEntraDeadCredential: the refresh token is gone (revoked, expired,
	// or already rotated away). Nothing Wardyn can do renews it.
	ErrADOEntraDeadCredential = errors.New("the captured Azure DevOps sign-in can no longer be renewed")
	// ErrADOEntraConsentRequired: the app is not consented for what was asked.
	ErrADOEntraConsentRequired = errors.New("access to Azure DevOps requires consent that has not been granted")
	// ErrADOEntraInteractionRequired: a Conditional Access policy wants the
	// human present. A control-plane renewal structurally cannot satisfy it.
	ErrADOEntraInteractionRequired = errors.New("access to Azure DevOps requires interactive sign-in")
	// ErrADOEntraUnavailable: the request did not complete, or completed
	// unusably. The credential is untouched and the next attempt redeems it
	// normally.
	ErrADOEntraUnavailable = errors.New("renewing the captured Azure DevOps sign-in did not complete")
)

// ADOEntraFailure is the same four classes as a machine-readable label, for a
// caller that puts the class in a refusal body or an audit row rather than
// branching on it. "" means no failure.
type ADOEntraFailure string

const (
	ADOEntraFailureNone                ADOEntraFailure = ""
	ADOEntraFailureNotCaptured         ADOEntraFailure = "not_captured"
	ADOEntraFailureDeadCredential      ADOEntraFailure = "dead_credential"
	ADOEntraFailureConsentRequired     ADOEntraFailure = "consent_required"
	ADOEntraFailureInteractionRequired ADOEntraFailure = "interaction_required"
	ADOEntraFailureUnavailable         ADOEntraFailure = "unavailable"
)

// ADOEntraClassify maps an error from this lane onto its class. An error from
// somewhere else classifies as unavailable — the one class that means "nothing
// about the credential is known to be wrong", which is the safe reading of an
// error this function does not recognise.
func ADOEntraClassify(err error) ADOEntraFailure {
	switch {
	case err == nil:
		return ADOEntraFailureNone
	case errors.Is(err, ErrADOEntraNotCaptured):
		return ADOEntraFailureNotCaptured
	case errors.Is(err, ErrADOEntraDeadCredential):
		return ADOEntraFailureDeadCredential
	case errors.Is(err, ErrADOEntraConsentRequired):
		return ADOEntraFailureConsentRequired
	case errors.Is(err, ErrADOEntraInteractionRequired):
		return ADOEntraFailureInteractionRequired
	default:
		return ADOEntraFailureUnavailable
	}
}

// adoEntraBlob is the stored credential. It holds NO access token: access
// tokens are minted per use, for the narrowest scope subset the caller needs,
// and storing one would only add a second thing to leak and to keep fresh.
//
// Scopes is the CONSENTED ceiling this sign-in was captured with, not the last
// subset redeemed against it: narrowing one redemption never narrows the
// consent behind it, so the set has to survive.
type adoEntraBlob struct {
	RefreshToken string `json:"refresh_token"`
	// Scopes is the granted scope set the capture's own token response
	// reported — what the authority SAID it gave, never what was requested.
	Scopes []string `json:"scopes"`
	// ExpiresAt is the expiry of the access token the last successful exchange
	// returned. It is a FRESHNESS signal, not the refresh token's own lifetime
	// (Entra publishes none): a credential whose ExpiresAt is long past has
	// simply not been used lately, which is not the same as dead.
	ExpiresAt time.Time `json:"expires_at"`
	TenantID  string    `json:"tenant_id"`
	ClientID  string    `json:"client_id"`
	// Subject is the id_token subject the capture bound this credential to —
	// the session subject it matched. Stored so a later read can prove the
	// binding still holds without another sign-in.
	Subject    string    `json:"subject"`
	CapturedAt time.Time `json:"captured_at"`
	// Source names HOW this credential arrived, so a review can tell a
	// browser sign-in from anything added later. One value today.
	Source string `json:"source"`
	// RenewedAt is when the stored refresh token was last rotated. Absent on a
	// credential that has never been redeemed.
	RenewedAt time.Time `json:"renewed_at,omitempty"`
	// DeadAt is when a renewal last met a refusal no renewal gets past: the
	// refresh token is gone, or a Conditional Access policy wants the person
	// present. It lives on the blob, not in a table, so /me/scm-access and the
	// launch gate can say expired_signin before the next run fails at its
	// sidecar's boot. A fresh capture writes a blob without it; a renewal that
	// succeeds later clears it.
	DeadAt time.Time `json:"dead_at,omitzero"`
	// DeadReason is the class that set DeadAt (dead_credential or
	// interaction_required).
	DeadReason ADOEntraFailure `json:"dead_reason,omitempty"`
}

// signInEnded reports whether the last renewal found this sign-in unusable
// until the person signs in again.
func (b adoEntraBlob) signInEnded() bool { return !b.DeadAt.IsZero() }

// adoEntraSourceSignIn is the only capture source in 0.7.10: the interactive
// browser sign-in this package serves.
const adoEntraSourceSignIn = "signin"

// valid is the SHAPE check every read applies: a blob missing any of these can
// never redeem, so serving it as "connected" would report access this
// deployment does not have. Not authentication — the authority is the only
// oracle for that.
func (b adoEntraBlob) valid() bool {
	return b.RefreshToken != "" && len(b.Scopes) > 0 && b.TenantID != "" && b.ClientID != "" && b.Subject != ""
}

// adoEntraSecretName is the reserved store name holding one provider row's
// captured sign-in. The row id is in the name because a deployment may offer
// more than one Azure DevOps organisation, each its own app registration and
// its own consent — one name per row, so two rows can never overwrite each
// other's credential.
//
// Callers must have validated rowID (adoEntraValidRowID) first; an invalid one
// panics rather than composing a name, because every door checks and a
// composed name is a key in a namespace shared with every other reserved
// secret.
func adoEntraSecretName(rowID string) string {
	if !adoEntraValidRowID(rowID) {
		panic("api: adoEntraSecretName called with an unvalidated provider row id")
	}
	return "wardyn-harness-" + adoEntraProviderPrefix + "-" + rowID + "-oauth"
}

// adoEntraValidRowID reports whether rowID may be composed into a secret name.
func adoEntraValidRowID(rowID string) bool {
	if !adoEntraRowIDPattern.MatchString(rowID) {
		return false
	}
	// A row id that itself ends in the reserved suffix, or carries the reserved
	// prefix, could be spelled so that two different rows produce one name.
	return !strings.HasSuffix(rowID, "-oauth") && !strings.Contains(rowID, "wardyn-harness-")
}

// readADOEntraBlob loads owner's captured sign-in for one provider row.
//
// The LIST comes first and is the whole reason this is not a one-liner: the
// owner view's Get falls back to the operator's row by contract, so without the
// list an operator's captured session would be served to any member who has
// captured nothing. For("").List() is never consulted, so the owner's own rows
// are all this can see. An empty owner is refused outright rather than read as
// the operator namespace: this credential is per-person by construction, and
// there is no principal to read it for.
func (s *Server) readADOEntraBlob(ctx context.Context, owner, rowID string) (adoEntraBlob, bool, error) {
	if s.cfg.Secrets == nil || owner == "" {
		return adoEntraBlob{}, false, nil
	}
	if !adoEntraValidRowID(rowID) {
		return adoEntraBlob{}, false, fmt.Errorf("azure devops provider row id %q is not a usable store name", rowID)
	}
	name := adoEntraSecretName(rowID)
	st := s.cfg.Secrets.For(owner)
	own, err := st.List(ctx)
	if err != nil {
		return adoEntraBlob{}, false, fmt.Errorf("list own azure devops sign-in: %w", err)
	}
	if !slices.Contains(own, name) {
		return adoEntraBlob{}, false, nil
	}
	raw, err := st.Get(ctx, name)
	if errors.Is(err, secretstore.ErrNotFound) {
		return adoEntraBlob{}, false, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the captured azure devops sign-in from the secret store failed",
			slog.String("row", rowID), slog.Any("err", err))
		return adoEntraBlob{}, false, fmt.Errorf("read azure devops sign-in: %w", err)
	}
	var blob adoEntraBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return adoEntraBlob{}, false, fmt.Errorf("parse azure devops sign-in blob: %w", err)
	}
	if !blob.valid() {
		return adoEntraBlob{}, false, nil
	}
	return blob, true, nil
}

// storeADOEntraBlob persists owner's captured sign-in. Put never falls back (it
// is scoped to the view's own row by contract), so no list dance is needed
// here — but it DOES need a non-empty owner, or the write would land in the
// operator namespace and hand every member one person's credential.
func (s *Server) storeADOEntraBlob(ctx context.Context, owner, rowID string, blob adoEntraBlob) error {
	if s.cfg.Secrets == nil {
		return fmt.Errorf("no secret store configured")
	}
	if owner == "" {
		return fmt.Errorf("a captured azure devops sign-in has no owner to store it under")
	}
	if !adoEntraValidRowID(rowID) {
		return fmt.Errorf("azure devops provider row id %q is not a usable store name", rowID)
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("marshal azure devops sign-in blob: %w", err)
	}
	return s.cfg.Secrets.For(owner).Put(ctx, adoEntraSecretName(rowID), raw)
}

// ── redemption ──────────────────────────────────────────────────────────────

// ADOEntraAccess is one minted access token and what it may do.
//
// Scopes is THE GRANTED SET THE AUTHORITY REPORTED, and a caller must read it
// rather than assume it got what it asked for: measured against a real tenant,
// Entra ignores a narrowing request for the Azure DevOps resource and issues a
// token carrying every scope the person consented to. The token is therefore
// the person's own identity with all of their consent, not a per-run
// capability — what bounds a run is Wardyn's own check in front of the
// resource. This field is the only honest evidence of what a token can do, and
// it is what a caller should record.
type ADOEntraAccess struct {
	AccessToken string
	Scopes      []string
	ExpiresAt   time.Time
}

// adoEntraFlight is the per-owner single-flight registry. Keyed by owner AND
// row, so two people redeeming at once never serialise behind each other while
// two redemptions for the SAME person and row always do — which is what keeps
// one rotating refresh token from being spent twice. Process-local, correct for
// the same reason every other in-memory bound in this package is: more than one
// replica is refused by construction.
type adoEntraFlight struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (f *adoEntraFlight) lock(owner, rowID string) func() {
	f.mu.Lock()
	if f.locks == nil {
		f.locks = map[string]*sync.Mutex{}
	}
	key := owner + "\x00" + rowID
	mu := f.locks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		f.locks[key] = mu
	}
	f.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// RedeemADOEntraAccess mints an Azure DevOps access token for owner, REQUESTING
// only the scopes named.
//
// IT DOES NOT PROMISE A NARROW TOKEN, and no caller may act as though it does.
// Measured against a real tenant, Entra ignores the requested subset for the
// Azure DevOps resource: the token comes back carrying every scope the person
// consented to, whatever was asked for. The subset is still sent because that
// is what a correct client sends, it costs nothing, and it is what would narrow
// if this ever changes — but the truth a caller must build on is the returned
// ADOEntraAccess.Scopes, and the layer that actually bounds a run is Wardyn's
// own capability check in front of the resource, not the token.
//
// `.default` is refused regardless: it asks for every permission the
// application holds rather than naming anything, so a request carrying one says
// nothing about what the caller believed it needed.
//
// SINGLE-FLIGHT, and the read happens INSIDE the lock: a second caller that had
// already read the pre-rotation blob would otherwise redeem the spent token,
// take an invalid_grant, and classify a perfectly good credential as dead.
// Re-reading under the lock means the second caller redeems what the first one
// persisted.
//
// A Put that fails AFTER a successful redeem does NOT fail the caller: the
// rotated token is already live at Entra and the presented one is already
// spent, so refusing would throw away a credential we hold. The caller is
// served from memory and the persist failure is logged — the next caller will
// find the stored token dead and say so honestly, which is strictly better than
// pretending this one failed.
func (s *Server) RedeemADOEntraAccess(ctx context.Context, cfg ADOEntraConfig, owner string, scopes []string) (ADOEntraAccess, error) {
	if err := cfg.validate(); err != nil {
		return ADOEntraAccess{}, err
	}
	if owner == "" {
		return ADOEntraAccess{}, fmt.Errorf("%w: no principal to redeem it for", ErrADOEntraNotCaptured)
	}
	if err := adoEntraCheckRequestedScopes(scopes); err != nil {
		return ADOEntraAccess{}, err
	}

	unlock := s.adoEntra.lock(owner, cfg.RowID)
	defer unlock()

	blob, found, err := s.readADOEntraBlob(ctx, owner, cfg.RowID)
	if err != nil {
		return ADOEntraAccess{}, fmt.Errorf("%w: %w", ErrADOEntraUnavailable, err)
	}
	if !found {
		return ADOEntraAccess{}, ErrADOEntraNotCaptured
	}
	// The requested subset must be inside the CONSENTED set the capture
	// recorded. Refusing here saves a round trip and, more importantly, names
	// the cause: a scope the consent never covered is a consent question, and
	// the authority would answer it with the same AADSTS number this returns.
	for _, want := range scopes {
		if !slices.Contains(blob.Scopes, want) {
			return ADOEntraAccess{}, fmt.Errorf("%w: %q is outside the scopes this sign-in was captured with", ErrADOEntraConsentRequired, want)
		}
	}

	resp, err := s.postADOEntraToken(ctx, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {blob.RefreshToken},
		"scope":         {strings.Join(scopes, " ")},
	})
	if err != nil {
		s.noteADOEntraSignInEnded(ctx, owner, cfg.RowID, blob, ADOEntraClassify(err))
		return ADOEntraAccess{}, err
	}

	// Mask BEFORE anything can log or persist the new values, and globally for
	// the same reason the AWS capture masks globally: one credential is reused
	// across every run that selects this lane.
	s.cfg.MaskRegistry.AddGlobal([]byte(resp.AccessToken))
	s.cfg.MaskRegistry.AddGlobal([]byte(resp.RefreshToken))

	granted := strings.Fields(resp.Scope)
	if len(granted) == 0 {
		// An authority that reports no granted scope has told us nothing about
		// what this token may do, and since the granted set is routinely WIDER
		// than the requested one, a caller falling back to what it asked for
		// would be recording a narrower token than it holds. Unusable rather
		// than dead — the credential itself is untouched.
		return ADOEntraAccess{}, fmt.Errorf("%w: the token response named no granted scope", ErrADOEntraUnavailable)
	}
	access := ADOEntraAccess{
		AccessToken: resp.AccessToken,
		Scopes:      granted,
		ExpiresAt:   s.cfg.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).UTC(),
	}

	// Persist the ROTATION. An absent refresh token in the response means keep
	// the one we hold — never blank it. A recorded end the authority has just
	// contradicted is cleared the same way.
	if (resp.RefreshToken != "" && resp.RefreshToken != blob.RefreshToken) || blob.signInEnded() {
		next := blob
		if resp.RefreshToken != "" {
			next.RefreshToken = resp.RefreshToken
		}
		next.ExpiresAt = access.ExpiresAt
		next.RenewedAt = s.cfg.Now().UTC()
		next.DeadAt, next.DeadReason = time.Time{}, ""
		if perr := s.storeADOEntraBlob(ctx, owner, cfg.RowID, next); perr != nil {
			slog.ErrorContext(ctx, "wardynd: persisting the rotated azure devops refresh token failed; serving this caller from memory",
				slog.String("row", cfg.RowID), slog.Any("err", perr))
		}
	}
	return access, nil
}

// noteADOEntraSignInEnded records, on the stored blob, a renewal refusal only a
// new sign-in can answer. Best-effort: the caller's own answer does not depend
// on it, and a failed write only means the next launch learns it at boot.
// Called under the redemption lock, so it cannot overwrite a rotation.
func (s *Server) noteADOEntraSignInEnded(ctx context.Context, owner, rowID string, blob adoEntraBlob, class ADOEntraFailure) {
	if class != ADOEntraFailureDeadCredential && class != ADOEntraFailureInteractionRequired {
		return
	}
	blob.DeadAt, blob.DeadReason = s.cfg.Now().UTC(), class
	if err := s.storeADOEntraBlob(ctx, owner, rowID, blob); err != nil {
		slog.WarnContext(ctx, "wardynd: could not record that an azure devops sign-in has ended",
			slog.String("row", rowID), slog.Any("err", err))
	}
}

// adoEntraCheckRequestedScopes holds a caller's subset to the two rules that
// are not the provider row's to decide: it must name something, and it must
// never name `.default`.
//
// `.default` is refused rather than filtered because it is not a scope — it is
// a request for EVERYTHING the app registration holds, across every resource,
// and it names nothing a reviewer could check a request against. A caller that
// wants the ceiling names the ceiling.
func adoEntraCheckRequestedScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("a redemption must name the scopes it needs")
	}
	if len(scopes) > adoEntraMaxScopes {
		return fmt.Errorf("a redemption may name at most %d scopes", adoEntraMaxScopes)
	}
	for _, sc := range scopes {
		if sc == "" || strings.ContainsAny(sc, " \t\r\n") {
			return fmt.Errorf("scope %q is not a single scope value", sc)
		}
		if sc == ".default" || strings.HasSuffix(sc, "/.default") {
			return fmt.Errorf("refusing to request %q: it asks for every permission the app registration holds, which is the opposite of a narrowed token", sc)
		}
	}
	return nil
}

// adoEntraTokenResponse is the subset of Entra's token response this lane
// consumes, plus the error shape a refusal answers with.
type adoEntraTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// postADOEntraToken performs one token request and classifies its answer.
//
// Hand-rolled over net/http rather than through an oauth2.TokenSource for one
// reason that matters: this lane chooses the requested SCOPE per redemption and
// must persist the ROTATED refresh token itself, and a TokenSource owns both
// decisions. http.DefaultTransport is the transport, so the call follows
// WARDYN_DAEMON_PROXY_URL when set, exactly like every other wardynd-side
// egress in this package.
func (s *Server) postADOEntraToken(ctx context.Context, cfg ADOEntraConfig, form url.Values) (adoEntraTokenResponse, error) {
	var out adoEntraTokenResponse
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	tokenURL, err := cfg.tokenURL()
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrADOEntraUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return out, fmt.Errorf("%w: build token request: %w", ErrADOEntraUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// CheckRedirect refuses to FOLLOW anything: this body carries the client
	// secret and the refresh token, and net/http replays a POST body on a
	// 307/308. The endpoint does not redirect, so a 3xx is answered as the
	// unusable response it is.
	client := &http.Client{
		Transport:     http.DefaultTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrADOEntraUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded read: the body is a handful of fields, and an unbounded read from
	// a host this process dials directly is an easy way to eat the daemon's
	// memory.
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if rerr != nil {
		return out, fmt.Errorf("%w: read token response: %w", ErrADOEntraUnavailable, rerr)
	}
	// An unparseable body is UNAVAILABLE, never a dead credential: only an
	// error the authority actually named may retire one.
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return out, fmt.Errorf("%w: parse token response (status %d): %w", ErrADOEntraUnavailable, resp.StatusCode, uerr)
	}
	if out.Error != "" {
		return out, classifyADOEntraError(out.Error, out.ErrorDescription)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return out, fmt.Errorf("%w: unusable token response (status %d)", ErrADOEntraUnavailable, resp.StatusCode)
	}
	return out, nil
}

// classifyADOEntraError maps an authority refusal onto one of this lane's four
// classes.
//
// CONSENT IS CHECKED FIRST, and the order is the whole point: Entra answers a
// revoked consent with `invalid_grant` on the refresh path and puts AADSTS65001
// in the description, so a classifier that read the OAuth code first would call
// it a dead credential and tell everyone in the estate to sign in again over an
// admin un-ticking one permission.
func classifyADOEntraError(code, desc string) error {
	switch {
	case code == entraOAuthConsentRequired || strings.Contains(desc, entraConsentRequiredCode):
		return fmt.Errorf("%w: %s (%s)", ErrADOEntraConsentRequired, code, entraConsentRequiredCode)
	case code == entraOAuthInteractionRequired:
		return fmt.Errorf("%w: %s", ErrADOEntraInteractionRequired, code)
	case code == "invalid_grant" || code == "invalid_client" || code == "unauthorized_client":
		return fmt.Errorf("%w: %s", ErrADOEntraDeadCredential, code)
	default:
		return fmt.Errorf("%w: the authority refused with %s", ErrADOEntraUnavailable, code)
	}
}

// auditADOCapture emits the one audit action the sign-in owns. outcome is
// "success" or "failure"; data names the row, the tenant, the scopes granted
// and — on a refusal — the class. It never carries a token.
func (s *Server) auditADOCapture(ctx context.Context, actor, rowID, outcome string, data map[string]any) {
	data["provider"] = adoEntraProviderPrefix
	target := "wardyn-harness-" + adoEntraProviderPrefix + "-oauth"
	if adoEntraValidRowID(rowID) {
		target = adoEntraSecretName(rowID)
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor,
		adoSignInCapturedAction, target, outcome, mustJSON(data)))
}
