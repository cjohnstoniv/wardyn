// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "context"

// tokenHistoryEraser is the store that keeps the tokens Wardyn minted for a person: the Azure
// DevOps tokens of their runs (ado_run_pats) and the delegated tokens portals exchanged for them.
type tokenHistoryEraser interface {
	DeleteSettledRunPATsOf(ctx context.Context, owners []string) (int64, error)
	DeleteDelegatedTokensOf(ctx context.Context, principals []string) (int64, error)
}

// eraseTokenHistory deletes, under each name the person holds credentials under (personNamespaces),
// their delegated tokens and the history of their runs' Azure DevOps tokens, as the credentials
// scope's last step. It runs after the scope revoked those tokens: a row Azure DevOps has not yet
// confirmed revoked stays for the sweep that retries it.
func (s *Server) eraseTokenHistory(ctx context.Context, person string, want *erasureNames) error {
	st, ok := s.cfg.Store.(tokenHistoryEraser)
	if !ok {
		return nil
	}
	namespaces, err := s.personNamespaces(ctx, person, want)
	if err != nil {
		return err
	}
	if _, err := st.DeleteSettledRunPATsOf(ctx, namespaces); err != nil {
		return err
	}
	_, err = st.DeleteDelegatedTokensOf(ctx, namespaces)
	return err
}
