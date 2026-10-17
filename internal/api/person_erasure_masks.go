// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
)

// eraseMaskCopies is the mask_copies scope: the person's live consumers are
// fenced first and their masking manifests deleted (FenceSubject), then every
// committed value under the person, per-run and global, is tombstoned. Each
// name the person holds credentials under (personNamespaces: the principal and
// the entra forms proved theirs alone) is covered, since a run started under
// the entra form owns its manifest by that form. It is done only when no row
// still holds ciphertext for the person. A durable owner generation refuses old
// global registrations; fresh work may require a retry.
func (s *Server) eraseMaskCopies(ctx context.Context, person string, want *erasureNames) (any, error) {
	namespaces, err := s.personNamespaces(ctx, person, want)
	if err != nil {
		return nil, err
	}
	fenced := 0
	detail := map[string]any{"runs_fenced": 0}
	for _, ns := range namespaces {
		runs, err := s.cfg.MaskManifests.FenceSubject(ctx, ns)
		fenced += len(runs)
		detail["runs_fenced"] = fenced
		if err != nil {
			return detail, err
		}
	}
	// The run tokens the person's runs hold in Postgres go with the manifests: the fence just
	// set is what stops a replica writing one back (adorunpat.Save).
	if s.cfg.ADORunPATs != nil {
		tokens := 0
		for _, ns := range namespaces {
			n, err := s.cfg.ADORunPATs.DeleteOwner(ctx, ns)
			tokens += n
			detail["run_tokens"] = tokens
			if err != nil {
				return detail, err
			}
		}
	}
	for _, ns := range namespaces {
		left, err := s.cfg.MaskRegistry.EraseOwner(ctx, ns)
		if err != nil {
			return detail, err
		}
		if left > 0 {
			return detail, fmt.Errorf("%d masking values of the person were registered while the erase ran; retry", left)
		}
	}
	return detail, nil
}
