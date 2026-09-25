// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
)

// managedTokenCacheTTL bounds how long managedCredProvider serves a token it
// read without reading it again (credential-storage design §2.3a.4). Without
// it, every resolve of the managed token is one read of the organisation's
// store; with it, one read a minute per wardynd, whatever the number of runs.
const managedTokenCacheTTL = 60 * time.Second

// managedCredProvider serves the Wardyn-managed captured token through the SAME
// subscription.Provider interface the resident host token uses, so the injection
// sink treats them identically. It depends ONLY on the secret store (not the
// Server), so it can be constructed in main.go BEFORE api.New builds the Server
// — no construction cycle.
//
// No refresh path (v1): setup-token tokens are long-lived and Wardyn is not
// their owner, so Current never mutates state — it returns the stored token and
// lets Anthropic reject it on the wire if it has been revoked (fail closed at
// the sink, surfaced as a run failure + an aging warning in setup status).
//
// It is the one value cache wardynd keeps for a stored credential, and it
// caches only a successful read: a failure is never served twice. A capture or
// a disconnect evicts it (Server.evictManagedToken), so a replaced or deleted
// token is not served for the rest of the minute on this replica.
type managedCredProvider struct {
	store    secretstore.Store
	provider string

	mu       sync.Mutex // also single-flights the fill
	cached   subscription.Token
	cachedAt time.Time
}

// NewManagedCredProvider builds a managed subscription provider over store for a
// provider id (e.g. "anthropic"). Returns nil when store is nil (managed mode
// simply unavailable).
func NewManagedCredProvider(store secretstore.Store, provider string) subscription.Provider {
	if store == nil {
		return nil
	}
	return &managedCredProvider{store: store, provider: provider}
}

func (p *managedCredProvider) read(ctx context.Context) (subscription.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached.Value != "" && time.Since(p.cachedAt) < managedTokenCacheTTL {
		return p.cached, nil
	}
	p.cached = subscription.Token{}
	tok, err := p.fetch(ctx)
	if err != nil {
		return subscription.Token{}, err
	}
	p.cached, p.cachedAt = tok, time.Now()
	return tok, nil
}

func (p *managedCredProvider) fetch(ctx context.Context) (subscription.Token, error) {
	// p.store is the operator-wide managed credential (NewManagedCredProvider's
	// caller passes the raw, unscoped store) — not per-principal.
	raw, err := p.store.Get(ctx, harnessCredSecretName(p.provider))
	if errors.Is(err, secretstore.ErrNotFound) {
		return subscription.Token{}, fmt.Errorf("no managed %s credential connected", p.provider)
	}
	if err != nil {
		// A store-layer failure (decrypt/age-key mismatch, backend down) is NOT
		// "not connected" — surface it distinctly so the sink fails closed on a
		// real error rather than silently reading as "unconfigured".
		return subscription.Token{}, fmt.Errorf("read managed %s credential: %w", p.provider, err)
	}
	var blob managedCredBlob
	if uerr := json.Unmarshal(raw, &blob); uerr != nil {
		return subscription.Token{}, fmt.Errorf("parse managed credential: %w", uerr)
	}
	if strings.TrimSpace(blob.Token) == "" {
		return subscription.Token{}, fmt.Errorf("managed %s credential is empty; reconnect via container login", p.provider)
	}
	// ExpiresAt zero = "no machine-readable expiry": the sink gives the proxy a
	// stored-key expiry instead (storedKeyTTL), so a revoked token stops being
	// injected on a clock of Wardyn's own.
	return subscription.Token{Value: blob.Token}, nil
}

func (p *managedCredProvider) evict() {
	p.mu.Lock()
	p.cached = subscription.Token{}
	p.mu.Unlock()
}

// Current returns the managed token (no refresh — see type doc).
func (p *managedCredProvider) Current(ctx context.Context) (subscription.Token, error) {
	return p.read(secretstore.WithPurpose(ctx, secretstore.PurposeManagedToken))
}

// Peek reads the store like Current (no refresh side effect to avoid), but
// neither uses nor fills the cache: its callers only ask whether a token is
// there (managedInjectReady), so its read is recorded as a status read, and
// the cache only ever holds a token read for injection.
func (p *managedCredProvider) Peek() (subscription.Token, error) {
	return p.fetch(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus))
}

// evictManagedToken drops the managed provider's cached token, so the next
// resolve reads the store again.
func (s *Server) evictManagedToken() {
	if p, ok := s.cfg.ManagedToken.(*managedCredProvider); ok {
		p.evict()
	}
}

// forgetCredential lets go of what wardynd holds in memory for the credential
// (owner, name) once it is deleted: its process-wide mask copies, which are
// retired and then swept (secretmask.Registry.EvictGlobal), and the managed
// token cache.
func (s *Server) forgetCredential(owner, name string) {
	s.cfg.MaskRegistry.EvictGlobal(owner, name, s.cfg.Now())
	s.evictManagedToken()
}
