// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// SubjectKeys is the per-subject key manager over this store's pool and KEKs.
// It is always live, whatever WARDYN_PRINCIPAL_KEYS says: that setting governs
// only whether credential rows are written under these keys.
func (s *Store) SubjectKeys() *subjectkey.Manager { return s.subjects }

// initSubjects arms SubjectKeys once the store's KEKs are set. A store with a
// pool places each owner's next generation by the key-domain assignments.
func (s *Store) initSubjects() {
	r := subjectkey.Resolver{Writer: s.pkWriter, Reader: s.pkReader}
	if s.pool != nil {
		r.Domain = keydomain.NewService(s.pool, s.domainNames()).Domain
	}
	s.subjects = subjectkey.New(s.pool, r)
}

// domainNames is the declared domains, sorted.
func (s *Store) domainNames() []string {
	names := make([]string, 0, len(s.domains))
	for n := range s.domains {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// VerifyKeyDomains is the boot check of key domains: no live principal key may
// name a domain the file does not declare, and none may name a kek_id its
// domain no longer reaches. Both refusals count the rows and name the remedy,
// which is never to delete them.
func (s *Store) VerifyKeyDomains(ctx context.Context) error {
	return subjectkey.Verify(ctx, s.pool, s.domainNames(), s.pkReader)
}

// pkWriter is the KEK a new principal key in domain is wrapped under: the
// declared domain's own key, or for the default domain the credential KEK, the
// key service when it writes, else the local credential KEK. Boot keys never
// come here (their owner is "").
func (s *Store) pkWriter(domain string) (kek.KEK, error) {
	if domain != subjectkey.DomainDefault {
		if k, ok := s.domains[domain]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("pg secretstore: key domain %q is not declared in WARDYN_KEY_DOMAINS_FILE", domain)
	}
	switch {
	case s.serviceWrites:
		return s.service, nil
	case s.kek != nil:
		return s.kek, nil
	}
	return nil, errors.New("pg secretstore: principal keys need a key to wrap under: WARDYN_AGE_KEY, or a WARDYN_KEK key service (transit or azurekv)")
}

// pkReader is the KEK that opens a principal key wrapped under kekID in domain,
// by its exact id: the domain's own key, or for the default domain the
// credential key service or the local credential KEK. Never the pre-split or
// platform KEK, which wrote no principal key, and never another domain's key.
func (s *Store) pkReader(domain, kekID string) (kek.KEK, error) {
	if domain != subjectkey.DomainDefault {
		k, ok := s.domains[domain]
		switch {
		case !ok:
			return nil, fmt.Errorf("pg secretstore: key domain %q is not declared in WARDYN_KEY_DOMAINS_FILE", domain)
		case kekID != k.ID():
			return nil, fmt.Errorf("pg secretstore: a principal key in key domain %q is sealed under key %q, which that domain no longer reaches (it names %q)", domain, kekID, k.ID())
		}
		return k, nil
	}
	switch {
	case s.service != nil && kekID == s.service.ID():
		return s.service, nil
	case s.kek != nil && kekID == s.kek.ID():
		return s.kek, nil
	}
	return nil, fmt.Errorf("pg secretstore: a principal key is sealed under key %q, which this wardynd is not configured to reach (WARDYN_KEK and WARDYN_AGE_KEY name the keys)", kekID)
}

// readDomainVersions reads each declared key domain's latest key version into
// res (DomainKeyServices, DomainKeyVersions) and latest, which keys it by the
// domain's kek_id so a principal key of that domain is moved only when behind.
func (s *Store) readDomainVersions(ctx context.Context, res *RewrapResult, latest map[string]string) error {
	for _, name := range s.domainNames() {
		k := s.domains[name]
		n, err := latestVersion(ctx, k)
		if err != nil {
			return fmt.Errorf("pg secretstore: rewrap: read the latest version of %s (key domain %q): %w", k.ID(), name, err)
		}
		if res.DomainKeyServices == nil {
			res.DomainKeyServices, res.DomainKeyVersions = map[string]string{}, map[string]string{}
		}
		res.DomainKeyServices[name] = k.ID()
		if n != "" {
			res.DomainKeyVersions[name], latest[k.ID()] = n, n
		}
	}
	return nil
}
