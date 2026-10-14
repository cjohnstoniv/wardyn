// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"unicode"
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
		s.ssoRefreshMu.Lock()
		if s.ssoRefreshRefused == nil {
			s.ssoRefreshRefused = map[string]string{}
		}
		s.ssoRefreshRefused[awsSSOTokenFingerprint(blob.RefreshToken)] = refused.detail()
		s.ssoRefreshMu.Unlock()
	}
	s.auditAWSSSORefresh(ctx, scope, "failure", data)
}

// clearAWSSSORefused drops a refused mark once its refresh token renews.
func (s *Server) clearAWSSSORefused(fingerprint string) {
	s.ssoRefreshMu.Lock()
	delete(s.ssoRefreshRefused, fingerprint)
	s.ssoRefreshMu.Unlock()
}

// awsSSORefusedFor reports the refusal AWS last answered this blob's refresh
// token with. Keyed by that token's fingerprint, so a fresh sign-in, which
// carries a new refresh token, is never pre-marked.
func (s *Server) awsSSORefusedFor(blob awsSSOBlob) (string, bool) {
	if blob.RefreshToken == "" {
		return "", false
	}
	s.ssoRefreshMu.Lock()
	defer s.ssoRefreshMu.Unlock()
	detail, ok := s.ssoRefreshRefused[awsSSOTokenFingerprint(blob.RefreshToken)]
	return detail, ok
}
