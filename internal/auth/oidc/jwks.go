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
// memory to inspect it. A real key set is a few hundred bytes per key and IdPs
// publish single digits of them; a megabyte is four orders of magnitude of
// headroom. A document ABOVE the cap is not rejected — it is streamed straight
// through untouched, so an unusual-but-legitimate IdP behaves exactly as it did
// before this file existed and only loses the per-key tolerance.
const maxTolerableJWKSBytes = 1 << 20

// tolerantJWKSTransport makes ONE JWKS entry that this JOSE stack cannot parse
// a skipped key instead of a total SSO outage.
//
// THE FAILURE. go-oidc hands the whole JWKS document to
// jose.JSONWebKeySet.UnmarshalJSON, which is all-or-nothing: one entry it
// cannot decode fails the DECODE, so RemoteKeySet loads NO keys and every
// id_token signature check fails. Every human is locked out of the console by
// one key Wardyn never needed and cannot control — the IdP's key set is the
// IdP's to publish. go-oidc v3.21.0 narrowed this to "unrepresentable kty/crv"
// (Ed448, X448, an unknown kty) by filtering those before the JOSE stack sees
// them, and quotes RFC 7517 section 5 as its reason:
//
//	Implementations SHOULD ignore JWKs within a JWK Set that use "kty" values
//	that are not understood by them, THAT ARE MISSING REQUIRED MEMBERS, OR FOR
//	WHICH VALUES ARE OUT OF THE SUPPORTED RANGES.
//
// It implements the first clause only. A malformed key of a SUPPORTED type —
// clauses two and three: an EC key with a short "x", an RSA key missing "n", an
// Ed25519 key whose "x" is not 32 bytes — still takes down the entire set. The
// go-jose v4.1.5 bump this wave shipped for its seven upstream security fixes
// MOVED malformed Ed25519 keys into that unprotected class (upstream #250,
// "Reject malformed Ed25519 JWKs"): at v4.1.4 such an entry was silently
// zero-padded and login survived, at v4.1.5 it fails the whole set. That
// rejection is CORRECT — a wrong-length key is not a key — and reverting it
// would reopen the supply-chain window. So the tolerance belongs here, on the
// document, rather than in the version pin.
//
// THE FIX, and why it is a transport. Everything that decides whether a
// signature is good — key selection by kid, the allowed-algorithm set, the
// refresh-on-unknown-kid rotation path, the actual verification — stays inside
// go-oidc and go-jose, untouched. This layer only sanitises the DOCUMENT they
// are handed: it unmarshals each entry with the very same
// jose.JSONWebKey.UnmarshalJSON go-oidc would use, and drops the entries that
// call rejects. An entry go-jose refuses to parse can never verify anything, so
// removing it cannot admit a signature that would otherwise have been refused —
// the change is strictly "the surviving keys still work", never "more keys are
// trusted". Re-implementing VerifySignature to get the same tolerance would
// have put key selection and algorithm checking in Wardyn's hands, which is the
// opposite trade.
//
// The surviving entries are re-emitted VERBATIM (json.RawMessage), never
// re-marshalled: a round trip through jose.JSONWebKey would quietly drop
// members it does not model, and the point is to change nothing about the keys
// that are fine.
type tolerantJWKSTransport struct {
	base http.RoundTripper
}

// RoundTrip fetches through base and, when the answer is a readable JWK Set,
// returns it with the unparseable entries removed.
//
// Every early return hands back the response UNCHANGED, so any shape this
// function is not sure about (a non-200, an oversized body, a document with no
// "keys" member, a body that is not JSON) behaves exactly as it did before —
// go-oidc's own decode produces its own error, and this layer is invisible.
func (t *tolerantJWKSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return resp, err
	}

	// Read only up to the cap. A body at or over it is put back in front of the
	// unread remainder — original Close preserved — and streamed through, so an
	// oversized key set is not turned into a new failure mode by this file.
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
		// One line per dropped entry, per fetch. RemoteKeySet caches the key
		// set and refetches only on rotation or an unknown kid, so this cannot
		// become a per-request log. It names the entry and the reason because
		// an operator seeing it needs to know their IdP is publishing a key
		// nothing can use — the login working is not a reason to stay silent.
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
// what was dropped. It returns body UNCHANGED (and nothing dropped) whenever it
// cannot confidently rewrite the document: not an object, no "keys" member,
// "keys" not an array, or NOTHING survived.
//
// The nothing-survived case is deliberate. Serving `{"keys":[]}` there would
// replace go-jose's precise complaint ("invalid EC public key, wrong length for
// x") with go-oidc's generic "failed to verify id token signature", and login
// is equally broken either way — so the operator keeps the error that names the
// key to go fix. This layer only ever converts a TOTAL outage into a partial
// key set; it never invents a quieter total outage.
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
		// The SAME call go-oidc's own decode makes on this entry, so this
		// filter cannot drift from what the JOSE stack would have accepted:
		// if it parses here it parses there, and if it does not, the whole
		// set was going to fail.
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
// when one is configured) with the per-key JWKS tolerance layered on top.
//
// It is a COPY of base rather than a fresh client so a caller-supplied timeout,
// cookie jar or redirect policy is not silently dropped — the only thing that
// changes is the transport. base may be nil (production's default, no
// split-horizon and no test injection), which is http.DefaultClient's
// behaviour.
//
// The returned client is handed ONLY to gooidc.NewRemoteKeySet, which requests
// nothing but the JWKS URL — so the filter never sees the discovery document,
// the token endpoint or userinfo. filterJWKS is written to pass anything it
// does not recognise through untouched anyway, but scoping the client is what
// makes that a second line of defence rather than the only one.
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
