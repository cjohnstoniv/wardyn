// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// If-Match/ETag optimistic concurrency for the whole-document-replace surfaces
// PUT /permissions/enforcement and PUT /site-config. Their in-process mutexes
// (handlePutSiteConfig's siteConfigMu, handlePutCapabilityEnforcement's
// capEnforcementMu) already stop two concurrent PUTs clobbering each other;
// If-Match adds a CLIENT-side guarantee: a caller that read the document is
// told 412 rather than silently overwriting a write that landed between its GET
// and its PUT. A caller that sends no If-Match works as before (additive).
//
// The ETag is a content hash, never a stored version column: no migration, and
// correct for any writer through the same Get/Put pair (including `wardyn
// site-config apply`), with nothing new to keep in sync.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// computeETag hashes v's canonical JSON encoding into a strong ETag value,
// quoted per RFC 9110 (a bare hash is not a valid ETag token).
// encoding/json sorts map keys on marshal, so this is deterministic for the
// map[string]bool enforcement document exactly as it is for the
// struct-shaped SiteConfig.
func computeETag(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// v is always a value this package marshals for its own JSON response
		// moments later — a marshal failure here fails the request anyway, and
		// an empty ETag simply never satisfies a real If-Match (fail closed).
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// ifMatchSatisfied reports whether r's If-Match header (if any) permits a
// write against a document whose CURRENT etag is current. No header is the
// "keep working" case — always permitted, matching how every caller of these
// endpoints behaves today. "*" (RFC 9110's "any representation") is always
// satisfied too: both documents this guards always exist in some form (a
// never-configured operator still gets a real zero-value row/map from GET),
// so there is never a "no representation yet" case for "*" to refuse.
func ifMatchSatisfied(r *http.Request, current string) bool {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return true
	}
	// A real client round-trips exactly the value the prior GET's ETag header
	// handed it, quotes included; also accept a caller that hand-typed it
	// without quotes, and a weak-comparison "W/" prefix — this guard's whole
	// job is catching genuine staleness, not quoting pedantry.
	for _, tok := range strings.Split(h, ",") {
		tok = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tok), "W/"))
		if `"`+strings.Trim(tok, `"`)+`"` == current {
			return true
		}
	}
	return false
}
