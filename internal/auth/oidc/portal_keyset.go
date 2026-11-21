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
	// portalCauseCanceled is a caller that left while it waited. It says nothing about the issuer's health.
	portalCauseCanceled = "request_canceled"
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
	payload []byte          // set only after the signature verified
	typ     portalHeaderTyp // the verified token's header typ, kept with whether it was present
	fresh   portalFresh     // the key fetch this exchange itself waited on, if any
	kid     string          // the key the signature verified under, and where it came from
	key     jose.JSONWebKey
	gen     uint64
}

// portalHeaderTyp is a token's typ header with its presence: a present header of any other shape is not an
// absent one.
type portalHeaderTyp struct {
	present  bool
	isString bool
	value    string
}

// portalFresh names the key fetch an exchange waited on, so the exchange that obtained fresh keys and still lacks
// its kid gets a plain refusal and not a cooldown answer.
type portalFresh struct {
	has bool
	seq uint64
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

// portalKeyFetch is one shared fetch of the key document. Every request that needs keys while it runs waits for
// it, and a caller that leaves stops waiting only for itself: the fetch has its own bounded context.
type portalKeyFetch struct {
	done chan struct{}
	err  error
}

// portalKeySet is Wardyn's own key set for one verifier. It fetches only on an unknown kid it has not seen
// removed, at most once per cooldown however many requests ask, and a successful fetch replaces the set.
type portalKeySet struct {
	clock func() time.Time
	fetch func(ctx context.Context) ([]jose.JSONWebKey, error)
	// onFail is told of every failed fetch, with where it came from ("scheduled" or "unknown_kid").
	onFail func(source string, err error)
	// afterSelect is a test hook, called once a key is selected and before the signature is checked.
	afterSelect func()

	mu        sync.RWMutex
	keys      map[string]jose.JSONWebKey
	removed   map[string]bool // kids the last successful fetch removed
	okAt      time.Time       // set only by a successful fetch
	attemptAt time.Time       // the last attempt, success or not: the cooldown runs from it
	seq       uint64          // fetches completed
	gen       uint64          // bumped each time the key map is replaced or dropped; the maps are never edited in place
	lastErr   error
	failed    bool // the last fetch failed; until one succeeds nothing verifies
	inflight  *portalKeyFetch
}

func newPortalKeySet(clock func() time.Time, fetch func(context.Context) ([]jose.JSONWebKey, error)) *portalKeySet {
	return &portalKeySet{clock: clock, fetch: fetch}
}

// startLocked begins the one fetch, with mu held and no fetch in flight. The cooldown runs from this moment.
func (ks *portalKeySet) startLocked(source string) *portalKeyFetch {
	f := &portalKeyFetch{done: make(chan struct{})}
	ks.inflight = f
	ks.attemptAt = ks.clock()
	go ks.run(f, source)
	return f
}

func (ks *portalKeySet) run(f *portalKeyFetch, source string) {
	ctx, cancel := context.WithTimeout(context.Background(), portalFetchTimeout)
	defer cancel()
	keys, err := ks.fetch(ctx)
	ks.mu.Lock()
	ks.seq++
	if err != nil {
		ks.lastErr, ks.failed = err, true
	} else {
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
		ks.keys, ks.removed, ks.okAt, ks.lastErr, ks.failed = next, removed, ks.clock(), nil, false
		ks.gen++
	}
	ks.inflight = nil
	ks.mu.Unlock()
	f.err = err
	if err != nil && ks.onFail != nil {
		ks.onFail(source, err)
	}
	close(f.done)
}

// refresh fetches now, joining a fetch already in flight, and waits for it. The caller decides whether the
// cooldown allows it.
func (ks *portalKeySet) refresh(ctx context.Context, source string) error {
	ks.mu.Lock()
	f := ks.inflight
	if f == nil {
		f = ks.startLocked(source)
	}
	ks.mu.Unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drop forgets every key: the verifier it served is gone, or its keys address changed.
func (ks *portalKeySet) drop() {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.keys, ks.removed, ks.okAt, ks.failed, ks.lastErr = nil, nil, time.Time{}, false, nil
	ks.gen++
}

// portalKeyCheck is the answer of recheck.
type portalKeyCheck int

const (
	portalKeyStillGood portalKeyCheck = iota
	portalKeyUnhealthy
	portalKeyWithdrawn
)

// recheck is the success-point test of an exchange that selected key sel (kid) from generation gen: the key set
// must not have failed since, and if the key map was replaced since, the same key must still be in it.
func (ks *portalKeySet) recheck(kid string, sel jose.JSONWebKey, gen uint64) portalKeyCheck {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	if ks.failed {
		return portalKeyUnhealthy
	}
	if ks.gen == gen {
		return portalKeyStillGood
	}
	cur, ok := ks.keys[kid]
	if !ok {
		return portalKeyWithdrawn
	}
	a, aok := cur.Key.(*rsa.PublicKey)
	b, bok := sel.Key.(*rsa.PublicKey)
	if !aok || !bok || !a.Equal(b) {
		return portalKeyWithdrawn
	}
	return portalKeyStillGood
}

func (ks *portalKeySet) state() (okAt, attemptAt time.Time, failed bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.okAt, ks.attemptAt, ks.failed
}

func (ks *portalKeySet) seqNow() uint64 {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.seq
}

// portalKeyLookup says what a lookup found.
type portalKeyLookup struct {
	key   jose.JSONWebKey
	gen   uint64 // the generation of the key map the key was taken from
	found bool
	fault *portalKeyFault
}

// lookup finds the key for kid, fetching only when the kid is unknown, has not been seen removed, and the
// cooldown allows. A kid a successful fetch removed is refused at once, inside the cooldown too. An exchange
// that itself waited on the latest successful fetch and still lacks its kid is told so plainly; a later one for a
// never-seen kid inside the cooldown is told to retry.
func (ks *portalKeySet) lookup(ctx context.Context, kid string, fresh portalFresh) portalKeyLookup {
	ks.mu.RLock()
	seq0, gen := ks.seq, ks.gen
	k, ok := ks.keys[kid]
	gone := ks.removed[kid]
	ks.mu.RUnlock()
	if ok {
		return portalKeyLookup{key: k, gen: gen, found: true}
	}
	if gone || kid == "" {
		return portalKeyLookup{}
	}
	ks.mu.Lock()
	k, ok = ks.keys[kid]
	gone = ks.removed[kid]
	seq, lastErr, attemptAt, gen := ks.seq, ks.lastErr, ks.attemptAt, ks.gen
	f := ks.inflight
	if ok || gone {
		ks.mu.Unlock()
		return portalKeyLookup{key: k, gen: gen, found: ok}
	}
	if f == nil {
		switch {
		case seq != seq0 || (fresh.has && seq == fresh.seq):
			// A fetch finished that this request waited for or took part in: its answer is this request's answer.
			ks.mu.Unlock()
			if lastErr != nil {
				return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysUnreachable}}
			}
			return portalKeyLookup{}
		case !attemptAt.IsZero() && attemptAt.Add(portalKeyCooldown).After(ks.clock()):
			ks.mu.Unlock()
			return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysCooldown, retryAfter: attemptAt.Add(portalKeyCooldown).Sub(ks.clock())}}
		}
		f = ks.startLocked("unknown_kid")
	}
	ks.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseCanceled}}
	}
	if f.err != nil {
		return portalKeyLookup{fault: &portalKeyFault{cause: portalCauseKeysUnreachable}}
	}
	ks.mu.RLock()
	k, ok = ks.keys[kid]
	gen = ks.gen
	ks.mu.RUnlock()
	return portalKeyLookup{key: k, gen: gen, found: ok}
}

// VerifySignature implements the go-oidc key set interface for RS256 tokens.
func (ks *portalKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	box := portalBoxFrom(ctx)
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(jws.Signatures) != 1 {
		return nil, errPortalBadToken
	}
	hdr := jws.Signatures[0].Header
	got := ks.lookup(ctx, hdr.KeyID, box.fresh)
	if got.fault != nil {
		box.fault = got.fault
		return nil, got.fault
	}
	if !got.found {
		return nil, errPortalBadToken
	}
	if ks.afterSelect != nil {
		ks.afterSelect()
	}
	payload, err := jws.Verify(got.key.Key)
	if err != nil {
		return nil, errPortalBadToken
	}
	box.payload, box.kid, box.key, box.gen = payload, hdr.KeyID, got.key, got.gen
	typ, present := hdr.ExtraHeaders[jose.HeaderType]
	str, isStr := typ.(string)
	box.typ = portalHeaderTyp{present: present, isString: isStr, value: str}
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
