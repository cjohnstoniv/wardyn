// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// awsSSORefusedError is a CreateToken refusal of the re-auth class
// (awsSSOErrorIsReauth): retrying gets the same answer and a fresh sign-in
// answers it, but nothing says the refresh token is gone, so the stored blob is
// neither deleted nor marked spent.
type awsSSORefusedError struct{ code, description string }

func (e awsSSORefusedError) Error() string { return "aws sso create-token refused: " + e.detail() }

// detail is the AWS code and, when AWS sent one, its description: what the
// run refusal names.
func (e awsSSORefusedError) detail() string {
	if e.description == "" {
		return e.code
	}
	return e.code + ": " + e.description
}

// awsSSOErrorIsReauth classifies the CreateToken codes a sign-in answers that
// awsSSOErrorIsSpent does not already retire.
func awsSSOErrorIsReauth(code string) bool {
	switch code {
	case "invalid_request", "access_denied", "invalid_scope", "unsupported_grant_type":
		return true
	}
	return false
}

// awsSSORefusal reports whether err is (or wraps) a re-auth refusal.
func awsSSORefusal(err error) (awsSSORefusedError, bool) {
	var refused awsSSORefusedError
	ok := errors.As(err, &refused)
	return refused, ok
}

// awsSSOExpiredFailure is the failure a non-spent renewal error returns when the
// access token has expired: the refusal sentence naming AWS's answer when AWS
// refused, else the "did not answer" one.
func awsSSOExpiredFailure(err error) string {
	if refused, ok := awsSSORefusal(err); ok {
		return awsSSORefreshRefusedSentence + refused.detail()
	}
	return awsSSORefreshUnavailableSentence
}

// awsSSODescriptionMax bounds the error_description a refusal keeps.
const awsSSODescriptionMax = 200

// awsSSOErrorDescription is AWS's error_description made safe for a run
// refusal and an audit row: printable runes only, cut to awsSSODescriptionMax
// bytes, and dropped whole if it carries any of the blob's own secret values.
func awsSSOErrorDescription(desc string, blob awsSSOBlob) string {
	for _, secret := range []string{blob.AccessToken, blob.RefreshToken, blob.ClientSecret} {
		if secret != "" && strings.Contains(desc, secret) {
			return ""
		}
	}
	desc = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, desc))
	if len(desc) > awsSSODescriptionMax {
		desc = strings.ToValidUTF8(desc[:awsSSODescriptionMax-len("…")], "") + "…"
	}
	return desc
}

// auditAWSSSORenewalFailure writes a failed redemption's refresh row. A re-auth
// refusal also says whether the client registration pair was sent
// (`registration`), and marks the refresh token refused so the readiness grade,
// Review and the banner agree with dispatch until a sign-in replaces it.
func (s *Server) auditAWSSSORenewalFailure(ctx context.Context, scope awsSSOScope, blob awsSSOBlob, spent bool, err error, attempts int, afterLostReply bool) {
	data := awsSSORefreshFailureData(spent, err, attempts, afterLostReply)
	if refused, ok := awsSSORefusal(err); ok {
		data["registration"] = "absent"
		if blob.ClientID != "" && blob.ClientSecret != "" {
			data["registration"] = "present"
		}
		s.markAWSSSORefused(ctx, awsSSOTokenFingerprint(blob.RefreshToken), scope.owner, refused.detail())
	}
	s.auditAWSSSORefresh(ctx, scope, "failure", data)
}

// awsSSORefusedMax bounds the refused marks the in-memory fallback keeps (a fingerprint and a short detail each).
const awsSSORefusedMax = 1024

// The refused mark lives in store.AWSSSORefusedTokenStore when the store has it, so every replica
// and a restarted process read the same answer; a store without it (no database) keeps the marks in
// ssoRefreshRefused, per process. The store is the only copy when it exists: a cached mark would
// outlive a renewal another replica made, since a renewal can keep the same refresh token.

// markAWSSSORefused records a refusal under owner, the secret-store namespace the blob was read from.
// A failed write (no table, or no grant on it) is logged and no replica holds the mark, this one included;
// AWS still refuses the dispatch.
func (s *Server) markAWSSSORefused(ctx context.Context, fingerprint, owner, detail string) {
	if st, ok := s.cfg.Store.(store.AWSSSORefusedTokenStore); ok {
		if err := st.MarkAWSSSOTokenRefused(ctx, fingerprint, owner, detail, s.cfg.Now().UTC()); err != nil {
			slog.WarnContext(ctx, "wardynd: persisting the AWS SSO refused-renewal mark failed", slog.Any("err", err))
		}
		return
	}
	s.ssoRefreshMu.Lock()
	defer s.ssoRefreshMu.Unlock()
	if s.ssoRefreshRefused == nil {
		s.ssoRefreshRefused = map[string]string{}
	}
	// A refresh token a sign-in replaced is never looked up again, so at the bound an arbitrary mark is evicted.
	// ponytail: random eviction; past awsSSORefusedMax live refusals an evicted one grades live again until
	// its next dispatch is refused and re-marks it.
	if _, ok := s.ssoRefreshRefused[fingerprint]; !ok && len(s.ssoRefreshRefused) >= awsSSORefusedMax {
		for k := range s.ssoRefreshRefused {
			delete(s.ssoRefreshRefused, k)
			break
		}
	}
	s.ssoRefreshRefused[fingerprint] = detail
}

// clearAWSSSORefused drops a refused mark once its refresh token renews.
func (s *Server) clearAWSSSORefused(ctx context.Context, fingerprint string) {
	if st, ok := s.cfg.Store.(store.AWSSSORefusedTokenStore); ok {
		if err := st.ClearAWSSSOTokenRefused(ctx, fingerprint); err != nil {
			slog.WarnContext(ctx, "wardynd: clearing the AWS SSO refused-renewal mark failed", slog.Any("err", err))
		}
		return
	}
	s.ssoRefreshMu.Lock()
	delete(s.ssoRefreshRefused, fingerprint)
	s.ssoRefreshMu.Unlock()
}

// awsSSORefusedFor reports the refusal AWS last answered this blob's refresh
// token with. Keyed by that token's fingerprint, so a fresh sign-in, which
// carries a new refresh token, is never pre-marked. A store that cannot be read
// answers not refused for this call, as the spent mark does.
func (s *Server) awsSSORefusedFor(blob awsSSOBlob) (string, bool) {
	if blob.RefreshToken == "" {
		return "", false
	}
	fingerprint := awsSSOTokenFingerprint(blob.RefreshToken)
	if st, ok := s.cfg.Store.(store.AWSSSORefusedTokenStore); ok {
		ctx := s.cfg.BaseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		detail, found, err := st.AWSSSOTokenRefused(ctx, fingerprint)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: reading the AWS SSO refused-renewal mark failed; treating it as not refused for this check only", slog.Any("err", err))
			return "", false
		}
		return detail, found
	}
	s.ssoRefreshMu.Lock()
	defer s.ssoRefreshMu.Unlock()
	detail, ok := s.ssoRefreshRefused[fingerprint]
	return detail, ok
}
