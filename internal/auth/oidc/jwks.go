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

// tolerantJWKSTransport turns one unparseable JWKS entry into a skipped key
// instead of a total SSO outage: go-oidc's UnmarshalJSON is all-or-nothing, so
// one bad key (e.g. a malformed Ed25519 entry, which go-jose v4.1.5 now
// correctly rejects) would lock out every user over a key Wardyn never needed.
// This sanitises only the document, using the same per-entry decode go-oidc
// would use, so verification itself is untouched: it can only keep a key
// working, never admit a signature that would otherwise be refused. Surviving
// entries are re-emitted VERBATIM (json.RawMessage) to avoid remarshal loss.
type tolerantJWKSTransport struct {
	base http.RoundTripper
}

// RoundTrip fetches through base and, when readable, returns the JWK Set with
// unparseable entries removed. Any shape it's unsure about (non-200, oversized
// body, no "keys" member, non-JSON) is returned UNCHANGED.
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
// what was dropped. It returns body UNCHANGED when it cannot confidently
// rewrite the document, or when NOTHING survived — serving `{"keys":[]}` there
// would trade go-jose's precise error for a generic one while login stays
// equally broken. This only ever turns a total outage into a partial key set,
// never a quieter total outage.
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
// through: base's behaviour layered with the per-key JWKS tolerance. It is a
// COPY of base (nil defaults like http.DefaultClient), not a fresh client, so
// a caller's timeout, cookie jar or redirect policy survives — only the
// transport changes. Handed only to gooidc.NewRemoteKeySet, so this filter
// never touches the discovery/token/userinfo docs.
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
