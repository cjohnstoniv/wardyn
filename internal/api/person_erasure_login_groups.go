// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "context"

// loginGroupEraser is the store that holds the IdP group snapshots (key_domain_login_groups).
type loginGroupEraser interface {
	DeleteLoginGroups(ctx context.Context, principals []string) (int64, error)
}

// eraseLoginGroups deletes the person's IdP group snapshot under each name they hold credentials under
// (personNamespaces), as the credentials scope's last step, for an erasure and a purge alike. Their
// key-domain assignment stays: it is an audited governance fact, not their data.
func (s *Server) eraseLoginGroups(ctx context.Context, person string, want *erasureNames) error {
	st, ok := s.cfg.Store.(loginGroupEraser)
	if !ok {
		return nil
	}
	namespaces, err := s.personNamespaces(ctx, person, want)
	if err != nil {
		return err
	}
	_, err = st.DeleteLoginGroups(ctx, namespaces)
	return err
}
