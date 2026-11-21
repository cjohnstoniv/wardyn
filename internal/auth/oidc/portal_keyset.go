// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// portalKeyCooldown is the shortest gap between two key fetches of one verifier.
const portalKeyCooldown = 30 * time.Second

// Causes of a 503 that the key set itself decides.
const (
	portalCauseKeysCooldown    = "keys_cooldown"
	portalCauseKeysUnreachable = "keys_unreachable"
	portalCauseIssuerDown      = "issuer_unreachable"
)

// errPortalBadToken is the key set's answer for a token that cannot be verified: never an outage.
var errPortalBadToken = errors.New("portal key set: token not verifiable")

// portalKeyFault is an outage answer (503). It travels in the per-request box, never in error text.
type portalKeyFault struct {
	cause      string
	retryAfter time.Duration
}

func (f *portalKeyFault) Error() string { return "portal key set: " + f.cause }

// portalBox is the per-request record the key set writes to. go-oidc flattens the errors a key set returns, so
// the category of a failure is read from here and never from error text.
type portalBox struct {
	fault   *portalKeyFault
	payload []byte // set only after the signature verified
	typ     string // the verified token's header typ
}

type portalBoxKey struct{}

func withPortalBox(ctx context.Context, b *portalBox) context.Context {
	return context.WithValue(ctx, portalBoxKey{}, b)
}

func portalBoxFrom(ctx context.Context) *portalBox {
	b, _ := ctx.Value(portalBoxKey{}).(*portalBox)
	if b == nil {
		return &portalBox{}
	}
	return b
}

// portalKeySet is Wardyn's own key set for one verifier. It fetches only on an unknown kid it has not seen
// removed, at most once per cooldown however many requests ask, and a successful fetch replaces the set.
type portalKeySet struct {
	clock func() time.Time
	fetch func(ctx context.Context) ([]jose.JSONWebKey, error)

	fetchMu sync.Mutex // one fetch at a time; a waiter takes the answer of the fetch it waited for

	mu        sync.RWMutex
	keys      map[string]jose.JSONWebKey
	removed   map[string]bool // kids the last successful fetch removed
	okAt      time.Time       // set only by a successful fetch
	attemptAt time.Time       // the last attempt, success or not: the cooldown runs from it
	seq       uint64          // fetches completed
	lastErr   error
	failed    bool // the last fetch failed; until one succeeds nothing verifies
}

func newPortalKeySet(clock func() time.Time, fetch func(context.Context) ([]jose.JSONWebKey, error)) *portalKeySet {
	return &portalKeySet{clock: clock, fetch: fetch}
}

// refresh fetches now, ignoring the cooldown. The hourly refresh and the retry after a failure use it.
func (ks *portalKeySet) refresh(ctx context.Context) error {
	ks.fetchMu.Lock()
	defer ks.fetchMu.Unlock()
	return ks.fetchLocked(ctx)
}

// fetchLocked is one fetch, with fetchMu held.
func (ks *portalKeySet) fetchLocked(ctx context.Context) error {
	now := ks.clock()
	keys, err := ks.fetch(ctx)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.attemptAt = now
	ks.seq++
	if err != nil {
		ks.lastErr, ks.failed = err, true
		return err
	}
	next := make(map[string]jose.JSONWebKey, len(keys))
	for _, k := range keys {
		next[k.KeyID] = k
	}
	removed := make(map[string]bool)
	for kid := range ks.keys {
		if _, ok := next[kid]; !ok {
			removed[kid] = true
		}
	}
	ks.keys, ks.removed, ks.okAt, ks.lastErr, ks.failed = next, removed, now, nil, false
	return nil
}

// drop forgets every key: the verifier it served is gone.
func (ks *portalKeySet) drop() {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.keys, ks.removed, ks.okAt, ks.failed, ks.lastErr = nil, nil, time.Time{}, false, nil
}

func (ks *portalKeySet) state() (okAt, attemptAt time.Time, failed bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.okAt, ks.attemptAt, ks.failed
}

// portalKeyLookup says what a lookup found.
type portalKeyLookup struct {
	key   jose.JSONWebKey
	found bool
	fault *portalKeyFault
}

// lookup finds the key for kid, fetching only when the kid is unknown, has not been seen removed, and the
// cooldown allows. A kid a successful fetch removed is refused at once, inside the cooldown too.
func (ks *portalKeySet) lookup(ctx context.Context, kid string) portalKeyLookup {
	ks.mu.RLock()
	seq0 := ks.seq
	k, ok := ks.keys[kid]
	gone := ks.removed[kid]
	ks.mu.RUnlock()
	if ok {
		return portalKeyLookup{key: k, found: true}
	}
	if gone || kid == "" {
		return portalKeyLookup{}
	}
	ks.fetchMu.Lock()
	defer ks.fetchMu.Unlock()
	ks.mu.RLock()
	k, ok = ks.keys[kid]
	gone = ks.removed[kid]
	seq, lastErr, attemptAt := ks.seq, ks.lastErr, ks.attemptAt
	ks.mu.RUnlock()
	if ok {
		return portalKeyLookup{key: k, found: true}
	}
	if gone {
		return portalKeyLookup{}
	}
	if seq != seq0 {
		// A fetch finished while this request waited: its answer is this request's answer.
		if lastErr != nil {
			return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysUnreachable}}
		}
		return portalKeyLookup{}
	}
	if wait := attemptAt.Add(portalKeyCooldown).Sub(ks.clock()); !attemptAt.IsZero() && wait > 0 {
		return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysCooldown, retryAfter: wait}}
	}
	if err := ks.fetchLocked(ctx); err != nil {
		return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysUnreachable}}
	}
	ks.mu.RLock()
	k, ok = ks.keys[kid]
	ks.mu.RUnlock()
	return portalKeyLookup{key: k, found: ok}
}

// VerifySignature implements the go-oidc key set interface for RS256 tokens.
func (ks *portalKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	box := portalBoxFrom(ctx)
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(jws.Signatures) != 1 {
		return nil, errPortalBadToken
	}
	hdr := jws.Signatures[0].Header
	got := ks.lookup(ctx, hdr.KeyID)
	if got.fault != nil {
		box.fault = got.fault
		return nil, got.fault
	}
	if !got.found {
		return nil, errPortalBadToken
	}
	payload, err := jws.Verify(got.key.Key)
	if err != nil {
		return nil, errPortalBadToken
	}
	box.payload = payload
	box.typ, _ = hdr.ExtraHeaders[jose.HeaderType].(string)
	return payload, nil
}

// portalFetchKeys fetches a key document and keeps the usable RS256 signing keys. An entry that does not parse
// is skipped, so one bad key never locks everyone out; a document with no usable key is a failed fetch.
func portalFetchKeys(ctx context.Context, client *http.Client, fetchURL string) ([]jose.JSONWebKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := portalGetJSON(ctx, client, fetchURL, &doc); err != nil {
		return nil, err
	}
	var out []jose.JSONWebKey
	seen := make(map[string]bool)
	for _, raw := range doc.Keys {
		var k jose.JSONWebKey
		if json.Unmarshal(raw, &k) != nil || !k.Valid() || !k.IsPublic() || k.KeyID == "" || seen[k.KeyID] {
			continue
		}
		if _, ok := k.Key.(*rsa.PublicKey); !ok || (k.Algorithm != "" && k.Algorithm != string(jose.RS256)) || (k.Use != "" && k.Use != "sig") {
			continue
		}
		seen[k.KeyID] = true
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "no usable keys"}
	}
	return out, nil
}
