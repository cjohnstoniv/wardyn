// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
)

// eraseMaskCopies is the mask_copies scope: the person's live consumers are
// fenced first and their masking manifests deleted (FenceSubject), then every
// committed value under the person, per-run and global, is tombstoned. It is
// done only when no row still holds ciphertext for the person. A durable owner
// generation refuses old global registrations; fresh work may require a retry.
func (s *Server) eraseMaskCopies(ctx context.Context, person string) (any, error) {
	runs, err := s.cfg.MaskManifests.FenceSubject(ctx, person)
	detail := map[string]any{"runs_fenced": len(runs)}
	if err != nil {
		return detail, err
	}
	// The run tokens the person's runs hold in Postgres go with the manifests: the fence just
	// set is what stops a replica writing one back (adorunpat.Save).
	if s.cfg.ADORunPATs != nil {
		n, err := s.cfg.ADORunPATs.DeleteOwner(ctx, person)
		detail["run_tokens"] = n
		if err != nil {
			return detail, err
		}
	}
	left, err := s.cfg.MaskRegistry.EraseOwner(ctx, person)
	if err != nil {
		return detail, err
	}
	if left > 0 {
		return detail, fmt.Errorf("%d masking values of the person were registered while the erase ran; retry", left)
	}
	return detail, nil
}
