// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	jose "github.com/go-jose/go-jose/v4"
)

// maxTolerableJWKSBytes bounds the JWKS document this package will read into
// memory to inspect. A document above the cap is not rejected — it is streamed
// straight through untouched, only losing the per-key tolerance.
const maxTolerableJWKSBytes = 1 << 20

// tolerantJWKSTransport makes ONE JWKS entry that this JOSE stack cannot parse
// a skipped key instead of a total SSO outage: go-oidc hands the whole document
// to jose.JSONWebKeySet.UnmarshalJSON, which is all-or-nothing, so one bad entry
// (e.g. a malformed Ed25519 key, since go-jose v4.1.5 correctly rejects those
// instead of silently zero-padding them) locks every user out over a key Wardyn
// never needed.
//
// The fix sanitises only the DOCUMENT: it unmarshals each entry with the same
// jose.JSONWebKey.UnmarshalJSON go-oidc would use and drops the ones that call
// rejects. Key selection, algorithm checks and verification stay untouched in
// go-oidc/go-jose, so this can only make surviving keys still work, never admit
// a signature that would otherwise be refused. Surviving entries are re-emitted
// VERBATIM (json.RawMessage) so nothing is lost to a re-marshal round trip.
type tolerantJWKSTransport struct {
	base http.RoundTripper
}

// RoundTrip fetches through base and, when the answer is a readable JWK Set,
// returns it with the unparseable entries removed. Every early return hands
// back the response UNCHANGED, so any shape this function is unsure about
// (non-200, oversized body, no "keys" member, non-JSON body) behaves exactly
// as it did before this layer existed.
func (t *tolerantJWKSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return resp, err
	}

	// Read only up to the cap; an oversized body is put back in front of the
	// unread remainder and streamed through untouched.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTolerableJWKSBytes+1))
	if readErr != nil || len(body) > maxTolerableJWKSBytes {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()

	filtered, dropped := filterJWKS(body)
	for _, d := range dropped {
		// One line per dropped entry, per fetch (RemoteKeySet caches and refetches rarely).
		slog.Warn("oidc: skipping a JWKS entry this JOSE stack cannot parse; the remaining keys still verify logins",
			"jwks_uri", req.URL.String(), "kid", d.kid, "kty", d.kty, "error", d.err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(filtered))
	resp.ContentLength = int64(len(filtered))
	resp.Header = resp.Header.Clone()
	resp.Header.Del("Content-Length")
	return resp, nil
}

// droppedJWK is one skipped entry, for the WARN line.
type droppedJWK struct {
	kid, kty string
	err      error
}

// filterJWKS returns body with every unparseable "keys" entry removed, plus
// what was dropped. It returns body UNCHANGED (nothing dropped) whenever it
// cannot confidently rewrite the document, or when NOTHING survived — serving
// `{"keys":[]}` there would trade go-jose's precise error for go-oidc's generic
// one while login stays equally broken, so the operator keeps the error that
// names the key to fix. This only ever converts a total outage into a partial
// key set, never a quieter total outage.
func filterJWKS(body []byte) (out []byte, dropped []droppedJWK) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, nil
	}
	rawKeys, ok := doc["keys"]
	if !ok {
		return body, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rawKeys, &entries); err != nil {
		return body, nil
	}

	kept := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		// Same call go-oidc's own decode makes, so this filter cannot drift from
		// what the JOSE stack would have accepted.
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(e); err != nil {
			var hdr struct {
				Kid string `json:"kid"`
				Kty string `json:"kty"`
			}
			_ = json.Unmarshal(e, &hdr) // best effort: the entry is malformed by definition
			dropped = append(dropped, droppedJWK{kid: hdr.Kid, kty: hdr.Kty, err: err})
			continue
		}
		kept = append(kept, e)
	}
	if len(dropped) == 0 || len(kept) == 0 {
		return body, nil
	}

	keysJSON, err := json.Marshal(kept)
	if err != nil {
		return body, nil
	}
	doc["keys"] = keysJSON
	out, err = json.Marshal(doc)
	if err != nil {
		return body, nil
	}
	return out, dropped
}

// newTolerantJWKSClient returns the HTTP client the ID-token key set fetches
// through: base's behaviour (including the split-horizon rewrite transport,
// when configured) with the per-key JWKS tolerance layered on top. It is a
// COPY of base, not a fresh client, so a caller-supplied timeout, cookie jar
// or redirect policy is preserved — only the transport changes. base may be
// nil (production default), matching http.DefaultClient's behaviour. The
// client is handed only to gooidc.NewRemoteKeySet, which requests nothing but
// the JWKS URL, scoping this filter away from the discovery/token/userinfo docs.
func newTolerantJWKSClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &tolerantJWKSTransport{base: rt}
	return c
}
