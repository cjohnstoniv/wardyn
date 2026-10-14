// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// upsertUserDriveGrantAs writes g with either verb: replacing the subject's allocation, or with
// additional beside its others. A super admin's write is writeUserDriveGrant. Anyone else allocates
// a drive with its own posture or narrower (types.DriveGrantWidens), judged inside the store's
// write transaction so a concurrent edit cannot land between the judgement and the write. removed
// is the drives a replacing write took from the subject. done means the response is written.
func (s *Server) upsertUserDriveGrantAs(w http.ResponseWriter, r *http.Request, g types.UserDriveGrant, stated, additional bool) (saved types.UserDriveGrant, removed []uuid.UUID, done bool, err error) {
	if s.isOperator(r.Context()) {
		saved, removed, err = s.writeUserDriveGrant(r.Context(), g, stated, additional)
		return saved, removed, false, err
	}
	tw, ok := s.cfg.Store.(store.UserDriveGrantTierWriter)
	if !ok {
		writeServerError(w, r, "upsert user drive grant", errors.New("the store cannot judge an allocation write"))
		return saved, nil, true, nil
	}
	saved, removed, err = tw.UpsertUserDriveGrantWithinTier(r.Context(), g, stated, additional)
	var widens *store.UserDriveGrantWidensError
	switch {
	case errors.As(err, &widens):
		field := widens.Field
		s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, "drive.grant."+field, field+": only a super admin may "+
			"set an allocation's directory name or make it more writable than its drive; allocate with the drive's own posture or read-only"))
		return saved, nil, true, nil
	case errors.Is(err, store.ErrUserDriveGrantChanged):
		writeErrorReason(w, http.StatusConflict, reasonUserDriveGrantConflict,
			"this subject's allocation changed while yours was being written; read it again and re-send")
		return saved, nil, true, nil
	}
	return saved, removed, false, err
}
