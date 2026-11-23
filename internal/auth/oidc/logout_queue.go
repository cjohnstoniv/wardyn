// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// The held back-channel logouts of a pending authenticator. A logout token that arrives while the provider has
// not been reached cannot be verified, and dropping it would leave a person the provider just signed out with
// a live console session. So a token that passes the unverified shape checks is held in memory, bounded, and
// verified when the provider answers: at its receipt time, against one key set fetched once. A verified token
// cuts the person's sessions the way a discovered server would. A token that does not verify is not applied
// (its sub is unverified, and applying it would let an anonymous caller cut anyone's sessions); it is recorded.
//
// The queue is in memory: a restart while pending loses it, and a crash records nothing. The held tokens are
// never written to a log, an audit row or an event: only their SHA-256.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// LogoutQueueCap is how many distinct logout tokens a pending authenticator holds. With the 16 KiB body bound
	// that is 4 MiB at most.
	LogoutQueueCap = 256
	// logoutSubMax bounds the unverified sub that goes into an audit row and a log line.
	logoutSubMax = 256
	// logoutRateSummaryEvery is the least gap between two rate_limited summaries.
	logoutRateSummaryEvery = time.Minute
	// logoutKeySetMax bounds the key document the drain reads.
	logoutKeySetMax = 4 << 20
)

// The states a LogoutQueueEvent carries.
const (
	LogoutQueued             = "queued"
	LogoutRefusedUnreadable  = "refused_unreadable"
	LogoutRefusedQueueFull   = "refused_queue_full"
	LogoutRefusedReserved    = "refused_reserved"
	LogoutUnverifiable       = "unverifiable"
	LogoutApplied            = "applied"
	LogoutLostAtShutdown     = "lost_at_shutdown"
	LogoutRateLimitedSummary = "rate_limited"
)

var (
	// ErrLogoutKeysUnavailable is returned by Discover when the provider's signing keys could not be fetched, or
	// held none Wardyn can use. No held logout was checked; they stay queued.
	ErrLogoutKeysUnavailable = errors.New("oidc: the signing keys needed to check held logouts are unavailable")
	// ErrLogoutDrainIncomplete is returned by Discover when its context ended before every held logout was checked.
	ErrLogoutDrainIncomplete = errors.New("oidc: the held logouts were not all checked before the attempt ended")
	// ErrLogoutCutFailed is returned by Discover when a verified held logout's session cut could not be written.
	ErrLogoutCutFailed = errors.New("oidc: a held logout's session cut could not be written")
)

// LogoutQueueEvent is the one value that crosses to the sink. It has no field for the raw token.
type LogoutQueueEvent struct {
	State         string
	SubUnverified string
	TokenSHA256   string
	ReceivedAt    time.Time
	Problem       string
	Count         int
	WindowStart   time.Time
	WindowEnd     time.Time
}

// LogoutQueueSink records the queue's events and applies a verified held logout.
type LogoutQueueSink interface {
	// Noted records one event. It must not fail the caller.
	Noted(ctx context.Context, ev LogoutQueueEvent)
	// Apply cuts sub's sessions for a held logout received at receivedAt, whose token hashes to sha.
	Apply(ctx context.Context, sub string, receivedAt time.Time, sha string) error
}

// LogoutQueueStats is the queue's size and what it has done since the process started.
type LogoutQueueStats struct {
	Held, Queued, Applied, Unverifiable, RefusedQueueFull, RateLimited int
}

type logoutEntry struct {
	raw, sha, sub string
	receivedAt    time.Time
}

type logoutQueue struct {
	mu      sync.Mutex
	entries []logoutEntry
	seen    map[string]struct{}

	sink     atomic.Pointer[LogoutQueueSink]
	reserved atomic.Pointer[func(string) bool]
	// now is the clock and beforeHold runs just before a pre-checked token takes the mutex; tests replace both.
	now        func() time.Time
	beforeHold func()

	queued, applied, unverifiable, refusedFull, rateLimited atomic.Int64
	// rlCount and rlStart are the rate-limited refusals not yet summarised, and when the window began.
	rlCount int
	rlStart time.Time
}

func (q *logoutQueue) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

func (q *logoutQueue) note(ctx context.Context, ev LogoutQueueEvent) {
	if s := q.sink.Load(); s != nil {
		(*s).Noted(ctx, ev)
	}
}

// AttachLogoutQueueSink sets where the queue's events go and who applies a verified held logout. Unattached,
// events are not recorded and a verified logout is cut with CutSessions.
func (a *Authenticator) AttachLogoutQueueSink(s LogoutQueueSink) {
	if s == nil {
		a.logouts.sink.Store(nil)
		return
	}
	a.logouts.sink.Store(&s)
}

// LogoutQueueStats reports the held-logout queue. The zero value reports zeros.
func (a *Authenticator) LogoutQueueStats() LogoutQueueStats {
	q := &a.logouts
	q.mu.Lock()
	held := len(q.entries)
	q.mu.Unlock()
	return LogoutQueueStats{
		Held:             held,
		Queued:           int(q.queued.Load()),
		Applied:          int(q.applied.Load()),
		Unverifiable:     int(q.unverifiable.Load()),
		RefusedQueueFull: int(q.refusedFull.Load()),
		RateLimited:      int(q.rateLimited.Load()),
	}
}

// NoteLogoutRateLimited counts one back-channel POST the route's limiter refused while pending. At most once a
// minute it also emits one rate_limited summary carrying the count since the last one, with one ERROR line.
func (a *Authenticator) NoteLogoutRateLimited(ctx context.Context) {
	q := &a.logouts
	q.rateLimited.Add(1)
	now := q.clock()
	q.mu.Lock()
	q.rlCount++
	if q.rlStart.IsZero() {
		q.rlStart = now
	}
	var n int
	var from time.Time
	if now.Sub(q.rlStart) >= logoutRateSummaryEvery {
		n, from = q.takeRateWindowLocked()
	}
	q.mu.Unlock()
	q.emitRateSummary(ctx, n, from, now)
}

// takeRateWindowLocked returns and clears the unsummarised count. The caller holds q.mu.
func (q *logoutQueue) takeRateWindowLocked() (int, time.Time) {
	n, from := q.rlCount, q.rlStart
	q.rlCount, q.rlStart = 0, time.Time{}
	return n, from
}

func (q *logoutQueue) emitRateSummary(ctx context.Context, n int, from, to time.Time) {
	if n == 0 {
		return
	}
	slog.Error("wardynd: back-channel logouts were refused as rate limited while the identity provider was unreachable and are not held",
		"count", n, "window_start", from, "window_end", to)
	q.note(ctx, LogoutQueueEvent{State: LogoutRateLimitedSummary, Count: n, WindowStart: from, WindowEnd: to})
}

// ReportLogoutsLostAtShutdown is the clean-stop record of a queue that still holds logouts: one ERROR line and
// one best-effort lost_at_shutdown event per held logout, then the remaining rate_limited summary. It
// is a function, not a method, and it waits for nothing past its own 5 s: a crash or a kill records nothing.
func ReportLogoutsLostAtShutdown(a *Authenticator) {
	q := &a.logouts
	q.mu.Lock()
	held := append([]logoutEntry(nil), q.entries...)
	n, from := q.takeRateWindowLocked()
	q.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, e := range held {
		slog.Error("wardynd: a logout held while the identity provider was unreachable was never applied: this server is stopping",
			"sub_unverified", e.sub, "token_sha256", e.sha)
		q.note(ctx, LogoutQueueEvent{State: LogoutLostAtShutdown, SubUnverified: e.sub, TokenSHA256: e.sha, ReceivedAt: e.receivedAt})
	}
	q.emitRateSummary(ctx, n, from, q.clock())
}

func tokenSHA256(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// precheckLogoutToken reads a logout token without verifying it and returns its sub, or the reason it cannot
// be a logout token for this client. Only public values are checked: the issuer and the client ID.
func (a *Authenticator) precheckLogoutToken(raw string, now time.Time) (sub, problem string) {
	if raw == "" {
		return "", "logout_token is missing"
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", "the logout token did not verify"
	}
	for _, p := range parts {
		if _, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p, "=")); err != nil || p == "" {
			return "", "the logout token did not verify"
		}
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return "", "the logout token claims are unreadable"
	}
	var iss string
	if json.Unmarshal(claims["iss"], &iss) != nil || types.NormIssuerURL(iss) != types.NormIssuerURL(a.cfg.IssuerURL) {
		return "", "the logout token did not verify"
	}
	if !audienceHas(claims["aud"], a.cfg.ClientID) {
		return "", "the logout token did not verify"
	}
	var events, event map[string]json.RawMessage
	if json.Unmarshal(claims["events"], &events) != nil {
		return "", "the logout token claims are unreadable"
	}
	switch {
	case json.Unmarshal(events[backchannelLogoutEvent], &event) != nil || event == nil:
		return "", "the logout token carries no back-channel logout event object"
	case claims["nonce"] != nil:
		return "", "a logout token carries no nonce"
	case len(claims["jti"]) == 0 || string(claims["jti"]) == `""`:
		return "", "the logout token has no jti"
	}
	var iat float64
	if json.Unmarshal(claims["iat"], &iat) != nil || iat <= 0 ||
		absDuration(now.Sub(time.Unix(int64(iat), 0))) > backchannelIATWindow {
		return "", "the logout token iat is too far from now"
	}
	var s string
	_ = json.Unmarshal(claims["sub"], &s)
	switch {
	case s == "":
		return "", "the logout token names no sub; sessions are not cut by sid"
	case len(s) > logoutSubMax:
		return "", "the logout token sub is too long"
	}
	return s, ""
}

// audienceHas reports whether a JWT aud claim (a string or an array of strings) names clientID.
func audienceHas(raw json.RawMessage, clientID string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, v := range many {
		if v == clientID {
			return true
		}
	}
	return false
}

// holdLogout appends a pre-checked token under the queue mutex, re-reading the published state there: a token
// that arrives after the publish is served on the discovered path instead.
func (a *Authenticator) holdLogout(raw, sha, sub string, now time.Time) string {
	q := &a.logouts
	q.mu.Lock()
	defer q.mu.Unlock()
	if a.prov.Load() != nil {
		return holdPublished
	}
	if _, dup := q.seen[sha]; dup {
		return holdDuplicate
	}
	if len(q.entries) >= LogoutQueueCap {
		return holdFull
	}
	if q.seen == nil {
		q.seen = make(map[string]struct{})
	}
	q.seen[sha] = struct{}{}
	q.entries = append(q.entries, logoutEntry{raw: raw, sha: sha, sub: sub, receivedAt: now})
	return holdQueued
}

const (
	holdQueued    = "queued"
	holdDuplicate = "duplicate"
	holdFull      = "full"
	holdPublished = "published"
)

// pendingBackchannel serves a back-channel logout request while the provider has not been reached. It reports
// false when the state was published meanwhile and the request must be served on the discovered path.
func (a *Authenticator) pendingBackchannel(w http.ResponseWriter, r *http.Request, reserved func(string) bool) bool {
	q := &a.logouts
	ctx := r.Context()
	now := q.clock()
	raw := r.PostForm.Get("logout_token")
	sha := ""
	if raw != "" {
		sha = tokenSHA256(raw)
	}
	sub, problem := a.precheckLogoutToken(raw, now)
	if problem != "" {
		q.note(ctx, LogoutQueueEvent{State: LogoutRefusedUnreadable, TokenSHA256: sha, ReceivedAt: now, Problem: problem})
		backchannelRefuse(w, http.StatusBadRequest, "invalid_request", problem)
		return true
	}
	if reserved != nil && reserved(sub) {
		q.note(ctx, LogoutQueueEvent{State: LogoutRefusedReserved, TokenSHA256: sha, ReceivedAt: now})
		backchannelRefuse(w, http.StatusBadRequest, "invalid_request", "the sub names a reserved principal")
		return true
	}
	if a.cfg.Revocations == nil {
		backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation is not configured")
		return true
	}
	if q.beforeHold != nil {
		q.beforeHold()
	}
	switch a.holdLogout(raw, sha, sub, now) {
	case holdPublished:
		return false
	case holdFull:
		q.refusedFull.Add(1)
		slog.Error("wardynd: a logout was refused because the held-logout queue is full while the identity provider is unreachable",
			"sub_unverified", sub, "token_sha256", sha)
		q.note(ctx, LogoutQueueEvent{State: LogoutRefusedQueueFull, SubUnverified: sub, TokenSHA256: sha, ReceivedAt: now})
		backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the held-logout queue is full; this logout was not held")
		return true
	case holdQueued:
		q.queued.Add(1)
		q.note(ctx, LogoutQueueEvent{State: LogoutQueued, SubUnverified: sub, TokenSHA256: sha, ReceivedAt: now})
	}
	backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable",
		"the identity provider has not been reached since this server started; this logout is held and applied once it is")
	return true
}

// drainAndPublish applies the held logouts, then publishes st. It fetches the provider's key set once per
// attempt, and only when something is held. A kept entry (the keys did not answer, the attempt ended, a cut
// could not be written) leaves the state unpublished and the queue as it was.
func (a *Authenticator) drainAndPublish(ctx context.Context, st *providerState) error {
	q := &a.logouts
	var ks *gooidc.StaticKeySet
	for {
		q.mu.Lock()
		if len(q.entries) == 0 {
			// Under the mutex, so no token arrives between the last drain and the publish.
			a.prov.CompareAndSwap(nil, st)
			n, from := q.takeRateWindowLocked()
			q.mu.Unlock()
			q.emitRateSummary(ctx, n, from, q.clock())
			a.pending.Store(false)
			return nil
		}
		e := q.entries[0]
		q.mu.Unlock()

		if err := ctx.Err(); err != nil {
			slog.Warn("oidc: the held logouts were not all checked before the attempt ended; they stay queued", "error", err)
			return fmt.Errorf("%w: %w", ErrLogoutDrainIncomplete, err)
		}
		if ks == nil {
			var err error
			if ks, err = fetchDrainKeySet(ctx, st); err != nil {
				return err
			}
		}
		if err := a.drainEntry(ctx, st, ks, e); err != nil {
			return err
		}
	}
}

// drainEntry checks one held logout against the static key set and removes it from the queue unless it must
// be kept.
func (a *Authenticator) drainEntry(ctx context.Context, st *providerState, ks *gooidc.StaticKeySet, e logoutEntry) error {
	q := &a.logouts
	v := gooidc.NewVerifier(st.issuer, ks, &gooidc.Config{
		ClientID:             a.cfg.ClientID,
		SupportedSigningAlgs: st.signingAlgs,
		Now:                  func() time.Time { return e.receivedAt },
	})
	sub, problem := logoutTokenSub(ctx, v, e.raw, e.receivedAt)
	switch {
	case problem != "":
		q.unverifiable.Add(1)
		slog.Error("wardynd: a logout held while the identity provider was unreachable failed verification and was not applied",
			"sub_unverified", e.sub, "token_sha256", e.sha, "problem", problem)
		q.note(ctx, LogoutQueueEvent{State: LogoutUnverifiable, SubUnverified: e.sub, TokenSHA256: e.sha, ReceivedAt: e.receivedAt, Problem: problem})
	case a.logoutReserved(sub):
		q.note(ctx, LogoutQueueEvent{State: LogoutRefusedReserved, TokenSHA256: e.sha, ReceivedAt: e.receivedAt})
	default:
		if err := a.applyHeld(ctx, sub, e); err != nil {
			slog.Warn("oidc: a held logout's session cut could not be written; it stays queued", "token_sha256", e.sha, "error", err)
			return fmt.Errorf("%w: %v", ErrLogoutCutFailed, err)
		}
		q.applied.Add(1)
		q.note(ctx, LogoutQueueEvent{State: LogoutApplied, TokenSHA256: e.sha, ReceivedAt: e.receivedAt})
	}
	q.mu.Lock()
	q.entries = q.entries[1:]
	delete(q.seen, e.sha)
	q.mu.Unlock()
	return nil
}

func (a *Authenticator) logoutReserved(sub string) bool {
	rf := a.logouts.reserved.Load()
	return rf != nil && (*rf)(sub)
}

func (a *Authenticator) applyHeld(ctx context.Context, sub string, e logoutEntry) error {
	if s := a.logouts.sink.Load(); s != nil {
		return (*s).Apply(ctx, sub, e.receivedAt, e.sha)
	}
	return CutSessions(ctx, a.cfg.Revocations, sub)
}

// jwksRecorder wraps the drain's key-fetch transport and remembers how its request ended.
type jwksRecorder struct {
	inner  http.RoundTripper
	mu     sync.Mutex
	status int // 0 until a response arrives
	failed bool
}

func (j *jwksRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := j.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(req)
	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil || resp == nil {
		j.failed = true
		return resp, err
	}
	j.status = resp.StatusCode
	return resp, nil
}

// fetchDrainKeySet GETs the provider's jwks_uri once and returns a static key set of the keys Wardyn can use
// to check a signature: RSA, ECDSA and Ed25519, with a `use` that is empty or `sig`. A static set never
// fetches, so junk in the queue cannot turn into key requests. go-oidc's StaticKeySet returns on the first
// key of any other type, so an unusable key is skipped here, with one WARN line naming its kid. No answer, a
// non-200, an undecodable body and a set with no usable key all wrap ErrLogoutKeysUnavailable.
func fetchDrainKeySet(ctx context.Context, st *providerState) (*gooidc.StaticKeySet, error) {
	rec := &jwksRecorder{}
	c := &http.Client{}
	if st.httpClient != nil {
		*c = *st.httpClient
	}
	rec.inner = c.Transport
	c.Transport = rec
	c = newTolerantJWKSClient(c)

	fail := func(why string, err error) (*gooidc.StaticKeySet, error) {
		slog.Warn("oidc: the provider's signing keys could not be used to check held logouts; they stay queued",
			"jwks_uri", st.jwksURL, "problem", why, "error", err)
		return nil, fmt.Errorf("%w: %s", ErrLogoutKeysUnavailable, why)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.jwksURL, nil)
	if err != nil {
		return fail("the key request could not be built", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fail("the key request got no response", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("the key request answered %d", resp.StatusCode), nil)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, logoutKeySetMax))
	if err != nil {
		return fail("the key response could not be read", err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return fail("the key response did not decode", err)
	}
	var keys []crypto.PublicKey
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			slog.Warn("oidc: skipping a signing key whose use is not sig when checking held logouts", "kid", k.KeyID, "use", k.Use)
			continue
		}
		switch pub := k.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			keys = append(keys, pub)
		default:
			slog.Warn("oidc: skipping a signing key of a type that cannot check held logouts", "kid", k.KeyID, "kty", fmt.Sprintf("%T", k.Key))
		}
	}
	if len(keys) == 0 {
		return fail("the key set holds no usable signing key", nil)
	}
	return &gooidc.StaticKeySet{PublicKeys: keys}, nil
}
