// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// eraseMemberDrivesOf is the member_drives scope: it deletes every drive the
// person created, tombstoned ones included, through deleteMemberDrive, the
// path a delete takes, so the lifecycle check and the substrate's in-use check
// run and the row goes only once its object has. A drive that cannot be deleted
// stays, usable, and fails the scope by name; the rest are still deleted, and a
// retry finishes. A tombstone older than a minute is taken over, so a delete
// that died part way is finished by the retry.
func (s *Server) eraseMemberDrivesOf(ctx context.Context, person string) (any, error) {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return nil, fmt.Errorf("%w: the store keeps no member drives", erasure.ErrNotAvailable)
	}
	owned, err := ms.ListMemberDrivesOf(ctx, types.CanonicalUserSubject(person))
	if err != nil {
		return nil, err
	}
	deleted := 0
	var errs []error
	for _, o := range owned {
		d, started, err := ms.BeginMemberDriveDeleteAdmin(ctx, o.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // deleted since the list
		case errors.Is(err, store.ErrMemberDriveBusy):
			errs = append(errs, fmt.Errorf("member drive %q is being deleted by another request", o.Name))
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("member drive %q: %w", o.Name, err))
			continue
		}
		res := s.deleteMemberDrive(ctx, ms, d, started, memberDriveByAdmin)
		switch {
		case res.err != nil:
			errs = append(errs, fmt.Errorf("member drive %q: %w", d.Name, res.err))
		case res.driverErr != nil:
			errs = append(errs, fmt.Errorf("member drive %q is in use or not reclaimable and was kept: %w", d.Name, res.driverErr))
		case res.reason == reasonMemberDriveDeleteUnavailable:
			errs = append(errs, fmt.Errorf("member drive %q was kept: this server cannot delete its storage", d.Name))
		case res.reason != "":
			errs = append(errs, fmt.Errorf("member drive %q was kept: deleting its storage failed", d.Name))
		default:
			deleted++
		}
	}
	return map[string]any{"drives": deleted}, errors.Join(errs...)
}
