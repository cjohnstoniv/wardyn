// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Directory autocomplete (0.7 §I / PF-29): the HTTP half of internal/directory.
//
// One route, GET /access/directory/search, proxying the configured connector so
// the console can offer a picker for every "who" field instead of a free-text
// box. The value of that is not convenience — it is that an Entra `groups` claim
// carries an OBJECT GUID and an App Role arrives as its manifest value, so a
// hand-typed "Platform Engineering" binds NOTHING and fails open to whatever the
// unassigned default is. The picker renders Entry.DisplayName and stores
// Entry.ClaimValue, which is the whole point of the two fields.
//
// TIER: securityOps, not an open read (routes.go). A directory search discloses
// org structure — who exists, which groups they are in — and while a security
// admin assigns governance to these very people (so the disclosure is
// acceptable and stated), it is not something every authenticated member should
// be able to enumerate.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/directory"
)

// directoryUnconfiguredCode is the machine-readable code on the 503 that means
// "no connector is configured" — the console's ABSENT-MODE signal, which it
// answers by degrading the combobox to a plain text input with no error
// surface. It exists as a distinct code (the mint-conflict "code" idiom,
// internal.go) precisely so absent never renders as broken: an operator who has
// simply not enabled the feature must not see an error, and an operator whose
// tenant is actually failing must not see silence.
const directoryUnconfiguredCode = "directory_unconfigured"

// directorySearchResponse wraps the entries. An object rather than a bare array
// so a later field (a truncation flag, a degraded-kind note) is an additive
// change instead of a breaking one.
type directorySearchResponse struct {
	Results []directory.Entry `json:"results"`
}

// handleDirectorySearch answers GET /access/directory/search?q=&type=.
//
// AUDIT, decided (§I): the connector's FAILURES are audited (below); individual
// searches are NOT. Auditing a search would write one row per KEYSTROKE, which
// is both noise on a read that changes no state and, worse, a record of every
// name an admin typed while looking someone up — turning the append-only
// governance log into a keylogger of admin curiosity. The disclosure the tier
// permits is "a security admin may look people up"; it is not "every lookup is
// retained forever".
func (s *Server) handleDirectorySearch(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Directory == nil {
		writeDirectoryUnconfigured(w)
		return
	}

	// The >= MinQueryLen floor is a REQUEST CONTRACT here, enforced before the
	// limiter and before the connector: one or two characters match most of a
	// directory, so the call would spend a Graph round trip to produce noise.
	// The connector holds the same floor as its own backstop (it returns an
	// empty result rather than an error); this returns 400 because for an HTTP
	// caller a too-short q is a malformed request, and a silent empty 200 would
	// read to the console as "no matches" — the wrong thing to render.
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < directory.MinQueryLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("q must be at least %d characters", directory.MinQueryLen))
		return
	}

	// Absent type = KindAny, which is IN the v1 contract: the People-step Value
	// field is one kind-LESS input accepting an App Role, a group or an email.
	kind := directory.KindAny
	if t := strings.TrimSpace(r.URL.Query().Get("type")); t != "" {
		kind = directory.Kind(strings.ToLower(t))
		if !kind.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown type %q: want user, group, approle or any", t))
			return
		}
	}

	// Per-principal, not global: this endpoint is typed into, so one admin
	// holding a key down must not starve a second admin's picker. Checked after
	// the cheap validation above (a too-short q costs no upstream call, so it
	// should not cost a token either) and before the connector, which is the
	// thing the bucket exists to protect.
	if !s.dirLimiter.allow(principalFromRequest(r), s.cfg.Now()) {
		writeError(w, http.StatusTooManyRequests, "too many directory searches; slow down")
		return
	}

	entries, err := s.cfg.Directory.Search(r.Context(), q, kind)
	switch {
	case errors.Is(err, directory.ErrUnconfigured):
		// A connector that exists but reports itself unconfigured is the SAME
		// answer to the console as no connector at all — the feature is off,
		// nothing upstream failed. Unreachable in production (boot refuses a
		// half-configured connector rather than wiring one), kept because the
		// interface permits it and a silent 500 here would be a lie.
		writeDirectoryUnconfigured(w)
	case err != nil:
		s.auditDirectoryFailure(r, err)
		// 500, deliberately NOT the 503 above: "broken" and "not configured"
		// are different states and the console renders them differently — an
		// error toast versus a plain text input.
		writeError(w, http.StatusInternalServerError, "directory search failed")
	default:
		if entries == nil {
			entries = []directory.Entry{}
		}
		writeJSON(w, http.StatusOK, directorySearchResponse{Results: entries})
	}
}

func writeDirectoryUnconfigured(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"code":  directoryUnconfiguredCode,
		"error": "no directory provider is configured",
	})
}

// auditDirectoryFailure records that the connector could not answer. This is the
// audited half of §I's decision: an operator who enabled a directory read needs
// to see it failing (a lapsed secret, a revoked consent, an unreachable tenant)
// without an admin having to report "the picker is empty".
//
// The DATA IS CONTENT-FREE, and that is the same care the searches-are-not-
// audited decision is made with: the provider, the failing operation and the
// upstream status come from directory.ProviderError's typed fields — never the
// query string, which is the name of a person an admin was looking up.
//
// ponytail: bounded only by the per-principal search limiter above (~5 rows/sec
// per principal while a tenant is hard-down). If a broken tenant ever floods the
// log, give this its own slower principalLimiter — the type is right here.
func (s *Server) auditDirectoryFailure(r *http.Request, err error) {
	data := map[string]any{"provider": directoryProviderName}
	var pe *directory.ProviderError
	if errors.As(err, &pe) {
		data["provider"] = pe.Provider
		data["op"] = pe.Op
		if pe.Status != 0 {
			data["upstream_status"] = pe.Status
		}
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
		"directory.search_failed", r.URL.Path, "failure", mustJSON(data)))
}

// directoryProviderName is the fallback stamped on a failure audit when the
// error is not a ProviderError (so the row still names something). The connector
// selection itself is boot config; internal/api never chooses it.
const directoryProviderName = "directory"

// ─── per-principal rate limit ────────────────────────────────────────────────

const (
	// dirRatePerSec / dirBurst bound one principal's directory searches. ~5/sec
	// is a fast typist's keystroke rate, with a burst so a paste or a quick
	// correction is not refused; sustained automation past it is.
	dirRatePerSec = 5.0
	dirBurst      = 10.0
	// dirLimiterMaxPrincipals caps the bucket map. Keys are principals that
	// already passed requireSecurityOperator, so cardinality is "how many
	// admins this org has" and this bound is never reached in practice — it is
	// here so the map cannot grow without one at all.
	dirLimiterMaxPrincipals = 1024
)

// principalLimiter is a per-key token bucket, the per-principal twin of
// http.go's process-global authFailedLimiter (same refill arithmetic, keyed).
// Zero value is ready to use.
//
// ponytail: a plain map with a clear-when-full ceiling, not an LRU. Clearing
// refills every bucket, i.e. it fails OPEN for one instant — acceptable because
// reaching 1024 distinct SECURITY-ADMIN principals is not a load an attacker
// can manufacture (they would each need a session first). Swap in per-entry
// eviction if this limiter is ever reused on a route a member, or an
// unauthenticated caller, can reach.
type principalLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	last   time.Time
	tokens float64
}

func (l *principalLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil || len(l.buckets) >= dirLimiterMaxPrincipals {
		l.buckets = make(map[string]*tokenBucket)
	}
	b := l.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: dirBurst}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.tokens+elapsed*dirRatePerSec, dirBurst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
