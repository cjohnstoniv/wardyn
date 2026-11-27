// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package spiretest is an in-memory SPIRE Server (spire.Server) and a static trust bundle for the spire
// provider's tests. It speaks no gRPC: the kind walk proves the real adapter.
package spiretest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/cjohnstoniv/wardyn/internal/identity/spire"
)

// KeyID is the kid the fake signs with.
const KeyID = "spiretest-key"

// ErrDown is what every call returns while the server is down.
var ErrDown = errors.New("spiretest: server unavailable")

// Calls counts each Server method's calls.
type Calls struct{ Mint, Find, Create, Delete, List int }

// Server is an in-memory spire.Server.
type Server struct {
	mu      sync.Mutex
	td      spiffeid.TrustDomain
	alg     jose.SignatureAlgorithm
	key     crypto.Signer
	entries map[string]spire.Entry
	next    int
	down    bool
	block   bool
	calls   Calls
	events  []string
	// ControlPlane is what ListEntriesByID returns for any ID.
	ControlPlane []spire.Entry
}

var _ spire.Server = (*Server)(nil)

// NewServer returns a fake for td signing with alg (ES256, RS256 or PS256).
func NewServer(td string, alg jose.SignatureAlgorithm) (*Server, error) {
	t, err := spiffeid.TrustDomainFromString(td)
	if err != nil {
		return nil, err
	}
	var key crypto.Signer
	switch alg {
	case jose.ES256:
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case jose.RS256, jose.PS256:
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		return nil, fmt.Errorf("spiretest: alg %s is not faked", alg)
	}
	if err != nil {
		return nil, err
	}
	return &Server{td: t, alg: alg, key: key, entries: map[string]spire.Entry{}}, nil
}

// Key is the signing key, for tests that forge tokens with it.
func (s *Server) Key() crypto.Signer { return s.key }

// SetDown makes every call fail with ErrDown while down is true.
func (s *Server) SetDown(down bool) { s.mu.Lock(); s.down = down; s.mu.Unlock() }

// SetBlockList makes ListEntriesByID wait for its context to end, as a server that never answers.
func (s *Server) SetBlockList(block bool) { s.mu.Lock(); s.block = block; s.mu.Unlock() }

// Calls returns the call counts so far.
func (s *Server) Calls() Calls { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// Note appends an event to the ordered log Events returns (tests note their own steps beside the fake's).
func (s *Server) Note(ev string) { s.mu.Lock(); s.events = append(s.events, ev); s.mu.Unlock() }

// Events is the ordered log of creates, deletes and noted events.
func (s *Server) Events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// Entries returns the live entries.
func (s *Server) Entries() []spire.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]spire.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

// Sign signs a JWT-SVID for sub with the fake's key, as the server's mint does.
func (s *Server) Sign(sub, audience string, iat, exp time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: s.alg, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", KeyID))
	if err != nil {
		return "", err
	}
	return jwt.Signed(signer).Claims(jwt.Claims{Subject: sub, Audience: jwt.Audience{audience},
		IssuedAt: jwt.NewNumericDate(iat), Expiry: jwt.NewNumericDate(exp)}).Serialize()
}

func (s *Server) MintJWTSVID(_ context.Context, id spiffeid.ID, audience string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	s.calls.Mint++
	down := s.down
	s.mu.Unlock()
	if down {
		return "", ErrDown
	}
	now := time.Now()
	return s.Sign(id.String(), audience, now, now.Add(ttl))
}

func (s *Server) FindEntries(_ context.Context, sel spire.Selector) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.Find++
	if s.down {
		return nil, ErrDown
	}
	var ids []string
	for id, e := range s.entries {
		for _, x := range e.Selectors {
			if x == sel {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids, nil
}

func (s *Server) CreateEntry(_ context.Context, e spire.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.Create++
	if s.down {
		return ErrDown
	}
	s.next++
	e.ID = "entry-" + strconv.Itoa(s.next)
	s.entries[e.ID] = e
	s.events = append(s.events, "create "+e.SPIFFEID.String())
	return nil
}

func (s *Server) DeleteEntries(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.Delete++
	if s.down {
		return ErrDown
	}
	for _, id := range ids {
		delete(s.entries, id)
		s.events = append(s.events, "delete "+id)
	}
	return nil
}

func (s *Server) ListEntriesByID(ctx context.Context, _ spiffeid.ID) ([]spire.Entry, error) {
	s.mu.Lock()
	s.calls.List++
	down, block, cp := s.down, s.block, s.ControlPlane
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if down {
		return nil, ErrDown
	}
	return cp, nil
}

func (s *Server) Close() error { return nil }

// Source returns a static svidsource.Source holding the fake's JWT authority. It serves no X.509-SVID.
func (s *Server) Source() *Source {
	b := jwtbundle.New(s.td)
	b.AddJWTAuthority(KeyID, s.key.Public())
	return &Source{td: s.td, jwt: b}
}

// Source is a static trust bundle (svidsource.Source) for the provider's Verify.
type Source struct {
	td  spiffeid.TrustDomain
	jwt *jwtbundle.Bundle
}

func (b *Source) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	if td != b.td {
		return nil, fmt.Errorf("spiretest: no JWT bundle for %s", td)
	}
	return b.jwt, nil
}

func (b *Source) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return x509bundle.FromX509Authorities(td, []*x509.Certificate{}), nil
}

func (b *Source) GetX509SVID() (*x509svid.SVID, error) {
	return nil, errors.New("spiretest: no X.509-SVID")
}

func (b *Source) Close() error { return nil }
