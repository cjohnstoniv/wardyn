// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/scim"
)

// SCIMConfig mounts the SCIM Users routes. Boot validates it (cmd/wardynd/boot_scim.go): a token of
// at least 32 bytes, an Entra issuer whose tenant is a single tenant, HTTPS only.
type SCIMConfig struct {
	// Token and TokenNext are the bearers the routes accept (WARDYN_SCIM_TOKEN, and the second one
	// rotation needs). TokenNext may be empty; Token may not.
	Token, TokenNext string
	// Issuer and Tenant key the identity rows a SCIM user is matched against: the OIDC issuer URL
	// sign-ins record, and the tenant id derived from it.
	Issuer, Tenant string
}

const (
	// scimMaxBody is SCIM's own request cap, far under the API's 1 MiB maxJSONBody: a provisioning
	// request is one user.
	scimMaxBody = 64 << 10

	// scimAuthActor names the boundary that refused a bearer, scimActor the caller of every SCIM write.
	scimAuthActor = "wardyn/scimAuth"
	scimActor     = "wardyn/scim"

	// authFailedInvalidSCIMToken is the auth.fail reason of every refused bearer, missing or wrong. It
	// names no slot and never carries the bearer or its digest.
	authFailedInvalidSCIMToken = "invalid_scim_token"

	scimSlotPrimary = "primary"
	scimSlotNext    = "next"

	// An admitted request is limited per slot per replica; the refused requests, which have no slot,
	// share one bucket, so guessing a bearer is bounded however many sources it comes from.
	scimRatePerSec        = 25.0
	scimBurst             = 100.0
	scimRefusedRatePerSec = 2.0
	scimRefusedBurst      = 10.0
)

type scimCallerKey struct{}

// withSCIMCaller marks ctx as a request scimAuth admitted, carrying the token slot that matched.
func withSCIMCaller(ctx context.Context, slot string) context.Context {
	return context.WithValue(ctx, scimCallerKey{}, slot)
}

// scimCallerSlot is the slot of the SCIM bearer that authenticated ctx's request; false for every
// other caller. neverOperator reads it, so the SCIM connector is never read as the admin token.
func scimCallerSlot(ctx context.Context) (string, bool) {
	slot, ok := ctx.Value(scimCallerKey{}).(string)
	return slot, ok
}

type scimState struct {
	admitted principalLimiter
	refused  principalLimiter
}

func newSCIMState() scimState {
	return scimState{
		admitted: principalLimiter{rate: scimRatePerSec, burst: scimBurst, max: 4},
		refused:  principalLimiter{rate: scimRefusedRatePerSec, burst: scimRefusedBurst, max: 1},
	}
}

// writeSCIM writes a SCIM body as application/scim+json.
func writeSCIM(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", scim.MediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSCIMError writes the RFC 7644 error envelope.
func writeSCIMError(w http.ResponseWriter, e *scim.Error) { writeSCIM(w, e.Status, e) }

func writeSCIMTooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeSCIMError(w, scim.NewError(http.StatusTooManyRequests, "", "too many requests; retry after the delay in Retry-After"))
}

// scimAuth admits a request carrying one of the two configured bearers, compared in constant time
// over SHA-256 digests so neither the value nor its length leaks. Every other request is a 401 and
// an auth.fail row that says only that the token was invalid.
func (s *Server) scimAuth(next http.Handler) http.Handler {
	primary := sha256.Sum256([]byte(s.cfg.SCIM.Token))
	var rotated [sha256.Size]byte
	hasNext := s.cfg.SCIM.TokenNext != ""
	if hasNext {
		rotated = sha256.Sum256([]byte(s.cfg.SCIM.TokenNext))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slot := ""
		if tok, ok := bearerToken(r); ok {
			d := sha256.Sum256([]byte(tok))
			isPrimary := subtle.ConstantTimeCompare(d[:], primary[:]) == 1
			isNext := hasNext && subtle.ConstantTimeCompare(d[:], rotated[:]) == 1
			switch {
			case isPrimary:
				slot = scimSlotPrimary
			case isNext:
				slot = scimSlotNext
			}
		}
		now := s.cfg.Now()
		if slot == "" {
			if !s.scimState.refused.allow("", now) {
				writeSCIMTooMany(w)
				return
			}
			s.auditAuthFailedAs(r, scimAuthActor, authFailedInvalidSCIMToken)
			w.Header().Set("WWW-Authenticate", `Bearer realm="wardyn-scim"`)
			writeSCIMError(w, scim.NewError(http.StatusUnauthorized, "", "the bearer token is missing or invalid"))
			return
		}
		if !s.scimState.admitted.allow(slot, now) {
			writeSCIMTooMany(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSCIMCaller(r.Context(), slot)))
	})
}

// readSCIMBody reads the request body under scimMaxBody; a failure writes the SCIM error and is false.
func readSCIMBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, scimMaxBody))
	if err == nil {
		return raw, true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeSCIMError(w, scim.NewError(http.StatusRequestEntityTooLarge, "", "the request body exceeds 64 KiB"))
	} else {
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidSyntax, "the request body could not be read"))
	}
	return nil, false
}
