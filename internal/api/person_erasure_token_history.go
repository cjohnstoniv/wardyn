// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "context"

// tokenHistoryEraser is the store that keeps the tokens Wardyn minted for a person: the Azure
// DevOps tokens of their runs (ado_run_pats) and the delegated tokens portals exchanged for them.
type tokenHistoryEraser interface {
	DeleteSettledRunPATsOf(ctx context.Context, owners []string) (deleted int64, left []string, err error)
	DeleteDelegatedTokensOf(ctx context.Context, principals []string) (int64, error)
}

// eraseTokenHistory deletes, under each name the person holds credentials under (personNamespaces),
// their delegated tokens and the history of their runs' Azure DevOps tokens, as the credentials
// scope's last step. It runs after the scope revoked those tokens: a row Azure DevOps has not yet
// confirmed revoked stays for the sweep that retries it, a row whose revoke it refused stays until
// its valid_to so a retry still says the token may work (the sweep then deletes it,
// DeleteErasedRefusedRunPATs), and live_left counts both, so an operator knows to run the erasure
// again once they expire. It adds its counts to data and returns the ids of the token rows it left.
func (s *Server) eraseTokenHistory(ctx context.Context, person string, want *erasureNames, data map[string]any) ([]string, error) {
	st, ok := s.cfg.Store.(tokenHistoryEraser)
	if !ok {
		return nil, nil
	}
	namespaces, err := s.personNamespaces(ctx, person, want)
	if err != nil {
		return nil, err
	}
	pats, left, err := st.DeleteSettledRunPATsOf(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	delegated, err := st.DeleteDelegatedTokensOf(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	data["pats_deleted"], data["delegated_deleted"], data["live_left"] = pats, delegated, int64(len(left))
	// The POST /runs Idempotency-Keys kept for them, direct or through a portal: a kept answer names their run.
	if ks, ok := s.cfg.Store.(runCreateKeyEraser); ok {
		keys, err := ks.DeleteRunCreateKeysOf(ctx, namespaces)
		if err != nil {
			return nil, err
		}
		data["run_create_keys_deleted"] = keys
	}
	return left, nil
}

// runCreateKeyEraser is the store that keeps POST /runs Idempotency-Keys (store.RunCreateKeyStore).
type runCreateKeyEraser interface {
	DeleteRunCreateKeysOf(ctx context.Context, principals []string) (int64, error)
}
