// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// SubjectKeys is the per-subject key manager over this store's pool and KEKs.
// It is always live, whatever WARDYN_PRINCIPAL_KEYS says: that setting governs
// only whether credential rows are written under these keys.
func (s *Store) SubjectKeys() *subjectkey.Manager { return s.subjects }

// initSubjects arms SubjectKeys once the store's KEKs are set.
func (s *Store) initSubjects() {
	s.subjects = subjectkey.New(s.pool, subjectkey.Resolver{Writer: s.pkWriter, Reader: s.pkReader})
}

// pkWriter is the KEK a new principal key in domain is wrapped under: the
// credential KEK, the key service when it writes, else the local credential
// KEK. Boot keys never come here (their owner is "").
func (s *Store) pkWriter(domain string) (kek.KEK, error) {
	if domain != subjectkey.DomainDefault {
		return nil, fmt.Errorf("pg secretstore: unknown key domain %q", domain)
	}
	switch {
	case s.serviceWrites:
		return s.service, nil
	case s.kek != nil:
		return s.kek, nil
	}
	return nil, errors.New("pg secretstore: principal keys need a key to wrap under: WARDYN_AGE_KEY, or a WARDYN_KEK key service (transit or azurekv)")
}

// pkReader is the KEK that opens a principal key wrapped under kekID, by its
// exact id: the credential key service, or the local credential KEK. Never the
// pre-split or platform KEK, which wrote no principal key.
func (s *Store) pkReader(domain, kekID string) (kek.KEK, error) {
	if domain != subjectkey.DomainDefault {
		return nil, fmt.Errorf("pg secretstore: unknown key domain %q", domain)
	}
	switch {
	case s.service != nil && kekID == s.service.ID():
		return s.service, nil
	case s.kek != nil && kekID == s.kek.ID():
		return s.kek, nil
	}
	return nil, fmt.Errorf("pg secretstore: a principal key is sealed under key %q, which this wardynd is not configured to reach (WARDYN_KEK and WARDYN_AGE_KEY name the keys)", kekID)
}
